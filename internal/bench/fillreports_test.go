package bench

import (
	"testing"
	"time"
)

// A report's time as the status gives it is when the scheduler RECEIVED it
// (status time less last_seen_ago_s). The agent read its memory a moment
// earlier, when it built the heartbeat: a report received just after a turn
// ended may hold a figure read just before. The agent sends its next
// heartbeat only after the scheduler answered the previous one, so a report
// is certainly read after T when the report before it was received after T
// (less the age of a GPU figure, which the stats exporter read earlier still).
func TestFillReports_ReadAfterNeedsTheReportBefore(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 14, 35, 44, 892_000_000, time.UTC) // the turn's end
	w := func(at time.Time, ageMS int64) WorkerInfo {
		v := int64(9000)
		return WorkerInfo{ID: "n1", AvailMiB: &v, SampleAt: at, SampleAgeMS: ageMS}
	}
	var l fillReports
	l.note(&StatusInfo{Workers: []WorkerInfo{w(t0.Add(-1900*time.Millisecond), 0)}}) // before the turn ended
	first := w(t0.Add(50*time.Millisecond), 0)                                         // received 50 ms after: read maybe before
	l.note(&StatusInfo{Workers: []WorkerInfo{first}})
	if lo, ok := l.readAfter(first); !ok || lo.After(t0) {
		t.Fatalf("the first report after the end proves nothing: lower bound %v ok %v", lo, ok)
	}
	// The same report seen again (another status read, the age rounded differently) is not a new one.
	again := w(t0.Add(53*time.Millisecond), 0)
	l.note(&StatusInfo{Workers: []WorkerInfo{again}})
	if lo, _ := l.readAfter(again); lo.After(t0) {
		t.Fatalf("a re-read of the same report counted as a new one: %v", lo)
	}
	second := w(t0.Add(2050*time.Millisecond), 0)
	l.note(&StatusInfo{Workers: []WorkerInfo{second}})
	if lo, ok := l.readAfter(second); !ok || !lo.After(t0) || !lo.Equal(first.SampleAt) {
		t.Fatalf("the second report was read after the end: lower bound %v ok %v", lo, ok)
	}
	// A GPU figure 3 s old in that second report was read before the end.
	gpu := w(t0.Add(2050*time.Millisecond), 3000)
	if lo, _ := l.readAfter(gpu); lo.After(t0) {
		t.Fatalf("a GPU figure older than the gap counted: %v", lo)
	}
	// A report never seen before has no known predecessor: no lower bound.
	var fresh fillReports
	if _, ok := fresh.readAfter(second); ok {
		t.Fatal("no predecessor seen, yet a lower bound")
	}
	// Unknown sample time: never.
	if _, ok := l.readAfter(w(time.Time{}, 0)); ok {
		t.Fatal("zero sample time gave a bound")
	}
}
