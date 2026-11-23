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
