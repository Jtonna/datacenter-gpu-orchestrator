package bench

import (
	"math"
	"sort"
)

// minExpectedS is the least an expected duration counts for: a phase that took
// no measurable time is still known, and must not weigh nothing.
const minExpectedS = 0.05

// How many earlier runs an expected duration looks at.
const expectedRuns = 20

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// MedianStepSeconds is the median duration of a benchmark's steps in earlier
// completed runs (the newest few). ok is false when there is none.
func (s *Store) MedianStepSeconds(benchmarkID string) (float64, bool) {
	rows, err := s.db.Query(`SELECT (st.finished_at - st.started_at) / 1000.0 FROM run_steps st JOIN runs r ON r.id = st.run_id
	  WHERE st.benchmark_id = ? AND r.state = 'completed' AND st.state = 'done' AND st.finished_at >= st.started_at AND st.started_at > 0
	  ORDER BY r.queued_at DESC, st.id DESC LIMIT ?`, benchmarkID, expectedRuns)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	var xs []float64
	for rows.Next() {
		var v float64
		if rows.Scan(&v) == nil && v >= 0 {
			xs = append(xs, v)
		}
	}
	if len(xs) == 0 {
		return 0, false
	}
	return math.Max(median(xs), minExpectedS), true
}

// MedianPhaseSeconds is the median duration of each phase (keyed by phaseKey:
// position and type) of a benchmark in earlier completed runs.
func (s *Store) MedianPhaseSeconds(benchmarkID string) map[string]float64 {
	out := map[string]float64{}
	rows, err := s.db.Query(`SELECT p.idx, p.type, (p.finished_at - p.started_at) / 1000.0 FROM run_phases p
	  JOIN run_steps st ON st.id = p.step_id JOIN runs r ON r.id = p.run_id
	  WHERE st.benchmark_id = ? AND r.state = 'completed' AND p.state = 'done' AND p.finished_at >= p.started_at AND p.started_at > 0
	  ORDER BY r.queued_at DESC, p.id DESC LIMIT ?`, benchmarkID, expectedRuns*64)
	if err != nil {
		return out
	}
	defer rows.Close()
	by := map[string][]float64{}
	for rows.Next() {
		var idx int
		var typ string
		var v float64
		if rows.Scan(&idx, &typ, &v) == nil && v >= 0 {
			k := phaseKey(idx, typ)
			if len(by[k]) < expectedRuns {
				by[k] = append(by[k], v)
			}
		}
	}
	for k, xs := range by {
		out[k] = math.Max(median(xs), minExpectedS)
	}
	return out
}
