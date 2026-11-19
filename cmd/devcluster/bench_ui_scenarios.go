package main

// Scenarios for the Benchmarks screens' empty and crowded states, as frozen
// bench worlds (see Bench in scenarios.go): the snapshot of the real database
// is changed through the real store before anything runs.

import (
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
)

func init() {
	registerScenario(scenario{Name: "bench-no-benchmarks", Description: "Benchmarks screen empty states: no benchmarks, no suites, no runs (a snapshot with everything deleted).", Status: scEmpty,
		Bench: &frozenSpec{Prepare: func(st *bench.Store) error { return clearBench(st, true, true) }}})

	registerScenario(scenario{Name: "bench-no-suites", Description: "Benchmarks exist but no suite and no run (a snapshot with the suites and runs deleted).", Status: scEmpty,
		Bench: &frozenSpec{Prepare: func(st *bench.Store) error { return clearBench(st, true, false) }}})

	registerScenario(scenario{Name: "bench-long-queue", Description: "A suite of every benchmark twice (20 entries) is running, held in the 3-worker Gemma load at 55%, with two more runs of it queued behind.", Status: scEmpty,
		Bench: &frozenSpec{Prepare: addEverything, Suite: "everything", Runs: 3, FreezeLoad: map[string]float64{"bench-gemma-12b-3w": 0.55}, Lead: 270 * time.Second}})
}

// addEverything adds the suite "everything": every benchmark that is not a parallel step, twice.
func addEverything(st *bench.Store) error {
	bs, err := st.ListBenchmarks()
	if err != nil {
		return err
	}
	var items []benchspec.SuiteItem
	for pass := 0; pass < 2; pass++ {
		for _, b := range bs {
			if !b.Spec.IsParallel() && b.Spec.ID != "gemma-idle-hold" {
				items = append(items, benchspec.SuiteItem{Ref: b.Spec.ID})
			}
		}
	}
	cd := 0
	su := &benchspec.Suite{ID: "everything", Name: "Everything, twice", Description: "Every benchmark, two passes.", Benchmarks: items, CooldownS: &cd}
	return st.SaveSuite(su, "api", time.Now().UnixMilli())
}
