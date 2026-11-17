package api

import (
	"sync"
	"time"
)

// activityRecord is one finished request (design A.4).
type activityRecord struct {
	ID         uint64
	Timestamp  time.Time // when the request finished
	Model      string    // profile name, never an alias
	ReqPath    string
	Status     int
	DurationMS int64
	In, Out    int     // -1 when unknown
	TPS        float64 // -1 when unknown
}

// activityLog keeps the last N finished requests in memory.
type activityLog struct {
	mu   sync.Mutex
	max  int
	next uint64
	recs []activityRecord // oldest first
}

func newActivityLog(max int) *activityLog { return &activityLog{max: max, next: 1} }

func (a *activityLog) add(r activityRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r.ID = a.next
	a.next++
	a.recs = append(a.recs, r)
	if len(a.recs) > a.max {
		a.recs = append(a.recs[:0], a.recs[len(a.recs)-a.max:]...)
	}
}

// snapshot returns the records oldest first, in the A.4 JSON shape.
func (a *activityLog) snapshot() []any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]any, 0, len(a.recs))
	for _, r := range a.recs {
		out = append(out, map[string]any{
			"id": r.ID, "timestamp": isoTime(r.Timestamp), "model": r.Model,
			"req_path": r.ReqPath, "resp_status_code": r.Status, "duration_ms": r.DurationMS,
			"tokens": map[string]any{"input_tokens": r.In, "output_tokens": r.Out, "tokens_per_second": r.TPS},
		})
	}
	return out
}
