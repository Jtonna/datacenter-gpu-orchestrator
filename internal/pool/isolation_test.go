package pool

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Work queued under p.mu that panics (a store write, a history sink) is
// reported; the rest of its batch still runs and the queue is not stuck for
// the next unlock, which may be the scheduler loop's.
func TestAfterQueueSurvivesAPanic(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { t.Errorf("escalated: %s", r) }})()
	p := &Pool{}
	var ran []string
	p.mu.Lock()
	p.afterLocked(func() { panic("store write bug") })
	p.afterLocked(func() { ran = append(ran, "second") })
	p.unlock()
	p.mu.Lock()
	p.afterLocked(func() { ran = append(ran, "next batch") })
	p.unlock()
	if len(ran) != 2 || ran[0] != "second" || ran[1] != "next batch" {
		t.Fatalf("ran %v", ran)
	}
	if supervise.Total() != 1 {
		t.Fatalf("failures %d", supervise.Total())
	}
}
