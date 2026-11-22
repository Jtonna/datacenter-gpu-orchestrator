package bench

import (
	"io"
	"log/slog"
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
