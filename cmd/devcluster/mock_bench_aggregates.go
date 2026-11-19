// DEV-ONLY MOCK of a server contract that is not built yet: GET
// /api/bench/aggregates and the metrics behind it, computed from the mock's
// run store (so they move after a mock run).

package main

import (
	"math"
	"net/http"
	"sort"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
)

var metricFuncs = map[string]func(*mStep) (float64, bool){
	"load_s": func(s *mStep) (float64, bool) {
		if s.Load == nil || s.Load.LoadS <= 0 {
			return 0, false
		}
		return s.Load.LoadS, true
	},
	"load_mb_s": func(s *mStep) (float64, bool) {
		if s.Load == nil || s.Load.AvgMBs <= 0 {
			return 0, false
		}
		return s.Load.AvgMBs, true
	},
	"decode_tps": func(s *mStep) (float64, bool) {
		if s.Stats == nil || s.Stats.Decode == nil {
			return 0, false
		}
		return s.Stats.Decode.Mean, true
	},
	"prefill_tps": func(s *mStep) (float64, bool) {
		if s.Stats == nil || s.Stats.Prefill == nil {
			return 0, false
		}
		return s.Stats.Prefill.Mean, true
	},
	"ttft_ms": func(s *mStep) (float64, bool) {
		if s.Stats == nil || s.Stats.TTFT == nil {
			return 0, false
		}
		return s.Stats.TTFT.Mean, true
	},
	"memory_mib": func(s *mStep) (float64, bool) {
		if s.Load == nil || s.Load.MemoryMiB <= 0 {
			return 0, false
		}
		return s.Load.MemoryMiB, true
	},
}

var metricOrder = []string{"load_s", "load_mb_s", "decode_tps", "prefill_tps", "ttft_ms", "memory_mib"}

// lowerIsBetter says which direction "best" takes.
var lowerIsBetter = map[string]bool{"load_s": true, "ttft_ms": true, "memory_mib": true}

type point struct {
	ts    int64
	run   string
	value float64
	reg   bool
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

func median(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func fullMetric(name string, pts []point) map[string]any {
	var vs []float64
	regs := 0
	for _, p := range pts {
		vs = append(vs, p.value)
		if p.reg {
			regs++
		}
	}
	sm := summarize(vs)
	last := vs[len(vs)-1]
	best := sm.Max
	if lowerIsBetter[name] {
		best = sm.Min
	}
	var change any
	if len(vs) > 1 {
		if m := median(vs[:len(vs)-1]); m != 0 {
			change = math.Round((last-m)/m*1000) / 10
		}
	}
	tail := pts
	if len(tail) > 30 {
		tail = tail[len(tail)-30:]
	}
	points := []map[string]any{}
	for _, p := range tail {
		points = append(points, map[string]any{"ts": p.ts, "run_id": p.run, "value": p.value})
	}
	return map[string]any{"count": len(vs), "min": sm.Min, "median": sm.Median, "mean": sm.Mean, "max": sm.Max, "stdev": sm.Stdev,
		"last": r2(last), "best": best, "change_pct": change, "regressions": regs, "points": points}
}

func shortMetric(vs []float64) map[string]any {
	sm := summarize(vs)
	return map[string]any{"count": sm.N, "median": sm.Median, "min": sm.Min, "max": sm.Max}
}

func (a *benchAPI) aggregates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fSuite, fBench, fProfile := q.Get("suite"), q.Get("benchmark"), q.Get("profile")
	since, until := qInt64(r, "since"), qInt64(r, "until")
	// env is accepted for the contract; the mock has one environment.

	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	type benchAgg struct {
		id, name, profile, model string
		workers                  int
		phaseTypes               []string
		steps                    int
		passed, judged           int
		outcomes                 map[string]int
		pts                      map[string][]point
	}
	aggs := map[string]*benchAgg{}
	var order []string
	type wkey struct {
		model   string
		workers int
	}
	byModelWorkers := map[wkey]map[string][]float64{}
	byModelRuns := map[wkey]int{}
	byWorkers := map[int]map[string][]float64{}
	byWorkersRuns := map[int]int{}
	type suiteAgg struct {
		runs, passed int
		dur          float64
		durN         int
		last         *mRun
	}
	suites := map[string]*suiteAgg{}

	for _, run := range a.store.runs {
		if fSuite != "" && run.SuiteID != fSuite {
			continue
		}
		if since > 0 && run.StartedAt < since || until > 0 && run.StartedAt > until {
			continue
		}
		if !bench.IsFinal(run.State) {
			continue
		}
		sa := suites[run.SuiteID]
		if sa == nil {
			sa = &suiteAgg{}
			suites[run.SuiteID] = sa
		}
		sa.runs++
		sa.last = run
		if run.State == bench.RunCompleted && run.Summary != nil && run.Summary.Failed == 0 {
			sa.passed++
		}
		if run.FinishedAt > run.StartedAt && run.StartedAt > 0 {
			sa.dur += float64(run.FinishedAt-run.StartedAt) / 1000
			sa.durN++
		}
		for _, s := range run.Steps {
			if fBench != "" && s.BenchmarkID != fBench || fProfile != "" && s.Profile != fProfile {
				continue
			}
			ag := aggs[s.BenchmarkID]
			if ag == nil {
				bm := a.store.benchmarks[s.BenchmarkID]
				if bm == nil {
					bm = s.Spec
				}
				ag = &benchAgg{id: s.BenchmarkID, name: s.Name, profile: s.Profile, model: s.ModelFile, workers: len(s.Nodes),
					phaseTypes: phaseTypes(bm), outcomes: map[string]int{"ready": 0, "failed": 0, "refused": 0, "error": 0, "cancelled": 0, "skipped": 0},
					pts: map[string][]point{}}
				aggs[s.BenchmarkID] = ag
				order = append(order, s.BenchmarkID)
			}
			ag.steps++
			if s.Outcome != "" {
				ag.outcomes[s.Outcome]++
			}
			if s.Outcome != bench.OutcomeSkipped && s.Outcome != bench.OutcomeCancelled && s.Expect != nil {
				ag.judged++
				if s.Expect.Pass {
					ag.passed++
				}
			}
			if s.Outcome != bench.OutcomeReady || (run.State != bench.RunCompleted && run.State != bench.RunFailed) {
				continue
			}
			reg := s.Comparison != nil && s.Comparison.Regression
			k := wkey{s.ModelFile, len(s.Nodes)}
			if byModelWorkers[k] == nil {
				byModelWorkers[k] = map[string][]float64{}
			}
			if byWorkers[len(s.Nodes)] == nil {
				byWorkers[len(s.Nodes)] = map[string][]float64{}
			}
			byModelRuns[k]++
			byWorkersRuns[len(s.Nodes)]++
			for _, m := range metricOrder {
				if v, ok := metricFuncs[m](s); ok {
					ag.pts[m] = append(ag.pts[m], point{ts: run.StartedAt, run: run.ID, value: v, reg: reg && (m == "load_s" || m == "decode_tps")})
					if m != "ttft_ms" {
						byModelWorkers[k][m] = append(byModelWorkers[k][m], v)
						byWorkers[len(s.Nodes)][m] = append(byWorkers[len(s.Nodes)][m], v)
					}
				}
			}
		}
	}

	sort.Strings(order)
	benches := []any{}
	for _, id := range order {
		ag := aggs[id]
		metrics := map[string]any{}
		for _, m := range metricOrder {
			if len(ag.pts[m]) > 0 {
				metrics[m] = fullMetric(m, ag.pts[m])
			}
		}
		var pass any
		if ag.judged > 0 {
			pass = math.Round(float64(ag.passed)/float64(ag.judged)*100) / 100
		}
		benches = append(benches, map[string]any{"benchmark_id": ag.id, "name": ag.name, "profile": ag.profile, "model_file": ag.model,
			"workers": ag.workers, "phase_types": ag.phaseTypes, "runs": ag.steps, "pass_rate": pass, "outcomes": ag.outcomes, "metrics": metrics})
	}

	suiteOut := []any{}
	var sids []string
	for id := range suites {
		sids = append(sids, id)
	}
	sort.Strings(sids)
	for _, id := range sids {
		sa := suites[id]
		name := id
		if su := a.store.suites[id]; su != nil {
			name, _ = su["name"].(string)
		}
		var mean any
		if sa.durN > 0 {
			mean = math.Round(sa.dur/float64(sa.durN)*10) / 10
		}
		var last any
		if sa.last != nil {
			last = map[string]any{"id": sa.last.ID, "state": sa.last.State, "started_at": sa.last.StartedAt, "finished_at": sa.last.FinishedAt, "summary": sa.last.Summary}
		}
		suiteOut = append(suiteOut, map[string]any{"suite_id": id, "name": name, "runs": sa.runs,
			"pass_rate": math.Round(float64(sa.passed)/float64(max(sa.runs, 1))*100) / 100, "mean_duration_s": mean, "last_run": last})
	}

	reduced := func(ms map[string][]float64) map[string]any {
		out := map[string]any{}
		for _, m := range []string{"load_s", "load_mb_s", "decode_tps", "prefill_tps", "memory_mib"} {
			if len(ms[m]) > 0 {
				out[m] = shortMetric(ms[m])
			}
		}
		return out
	}
	models := map[string][]int{}
	for k := range byModelWorkers {
		models[k.model] = append(models[k.model], k.workers)
	}
	var mnames []string
	for m := range models {
		mnames = append(mnames, m)
	}
	sort.Strings(mnames)
	byModel := []any{}
	for _, m := range mnames {
		ws := models[m]
		sort.Ints(ws)
		total := 0
		var bw []any
		for _, wn := range ws {
			k := wkey{m, wn}
			total += byModelRuns[k]
			bw = append(bw, map[string]any{"workers": wn, "runs": byModelRuns[k], "metrics": reduced(byModelWorkers[k])})
		}
		byModel = append(byModel, map[string]any{"model_file": m, "runs": total, "by_workers": bw})
	}
	var wns []int
	for wn := range byWorkers {
		wns = append(wns, wn)
	}
	sort.Ints(wns)
	byW := []any{}
	for _, wn := range wns {
		byW = append(byW, map[string]any{"workers": wn, "runs": byWorkersRuns[wn], "metrics": reduced(byWorkers[wn])})
	}
	writeJSON(w, 200, map[string]any{"benchmarks": benches, "suites": suiteOut, "by_model": byModel, "by_workers": byW})
}
