package bench

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Data model step 1: a benchmark runs on its own, without a suite.
func TestRunOneBenchmarkDirectly(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"b","name":"Gemma load","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	r, err := tm.m.EnqueueBenchmark("b", BenchRunOptions{Note: "direct", OnlyWhenIdle: boolp(false)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != RunKindBenchmark || r.BenchmarkID != "b" || r.SuiteID != "" || r.SuiteName != "Gemma load" {
		t.Fatalf("queued %+v", r)
	}
	r = tm.finished(r.ID)
	if r.State != RunCompleted || len(r.Steps) != 1 || r.Steps[0].BenchmarkID != "b" || r.Kind != RunKindBenchmark || r.BenchmarkID != "b" || r.Note != "direct" {
		t.Fatalf("%s %s %+v", r.State, r.Error, r)
	}
	// A second direct run takes the first as its baseline; a suite run of the
	// same benchmark is neither its baseline nor listed with it.
	tm.suite(`{"id":"s","benchmarks":["b"],"cooldown_s":0,"only_when_idle":false}`)
	sr := tm.finished(tm.run("s").ID)
	if sr.Kind != RunKindSuite || sr.BenchmarkID != "" {
		t.Fatalf("suite run %+v", sr)
	}
	r2, err := tm.m.EnqueueBenchmark("b", BenchRunOptions{OnlyWhenIdle: boolp(false)})
	if err != nil {
		t.Fatal(err)
	}
	r2 = tm.finished(r2.ID)
	if r2.BaselineRunID != r.ID {
		t.Fatalf("baseline %q, want %q", r2.BaselineRunID, r.ID)
	}
	direct, _ := tm.store.ListRuns(RunFilter{Benchmark: "b", Kind: RunKindBenchmark})
	if len(direct) != 2 || direct[0].ID != r2.ID {
		t.Fatalf("direct runs %+v", direct)
	}
	all, _ := tm.store.ListRuns(RunFilter{Benchmark: "b"})
	if len(all) != 3 {
		t.Fatalf("every run of b: %d", len(all))
	}
	suites, _ := tm.store.ListRuns(RunFilter{Kind: RunKindSuite})
	if len(suites) != 1 || suites[0].ID != sr.ID {
		t.Fatalf("suite runs %+v", suites)
	}
	// The suite aggregates leave direct runs out.
	agg, err := tm.m.Aggregates(AggFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range agg.Suites {
		if s.SuiteID == "" {
			t.Fatalf("a direct run became a suite: %+v", agg.Suites)
		}
	}
	if _, err := tm.m.EnqueueBenchmark("nope", BenchRunOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	tm.bench(`{"id":"gone","profile":"missing","phases":[{"type":"load"}]}`)
	tm.m.o.ProfileOK = func(p string) bool { return p != "missing" }
	if _, err := tm.m.EnqueueBenchmark("gone", BenchRunOptions{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing profile: %v", err)
	}
	if _, err := tm.m.EnqueueBenchmark("b", BenchRunOptions{Note: strings.Repeat("x", 501)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long note: %v", err)
	}
}

func boolp(v bool) *bool { return &v }

func findM(ms []Measurement, f func(m Measurement) bool) []Measurement {
	var out []Measurement
	for _, m := range ms {
		if f(m) {
			out = append(out, m)
		}
	}
	return out
}

func metricIs(name string, phase int) func(Measurement) bool {
	return func(m Measurement) bool { return m.Metric == name && m.Phase != nil && *m.Phase == phase }
}

// Every number a step measured is also a measurement row of one shape.
func TestEveryStepNumberIsAMeasurement(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load","cold":true},{"type":"warmup","count":1,"request":{"name":"w","prompt":"hi","max_tokens":8}},
	  {"type":"requests","repeats":2,"stream":true,"requests":[{"name":"q","prompt":"hi","max_tokens":8}]},
	  {"type":"conversation","turns":[{"content":"a"},{"content":"b"}]},
	  {"type":"concurrent_requests","n":2,"request":{"name":"c","prompt":"hi","max_tokens":8}},
	  {"type":"unload"}]}`)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
	st := r.Steps[0]
	ms, err := tm.store.RunMeasurements(r.ID, MeasureQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no measurements")
	}
	for _, m := range ms {
		if m.RunID != r.ID || m.Step != 0 || m.BenchmarkID != "b" || m.Profile != "gemma" || m.Unit == "" || m.Metric == "" {
			t.Fatalf("bad row %+v", m)
		}
	}
	// load: the same figures as the phase, with the cold conditions
	load := st.Phases[0].Load
	got := findM(ms, metricIs("load_s", 0))
	if len(got) != 1 || got[0].Value != load.LoadS || got[0].Unit != "s" || got[0].Conditions["cold"] != true {
		t.Fatalf("load_s %+v (phase %+v)", got, load)
	}
	if _, ok := got[0].Conditions["cold_verified"]; !ok {
		t.Fatalf("cold_verified missing: %+v", got[0].Conditions)
	}
	// warmup: one row per figure, generation first; requests: steady, one per repeat
	w := findM(ms, metricIs("decode_tps", 1))
	if len(w) != 1 || w[0].Conditions["generation"] != "first" || w[0].Subject != "w" {
		t.Fatalf("warmup decode %+v", w)
	}
	q := findM(ms, metricIs("ttft_ms", 2))
	if len(q) != 2 || q[0].Conditions["generation"] != "steady" || q[0].Seq != 1 || q[1].Seq != 2 {
		t.Fatalf("requests ttft %+v", q)
	}
	var reqTTFT []float64
	for _, rq := range st.Requests {
		if rq.PhaseIdx == 2 && rq.TTFTMS != nil {
			reqTTFT = append(reqTTFT, *rq.TTFTMS)
		}
	}
	if len(reqTTFT) != 2 || q[0].Value != reqTTFT[0] {
		t.Fatalf("ttft %v vs %v", q, reqTTFT)
	}
	// conversation: per turn, with the reused tokens
	turns := findM(ms, metricIs("reused_tokens", 3))
	if len(turns) != 2 || turns[1].Subject != "turn 2" || turns[1].Value != float64(st.Phases[3].Turns[1].CacheN) {
		t.Fatalf("turns %+v", turns)
	}
	// concurrent: the total and per request
	if c := findM(ms, metricIs("concurrent_wall_s", 4)); len(c) != 1 || c[0].Value != round3(st.Phases[4].Concurrent.WallMS/1000) {
		t.Fatalf("concurrent %+v", c)
	}
	if c := findM(ms, metricIs("wait_ms", 4)); len(c) != 2 {
		t.Fatalf("concurrent waits %+v", c)
	}
	if c := findM(ms, metricIs("errors", 4)); len(c) != 1 || c[0].Value != 0 || c[0].Unit != "count" {
		t.Fatalf("errors %+v", c)
	}
	// unload
	if u := findM(ms, metricIs("unload_s", 5)); len(u) != 1 || u[0].Value != st.Phases[5].Unload.UnloadS {
		t.Fatalf("unload %+v", u)
	}
	// filters
	only, _ := tm.store.RunMeasurements(r.ID, MeasureQuery{Metrics: []string{"load_s", "unload_s"}})
	if len(only) != 2 {
		t.Fatalf("metric filter %+v", only)
	}
	// history across runs, by benchmark and metric, with the run's release
	hist, err := tm.store.QueryMeasurements(MeasureFilter{Benchmark: "b", Metrics: []string{"load_s"}})
	if err != nil || len(hist) != 1 || hist[0].Release != "test" || hist[0].RunState != RunCompleted {
		t.Fatalf("query %v %+v", err, hist)
	}
	if h, _ := tm.store.QueryMeasurements(MeasureFilter{Profile: "other"}); len(h) != 0 {
		t.Fatalf("profile filter %+v", h)
	}
}

// A tasks phase gives per task figures, and posted scores become task_points rows.
func TestTasksAndScoresAreMeasurements(t *testing.T) {
	tm, r := scoredRun(t)
	ms, _ := tm.store.RunMeasurements(r.ID, MeasureQuery{Metrics: []string{"completion_tokens"}})
	if len(ms) != 3 || ms[0].Subject != "t1" || ms[0].Conditions["thinking"] == nil {
		t.Fatalf("task tokens %+v", ms)
	}
	if _, err := tm.store.PutScores(r.ID, "kit", ScoresDoc{Scores: []ScoreInput{{Step: ip(0), Task: "t1", Points: 3, MaxPoints: 4}}}, 9); err != nil {
		t.Fatal(err)
	}
	pts, _ := tm.store.RunMeasurements(r.ID, MeasureQuery{Metrics: []string{"task_points"}})
	if len(pts) != 1 || pts[0].Value != 3 || pts[0].Of == nil || *pts[0].Of != 4 || pts[0].Conditions["scorer"] != "kit" || pts[0].Subject != "t1" {
		t.Fatalf("points %+v", pts)
	}
	if err := tm.store.DeleteScores(r.ID, "kit"); err != nil {
		t.Fatal(err)
	}
	if pts, _ := tm.store.RunMeasurements(r.ID, MeasureQuery{Metrics: []string{"task_points"}}); len(pts) != 0 {
		t.Fatalf("points after delete %+v", pts)
	}
}

// A database written before measurements existed gains them for its old runs.
func TestOldRunsAreMappedIntoMeasurements(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"requests","requests":[{"name":"q","prompt":"hi","max_tokens":8}]},{"type":"unload"}]}`)
	dir := filepath.Dir(tm.store.Path())
	n, _ := tm.store.RunMeasurements(r.ID, MeasureQuery{})
	// Forget them, as an older scheduler would never have written them.
	if _, err := tm.store.db.Exec(`DELETE FROM measurements; DELETE FROM bench_meta`); err != nil {
		t.Fatal(err)
	}
	tm.m.Stop(t.Context())
	tm.store.Close()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.RunMeasurements(r.ID, MeasureQuery{})
	if err != nil || len(got) != len(n) || len(got) == 0 {
		t.Fatalf("%v: %d rows after the backfill, %d before", err, len(got), len(n))
	}
}

// A step written by the first runner (no phases) still maps: its load and
// its measured requests.
func TestMeasurementsOfAStepWithoutPhases(t *testing.T) {
	ttft := 120.0
	mib := 4100.0
	st := &Step{RunID: "r", Idx: 0, BenchmarkID: "old", Profile: "p", ModelFile: "m.gguf", StartedAt: 5,
		Load: &LoadResult{LoadS: 30, Bytes: 1 << 30, AvgMBs: 35.8, MemoryMiB: 8000, Nodes: []NodeLoad{{Node: "console-1", MemoryMiB: &mib}}},
		Requests: []RequestResult{{Name: "q", Phase: PhaseMeasured, Status: 200, TTFTMS: &ttft, DecodeTPS: 20, PromptTPS: 300, WallMS: 900, Seq: 1},
			{Name: "q", Phase: PhaseMeasured, Status: 502, Seq: 2, Error: "boom"}}}
	ms := MeasurementsOf(st)
	if l := findM(ms, func(m Measurement) bool { return m.Metric == "load_s" }); len(l) != 1 || l[0].Phase != nil || l[0].Value != 30 || l[0].Model != "m.gguf" {
		t.Fatalf("load %+v", l)
	}
	if n := findM(ms, func(m Measurement) bool { return m.Metric == "node_memory_mib" }); len(n) != 1 || n[0].Node != "console-1" || n[0].Value != 4100 {
		t.Fatalf("node memory %+v", n)
	}
	if d := findM(ms, func(m Measurement) bool { return m.Metric == "decode_tps" }); len(d) != 1 || d[0].Value != 20 {
		t.Fatalf("decode: the failed request has no figures %+v", d)
	}
}
