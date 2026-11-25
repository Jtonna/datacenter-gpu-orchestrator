package bench

import (
	"sort"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// fillReports keeps, per node, the reports a context_fill phase has seen:
// when each reached the scheduler (WorkerInfo.SampleAt). That time is when
// the scheduler RECEIVED the heartbeat; the agent read its memory when it
// built it, a moment earlier (the transit), so a report received just after
// a time T may hold a figure read just before T. An agent sends its next
// heartbeat only after the scheduler has answered the one before
// (internal/agent/agent/agent.go heartbeatLoop: build, post, read the
// answer, wait for the tick), so the figure of a report was certainly read
// after the report before it was received; less the figure's own age (a GPU
// figure from the stats exporter, read age_ms before the heartbeat).
type fillReports struct {
	tol time.Duration // two sightings closer than this are the same report

	mu   sync.Mutex
	seen map[string][]time.Time // by node, received times, rising
}

func (l *fillReports) tolerance() time.Duration {
	if l.tol > 0 {
		return l.tol
	}
	return 250 * time.Millisecond
}

// note records the reports of a status.
func (l *fillReports) note(s *StatusInfo) {
	l.mu.Lock()
	defer supervise.Unlocked("bench", l.mu.Unlock)
	if l.seen == nil {
		l.seen = map[string][]time.Time{}
	}
	for _, w := range s.Workers {
		if w.AvailMiB == nil || w.SampleAt.IsZero() {
			continue
		}
		if l.indexLocked(w.ID, w.SampleAt) >= 0 {
			continue
		}
		xs := append(l.seen[w.ID], w.SampleAt)
		sort.Slice(xs, func(i, j int) bool { return xs[i].Before(xs[j]) })
		l.seen[w.ID] = xs
	}
}

// indexLocked is the index of the report seen at about `at`, or -1.
func (l *fillReports) indexLocked(id string, at time.Time) int {
	tol := l.tolerance()
	for i, x := range l.seen[id] {
		if d := x.Sub(at); d < tol && d > -tol {
			return i
		}
	}
	return -1
}

// canon is the first-seen time of the report seen at about `at` (at itself
// when it was not seen).
func (l *fillReports) canon(id string, at time.Time) time.Time {
	l.mu.Lock()
	defer supervise.Unlocked("bench", l.mu.Unlock)
	if i := l.indexLocked(id, at); i >= 0 {
		return l.seen[id][i]
	}
	return at
}

// readAfter is the time w's figure was certainly read after: the report
// before it was received then, less the figure's age. ok is false when the
// report before it was not seen (or w has no sample time).
func (l *fillReports) readAfter(w WorkerInfo) (time.Time, bool) {
	if w.SampleAt.IsZero() {
		return time.Time{}, false
	}
	l.mu.Lock()
	defer supervise.Unlocked("bench", l.mu.Unlock)
	i := l.indexLocked(w.ID, w.SampleAt)
	if i <= 0 {
		return time.Time{}, false
	}
	return l.seen[w.ID][i-1].Add(-time.Duration(w.SampleAgeMS) * time.Millisecond), true
}

// fresh says whether w's figure was certainly read after t.
func (l *fillReports) fresh(w WorkerInfo, t time.Time) bool {
	lo, ok := l.readAfter(w)
	return ok && !lo.Before(t)
}
