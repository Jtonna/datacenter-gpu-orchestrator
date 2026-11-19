package main

// Scenarios for the Benchmarks screens, in a file of their own (see the
// BenchSetup hook in scenarios.go).

import (
	"sort"
	"time"
)

func init() {
	registerScenario(scenario{Name: "bench-no-benchmarks", Description: "Benchmarks screen empty states: no benchmarks, no suites, no runs.", Status: scEmpty,
		BenchSetup: func(e *benchEngine, now time.Time) {
			e.store.mu.Lock()
			defer e.store.mu.Unlock()
			e.store.benchmarks, e.store.suites, e.store.runs = map[string]map[string]any{}, map[string]map[string]any{}, nil
		}})

	registerScenario(scenario{Name: "bench-no-suites", Description: "Benchmarks exist but no suite and no run.", Status: scEmpty,
		BenchSetup: func(e *benchEngine, now time.Time) {
			e.store.mu.Lock()
			defer e.store.mu.Unlock()
			e.store.suites, e.store.runs = map[string]map[string]any{}, nil
		}})

	registerScenario(scenario{Name: "bench-long-queue", Description: "A suite of every benchmark twice (about 20 entries) is running, with two more runs of it queued behind.", Status: scEmpty,
		BenchSetup: func(e *benchEngine, now time.Time) {
			e.store.mu.Lock()
			ids := make([]string, 0, len(e.store.benchmarks))
			for id, b := range e.store.benchmarks {
				if _, par := b["parallel"]; !par {
					ids = append(ids, id)
				}
			}
			sort.Strings(ids)
			all := []any{}
			for pass := 0; pass < 2; pass++ {
				for _, id := range ids {
					all = append(all, id)
				}
			}
			e.store.suites["everything"] = map[string]any{"id": "everything", "name": "Everything, twice", "description": "Every benchmark, two passes.",
				"benchmarks": all, "cooldown_s": float64(5)}
			e.store.mu.Unlock()
			for i := 0; i < 3; i++ {
				e.enqueue("everything", "", "long queue scenario", "manual")
			}
		}})
}
