package bench

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Run history that aggregates (design 9.3): what a benchmark, a suite, a model
// file and a model file on N worker nodes have done over the stored runs.

// AggFilter selects the runs and steps to aggregate.
type AggFilter struct {
	Suite     string
	Benchmark string
	Profile   string
	Since     int64 // unix ms, 0 for no lower bound
	Until     int64 // unix ms, 0 for no upper bound
	LimitRuns int   // the newest this many runs (default 200, at most 1000)
	EnvSame   bool  // only runs whose environment equals the latest run's
}

// Aggregate limits.
const (
	DefaultAggRuns = 200
	MaxAggRuns     = 1000
	aggPoints      = 30
)

// Point is one value of a metric: a run and when it ran.
type Point struct {
	RunID string  `json:"run_id"`
	At    int64   `json:"at"`
	Value float64 `json:"value"`
}

// MetricAgg is what a metric did over the runs considered.
type MetricAgg struct {
	Count     int      `json:"count"`
	Min       float64  `json:"min"`
	Median    float64  `json:"median"`
	Mean      float64  `json:"mean"`
	Max       float64  `json:"max"`
	Stdev     float64  `json:"stdev"`
	Last      float64  `json:"last"`
	Best      float64  `json:"best"`
	ChangePct *float64 `json:"change_pct"` // the last value against the median of the earlier ones; null with fewer than 2 values
	// ThresholdPct is the regression limit used for this metric; Regression is
	// set when the last value is worse than the earlier median by more than it.
	ThresholdPct float64 `json:"threshold_pct"`
	Regression   bool    `json:"regression"`
	Points       []Point `json:"points"`
}

// metricDef says what a metric is: which way is better and what limit counts as a regression.
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
	{"lowest_available_mib", false, false, true},     // the lowest available system memory of any node (stepfacts.go)
	{"lowest_available_gpu_mib", false, false, true}, // the lowest free GPU memory of any node with a dedicated GPU
}

func defOf(name string) metricDef {
	for _, d := range metricDefs {
		if d.name == name {
			return d
		}
	}
	return metricDef{name: name}
}

type sample struct {
	runID string
	at    int64
	v     float64
}

// summarizeMetric builds the aggregate of samples, which must be ordered oldest first.
func summarizeMetric(def metricDef, xs []sample, th Thresholds) *MetricAgg {
	if len(xs) == 0 {
		return nil
	}
	vals := make([]float64, len(xs))
	for i, x := range xs {
		vals[i] = x.v
	}
	sm := Summarize(vals)
	m := &MetricAgg{Count: sm.N, Min: sm.Min, Median: sm.Median, Mean: sm.Mean, Max: sm.Max, Stdev: sm.Stdev, Last: vals[len(vals)-1]}
	m.Best = sm.Max
	if def.lowerBetter {
		m.Best = sm.Min
	}
	m.ThresholdPct = th.DecodePct
	if def.loadLimit {
		m.ThresholdPct = th.LoadPct
	}
	if len(vals) >= 2 {
		base := median(vals[:len(vals)-1])
		if base != 0 {
			c := (m.Last - base) / base * 100
			m.ChangePct = &c
			worse := c
			if !def.lowerBetter {
				worse = -c
			}
			m.Regression = !def.neutral && worse > m.ThresholdPct
		}
	}
	from := 0
	if len(xs) > aggPoints {
		from = len(xs) - aggPoints
	}
	for _, x := range xs[from:] {
		m.Points = append(m.Points, Point{RunID: x.runID, At: x.at, Value: round3(x.v)})
	}
	return m
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// TurnAgg is a conversation turn over the runs.
type TurnAgg struct {
	Phase   int                   `json:"phase"`
	Turn    int                   `json:"turn"`
	Metrics map[string]*MetricAgg `json:"metrics"`
}

// BenchmarkAgg is one benchmark over the runs.
type BenchmarkAgg struct {
	BenchmarkID string                `json:"benchmark_id"`
	Name        string                `json:"name"`
	Profile     string                `json:"profile"`
	Runs        int                   `json:"runs"`
	Steps       int                   `json:"steps"`
	PassRate    float64               `json:"pass_rate"`
	Outcomes    map[string]int        `json:"outcomes"`
	Metrics     map[string]*MetricAgg `json:"metrics"`
	Turns       []TurnAgg             `json:"turns,omitempty"`
	LastRunID   string                `json:"last_run_id"`
	LastAt      int64                 `json:"last_at"`
	// Recent is its pass/fail history, oldest first, at most MaxRecent runs.
	Recent []RecentMark `json:"recent"`
}

// SuiteLastRun is the newest run of a suite.
type SuiteLastRun struct {
	ID         string      `json:"id"`
	State      string      `json:"state"`
	StartedAt  int64       `json:"started_at"`
	DurationS  float64     `json:"duration_s"`
	Summary    *RunSummary `json:"summary,omitempty"`
	Regression bool        `json:"regression"`
}

// SuiteAgg is one suite over the runs.
type SuiteAgg struct {
	SuiteID       string         `json:"suite_id"`
	Name          string         `json:"name"`
	Runs          int            `json:"runs"`
	States        map[string]int `json:"states"`
	PassRate      float64        `json:"pass_rate"`
	MeanDurationS float64        `json:"mean_duration_s"`
	LastRun       *SuiteLastRun  `json:"last_run,omitempty"`
	// Recent is its pass/fail history, oldest first, at most MaxRecent runs.
	Recent []RecentMark `json:"recent"`
}

// ModelAgg is a model file (on a number of worker nodes) over the runs.
type ModelAgg struct {
	ModelFile string `json:"model_file"`
	Nodes     *int   `json:"nodes,omitempty"` // worker nodes; set in the by-nodes grouping
	// LoadMode is set in the by-load-mode grouping: file_read, pushed, local,
	// or "not_recorded" for steps stored before the load mode was recorded.
	LoadMode   *string               `json:"load_mode,omitempty"`
	Runs       int                   `json:"runs"`
	Steps      int                   `json:"steps"`
	Benchmarks []string              `json:"benchmarks"`
	Profiles   []string              `json:"profiles"`
	Metrics    map[string]*MetricAgg `json:"metrics"`
}

// AggregatesDoc is GET /api/bench/aggregates.
type AggregatesDoc struct {
	GeneratedAt    int64          `json:"generated_at"`
	Filter         map[string]any `json:"filter"`
	RunsConsidered int            `json:"runs_considered"`
	Environment    *Environment   `json:"environment,omitempty"` // the latest run's
	Benchmarks     []BenchmarkAgg `json:"benchmarks"`
	Suites         []SuiteAgg     `json:"suites"`
	ByModel        []ModelAgg     `json:"by_model"`
	ByModelNodes   []ModelAgg     `json:"by_model_nodes"`
	// ByModelLoadMode groups by model file, worker nodes and load mode.
	ByModelLoadMode []ModelAgg `json:"by_model_load_mode"`
}

// LoadModeNotRecorded is the by-load-mode group of steps stored before the
// load mode was recorded.
const LoadModeNotRecorded = "not_recorded"

func runTime(r *Run) int64 {
	if r.StartedAt > 0 {
		return r.StartedAt
	}
	return r.QueuedAt
}

// aggRuns returns the runs to aggregate, newest first, with their steps.
func (s *Store) aggRuns(f AggFilter) ([]Run, error) {
	q := `SELECT ` + runCols + ` FROM runs WHERE state IN ('completed','failed','cancelled')`
	var args []any
	if f.Suite != "" {
		q += ` AND suite_id=?`
		args = append(args, f.Suite)
	}
	if f.Since > 0 {
		q += ` AND (CASE WHEN started_at>0 THEN started_at ELSE queued_at END) >= ?`
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		q += ` AND (CASE WHEN started_at>0 THEN started_at ELSE queued_at END) <= ?`
		args = append(args, f.Until)
	}
	if f.Benchmark != "" || f.Profile != "" {
		q += ` AND EXISTS (SELECT 1 FROM run_steps st WHERE st.run_id=runs.id`
		if f.Benchmark != "" {
			q += ` AND st.benchmark_id=?`
			args = append(args, f.Benchmark)
		}
		if f.Profile != "" {
			q += ` AND st.profile=?`
			args = append(args, f.Profile)
		}
		q += `)`
	}
	limit := f.LimitRuns
	if limit <= 0 {
		limit = DefaultAggRuns
	}
	if limit > MaxAggRuns {
		limit = MaxAggRuns
	}
	q += ` ORDER BY queued_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		r.Suite = nil
		out = append(out, *r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		st, err := s.stepsOpt(out[i].ID, false)
		if err != nil {
			return nil, err
		}
		out[i].Steps = st
	}
	return out, nil
}

// stepValues are the numbers one step contributes (only a step that loaded and served has any).
func stepValues(st *Step) map[string]float64 {
	v := map[string]float64{}
	if st.Outcome != OutcomeReady {
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
	if low := LowestMiB(st); low != nil {
		v["lowest_available_mib"] = float64(*low)
	}
	if low := LowestGPUMiB(st); low != nil {
		v["lowest_available_gpu_mib"] = float64(*low)
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

// metricSet collects samples per metric and summarizes them.
type metricSet map[string][]sample

func (ms metricSet) add(runID string, at int64, vals map[string]float64) {
	for k, v := range vals {
		ms[k] = append(ms[k], sample{runID, at, v})
	}
}

func (ms metricSet) summarize(th Thresholds) map[string]*MetricAgg {
	out := map[string]*MetricAgg{}
	for k, xs := range ms {
		if m := summarizeMetric(defOf(k), xs, th); m != nil {
			out[k] = m
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// Aggregate works out the aggregates of the stored runs.
func (s *Store) Aggregate(f AggFilter, th Thresholds, now time.Time) (*AggregatesDoc, error) {
	runs, err := s.aggRuns(f)
	if err != nil {
		return nil, err
	}
	doc := &AggregatesDoc{GeneratedAt: now.UnixMilli(), Benchmarks: []BenchmarkAgg{}, Suites: []SuiteAgg{}, ByModel: []ModelAgg{}, ByModelNodes: []ModelAgg{}, ByModelLoadMode: []ModelAgg{},
		Filter: map[string]any{"suite": f.Suite, "benchmark": f.Benchmark, "profile": f.Profile, "since": f.Since, "until": f.Until, "limit_runs": f.LimitRuns, "env": ""}}
	if len(runs) > 0 {
		doc.Environment = runs[0].Env
	}
	if f.EnvSame {
		doc.Filter["env"] = "same"
		if len(runs) > 0 {
			want := runs[0].Env.Key()
			kept := runs[:0:0]
			for _, r := range runs {
				if r.Env.Key() == want {
					kept = append(kept, r)
				}
			}
			runs = kept
		}
	}
	doc.RunsConsidered = len(runs)
	// oldest first, so that "last" is the newest
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}

	type benchAcc struct {
		agg      BenchmarkAgg
		runs     map[string]bool
		passed   int
		metrics  metricSet
		turns    map[[2]int]metricSet
		hasTurns bool
	}
	benches := map[string]*benchAcc{}
	type modelKey struct {
		file  string
		nodes int
		mode  string
	}
	type modelAcc struct {
		runs     map[string]bool
		steps    int
		benches  map[string]bool
		profiles map[string]bool
		metrics  metricSet
	}
	byModel := map[string]*modelAcc{}
	byNodes := map[modelKey]*modelAcc{}
	byMode := map[modelKey]*modelAcc{}
	suites := map[string]*SuiteAgg{}
	suiteDur := map[string][]float64{}
	suitePass := map[string]int{}

	for ri := range runs {
		r := &runs[ri]
		at := runTime(r)
		// suites
		sa := suites[r.SuiteID]
		if sa == nil {
			sa = &SuiteAgg{SuiteID: r.SuiteID, States: map[string]int{}}
			suites[r.SuiteID] = sa
		}
		sa.Name = r.SuiteName
		sa.Runs++
		sa.States[r.State]++
		if r.State == RunCompleted && r.Summary != nil && r.Summary.Failed == 0 {
			suitePass[r.SuiteID]++
		}
		dur := 0.0
		if r.StartedAt > 0 && r.FinishedAt > r.StartedAt {
			dur = float64(r.FinishedAt-r.StartedAt) / 1000
			suiteDur[r.SuiteID] = append(suiteDur[r.SuiteID], dur)
		}
		reg := r.Summary != nil && r.Summary.Regressions > 0
		sa.LastRun = &SuiteLastRun{ID: r.ID, State: r.State, StartedAt: at, DurationS: round3(dur), Summary: r.Summary, Regression: reg}

		for si := range r.Steps {
			st := &r.Steps[si]
			if st.State == StepSkipped || st.Outcome == OutcomeSkipped {
				continue
			}
			if f.Benchmark != "" && st.BenchmarkID != f.Benchmark || f.Profile != "" && st.Profile != f.Profile {
				continue
			}
			ba := benches[st.BenchmarkID]
			if ba == nil {
				ba = &benchAcc{runs: map[string]bool{}, metrics: metricSet{}, turns: map[[2]int]metricSet{},
					agg: BenchmarkAgg{BenchmarkID: st.BenchmarkID, Outcomes: map[string]int{"ready": 0, "refused": 0, "failed": 0}}}
				benches[st.BenchmarkID] = ba
			}
			ba.agg.Name, ba.agg.Profile = st.Name, st.Profile
			ba.agg.LastRunID, ba.agg.LastAt = r.ID, at
			ba.runs[r.ID] = true
			ba.agg.Steps++
			ba.agg.Outcomes[st.Outcome]++
			if st.Expect != nil && st.Expect.Pass {
				ba.passed++
			}
			vals := stepValues(st)
			ba.metrics.add(r.ID, at, vals)
			for _, p := range st.Phases {
				if p.Type != "conversation" || st.Outcome != OutcomeReady {
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
					ba.hasTurns = true
				}
			}
			// groupings by model file (and by worker nodes)
			if st.ModelFile != "" && st.Outcome == OutcomeReady {
				add := func(acc *modelAcc) {
					acc.runs[r.ID] = true
					acc.steps++
					acc.benches[st.BenchmarkID] = true
					acc.profiles[st.Profile] = true
					acc.metrics.add(r.ID, at, vals)
				}
				ma := byModel[st.ModelFile]
				if ma == nil {
					ma = &modelAcc{runs: map[string]bool{}, benches: map[string]bool{}, profiles: map[string]bool{}, metrics: metricSet{}}
					byModel[st.ModelFile] = ma
				}
				add(ma)
				k := modelKey{file: st.ModelFile, nodes: st.NodeCount}
				na := byNodes[k]
				if na == nil {
					na = &modelAcc{runs: map[string]bool{}, benches: map[string]bool{}, profiles: map[string]bool{}, metrics: metricSet{}}
					byNodes[k] = na
				}
				add(na)
				mode := StepLoadMode(st)
				if mode == "" {
					mode = LoadModeNotRecorded
				}
				k.mode = mode
				la := byMode[k]
				if la == nil {
					la = &modelAcc{runs: map[string]bool{}, benches: map[string]bool{}, profiles: map[string]bool{}, metrics: metricSet{}}
					byMode[k] = la
				}
				add(la)
			}
		}
	}

	suiteMarks, benchMarks := RecentOf(runs)
	for _, id := range sortedKeys(benches) {
		ba := benches[id]
		a := ba.agg
		a.Recent = clipRecent(benchMarks[id])
		a.Runs = len(ba.runs)
		if a.Steps > 0 {
			a.PassRate = round4(float64(ba.passed) / float64(a.Steps))
		}
		a.Metrics = ba.metrics.summarize(th)
		if ba.hasTurns {
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
			for _, k := range keys {
				a.Turns = append(a.Turns, TurnAgg{Phase: k[0], Turn: k[1], Metrics: ba.turns[k].summarize(th)})
			}
		}
		doc.Benchmarks = append(doc.Benchmarks, a)
	}
	for _, id := range sortedKeys(suites) {
		sa := suites[id]
		sa.Recent = clipRecent(suiteMarks[id])
		if sa.Runs > 0 {
			sa.PassRate = round4(float64(suitePass[id]) / float64(sa.Runs))
		}
		if d := suiteDur[id]; len(d) > 0 {
			var sum float64
			for _, x := range d {
				sum += x
			}
			sa.MeanDurationS = round3(sum / float64(len(d)))
		}
		doc.Suites = append(doc.Suites, *sa)
	}
	mk := func(file string, nodes *int, acc *modelAcc) ModelAgg {
		return ModelAgg{ModelFile: file, Nodes: nodes, Runs: len(acc.runs), Steps: acc.steps, Benchmarks: sortedKeys(acc.benches), Profiles: sortedKeys(acc.profiles), Metrics: acc.metrics.summarize(th)}
	}
	for _, file := range sortedKeys(byModel) {
		doc.ByModel = append(doc.ByModel, mk(file, nil, byModel[file]))
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
	for _, k := range keys {
		n := k.nodes
		doc.ByModelNodes = append(doc.ByModelNodes, mk(k.file, &n, byNodes[k]))
	}
	keys = keys[:0]
	for k := range byMode {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.file != b.file {
			return a.file < b.file
		}
		if a.nodes != b.nodes {
			return a.nodes < b.nodes
		}
		return a.mode < b.mode
	})
	for _, k := range keys {
		n, mode := k.nodes, k.mode
		ma := mk(k.file, &n, byMode[k])
		ma.LoadMode = &mode
		doc.ByModelLoadMode = append(doc.ByModelLoadMode, ma)
	}
	return doc, nil
}

// ParseAggTime reads a time bound: unix milliseconds or seconds, an RFC 3339
// time, or a span back from now such as 24h, 7d or 90m.
func ParseAggTime(v string, now time.Time) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("a time cannot be negative")
		}
		if n < 100_000_000_000 { // seconds
			n *= 1000
		}
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), nil
	}
	if len(v) > 1 {
		unit := v[len(v)-1]
		if n, err := strconv.ParseFloat(v[:len(v)-1], 64); err == nil && n >= 0 {
			var d time.Duration
			switch unit {
			case 'd':
				d = time.Duration(n * float64(24*time.Hour))
			case 'h':
				d = time.Duration(n * float64(time.Hour))
			case 'm':
				d = time.Duration(n * float64(time.Minute))
			case 'w':
				d = time.Duration(n * float64(7*24*time.Hour))
			}
			if d > 0 {
				return now.Add(-d).UnixMilli(), nil
			}
		}
	}
	return 0, fmt.Errorf("%q is not a time: use unix milliseconds, an RFC 3339 time or a span such as 24h or 7d", v)
}
