package main

// DEV-ONLY: the frozen bench-* scenarios. A frozen scenario shows the
// Benchmarks screens in a state that is hard to catch live (a load at 40%,
// a conversation half way through a turn, a parallel step, a long queue).
// It is made with REAL code, not with hand-built documents:
//
//   - the real database is copied (VACUUM INTO) to a snapshot, which a
//     scenario may change through the real store (delete everything, add a
//     suite, lengthen a hold);
//
//   - a second REAL benchmark manager runs a suite on the snapshot against
//     the scripted scheduler (bench_scripted.go), which lets every step before
//     the chosen point finish at once and then holds the run there (a load
//     that stays at 40%, a request that streams part of its answer and stops);
//
//   - a second REAL api.Server on that manager and snapshot answers every
//     /api/bench/... request while the scenario is selected, and the WebSocket
//     relay sends that manager's bench frames instead of the real server's.
//
// So /api/bench/state, the run, its steps and phases, progress, eta and
// detail.members are the real progress model's output. Writes made in a
// frozen scenario go to the snapshot and are dropped when the scenario changes.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/api"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// frozenSpec describes a frozen bench scenario.
type frozenSpec struct {
	// Prepare changes the snapshot before the run (may be nil).
	Prepare func(st *bench.Store) error
	// Suite is the suite to run on the snapshot ("" runs nothing); Runs is how many runs of it to queue (default 1).
	Suite string
	Runs  int
	// FreezeLoad holds the load of a profile at a fraction; FreezeChat makes
	// the nth request (1-based, counted per profile) and every later one
	// stream part of its answer and stop.
	FreezeLoad map[string]float64
	FreezeChat map[string]int
	// Lead is how long before now the run is made to start (default 4 min):
	// the steps before the frozen point fill that time on the run's clock,
	// which never runs ahead of the real time. Set it a little under what those
	// steps take, so the clock has caught up with the real time at the point.
	Lead time.Duration
}

// frozenWorld is a running frozen scenario.
type frozenWorld struct {
	dir  string
	st   *bench.Store
	sa   *scriptedAPI
	mgr  *bench.Manager
	srv  *api.Server
	stop func()
	// restore puts back the suite the run was started with zero cooldown,
	// once the run stands at its point (nil when there is nothing to restore).
	restore func()
}

var frozenSeq atomic.Int64

// startFrozen builds the snapshot world of a spec. emit receives the bench frames.
func startFrozen(c *cluster, log *slog.Logger, spec *frozenSpec, emit func(map[string]any)) (*frozenWorld, error) {
	dir := filepath.Join(c.tmp, fmt.Sprintf("frozen-%d", frozenSeq.Add(1)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	snap := filepath.Join(dir, bench.DBFile)
	if _, err := c.db.DB().Exec(`VACUUM INTO '` + strings.ReplaceAll(filepath.ToSlash(snap), "'", "''") + `'`); err != nil {
		return nil, fmt.Errorf("snapshot of the database: %w", err)
	}
	st, err := bench.OpenStore(dir)
	if err != nil {
		return nil, err
	}
	fw := &frozenWorld{dir: dir, st: st}
	ok := false
	defer func() {
		if !ok {
			fw.close()
		}
	}()
	// The suites keep their schedules (an "at" schedule is next due at its
	// next clock time, and the run's clock never passes the real time), so the
	// suite list reads as on the real server. Only the frozen suite runs with
	// no cooldown, so the steps before the point take no real time; its
	// stored spec is put back by settle.
	if spec.Suite != "" {
		if su, err := st.GetSuite(spec.Suite); err == nil {
			orig := *su.Spec
			cp := orig
			zero := 0
			cp.CooldownS = &zero
			if err := st.SaveSuite(&cp, su.Source, su.UpdatedAt); err != nil {
				return nil, err
			}
			fw.restore = func() { _ = st.SaveSuite(&orig, su.Source, su.UpdatedAt) }
		}
	}
	if spec.Prepare != nil {
		if err := spec.Prepare(st); err != nil {
			return nil, fmt.Errorf("preparing the snapshot: %w", err)
		}
	}
	clock := &vclock{}
	lead := spec.Lead
	if lead == 0 {
		lead = 4 * time.Minute
	}
	clock.set(time.Now().Add(-lead))
	fw.sa = newScriptedAPI(clock)
	fw.sa.revision = fmt.Sprint(c.svc.Revision())
	for k, v := range spec.FreezeLoad {
		fw.sa.freezeLoad[k] = v
	}
	for k, v := range spec.FreezeChat {
		fw.sa.freezeChat[k] = v
	}
	cfg := c.svc.Config()
	fw.mgr, err = bench.NewManager(bench.ManagerOptions{Store: st, API: fw.sa, Version: devVersion, Logger: log,
		Thresholds: bench.Thresholds{LoadPct: cfg.Bench.RegressLoadPct, DecodePct: cfg.Bench.RegressDecodePct},
		ProfileOK:  func(id string) bool { _, _, ok := c.svc.Config().Resolve(id); return ok },
		Now:        clock.Now, IdlePoll: 50 * time.Millisecond, LoadPoll: 100 * time.Millisecond, GoneTimeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	fw.srv = api.New(cfg, c.sched, api.Options{Logger: log, Version: devVersion, Bench: fw.mgr, BenchStore: st, Config: c.svc})
	cancelSub := fw.mgr.Subscribe(emit)
	ctx, cancel := context.WithCancel(context.Background())
	fw.mgr.Start(ctx)
	fw.stop = func() {
		cancelSub()
		fw.sa.stop()
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		fw.mgr.Stop(sctx)
		scancel()
		cancel()
	}
	if spec.Suite != "" {
		n := max(spec.Runs, 1)
		for i := 0; i < n; i++ {
			note := "scheduled"
			if i > 0 {
				note = "queued behind the first"
			}
			if _, err := fw.mgr.Enqueue(spec.Suite, bench.EnqueueOptions{Trigger: "schedule", Note: note}); err != nil {
				return nil, fmt.Errorf("queueing %s on the snapshot: %w", spec.Suite, err)
			}
		}
	}
	ok = true
	return fw, nil
}

// settle waits (at most d) until the run stands at its frozen point: its
// current benchmark and step count have not changed for two and a half
// seconds (a step before the point may take a second of real time). Then the
// frozen suite's stored spec is put back.
func (fw *frozenWorld) settle(d time.Duration) {
	if fw.restore != nil {
		defer fw.restore()
	}
	deadline := time.Now().Add(d)
	last, still := "", 0
	for time.Now().Before(deadline) {
		p := fw.mgr.Progress()
		cur := fmt.Sprint(p["current"], p["steps_done"])
		if p != nil && cur == last {
			still++
			if still >= 25 {
				return
			}
		} else {
			still = 0
		}
		last = cur
		time.Sleep(100 * time.Millisecond)
	}
}

func (fw *frozenWorld) ServeHTTP(w http.ResponseWriter, r *http.Request) { fw.srv.ServeHTTP(w, r) }

func (fw *frozenWorld) close() {
	if fw.stop != nil {
		fw.stop()
	}
	if fw.srv != nil {
		fw.srv.Close()
	}
	if fw.st != nil {
		fw.st.Close()
	}
	os.RemoveAll(fw.dir)
}

// ---- helpers for Prepare ----

// clearBench deletes the runs and, when what says so, the suites and benchmarks of a snapshot.
func clearBench(st *bench.Store, suites, benchmarks bool) error {
	if err := st.Write(func(tx *sql.Tx) error {
		for _, t := range []string{"requests", "run_phases", "run_steps", "runs"} {
			if _, err := tx.Exec("DELETE FROM " + t); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if suites || benchmarks {
		ss, _ := st.ListSuites()
		for _, s := range ss {
			if err := st.DeleteSuite(s.Spec.ID); err != nil {
				return err
			}
		}
	}
	if benchmarks {
		bs, _ := st.ListBenchmarks()
		for _, b := range bs { // parallel ones first: they name the others
			if b.Spec.IsParallel() {
				if err := st.DeleteBenchmark(b.Spec.ID); err != nil {
					return err
				}
			}
		}
		for _, b := range bs {
			if !b.Spec.IsParallel() {
				if err := st.DeleteBenchmark(b.Spec.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// setHold makes every hold phase of a benchmark last secs (and its timeout fit).
func setHold(st *bench.Store, id string, secs int) error {
	sb, err := st.GetBenchmark(id)
	if err != nil {
		return err
	}
	cp := *sb.Spec
	cp.Phases = append([]benchspec.Phase(nil), sb.Spec.Phases...)
	for i := range cp.Phases {
		if cp.Phases[i].Type == benchspec.PhaseHold {
			s := secs
			cp.Phases[i].Seconds = &s
		}
	}
	cp.TimeoutS = secs + 600
	return st.SaveBenchmark(&cp, sb.Source, time.Now().UnixMilli())
}
