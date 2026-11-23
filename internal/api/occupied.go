package api

import (
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// occupiedTotals is GET /api/system's occupied_only: the totals of the
// scheduler's occupiedOnly events (scheduler/occupied.go), how often a
// request's only option was a console another instance held, and how each
// such case ended. They are kept across restarts in the history store:
// rebuilt at start from the occupiedOnly events it still holds (so they
// cover its retention, since the oldest event it keeps), then counted as
// new events are recorded.
type occupiedTotals struct {
	mu        sync.Mutex
	seeded    bool
	pending   []history.Event
	total     int
	byOutcome map[string]int
	waitSum   float64
	waitMax   float64
	since     int64
	cancel    func()
}

type evKey struct {
	ts  int64
	seq uint64
}

// startOccupied subscribes to new occupiedOnly events and adds the ones
// the store already holds. An event recorded while the store is read is
// counted once (by time and sequence).
func startOccupied(h *history.Store) *occupiedTotals {
	o := &occupiedTotals{byOutcome: map[string]int{scheduler.OccWaited: 0, scheduler.OccEvicted: 0, scheduler.OccTimedOut: 0, scheduler.OccCancelled: 0}}
	o.cancel = h.Subscribe(func(e history.Event) {
		if e.Type != history.TOccupiedOnly {
			return
		}
		o.mu.Lock()
		defer supervise.Unlocked("occupied", o.mu.Unlock)
		if !o.seeded {
			o.pending = append(o.pending, e)
			return
		}
		o.addLocked(e)
	})
	seen := map[evKey]bool{}
	var past []history.Event
	h.ScanType(history.TOccupiedOnly, func(e history.Event) {
		seen[evKey{e.Ts, e.Seq}] = true
		past = append(past, e)
	})
	since := h.OldestTs()
	o.mu.Lock()
	defer supervise.Unlocked("occupied", o.mu.Unlock)
	o.since = since
	for _, e := range past {
		o.addLocked(e)
	}
	for _, e := range o.pending {
		if !seen[evKey{e.Ts, e.Seq}] {
			o.addLocked(e)
		}
	}
	o.pending, o.seeded = nil, true
	return o
}

func (o *occupiedTotals) addLocked(e history.Event) {
	o.total++
	if out, _ := e.Data["outcome"].(string); out != "" {
		o.byOutcome[out]++
	}
	var w float64
	switch v := e.Data["wait_s"].(type) {
	case float64:
		w = v
	case int:
		w = float64(v)
	}
	o.waitSum += w
	o.waitMax = max(o.waitMax, w)
	if o.since == 0 || (e.Ts > 0 && e.Ts < o.since) {
		o.since = e.Ts
	}
}

// view is the JSON of occupied_only.
func (o *occupiedTotals) view() map[string]any {
	o.mu.Lock()
	defer supervise.Unlocked("occupied", o.mu.Unlock)
	by := map[string]any{}
	for k, v := range o.byOutcome {
		by[k] = v
	}
	var since any
	if o.since > 0 {
		since = isoTime(time.UnixMilli(o.since))
	}
	return map[string]any{"total": o.total, "by_outcome": by,
		"wait_seconds": map[string]any{"sum": round1(o.waitSum), "max": round1(o.waitMax)}, "since": since}
}
