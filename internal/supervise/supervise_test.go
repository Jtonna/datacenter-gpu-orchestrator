package supervise

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake is a test configuration: a settable clock, a fast backoff, and
// counters for Fatal and Record.
type fake struct {
	mu      sync.Mutex
	at      time.Time
	fatals  []string
	records []Failure
	log     bytes.Buffer
	logMu   sync.Mutex
}

func (f *fake) Write(p []byte) (int, error) { f.logMu.Lock(); defer f.logMu.Unlock(); return f.log.Write(p) }
func (f *fake) logs() string                { f.logMu.Lock(); defer f.logMu.Unlock(); return f.log.String() }
func (f *fake) now() time.Time              { f.mu.Lock(); defer f.mu.Unlock(); return f.at }
func (f *fake) advance(d time.Duration)     { f.mu.Lock(); f.at = f.at.Add(d); f.mu.Unlock() }
func (f *fake) fatalCount() int             { f.mu.Lock(); defer f.mu.Unlock(); return len(f.fatals) }
func (f *fake) recordCount() int            { f.mu.Lock(); defer f.mu.Unlock(); return len(f.records) }

func setup(t *testing.T) *fake {
	t.Helper()
	ResetForTest()
	f := &fake{at: time.Unix(1_000_000, 0)}
	restore := Configure(Options{
		Logger:  slog.New(slog.NewTextHandler(f, nil)),
		Now:     f.now,
		Backoff: func(int) time.Duration { return time.Millisecond },
		Fatal:   func(r string) { f.mu.Lock(); f.fatals = append(f.fatals, r); f.mu.Unlock() },
		Record:  func(x Failure) { f.mu.Lock(); f.records = append(f.records, x); f.mu.Unlock() },
	})
	t.Cleanup(func() { restore(); ResetForTest() })
	return f
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func stat(name string) (TaskStats, bool) {
	for _, s := range Stats() {
		if s.Name == name {
			return s, true
		}
	}
	return TaskStats{}, false
}

// A restarted task runs again after its Rebuild, the failure is logged with
// its stack and recorded, and Done fires only on the final return.
func TestGoRestartsAfterRebuild(t *testing.T) {
	f := setup(t)
	var runs, rebuilds atomic.Int64
	done := make(chan struct{})
	Go(context.Background(), Spec{Name: "t.loop", Policy: Restart,
		Rebuild: func(p any) { rebuilds.Add(1) },
		Done:    func() { close(done) }}, func(ctx context.Context) {
		if runs.Add(1) == 1 {
			panic("boom")
		}
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the task did not finish")
	}
	if runs.Load() != 2 || rebuilds.Load() != 1 {
		t.Fatalf("runs %d rebuilds %d, want 2 and 1", runs.Load(), rebuilds.Load())
	}
	if f.fatalCount() != 0 {
		t.Fatalf("one failure escalated: %v", f.fatals)
	}
	eventually(t, "the record", func() bool { return f.recordCount() == 1 })
	l := f.logs()
	if !strings.Contains(l, "level=ERROR") || !strings.Contains(l, `task=t.loop`) || !strings.Contains(l, "panic=boom") || !strings.Contains(l, "supervise_test.go") {
		t.Fatalf("the failure is not logged at error level with its stack:\n%s", l)
	}
	s, ok := stat("t.loop")
	if !ok || s.Restarts != 1 || s.Failures != 1 || s.FailuresLastMin != 1 || s.State != "done" || s.LastFailure != "boom" || s.Policy != "restart" {
		t.Fatalf("stats %+v", s)
	}
	if Total() != 1 {
		t.Fatalf("total %d", Total())
	}
}

// The third failure of a task name within a minute escalates; failures
// further apart do not.
func TestEscalationThreeInAMinute(t *testing.T) {
	f := setup(t)
	for i := 0; i < 2; i++ {
		Guard("t.guard", func() { panic("x") }, nil)
		f.advance(40 * time.Second)
	}
	// 80 s span: the first failure left the window.
	Guard("t.guard", func() { panic("x") }, nil)
	if f.fatalCount() != 0 {
		t.Fatalf("failures over 80 s escalated")
	}
	f.advance(10 * time.Second)
	Guard("t.guard", func() { panic("x") }, nil) // three within 50 s
	if f.fatalCount() != 1 {
		t.Fatalf("fatals %d, want 1", f.fatalCount())
	}
	if !strings.HasPrefix(f.fatals[0], "t.guard: x") {
		t.Fatalf("reason %q", f.fatals[0])
	}
}

// A long-lived task that fails three times in a minute stops restarting and
// calls Fatal once.
func TestGoEscalates(t *testing.T) {
	f := setup(t)
	done := make(chan struct{})
	var runs atomic.Int64
	Go(context.Background(), Spec{Name: "t.bad", Done: func() { close(done) }}, func(context.Context) {
		runs.Add(1)
		panic("always")
	})
	<-done
	if runs.Load() != 3 || f.fatalCount() != 1 {
		t.Fatalf("runs %d fatals %d", runs.Load(), f.fatalCount())
	}
}

// An Exit task calls Fatal on its first failure and is not restarted.
func TestExitPolicyFatalOnce(t *testing.T) {
	f := setup(t)
	done := make(chan struct{})
	var runs atomic.Int64
	Go(context.Background(), Spec{Name: "t.exit", Policy: Exit, Done: func() { close(done) }}, func(context.Context) {
		runs.Add(1)
		panic("loop")
	})
	<-done
	if runs.Load() != 1 || f.fatalCount() != 1 {
		t.Fatalf("runs %d fatals %d", runs.Load(), f.fatalCount())
	}
	if s, _ := stat("t.exit"); s.State != "failed" || s.Policy != "exit" {
		t.Fatalf("stats %+v", s)
	}
}

// Restarts stop when ctx ends, also during the backoff, and Done fires.
func TestGoStopsInBackoff(t *testing.T) {
	setup(t)
	restore := Configure(Options{Backoff: func(int) time.Duration { return time.Hour }, Fatal: func(string) {}})
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	Go(ctx, Spec{Name: "t.backoff", Done: func() { close(done) }}, func(context.Context) { panic("x") })
	eventually(t, "backoff", func() bool { s, _ := stat("t.backoff"); return s.State == "backoff" })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the backoff held the stop up")
	}
}

// Once turns a panic into its failure result and does not run again.
func TestOnceCallsOnPanic(t *testing.T) {
	f := setup(t)
	got := make(chan any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Once("t.once", func() { panic("start failed") }, func(p any) { got <- p })
	}()
	<-done
	if p := <-got; p != "start failed" {
		t.Fatalf("onPanic got %v", p)
	}
	if s, ok := stat("t.once"); !ok || s.Failures != 1 || s.Policy != "once" {
		t.Fatalf("stats %+v", s)
	}
	if f.fatalCount() != 0 {
		t.Fatal("escalated")
	}
	// A one-shot that never failed is not listed.
	Once("t.quiet", func() {}, nil)
	if _, ok := stat("t.quiet"); ok {
		t.Fatal("a one-shot without failures is listed")
	}
}

// Report never escalates.
func TestReportNeverEscalates(t *testing.T) {
	f := setup(t)
	for i := 0; i < 5; i++ {
		Report("http", "handler", nil)
	}
	if f.fatalCount() != 0 {
		t.Fatal("Report escalated")
	}
	if s, _ := stat("http"); s.Failures != 5 {
		t.Fatalf("stats %+v", s)
	}
}

// A Rebuild that panics ends the process.
func TestRebuildPanicIsFatal(t *testing.T) {
	f := setup(t)
	done := make(chan struct{})
	Go(context.Background(), Spec{Name: "t.rb", Rebuild: func(any) { panic("rebuild") }, Done: func() { close(done) }},
		func(context.Context) { panic("x") })
	<-done
	if f.fatalCount() != 1 || !strings.HasPrefix(f.fatals[0], "t.rb.rebuild") {
		t.Fatalf("fatals %v", f.fatals)
	}
}

// GuardExit reports and calls Fatal on the first panic.
func TestGuardExit(t *testing.T) {
	f := setup(t)
	if GuardExit("t.apply", func() {}) {
		t.Fatal("no panic reported as one")
	}
	if !GuardExit("t.apply", func() { panic("half applied") }) || f.fatalCount() != 1 {
		t.Fatalf("fatals %d", f.fatalCount())
	}
}

// The hook fires only at its point and only n times.
func TestHook(t *testing.T) {
	setup(t)
	Point("nothing installed")
	h, fired := PanicAtForTest("p", 1)
	restore := SetHookForTest(h)
	defer restore()
	Point("other")
	if !Guard("t.hook", func() { Point("p") }, nil) {
		t.Fatal("the hook did not panic")
	}
	if Guard("t.hook", func() { Point("p") }, nil) {
		t.Fatal("the hook panicked twice")
	}
	if fired() != 1 {
		t.Fatalf("fired %d", fired())
	}
}

// The backoff schedule: 100 ms, 1 s, 5 s, then doubling up to 30 s.
func TestDelay(t *testing.T) {
	setup(t)
	restore := Configure(Options{})
	defer restore()
	want := []time.Duration{100 * time.Millisecond, time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if d := delay(i); d != w {
			t.Errorf("delay(%d) = %s, want %s", i, d, w)
		}
	}
}
