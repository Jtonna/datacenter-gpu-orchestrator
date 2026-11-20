package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// Runner tests: real goroutines over the fakes. Meaningful under -race
// (Linux CI); on a machine without cgo they still check behaviour.

type runFixture struct {
	t        *testing.T
	clk      *fakeClock
	view     *fakeView
	prober   *fakeProber
	launcher *fakeLauncher
	cfg      *config.Config
	s        *Scheduler
	runErr   chan error
}

func startScheduler(t *testing.T, nodes []core.Node, profiles ...*core.Profile) *runFixture {
	t.Helper()
	clk := newFakeClock()
	view := newFakeView(clk)
	view.set(nodes...)
	cfg := testConfig()
	// These tests drive the real clock-following loop; the forced-node
	// fail-fast (which answers in 30 s) would race with the queue_timeout
	// test, so it is set out of the way here. Its own rules are in
	// rules_forcedgone_test.go.
	cfg.Defaults.ForcedNodeGrace = 24 * time.Hour
	for _, p := range profiles {
		cfg.Profiles[p.Name] = p
	}
	f := &runFixture{t: t, clk: clk, view: view, prober: newFakeProber(view),
		launcher: &fakeLauncher{nextPort: 5800, autoHealthy: true, autoExit: true}, cfg: cfg, runErr: make(chan error, 1)}
	s, err := New(cfg, Options{
		Clock: clk, View: view, Prober: f.prober, Launcher: f.launcher, WorkerProber: newFakeWorkerProber(),
		Needs: NeedFunc(func(p *core.Profile) (NeedInfo, error) {
			if p.Name == "upload" {
				return NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1100 * mb}, nil
			}
			return NeedInfo{Need: placer.Measured(p.NeedsMiB)}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	go func() { f.runErr <- s.Run(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return f
}

func llamaP(name string, needs int, mod ...func(*core.Profile)) *core.Profile {
	p := &core.Profile{Name: name, Kind: core.KindLlama, Model: name + ".gguf", Ctx: 4096, Parallel: 1, NeedsMiB: needs}
	for _, m := range mod {
		m(p)
	}
	return p
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestScheduler_UnknownModel404_AliasResolved(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)},
		llamaP("gemma-4-12b", 8000, func(p *core.Profile) { p.Aliases = []string{"gemma-4-12b-1pool-8k"} }))
	_, err := f.s.Acquire(ctxT(t), "nope")
	var se *Error
	if !errors.As(err, &se) || se.Status != http.StatusNotFound || se.Reason != ReasonUnknownModel {
		t.Fatalf("unknown model: %v", err)
	}
	l, err := f.s.Acquire(ctxT(t), "gemma-4-12b-1pool-8k")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if l.Profile != "gemma-4-12b" || !l.Alias || l.Addr != "127.0.0.1:5801" {
		t.Fatalf("lease %+v", l)
	}
	if a, ok := f.s.Upstream("gemma-4-12b-1pool-8k"); !ok || a != l.Addr {
		t.Fatalf("upstream %q %v", a, ok)
	}
	if r := f.s.Running(); len(r) != 1 || r[0].Profile != "gemma-4-12b" || r[0].State != RunningReady {
		t.Fatalf("running %+v", r)
	}
}

func TestScheduler_ConcurrentAcquireRelease(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000), ps5("b", 4, 15000)},
		llamaP("m", 8000), llamaP("n", 8000))
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := f.s.Acquire(ctxT(t), []string{"m", "n"}[i%2])
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			ok.Add(1)
			l.Release()
			l.Release() // idempotent
		}(i)
	}
	wg.Wait()
	if ok.Load() != 64 {
		t.Fatalf("served %d", ok.Load())
	}
	st, err := f.s.Status(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range st.Instances {
		if in.InFlight != 0 {
			t.Fatalf("in flight left: %+v", in)
		}
	}
}

func TestScheduler_CancelWhileWaiting_NoLeakedLease(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("m", 8000))
	f.launcher.mu.Lock()
	f.launcher.autoHealthy = false
	f.launcher.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			go cancel()
			_, err := f.s.Acquire(ctx, "m")
			var se *Error
			if err != nil && (!errors.As(err, &se) || se.Reason != ReasonClientGone) {
				t.Errorf("err %v", err)
			}
		}()
	}
	wg.Wait()
	waitFor(t, func() bool {
		for _, ch := range f.launcher.all() { // the child may start after the cancellations
			ch.becomeHealthy()
		}
		r := f.s.Running()
		return len(r) == 1 && r[0].State == RunningReady
	})
	st, _ := f.s.Status(ctxT(t))
	if st.Instances[0].InFlight != 0 || len(st.Queue) != 0 {
		t.Fatalf("leaked: %+v %+v", st.Instances[0], st.Queue)
	}
}

func TestScheduler_ShutdownWithConcurrentAcquires(t *testing.T) {
	before := runtime.NumGoroutine()
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("big", 12000), llamaP("other", 12000))
	l, err := f.s.Acquire(ctxT(t), "big")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.Acquire(ctxT(t), "other") // queued behind busy big
			errs <- err
		}()
	}
	waitFor(t, func() bool {
		st, _ := f.s.Status(ctxT(t))
		return len(st.Queue) == 1 && st.Queue[0].Waiters == 32
	})
	done := make(chan error, 1)
	go func() { done <- f.s.Shutdown(ctxT(t)) }()
	wg.Wait()
	close(errs)
	for err := range errs {
		var se *Error
		if !errors.As(err, &se) || se.Reason != ReasonShuttingDown || se.Status != 503 {
			t.Fatalf("queued waiter got %v", err)
		}
	}
	if _, err := f.s.Acquire(ctxT(t), "big"); err == nil {
		t.Fatalf("admitted during shutdown")
	}
	l.Release() // in-flight finishes; the scheduler stops everything
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-f.runErr; err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Acquire(ctxT(t), "big"); err == nil {
		t.Fatalf("acquire after shutdown")
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
}

func TestScheduler_ShutdownContextEndsForcesKill(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("m", 8000))
	l, err := f.s.Acquire(ctxT(t), "m")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
	modes := f.launcher.all()[0].stopModes()
	if len(modes) == 0 || modes[len(modes)-1] != instance.StopKill {
		t.Fatalf("modes %v", modes)
	}
}

func TestScheduler_SlowSubscriberNeverBlocks_DropOldest(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("m", 8000))
	ch, cancel := f.s.Subscribe(2)
	defer cancel()
	for i := 0; i < 50; i++ {
		l, err := f.s.Acquire(ctxT(t), "m")
		if err != nil {
			t.Fatal(err)
		}
		l.Release()
	}
	if _, err := f.s.Status(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	var got []FeedEvent
	for len(ch) > 0 {
		got = append(got, <-ch)
	}
	if len(got) != 2 || got[1].Kind != FeedFinished || got[1].Seq <= got[0].Seq {
		t.Fatalf("kept %+v", got)
	}
	cancel()
	cancel()
}

func TestScheduler_FeedReportsStatesAndRequests(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("m", 8000))
	ch, cancel := f.s.Subscribe(64)
	defer cancel()
	l, err := f.s.Acquire(ctxT(t), "m")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	want := []FeedKind{FeedInstance, FeedLoadStarted, FeedLoadFinished, FeedInstance, FeedAdmitted, FeedFinished}
	for i, k := range want {
		select {
		case ev := <-ch:
			if ev.Kind != k || ev.Profile != "m" {
				t.Fatalf("event %d: %+v, want kind %v", i, ev, k)
			}
			if k == FeedInstance && i == 3 && (ev.State != RunningReady || ev.ReadySince.IsZero()) {
				t.Fatalf("ready event %+v", ev)
			}
			if k == FeedFinished && ev.RequestID != l.ID {
				t.Fatalf("finished id %d, want %d", ev.RequestID, l.ID)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d missing", i)
		}
	}
}

func TestScheduler_TimersFollowTheClock(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)},
		llamaP("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"ghost"} }))
	errc := make(chan error, 1)
	go func() { _, err := f.s.Acquire(ctxT(t), "f"); errc <- err }()
	waitFor(t, func() bool { st, _ := f.s.Status(ctxT(t)); return len(st.Queue) == 1 })
	f.clk.Advance(599 * time.Second)
	f.s.Status(ctxT(t))
	select {
	case err := <-errc:
		t.Fatalf("answered early: %v", err)
	default:
	}
	f.clk.Advance(2 * time.Second)
	select {
	case err := <-errc:
		var se *Error
		if !errors.As(err, &se) || se.Reason != ReasonForcedUnavailable {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("queue_timeout timer did not fire")
	}
}

func TestScheduler_LoadPlanUnload(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("m", 8000), llamaP("huge", 90000))
	if r, err := f.s.Plan(ctxT(t), "m"); err != nil || r.Action != PlanStart {
		t.Fatalf("plan %+v %v", r, err)
	}
	if err := f.s.Load(ctxT(t), "huge"); err == nil {
		t.Fatalf("load of a need that never fits accepted")
	}
	if err := f.s.Load(ctxT(t), "m"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, ok := f.s.Upstream("m"); return ok })
	if p := f.s.Profiles(); p[1].Name != "m" || p[1].State != RunningReady {
		t.Fatalf("profiles %+v", p)
	}
	if got, err := f.s.UnloadAllIdle(ctxT(t)); err != nil || len(got) != 1 {
		t.Fatalf("unload idle %v %v", got, err)
	}
	waitFor(t, func() bool { return len(f.s.Running()) == 0 })
	if err := f.s.Unload(ctxT(t), "m", false); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("unload: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached")
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

var _ = fmt.Sprint

// The real loop reads the byte count off the event loop and folds the
// result back in: a loading remote instance becomes "measured".
func TestScheduler_LoadProgressIsMeasuredThroughTheRunner(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("upload", 8000))
	f.launcher.autoHealthy = false
	f.launcher.sent = 400_000_000
	if err := f.s.Load(ctxT(t), "upload"); err != nil {
		t.Fatal(err)
	}
	var in InstanceStatus
	waitFor(t, func() bool {
		f.clk.Advance(time.Second)
		st, err := f.s.Status(ctxT(t))
		if err != nil || len(st.Instances) != 1 {
			return false
		}
		in = st.Instances[0]
		return in.LoadProgressSource != nil && *in.LoadProgressSource == "measured"
	})
	if in.LoadBytesSent == nil || *in.LoadBytesSent != 400_000_000 {
		t.Fatalf("load_bytes_sent = %v", in.LoadBytesSent)
	}
	if r := f.s.Running(); len(r) != 1 || r[0].LoadProgress == nil {
		t.Fatalf("running %+v", r)
	}
}
