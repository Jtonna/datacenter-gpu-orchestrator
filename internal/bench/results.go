package bench

// Results over runs, for the suite and benchmark pages of the web UI: a short
// pass/fail history (the dots of a health monitor) on every aggregate, and the
// figures of one benchmark run by run.

// Results of a run or of one benchmark in a run.
const (
	ResultPass      = "pass"      // every expectation held
	ResultPartial   = "partial"   // a suite run: some benchmarks passed, some failed
	ResultFail      = "fail"      // an expectation did not hold, or the run failed
	ResultCancelled = "cancelled" // stopped before it finished
	ResultNone      = "none"      // skipped, or nothing to judge
)

// MaxRecent is how many marks an aggregate carries (the newest ones).
const MaxRecent = 30

// RecentMark is one run in a pass/fail history, oldest first in a list.
type RecentMark struct {
	RunID  string `json:"run_id"`
	At     int64  `json:"at"`
	Result string `json:"result"`
	// Passed and Failed count the benchmarks of a suite run; for one benchmark
	// Outcome is what happened (ready, refused, failed, ...).
	Passed  int    `json:"passed,omitempty"`
	Failed  int    `json:"failed,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// StepResult judges one step the way the pass rate counts it.
func StepResult(st *Step) string {
	switch {
	case st.State == StepSkipped || st.Outcome == OutcomeSkipped:
		return ResultNone
	case st.Outcome == OutcomeCancelled:
		return ResultCancelled
	case st.Expect != nil && st.Expect.Pass:
		return ResultPass
	case st.Expect == nil && st.Outcome == "":
		return ResultNone
	}
	return ResultFail
}

// RunResult judges a whole run.
func RunResult(r *Run) string {
	switch r.State {
	case RunCancelled:
		return ResultCancelled
	case RunSkipped, RunQueued, RunRunning, RunPaused:
		return ResultNone
	}
	if r.Summary == nil {
		if r.State == RunFailed {
			return ResultFail
		}
		return ResultNone
	}
	switch {
	case r.Summary.Failed == 0 && r.State == RunCompleted:
		return ResultPass
	case r.Summary.Passed > 0 && r.Summary.Failed > 0:
		return ResultPartial
	}
	return ResultFail
}

func runMark(r *Run) RecentMark {
	m := RecentMark{RunID: r.ID, At: runTime(r), Result: RunResult(r)}
	if r.Summary != nil {
		m.Passed, m.Failed = r.Summary.Passed, r.Summary.Failed
	}
	return m
}

func clipRecent(m []RecentMark) []RecentMark {
	if len(m) > MaxRecent {
		m = m[len(m)-MaxRecent:]
	}
	if m == nil {
		m = []RecentMark{}
	}
	return m
}

// RecentOf is the pass/fail history of every suite and benchmark in runs
// (which must be oldest first and carry their steps).
func RecentOf(runs []Run) (suites, benchmarks map[string][]RecentMark) {
	suites, benchmarks = map[string][]RecentMark{}, map[string][]RecentMark{}
	for i := range runs {
		r := &runs[i]
		suites[r.SuiteID] = append(suites[r.SuiteID], runMark(r))
		for j := range r.Steps {
			st := &r.Steps[j]
			benchmarks[st.BenchmarkID] = append(benchmarks[st.BenchmarkID], RecentMark{RunID: r.ID, At: runTime(r), Result: StepResult(st), Outcome: st.Outcome})
		}
	}
	for k, v := range suites {
		suites[k] = clipRecent(v)
	}
	for k, v := range benchmarks {
		benchmarks[k] = clipRecent(v)
	}
	return suites, benchmarks
}

// BenchmarkResult is one benchmark in one run: GET /api/bench/benchmarks/{id}/results.
type BenchmarkResult struct {
	RunID      string        `json:"run_id"`
	SuiteID    string        `json:"suite_id"`
	SuiteName  string        `json:"suite_name"`
	RunState   string        `json:"run_state"`
	At         int64         `json:"at"`
	Result     string        `json:"result"`
	Outcome    string        `json:"outcome,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Profile    string        `json:"profile"`
	ModelFile  string        `json:"model_file,omitempty"`
	Nodes      int           `json:"nodes,omitempty"`
	DurationS  float64       `json:"duration_s,omitempty"`
	LoadS      *float64      `json:"load_s,omitempty"`
	LoadMBs    *float64      `json:"load_mb_s,omitempty"`
	MemoryMiB  *float64      `json:"memory_mib,omitempty"`
	DecodeTPS  *float64      `json:"decode_tps,omitempty"`
	PrefillTPS *float64      `json:"prefill_tps,omitempty"`
	TTFTMS     *float64      `json:"ttft_ms,omitempty"`
	Expect     *ExpectResult `json:"expect,omitempty"`
	Regression bool          `json:"regression,omitempty"`
	// LowestMiB is the lowest available memory of any node during the step and
	// MemLow the figure per node; LoadMode, Host, Workers, Roles and Reload say
	// what the step ran on. All are absent for a step stored before they were
	// recorded.
	LowestMiB *int64       `json:"lowest_available_mib,omitempty"`
	MemLow    []NodeMemLow `json:"mem_low,omitempty"`
	LoadMode  string       `json:"load_mode,omitempty"`
	Host      string       `json:"host,omitempty"`
	Workers   []string     `json:"workers,omitempty"`
	Roles     []NodeRole   `json:"roles,omitempty"`
	Reload    []NodeReload `json:"reload_from_cache,omitempty"`
}

// ResultsOf lists what one benchmark did in each of runs (any order in, oldest first out).
func ResultsOf(runs []Run, benchmarkID string) []BenchmarkResult {
	out := []BenchmarkResult{}
	for i := range runs {
		r := &runs[i]
		for j := range r.Steps {
			st := &r.Steps[j]
			if st.BenchmarkID != benchmarkID {
				continue
			}
			br := BenchmarkResult{RunID: r.ID, SuiteID: r.SuiteID, SuiteName: r.SuiteName, RunState: r.State, At: runTime(r), Result: StepResult(st),
				Outcome: st.Outcome, Reason: st.Reason, Profile: st.Profile, ModelFile: st.ModelFile, Nodes: st.NodeCount, Expect: st.Expect,
				Regression: st.Comparison != nil && st.Comparison.Regression,
				LowestMiB:  LowestMiB(st), MemLow: st.MemLow, LoadMode: StepLoadMode(st), Host: StepHost(st), Workers: StepWorkers(st),
				Roles: st.Roles, Reload: st.Reload}
			if st.StartedAt > 0 && st.FinishedAt > st.StartedAt {
				br.DurationS = round3(float64(st.FinishedAt-st.StartedAt) / 1000)
			}
			vals := stepValues(st)
			pick := func(k string) *float64 {
				if v, ok := vals[k]; ok {
					return &v
				}
				return nil
			}
			br.LoadS, br.LoadMBs, br.MemoryMiB = pick("load_s"), pick("load_mb_s"), pick("memory_mib")
			br.DecodeTPS, br.PrefillTPS, br.TTFTMS = pick("decode_tps"), pick("prefill_tps"), pick("ttft_ms")
			out = append(out, br)
		}
	}
	for i := 1; i < len(out); i++ { // insertion sort by time: the lists are short
		for j := i; j > 0 && out[j].At < out[j-1].At; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// BenchmarkResults is ResultsOf over the newest stored runs that ran the benchmark.
func (s *Store) BenchmarkResults(benchmarkID string, limit int) ([]BenchmarkResult, error) {
	if limit <= 0 || limit > MaxAggRuns {
		limit = DefaultAggRuns
	}
	runs, err := s.aggRuns(AggFilter{Benchmark: benchmarkID, LimitRuns: limit})
	if err != nil {
		return nil, err
	}
	return ResultsOf(runs, benchmarkID), nil
}
