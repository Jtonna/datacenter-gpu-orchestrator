// DEV-ONLY MOCK of GET /api/bench/aggregates: a port of Store.Aggregate of
// feat/bench-phases (internal/bench/aggregates.go) over the mock's run store,
// so the numbers move after a mock run.

package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
)

const aggPoints = 30

type metricDef struct {
	name        string
	lowerBetter bool
	loadLimit   bool // uses the load threshold (else the decode threshold)
	neutral     bool // neither direction is better (never flagged)
}

var metricDefs = []metricDef{
	{"load_s", true, true, false},
	{"load_mb_s", false, false, false},
	{"decode_tps", false, false, false},
	{"prefill_tps", false, false, false},
	{"ttft_ms", true, true, false},
	{"memory_mib", true, true, false},
	{"unload_s", true, true, false},
	{"concurrent_wall_s", true, true, false},
	{"wall_ms", true, true, false},
	{"reused_tokens", false, false, false},
	{"prompt_tokens", false, false, true},
}

func defOf(name string) metricDef {
	for _, d := range metricDefs {
		if d.name == name {
			return d
		}
	}
	return metricDef{name: name}
}

type aggSample struct {
	runID string
	at    int64
	v     float64
}

type metricSet map[string][]aggSample

func (ms metricSet) add(runID string, at int64, vals map[string]float64) {
	for k, v := range vals {
		ms[k] = append(ms[k], aggSample{runID, at, v})
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// summarizeMetric is the MetricAgg of samples ordered oldest first.
func summarizeMetric(def metricDef, xs []aggSample) map[string]any {
	vals := make([]float64, len(xs))
	for i, x := range xs {
		vals[i] = x.v
	}
	sm := summarize(vals)
	last := vals[len(vals)-1]
	best := sm.Max
	if def.lowerBetter {
		best = sm.Min
	}
	th := thDecodePct
	if def.loadLimit {
		th = thLoadPct
	}
	var change any
	reg := false
	if len(vals) >= 2 {
		if b := median(vals[:len(vals)-1]); b != 0 {
			c := (last - b) / b * 100
			change = round3(c)
			worse := c
			if !def.lowerBetter {
				worse = -c
			}
			reg = !def.neutral && worse > th
		}
	}
	from := max(len(xs)-aggPoints, 0)
	pts := []map[string]any{}
	for _, x := range xs[from:] {
		pts = append(pts, map[string]any{"run_id": x.runID, "at": x.at, "value": round3(x.v)})
	}
	return map[string]any{"count": sm.N, "min": sm.Min, "median": sm.Median, "mean": sm.Mean, "max": sm.Max, "stdev": sm.Stdev,
		"last": last, "best": best, "change_pct": change, "threshold_pct": th, "regression": reg, "points": pts}
}

func (ms metricSet) summarize() map[string]any {
	out := map[string]any{}
	for k, xs := range ms {
		if len(xs) > 0 {
			out[k] = summarizeMetric(defOf(k), xs)
		}
	}
	return out
}

// stepValues is the metrics one step contributes (ready steps only).
func stepValues(st *mStep) map[string]float64 {
	v := map[string]float64{}
	if st.Outcome != bench.OutcomeReady {
		return v
	}
	if st.Load != nil {
		if st.Load.LoadS > 0 {
			v["load_s"] = st.Load.LoadS
		}
		if st.Load.AvgMBs > 0 {
			v["load_mb_s"] = st.Load.AvgMBs
		}
		if st.Load.MemoryMiB > 0 {
			v["memory_mib"] = st.Load.MemoryMiB
		}
	}
	if s := st.Stats; s != nil {
		if s.Decode != nil && s.Decode.Mean > 0 {
			v["decode_tps"] = s.Decode.Mean
		}
		if s.Prefill != nil && s.Prefill.Mean > 0 {
			v["prefill_tps"] = s.Prefill.Mean
		}
		if s.TTFT != nil && s.TTFT.Mean > 0 {
			v["ttft_ms"] = s.TTFT.Mean
		}
	}
	for _, p := range st.Phases {
		switch {
		case p.Type == "unload" && p.Unload != nil && p.Unload.Loaded && p.Unload.Gone:
			v["unload_s"] = p.Unload.UnloadS
		case p.Type == "concurrent_requests" && p.Concurrent != nil && p.Concurrent.WallMS > 0:
			v["concurrent_wall_s"] = p.Concurrent.WallMS / 1000
		}
	}
	return v
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

func runTime(r *mRun) int64 {
	if r.StartedAt > 0 {
		return r.StartedAt
	}
	return r.QueuedAt
}

// parseAggTime is bench.ParseAggTime: unix ms (or s), RFC 3339, or a span back from now (24h, 7d, 2w).
func parseAggTime(v string, now time.Time) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("a time cannot be negative")
		}
		if n < 100_000_000_000 {
			n *= 1000
		}
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), nil
	}
	if len(v) > 1 {
		if n, err := strconv.ParseFloat(v[:len(v)-1], 64); err == nil && n >= 0 {
			unit := map[byte]time.Duration{'d': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 'w': 7 * 24 * time.Hour}[v[len(v)-1]]
			if d := time.Duration(n * float64(unit)); d > 0 {
				return now.Add(-d).UnixMilli(), nil
			}
		}
	}
	return 0, fmt.Errorf("%q is not a time: use unix milliseconds, an RFC 3339 time or a span such as 24h or 7d", v)
}

func badParam(w http.ResponseWriter, format string, args ...any) {
	writeErr(w, 400, fmt.Sprintf(format, args...), "bad request")
}

func (a *benchAPI) aggregates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	fSuite, fBench, fProfile := q.Get("suite"), q.Get("benchmark"), q.Get("profile")
	since, err := parseAggTime(q.Get("since"), now)
	if err != nil {
		badParam(w, "since: %v", err)
		return
	}
	until, err := parseAggTime(q.Get("until"), now)
	if err != nil {
		badParam(w, "until: %v", err)
		return
	}
	limit := 200
	if v := q.Get("limit_runs"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			badParam(w, "limit_runs must be 1 to 1000, got %q", v)
			return
		}
		limit = n
	}
	envSame := false
	switch v := q.Get("env"); v {
	case "":
	case "same":
		envSame = true
	default:
		badParam(w, "env must be same (or left out), got %q", v)
		return
	}

	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	// aggRuns: final runs (not skipped), newest first, at most limit
	var runs []*mRun
	for i := len(a.store.runs) - 1; i >= 0 && len(runs) < limit; i-- {
		r := a.store.runs[i]
		if r.State != bench.RunCompleted && r.State != bench.RunFailed && r.State != bench.RunCancelled {
			continue
		}
		if fSuite != "" && r.SuiteID != fSuite || since > 0 && runTime(r) < since || until > 0 && runTime(r) > until {
			continue
		}
		if fBench != "" || fProfile != "" {
			hit := false
			for _, s := range r.Steps {
				hit = hit || (fBench == "" || s.BenchmarkID == fBench) && (fProfile == "" || s.Profile == fProfile)
			}
			if !hit {
				continue
			}
		}
		runs = append(runs, r)
	}
	doc := map[string]any{"generated_at": now.UnixMilli(),
		"filter": map[string]any{"suite": fSuite, "benchmark": fBench, "profile": fProfile, "since": since, "until": until, "limit_runs": qInt(r, "limit_runs", 0), "env": ""}}
	if len(runs) > 0 {
		doc["environment"] = runs[0].Env
	}
	if envSame {
		doc["filter"].(map[string]any)["env"] = "same"
		if len(runs) > 0 {
			want := runs[0].Env.key()
			var kept []*mRun
			for _, r := range runs {
				if r.Env.key() == want {
					kept = append(kept, r)
				}
			}
			runs = kept
		}
	}
	doc["runs_considered"] = len(runs)
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}

	type benchAcc struct {
		id, name, profile string
		runs              map[string]bool
		steps, passed     int
		outcomes          map[string]int
		metrics           metricSet
		turns             map[[2]int]metricSet
		lastRun           string
		lastAt            int64
	}
	type modelKey struct {
		file  string
		nodes int
	}
	type modelAcc struct {
		runs     map[string]bool
		steps    int
		benches  map[string]bool
		profiles map[string]bool
		metrics  metricSet
	}
	newModel := func() *modelAcc {
		return &modelAcc{runs: map[string]bool{}, benches: map[string]bool{}, profiles: map[string]bool{}, metrics: metricSet{}}
	}
	type suiteAcc struct {
		name   string
		runs   int
		states map[string]int
		passed int
		durs   []float64
		last   map[string]any
	}
	benches := map[string]*benchAcc{}
	byModel := map[string]*modelAcc{}
	byNodes := map[modelKey]*modelAcc{}
	suites := map[string]*suiteAcc{}

	for _, r := range runs {
		at := runTime(r)
		sa := suites[r.SuiteID]
		if sa == nil {
			sa = &suiteAcc{states: map[string]int{}}
			suites[r.SuiteID] = sa
		}
		sa.name = r.SuiteName
		sa.runs++
		sa.states[r.State]++
		if r.State == bench.RunCompleted && r.Summary != nil && r.Summary.Failed == 0 {
			sa.passed++
		}
		dur := 0.0
		if r.StartedAt > 0 && r.FinishedAt > r.StartedAt {
			dur = float64(r.FinishedAt-r.StartedAt) / 1000
			sa.durs = append(sa.durs, dur)
		}
		last := map[string]any{"id": r.ID, "state": r.State, "started_at": at, "duration_s": round3(dur),
			"regression": r.Summary != nil && r.Summary.Regressions > 0}
		if r.Summary != nil {
			last["summary"] = r.Summary
		}
		sa.last = last

		for _, st := range r.Steps {
			if st.State == bench.StepSkipped || st.Outcome == bench.OutcomeSkipped {
				continue
			}
			if fBench != "" && st.BenchmarkID != fBench || fProfile != "" && st.Profile != fProfile {
				continue
			}
			ba := benches[st.BenchmarkID]
			if ba == nil {
				ba = &benchAcc{id: st.BenchmarkID, runs: map[string]bool{}, metrics: metricSet{}, turns: map[[2]int]metricSet{},
					outcomes: map[string]int{"ready": 0, "refused": 0, "failed": 0}}
				benches[st.BenchmarkID] = ba
			}
			ba.name, ba.profile, ba.lastRun, ba.lastAt = st.Name, st.Profile, r.ID, at
			ba.runs[r.ID] = true
			ba.steps++
			ba.outcomes[st.Outcome]++
			if st.Expect != nil && st.Expect.Pass {
				ba.passed++
			}
			vals := stepValues(st)
			ba.metrics.add(r.ID, at, vals)
			for _, p := range st.Phases {
				if p.Type != "conversation" || st.Outcome != bench.OutcomeReady {
					continue
				}
				for _, t := range p.Turns {
					key := [2]int{p.Idx, t.Turn}
					if ba.turns[key] == nil {
						ba.turns[key] = metricSet{}
					}
					tv := map[string]float64{"wall_ms": t.WallMS, "prompt_tokens": float64(t.PromptTokens), "reused_tokens": float64(t.CacheN)}
					if t.DecodeTPS > 0 {
						tv["decode_tps"] = t.DecodeTPS
					}
					if t.PrefillTPS > 0 {
						tv["prefill_tps"] = t.PrefillTPS
					}
					if t.TTFTMS != nil {
						tv["ttft_ms"] = *t.TTFTMS
					}
					ba.turns[key].add(r.ID, at, tv)
				}
			}
			if st.ModelFile != "" && st.Outcome == bench.OutcomeReady {
				add := func(acc *modelAcc) {
					acc.runs[r.ID] = true
					acc.steps++
					acc.benches[st.BenchmarkID] = true
					acc.profiles[st.Profile] = true
					acc.metrics.add(r.ID, at, vals)
				}
				if byModel[st.ModelFile] == nil {
					byModel[st.ModelFile] = newModel()
				}
				add(byModel[st.ModelFile])
				k := modelKey{st.ModelFile, st.NodeCount}
				if byNodes[k] == nil {
					byNodes[k] = newModel()
				}
				add(byNodes[k])
			}
		}
	}

	benchOut := []any{}
	for _, id := range sortedKeys(benches) {
		ba := benches[id]
		m := map[string]any{"benchmark_id": ba.id, "name": ba.name, "profile": ba.profile, "runs": len(ba.runs), "steps": ba.steps,
			"pass_rate": 0.0, "outcomes": ba.outcomes, "metrics": ba.metrics.summarize(), "last_run_id": ba.lastRun, "last_at": ba.lastAt}
		if ba.steps > 0 {
			m["pass_rate"] = round4(float64(ba.passed) / float64(ba.steps))
		}
		if len(ba.turns) > 0 {
			keys := make([][2]int, 0, len(ba.turns))
			for k := range ba.turns {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				if keys[i][0] != keys[j][0] {
					return keys[i][0] < keys[j][0]
				}
				return keys[i][1] < keys[j][1]
			})
			var tl []any
			for _, k := range keys {
				tl = append(tl, map[string]any{"phase": k[0], "turn": k[1], "metrics": ba.turns[k].summarize()})
			}
			m["turns"] = tl
		}
		benchOut = append(benchOut, m)
	}
	suiteOut := []any{}
	for _, id := range sortedKeys(suites) {
		sa := suites[id]
		m := map[string]any{"suite_id": id, "name": sa.name, "runs": sa.runs, "states": sa.states, "pass_rate": 0.0, "mean_duration_s": 0.0, "last_run": sa.last}
		if sa.runs > 0 {
			m["pass_rate"] = round4(float64(sa.passed) / float64(sa.runs))
		}
		if len(sa.durs) > 0 {
			sum := 0.0
			for _, x := range sa.durs {
				sum += x
			}
			m["mean_duration_s"] = round3(sum / float64(len(sa.durs)))
		}
		suiteOut = append(suiteOut, m)
	}
	mk := func(file string, nodes *int, acc *modelAcc) map[string]any {
		m := map[string]any{"model_file": file, "runs": len(acc.runs), "steps": acc.steps, "benchmarks": sortedKeys(acc.benches),
			"profiles": sortedKeys(acc.profiles), "metrics": acc.metrics.summarize()}
		if nodes != nil {
			m["nodes"] = *nodes
		}
		return m
	}
	modelOut := []any{}
	for _, f := range sortedKeys(byModel) {
		modelOut = append(modelOut, mk(f, nil, byModel[f]))
	}
	var keys []modelKey
	for k := range byNodes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].file != keys[j].file {
			return keys[i].file < keys[j].file
		}
		return keys[i].nodes < keys[j].nodes
	})
	nodesOut := []any{}
	for _, k := range keys {
		n := k.nodes
		nodesOut = append(nodesOut, mk(k.file, &n, byNodes[k]))
	}
	doc["benchmarks"], doc["suites"], doc["by_model"], doc["by_model_nodes"] = benchOut, suiteOut, modelOut, nodesOut
	writeJSON(w, 200, doc)
}
