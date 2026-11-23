package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

// The readiness crash test's shape, in process: a scheduler with the real
// need provider (GGUF headers in a models folder), a store of about 80
// measurements and 300 profiles, mostly derived; 200 configuration writes
// (a profile created, then deleted), and every tenth cycle a load storing a
// new measurement, while readers list the profiles (status, then a plan
// per profile) and the needs. Measured: the longest time one event takes in
// the core loop (Scheduler.stepMax), which bounds every stall a read or a
// heartbeat can meet; and the GGUF headers read, once per file, never again
// after a configuration write or a new measurement.
func TestDerivedNeed_ConfigWritesAgainstListings(t *testing.T) {
	dir := t.TempDir()
	const models = 12
	for m := range models {
		if err := ggufgen.Write(filepath.Join(dir, fmt.Sprintf("model%02d.gguf", m)), ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8}); err != nil {
			t.Fatal(err)
		}
	}
	st := openNeedStore(t)
	rows := 0
	for m := range models {
		model := fmt.Sprintf("model%02d.gguf", m)
		fi, err := os.Stat(filepath.Join(dir, model))
		if err != nil {
			t.Fatal(err)
		}
		for i, ctx := range []int{2048, 8192, 32768} {
			for d := 1; d <= 3; d++ {
				if (m+i+d)%4 == 0 {
					continue
				}
				r := needstore.Record{Profile: fmt.Sprintf("m%d-%d-%d", m, ctx, d), Kind: string(core.KindLlama), Model: model, ModelBytes: fi.Size(),
					Ctx: ctx, Parallel: 1, KVQuant: "f16", TotalMiB: 1000 + ctx/8 + d*700, Devices: d, Cause: needstore.CauseLoad,
					MeasuredAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), Release: "0.1.3", LlamaBuild: "aaaaaaa1"}
				if err := st.Put(r); err != nil {
					t.Fatal(err)
				}
				rows++
			}
		}
	}
	clk := newFakeClock()
	view := newFakeView(clk)
	view.set(psNode("a", 2, 12000))
	cfg := testConfig()
	cfg.ModelsDir = dir
	for i := range 300 {
		ctx := []int{4096, 16384, 65536, 1024, 24576}[i%5]
		cfg.Profiles[fmt.Sprintf("p%03d", i)] = llamaP(fmt.Sprintf("p%03d", i), 0, func(p *core.Profile) { p.Model, p.Ctx = fmt.Sprintf("model%02d.gguf", i%models), ctx })
	}
	needs := &ConfigNeeds{Cfg: cfg}
	s, err := New(cfg, Options{Clock: clk, View: view, Prober: newFakeProber(view), Launcher: newFakeLauncher(), WorkerProber: newFakeWorkerProber(),
		NeedStore: st, Release: "0.1.3", LlamaBuild: "aaaaaaa1", ReleaseBuild: func(string) string { return "aaaaaaa1" },
		Needs: needs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	go s.Run(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	t0 := time.Now()
	doc, err := s.Needs(ctxT(t))
	t.Logf("first GET /api/needs after the start: %v; longest core event so far %v (%v)", time.Since(t0),
		time.Duration(s.stepMax.Load()), s.stepMaxEv.Load())
	if err != nil || len(doc.Rows) < rows || len(doc.DerivePairs) == 0 {
		t.Fatalf("needs %d rows (%d stored) %d pairs, %v", len(doc.Rows), rows, len(doc.DerivePairs), err)
	}
	derived := 0
	for name := range cfg.Profiles {
		if r, err := s.Plan(ctxT(t), name); err == nil && r.DerivedFromCtx > 0 {
			derived++
		}
	}
	if derived < 100 {
		t.Fatalf("only %d derived profiles: the test does not exercise the derivation", derived)
	}

	var stop atomic.Bool
	var timedOut, listings atomic.Int32
	var wg sync.WaitGroup
	read := func(what string, f func(ctx context.Context) error) {
		defer wg.Done()
		for !stop.Load() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) // a hang only: stalls are stepMax
			err := f(ctx)
			cancel()
			if err != nil {
				timedOut.Add(1)
				t.Errorf("%s: %v", what, err)
				return
			}
			listings.Add(1)
		}
	}
	// GET /api/profiles: the status, then a plan per profile; GET /api/needs
	profiles := func(ctx context.Context) error {
		if _, err := s.Status(ctx); err != nil {
			return err
		}
		for name := range s.Config().Profiles {
			if _, err := s.Plan(ctx, name); err != nil && ctx.Err() != nil {
				return err
			}
		}
		return nil
	}
	wg.Add(3)
	go read("profiles", profiles)
	go read("needs", func(ctx context.Context) error { _, err := s.Needs(ctx); return err })
	go read("models", func(ctx context.Context) error { _, err := s.LearnedRows(ctx); return err })
	readsBefore := needReads(needs)
	s.stepMax.Store(0)
	{
		nc := cloneCfg(cfg)
		nc.Revision = cfg.Revision + 1
		s.ApplyConfig(nc)
		t0 := time.Now()
		if _, err := s.Needs(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		t.Logf("first GET /api/needs after a configuration write: %v", time.Since(t0))
		cfg = nc
	}
	cur := cfg
	for i := range 200 {
		nc := cloneCfg(cur)
		nc.Revision = cur.Revision + 1
		name := fmt.Sprintf("churn%d", i)
		nc.Profiles[name] = llamaP(name, 0, func(p *core.Profile) { p.Model, p.Ctx = "model01.gguf", 49152 })
		s.ApplyConfig(nc)
		cur = nc
		if _, err := s.Needs(ctxT(t)); err != nil { // the write applied, and a read answered after it
			t.Fatal(err)
		}
		nc = cloneCfg(cur)
		nc.Revision = cur.Revision + 1
		delete(nc.Profiles, name)
		s.ApplyConfig(nc)
		cur = nc
		if i%10 == 0 { // a load ends and stores its measurement (the core's own step of it)
			m := i / 10 % models
			p := llamaP("load", 0, func(p *core.Profile) { p.Model, p.Ctx = fmt.Sprintf("model%02d.gguf", m), 4096+i*8 })
			if _, err := askRead(s, ctxT(t), "test: a load stores a measurement", func(c *schedCore) int {
				c.putLearned(&learnedNeed{TotalMiB: 1000 + p.Ctx/8 + 700, key: needKeyOf(p), profile: "load", devices: 1, modelBytes: c.needs_[fmt.Sprintf("p%03d", m)].FileBytes, at: c.now})
				return 0
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	if timedOut.Load() != 0 {
		t.Fatalf("%d reads failed (%d answered)", timedOut.Load(), listings.Load())
	}
	longest, bound := time.Duration(s.stepMax.Load()), 50*time.Millisecond
	if raceOn {
		bound = 250 * time.Millisecond // the race detector slows everything about 5 to 10 times
	}
	t.Logf("longest core event during the run: %v (%v); %d listings answered", longest, s.stepMaxEv.Load(), listings.Load())
	if longest > bound {
		t.Fatalf("the core loop stalled %v on one event (%v), bound %v", longest, s.stepMaxEv.Load(), bound)
	}
	// Work: 400 configuration writes and 20 new measurements read no GGUF
	// header again (every model's was read at start).
	if got := needReads(needs) - readsBefore; got != 0 {
		t.Fatalf("%d GGUF header reads during 400 configuration writes", got)
	}
}

func needReads(c *ConfigNeeds) int {
	c.mu.Lock()
	defer supervise.Unlocked("test", c.mu.Unlock)
	return c.reads
}
