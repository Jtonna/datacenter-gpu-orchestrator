package bench

import (
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// A panicking bench subscriber (the catalog, the live channel) does not end
// the emitter, and the other subscribers still get the event.
func TestSubscriberPanicDoesNotStopEmit(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	restore := supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { t.Errorf("escalated: %s", r) }})
	defer restore()
	tm := newTestMgr(t, newFake())
	tm.m.Subscribe(func(map[string]any) { panic("subscriber bug") })
	tm.m.emit(map[string]any{"kind": "probe"})
	tm.evMu.Lock()
	defer tm.evMu.Unlock()
	if len(tm.events) == 0 || tm.events[len(tm.events)-1]["kind"] != "probe" {
		t.Fatalf("the other subscriber missed the event: %v", tm.events)
	}
	if supervise.Total() != 1 {
		t.Fatalf("failures %d, want 1", supervise.Total())
	}
}

// The manager's loop fails in the middle of a run: the run is failed in the
// store with the internal error, what it loaded is unloaded, and the next
// queued run still starts and completes.
func TestManagerPanicFailsTheRunAndGoesOn(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	restore := supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { t.Errorf("escalated: %s", r) }})
	defer restore()
	api := newFake().prof("gemma", fakeProf{}).prof("second", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"b","profile":"gemma","phases":[{"type":"load"}]}`)
	tm.bench(`{"id":"next","profile":"second","phases":[{"type":"load"},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["b","next"],"cooldown_s":0,"only_when_idle":false}`)
	tm.suite(`{"id":"s2","benchmarks":["next"],"cooldown_s":0,"only_when_idle":false}`)
	var items atomic.Int64
	defer supervise.SetHookForTest(func(point string) {
		if point == "bench.item" && items.Add(1) == 2 { // the second item of the first run
			panic("injected")
		}
	})()
	first := tm.run("s")
	second := tm.run("s2")
	r := tm.finished(first.ID)
	if r.State != RunFailed || !strings.Contains(r.Error, "internal error: injected") {
		t.Fatalf("the failed run: %s %q", r.State, r.Error)
	}
	if calls := strings.Join(api.callList(), " "); !strings.Contains(calls, "load:gemma") || !strings.Contains(calls, "unload:gemma") || api.has("gemma") {
		t.Fatalf("the failed run's model is still loaded: %v", api.callList())
	}
	if r2 := tm.finished(second.ID); r2.State != RunCompleted {
		t.Fatalf("the next run: %s %q", r2.State, r2.Error)
	}
}
