package scheduler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// A start worker that fails after its child started turns the panic into
// the start's failure: the child is killed, the request is answered with a
// failed load (not left waiting on an instance stuck in starting), and the
// scheduler keeps serving.
func TestStartWorkerPanicFailsTheLoad(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { t.Errorf("escalated: %s", r) }})()
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("m", 8000))
	h, fired := supervise.PanicAtForTest(supervise.PointName("scheduler.start", f.s), 1)
	defer supervise.SetHookForTest(h)()
	done := make(chan error, 1)
	go func() {
		l, err := f.s.Acquire(ctxT(t), "m")
		if err == nil {
			l.Release()
		}
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the request still waits: the instance is stuck")
	}
	var se *Error
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway || se.Reason != ReasonLoadFailed || !strings.Contains(se.Message, "internal error") {
		t.Fatalf("the request got %v", err)
	}
	if fired() != 1 {
		t.Fatalf("fired %d", fired())
	}
	f.launcher.mu.Lock()
	ch := f.launcher.children[0]
	f.launcher.mu.Unlock()
	ch.mu.Lock()
	stops := append([]instance.StopMode(nil), ch.stops...)
	ch.mu.Unlock()
	if len(stops) == 0 || stops[0] != instance.StopKill {
		t.Fatalf("the started child was not killed: %v", stops)
	}
	// still serving: the loop answers
	if _, err := f.s.Plan(ctxT(t), "m"); err != nil {
		t.Fatalf("the scheduler no longer answers: %v", err)
	}
}

// Review IB1: a panic inside the needs writer's lock (task scheduler.needs,
// policy restart) releases the lock and takes the fatal path instead of a
// restart: the queue may be half-taken.
func TestNeedsWriterPanicInsideItsLockIsFatal(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	fatals := make(chan string, 4)
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { fatals <- r }})()
	w := newNeedWriter(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, fired := supervise.PanicAtForTest(supervise.PointName("scheduler.needs_flush", w), 1)
	defer supervise.SetHookForTest(h)()
	w.start()
	w.queue([]needOp{{shape: "s"}})
	select {
	case r := <-fatals:
		if !strings.Contains(r, "inside a scheduler lock") {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no fatal path (fired %d)", fired())
	}
	if !w.mu.TryLock() {
		t.Fatal("w.mu is still held")
	}
	w.mu.Unlock()
	w.close() // the writer ended for good; close does not wait for a restart
}
