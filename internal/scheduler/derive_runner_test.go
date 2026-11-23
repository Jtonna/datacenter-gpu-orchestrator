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
// (a profile created, then deleted) while readers list the profiles and the
// needs. No read may time out, and the GGUF headers are read once per file,
// not once per configuration write.
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
	doc, err := s.Needs(ctxT(t))
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
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	}
	stop.Store(true)
	wg.Wait()
	if timedOut.Load() != 0 {
		t.Fatalf("%d reads timed out (%d answered)", timedOut.Load(), listings.Load())
	}
	// Work, not time: 400 configuration writes read no GGUF header again
	// (the churn profile's model was read at start).
	if got := needReads(needs) - readsBefore; got != 0 {
		t.Fatalf("%d GGUF header reads during 400 configuration writes", got)
	}
}

func needReads(c *ConfigNeeds) int {
	c.mu.Lock()
	defer supervise.Unlocked("test", c.mu.Unlock)
	return c.reads
}
