package bench

import (
	"math"
	"testing"
	"time"
)

type fakeStep struct {
	bench, profile, model string
	nodes                 int
	outcome               string
	pass                  bool
	loadS, mbs, mem       float64
	decode, prefill, ttft float64
	turns                 []TurnResult
	unloadS               float64
	loadMode              string
	lowMiB                int64
}

// addRun stores a finished run with the given steps.
func addRun(t *testing.T, s *Store, id, suiteID string, at int64, env *Environment, state string, steps ...fakeStep) {
	t.Helper()
	r := newRun(id, suiteID, at)
	r.State = state
	r.Env = env
	if err := s.CreateRun(r); err != nil {
		t.Fatal(err)
	}
	sum := &RunSummary{Steps: len(steps)}
	for i, fs := range steps {
		if fs.outcome == "" {
			fs.outcome = OutcomeReady
		}
		st := &Step{RunID: id, Idx: i, BenchmarkID: fs.bench, Name: fs.bench, Profile: fs.profile, State: StepDone, Outcome: fs.outcome,
			StartedAt: at, FinishedAt: at + 1000, ModelFile: fs.model, NodeCount: fs.nodes, Expect: &ExpectResult{Pass: fs.pass}}
		if fs.pass {
			sum.Passed++
		} else {
			sum.Failed++
		}
		if fs.loadS > 0 {
			st.Load = &LoadResult{LoadS: fs.loadS, AvgMBs: fs.mbs, MemoryMiB: fs.mem, LoadMode: fs.loadMode}
		}
		if fs.lowMiB > 0 {
			low := fs.lowMiB
			st.MemLow = []NodeMemLow{{ID: "n1", Node: "ps5-a", StepMiB: &low, Samples: 10}}
		}
		if fs.decode > 0 || fs.prefill > 0 || fs.ttft > 0 {
			st.Stats = &Stats{Requests: 3}
			if fs.decode > 0 {
				st.Stats.Decode = &Summary{N: 3, Mean: fs.decode}
			}
			if fs.prefill > 0 {
				st.Stats.Prefill = &Summary{N: 3, Mean: fs.prefill}
			}
			if fs.ttft > 0 {
				st.Stats.TTFT = &Summary{N: 3, Mean: fs.ttft}
			}
		}
		if err := s.AddStep(st); err != nil {
			t.Fatal(err)
		}
		if len(fs.turns) > 0 {
			s.AddPhase(&PhaseResult{StepID: st.ID, RunID: id, Idx: 1, Type: "conversation", State: PhaseDone, Turns: fs.turns})
		}
		if fs.unloadS > 0 {
			s.AddPhase(&PhaseResult{StepID: st.ID, RunID: id, Idx: 2, Type: "unload", State: PhaseDone, Unload: &UnloadResult{Loaded: true, Gone: true, UnloadS: fs.unloadS}})
		}
	}
	s.SetRunSummary(id, sum, "")
	s.SetRunState(id, state, at, at+60_000, "")
}

var th = Thresholds{LoadPct: 15, DecodePct: 7}

func close2(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// rateOf is a pass rate, or -1 when there is none (nothing judged).
func rateOf(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

func TestAggregateMathsOverARunHistory(t *testing.T) {
	s, _ := openTemp(t)
	loads := []float64{60, 62, 61, 80}
	decodes := []float64{30, 30, 30, 26}
	for i := range loads {
		addRun(t, s, "run-"+string(rune('a'+i)), "nightly", int64(1000*(i+1)), nil, RunCompleted,
			fakeStep{bench: "gemma", profile: "gemma-4", model: "gemma.gguf", nodes: 2, pass: true, loadS: loads[i], mbs: 100 - loads[i]/10, mem: 7900, decode: decodes[i], prefill: 200, ttft: 50})
	}
	addRun(t, s, "run-e", "nightly", 5000, nil, RunCompleted, fakeStep{bench: "gemma", profile: "gemma-4", outcome: OutcomeRefused})
	addRun(t, s, "run-f", "nightly", 6000, nil, RunFailed, fakeStep{bench: "gemma", profile: "gemma-4", outcome: OutcomeFailed})
	doc, err := s.Aggregate(AggFilter{}, th, time.UnixMilli(10_000))
	if err != nil {
		t.Fatal(err)
	}
	if doc.RunsConsidered != 6 || len(doc.Benchmarks) != 1 {
		t.Fatalf("%+v", doc)
	}
	b := doc.Benchmarks[0]
	if b.BenchmarkID != "gemma" || b.Runs != 6 || b.Steps != 6 || b.Outcomes["ready"] != 4 || b.Outcomes["refused"] != 1 || b.Outcomes["failed"] != 1 || !close2(rateOf(b.PassRate), 4.0/6) || b.Judged != 6 {
		t.Fatalf("%+v", b)
	}
	if b.LastRunID != "run-f" {
		t.Fatalf("last run %s", b.LastRunID)
	}
	l := b.Metrics["load_s"]
	// 60, 62, 61, 80: the refused and failed steps carry no numbers
	if l == nil || l.Count != 4 || l.Min != 60 || l.Max != 80 || !close2(l.Median, 61.5) || !close2(l.Mean, 65.75) || l.Last != 80 || l.Best != 60 {
		t.Fatalf("%+v", l)
	}
	if want := math.Sqrt((5.75*5.75 + 3.75*3.75 + 4.75*4.75 + 14.25*14.25) / 3); !close2(l.Stdev, want) {
		t.Fatalf("stdev %v want %v", l.Stdev, want)
	}
	if l.ChangePct == nil || !close2(*l.ChangePct, (80.0-61)/61*100) || !l.Regression || l.ThresholdPct != 15 {
		t.Fatalf("%+v %v", l, l.ChangePct)
	}
	d := b.Metrics["decode_tps"]
	if d.Best != 30 || d.Last != 26 || !close2(*d.ChangePct, (26.0-30)/30*100) || !d.Regression || d.ThresholdPct != 7 {
		t.Fatalf("%+v", d)
	}
	if p := b.Metrics["prefill_tps"]; p.Regression || !close2(*p.ChangePct, 0) || p.Count != 4 {
		t.Fatalf("%+v", p)
	}
	if len(l.Points) != 4 || l.Points[0].RunID != "run-a" || l.Points[3].RunID != "run-d" || l.Points[3].Value != 80 || l.Points[3].At != 4000 {
		t.Fatalf("%+v", l.Points)
	}
	if m := b.Metrics["memory_mib"]; m.Count != 4 || m.Last != 7900 || m.Regression {
		t.Fatalf("%+v", m)
	}
	if m := b.Metrics["load_mb_s"]; m.Best != 94 || m.Last != 92 { // higher is better
		t.Fatalf("%+v", m)
	}
	// suite
	if len(doc.Suites) != 1 {
		t.Fatalf("%+v", doc.Suites)
	}
	su := doc.Suites[0]
	if su.SuiteID != "nightly" || su.Runs != 6 || su.States[RunCompleted] != 5 || su.States[RunFailed] != 1 || !close2(rateOf(su.PassRate), 4.0/6) || su.Judged != 6 || !close2(su.MeanDurationS, 60) {
		t.Fatalf("%+v", su)
	}
	if su.LastRun == nil || su.LastRun.ID != "run-f" || su.LastRun.State != RunFailed || su.LastRun.Summary == nil || su.LastRun.Summary.Failed != 1 {
		t.Fatalf("%+v", su.LastRun)
	}
}

func TestAggregateKeepsTheLastThirtyPoints(t *testing.T) {
	s, _ := openTemp(t)
	for i := 0; i < 40; i++ {
		addRun(t, s, "run-"+string(rune('A'+i/26))+string(rune('a'+i%26)), "s", int64(1000*(i+1)), nil, RunCompleted,
			fakeStep{bench: "g", profile: "p", pass: true, loadS: float64(i + 1)})
	}
	doc, _ := s.Aggregate(AggFilter{LimitRuns: 100}, th, time.Now())
	l := doc.Benchmarks[0].Metrics["load_s"]
	if l.Count != 40 || len(l.Points) != 30 || l.Points[0].Value != 11 || l.Points[29].Value != 40 {
		t.Fatalf("count %d points %d first %v last %v", l.Count, len(l.Points), l.Points[0].Value, l.Points[29].Value)
	}
	doc, _ = s.Aggregate(AggFilter{LimitRuns: 10}, th, time.Now())
	if doc.RunsConsidered != 10 || doc.Benchmarks[0].Metrics["load_s"].Min != 31 {
		t.Fatalf("limit_runs takes the newest: %+v", doc.Benchmarks[0].Metrics["load_s"])
	}
}

func TestAggregateFilters(t *testing.T) {
	s, _ := openTemp(t)
	addRun(t, s, "r1", "nightly", 1000, nil, RunCompleted, fakeStep{bench: "a", profile: "p1", pass: true, loadS: 10}, fakeStep{bench: "b", profile: "p2", pass: true, loadS: 20})
	addRun(t, s, "r2", "weekly", 2000, nil, RunCompleted, fakeStep{bench: "a", profile: "p1", pass: true, loadS: 12})
	addRun(t, s, "r3", "nightly", 3000, nil, RunCancelled, fakeStep{bench: "b", profile: "p2", pass: false})
	addRun(t, s, "r4", "nightly", 4000, nil, RunSkipped)
	all, _ := s.Aggregate(AggFilter{}, th, time.Now())
	if all.RunsConsidered != 3 || len(all.Benchmarks) != 2 || len(all.Suites) != 2 {
		t.Fatalf("finished runs only: %d runs, %d benchmarks, %d suites", all.RunsConsidered, len(all.Benchmarks), len(all.Suites))
	}
	for name, tc := range map[string]struct {
		f        AggFilter
		runs     int
		benches  int
		aSteps   int
		bFirstLd float64
	}{
		"suite":     {AggFilter{Suite: "nightly"}, 2, 2, 1, 20},
		"benchmark": {AggFilter{Benchmark: "a"}, 2, 1, 2, 0},
		"profile":   {AggFilter{Profile: "p2"}, 2, 1, 0, 20},
		"since":     {AggFilter{Since: 2000}, 2, 2, 1, 0},
		"until":     {AggFilter{Until: 2000}, 2, 2, 2, 20},
	} {
		d, err := s.Aggregate(tc.f, th, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if d.RunsConsidered != tc.runs || len(d.Benchmarks) != tc.benches {
			t.Errorf("%s: %d runs, %d benchmarks", name, d.RunsConsidered, len(d.Benchmarks))
			continue
		}
		for _, b := range d.Benchmarks {
			if b.BenchmarkID == "a" && b.Steps != tc.aSteps {
				t.Errorf("%s: a has %d steps", name, b.Steps)
			}
			if b.BenchmarkID == "b" && tc.bFirstLd > 0 && b.Metrics["load_s"].Min != tc.bFirstLd {
				t.Errorf("%s: b load %+v", name, b.Metrics["load_s"])
			}
		}
	}
	// a benchmark filter does not leak the other steps of the same run
	d, _ := s.Aggregate(AggFilter{Benchmark: "b"}, th, time.Now())
	if len(d.Benchmarks) != 1 || d.Benchmarks[0].BenchmarkID != "b" {
		t.Fatalf("%+v", d.Benchmarks)
	}
}

func TestAggregateGroupsByModelAndByWorkerCount(t *testing.T) {
	s, _ := openTemp(t)
	// the same model on 1, 2 and 3 workers, two runs each; another model on 2
	at := int64(1000)
	for i := 0; i < 2; i++ {
		for _, n := range []int{1, 2, 3} {
			at += 1000
			addRun(t, s, "m"+string(rune('0'+n))+string(rune('a'+i)), "s", at, nil, RunCompleted,
				fakeStep{bench: "load", profile: "big", model: "big.gguf", nodes: n, pass: true, loadS: 100 / float64(n) * (1 + float64(i)*0.1), mbs: 80 * float64(n), mem: 20000, decode: 10 * float64(n), prefill: 100})
		}
	}
	at += 1000
	addRun(t, s, "other", "s", at, nil, RunCompleted, fakeStep{bench: "load2", profile: "small", model: "small.gguf", nodes: 2, pass: true, loadS: 5, decode: 50},
		fakeStep{bench: "conv", profile: "small", pass: true, loadS: 5, decode: 50}) // no model file: left out of the groupings
	d, err := s.Aggregate(AggFilter{}, th, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ByModel) != 2 || d.ByModel[0].ModelFile != "big.gguf" || d.ByModel[0].Steps != 6 || d.ByModel[0].Runs != 6 || d.ByModel[1].ModelFile != "small.gguf" || d.ByModel[1].Steps != 1 {
		t.Fatalf("by model %+v", d.ByModel)
	}
	if d.ByModel[0].Nodes != nil || len(d.ByModel[0].Benchmarks) != 1 || d.ByModel[0].Profiles[0] != "big" {
		t.Fatalf("%+v", d.ByModel[0])
	}
	if l := d.ByModel[0].Metrics["load_s"]; l.Count != 6 || l.Min < 33 || l.Max > 111 {
		t.Fatalf("%+v", l)
	}
	if len(d.ByModelNodes) != 4 {
		t.Fatalf("by model and nodes: %d groups", len(d.ByModelNodes))
	}
	want := []struct {
		file  string
		nodes int
		load  float64 // the mean
		dec   float64
	}{{"big.gguf", 1, 105, 10}, {"big.gguf", 2, 52.5, 20}, {"big.gguf", 3, 35, 30}, {"small.gguf", 2, 5, 50}}
	for i, w := range want {
		g := d.ByModelNodes[i]
		if g.ModelFile != w.file || g.Nodes == nil || *g.Nodes != w.nodes || !close2(g.Metrics["load_s"].Mean, w.load) || !close2(g.Metrics["decode_tps"].Mean, w.dec) {
			t.Errorf("group %d: %s %v load %v decode %v, want %+v", i, g.ModelFile, g.Nodes, g.Metrics["load_s"].Mean, g.Metrics["decode_tps"].Mean, w)
		}
	}
}

func TestAggregatePerTurnMetricsOfConversations(t *testing.T) {
	s, _ := openTemp(t)
	for i := 0; i < 3; i++ {
		ttft := 40.0 + float64(i)
		turns := []TurnResult{
			{Turn: 1, PromptTokens: 30, PromptN: 30, CacheN: 0, DecodeTPS: 20, PrefillTPS: 100, TTFTMS: &ttft, WallMS: 500},
			{Turn: 2, PromptTokens: 90, PromptN: 20, CacheN: 70 - float64ToInt(float64(i)*10), DecodeTPS: 19, PrefillTPS: 120, WallMS: 450},
		}
		addRun(t, s, "c"+string(rune('a'+i)), "s", int64(1000*(i+1)), nil, RunCompleted, fakeStep{bench: "talk", profile: "p", model: "m.gguf", nodes: 1, pass: true, loadS: 5, decode: 20, turns: turns, unloadS: 2 + float64(i)})
	}
	d, _ := s.Aggregate(AggFilter{}, th, time.Now())
	b := d.Benchmarks[0]
	if len(b.Turns) != 2 || b.Turns[0].Turn != 1 || b.Turns[1].Turn != 2 || b.Turns[0].Phase != 1 {
		t.Fatalf("%+v", b.Turns)
	}
	if m := b.Turns[0].Metrics["ttft_ms"]; m == nil || m.Count != 3 || m.Last != 42 || m.Min != 40 {
		t.Fatalf("%+v", m)
	}
	if m := b.Turns[1].Metrics["reused_tokens"]; m == nil || m.Count != 3 || m.Last != 50 || m.Best != 70 || !m.Regression || !close2(*m.ChangePct, (50.0-65)/65*100) {
		t.Fatalf("a drop in reused tokens is a regression: %+v", m)
	}
	if m := b.Turns[1].Metrics["prompt_tokens"]; m == nil || m.Regression {
		t.Fatalf("%+v", m)
	}
	if m := b.Turns[1].Metrics["ttft_ms"]; m != nil {
		t.Fatalf("turn 2 had no ttft: %+v", m)
	}
	if m := b.Metrics["unload_s"]; m == nil || m.Count != 3 || m.Last != 4 {
		t.Fatalf("%+v", m)
	}
}

func float64ToInt(v float64) int { return int(v) }

func TestAggregateEnvSameKeepsOnlyTheLatestEnvironment(t *testing.T) {
	s, _ := openTemp(t)
	envA := &Environment{SchedulerVersion: "1", ConfigRevision: "r1", LlamaBinary: "llama", Workers: []WorkerEnv{{Node: "a", RPCServerVersion: "b1"}}}
	envB := &Environment{SchedulerVersion: "1", ConfigRevision: "r1", LlamaBinary: "llama", Workers: []WorkerEnv{{Node: "a", RPCServerVersion: "b2"}}} // a worker was updated
	addRun(t, s, "r1", "s", 1000, envA, RunCompleted, fakeStep{bench: "g", profile: "p", pass: true, loadS: 10})
	addRun(t, s, "r2", "s", 2000, envA, RunCompleted, fakeStep{bench: "g", profile: "p", pass: true, loadS: 11})
	addRun(t, s, "r3", "s", 3000, envB, RunCompleted, fakeStep{bench: "g", profile: "p", pass: true, loadS: 30})
	addRun(t, s, "r4", "s", 4000, nil, RunCompleted, fakeStep{bench: "g", profile: "p", pass: true, loadS: 50})
	all, _ := s.Aggregate(AggFilter{}, th, time.Now())
	if all.RunsConsidered != 4 || all.Environment != nil {
		t.Fatalf("%d %+v", all.RunsConsidered, all.Environment)
	}
	same, _ := s.Aggregate(AggFilter{EnvSame: true}, th, time.Now())
	if same.RunsConsidered != 1 || same.Benchmarks[0].Metrics["load_s"].Count != 1 {
		t.Fatalf("the latest run has no environment recorded: only runs like it count: %+v", same.Benchmarks)
	}
	addRun(t, s, "r5", "s", 5000, envB, RunCompleted, fakeStep{bench: "g", profile: "p", pass: true, loadS: 32})
	same, _ = s.Aggregate(AggFilter{EnvSame: true}, th, time.Now())
	l := same.Benchmarks[0].Metrics["load_s"]
	if same.RunsConsidered != 2 || l.Count != 2 || l.Min != 30 || l.Max != 32 || same.Environment == nil || same.Environment.Workers[0].RPCServerVersion != "b2" {
		t.Fatalf("%d %+v", same.RunsConsidered, l)
	}
	if same.Filter["env"] != "same" {
		t.Fatalf("%v", same.Filter)
	}
}

func TestParseAggTime(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]int64{
		"":                     0,
		"1700000000000":        1700000000000,
		"1700000000":           1700000000000,
		"2026-11-18T00:00:00Z": time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC).UnixMilli(),
		"24h":                  now.Add(-24 * time.Hour).UnixMilli(),
		"7d":                   now.Add(-7 * 24 * time.Hour).UnixMilli(),
		"90m":                  now.Add(-90 * time.Minute).UnixMilli(),
	} {
		got, err := ParseAggTime(in, now)
		if err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "-5", "5x", "d"} {
		if _, err := ParseAggTime(bad, now); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

// A run through the manager records its environment and announces new aggregates.
func TestRunRecordsEnvironmentAndEmitsAggregateUpdated(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.revision = "rev-9"
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["b"],"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.Env == nil || r.Env.ConfigRevision != "rev-9" || r.Env.LlamaBinary != "llama-server-rpc" || r.Env.SchedulerVersion != "test" {
		t.Fatalf("%+v", r.Env)
	}
	// The message is sent after the run is stored as finished, so wait for it
	// instead of sleeping a fixed time (30 ms was too short on a loaded machine).
	found := false
	for deadline := time.Now().Add(3 * time.Second); !found && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		tm.evMu.Lock()
		for _, e := range tm.events {
			if e["kind"] == "aggregate_updated" && e["run_id"] == r.ID && e["suite_id"] == "s" {
				found = true
			}
		}
		tm.evMu.Unlock()
	}
	if !found {
		t.Fatal("no aggregate_updated message")
	}
	doc, err := tm.m.Aggregates(AggFilter{})
	if err != nil || doc.RunsConsidered != 1 || doc.Benchmarks[0].Metrics["load_s"] == nil || doc.Environment == nil {
		t.Fatalf("%+v %v", doc, err)
	}
	if mn := doc.ByModelNodes; len(mn) != 1 || mn[0].ModelFile != "gemma.gguf" || *mn[0].Nodes != 1 {
		t.Fatalf("%+v", mn)
	}
}

// The same model on the same nodes, loaded with the file read by the workers
// and with the weights pushed, falls into two groups; steps stored before the
// load mode was recorded form a "not_recorded" group. The lowest available
// memory is a metric of each group.
func TestAggregateGroupsByLoadMode(t *testing.T) {
	s, _ := openTemp(t)
	addRun(t, s, "r1", "s", 1000, nil, RunCompleted, fakeStep{bench: "b", profile: "p", model: "m.gguf", nodes: 2, pass: true, loadS: 20, loadMode: LoadModeFileRead, lowMiB: 2100})
	addRun(t, s, "r2", "s", 2000, nil, RunCompleted, fakeStep{bench: "b", profile: "p", model: "m.gguf", nodes: 2, pass: true, loadS: 60, loadMode: LoadModePushed, lowMiB: 3000})
	addRun(t, s, "r3", "s", 3000, nil, RunCompleted, fakeStep{bench: "b", profile: "p", model: "m.gguf", nodes: 2, pass: true, loadS: 22, loadMode: LoadModeFileRead, lowMiB: 1900})
	addRun(t, s, "r4", "s", 4000, nil, RunCompleted, fakeStep{bench: "b", profile: "p", model: "m.gguf", nodes: 2, pass: true, loadS: 50})
	d, err := s.Aggregate(AggFilter{}, th, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ByModelLoadMode) != 3 {
		t.Fatalf("groups %+v", d.ByModelLoadMode)
	}
	want := []struct {
		mode  string
		steps int
		load  float64
		low   float64 // the minimum of the lowest available memory; 0 when none
	}{{LoadModeFileRead, 2, 21, 1900}, {LoadModeNotRecorded, 1, 50, 0}, {LoadModePushed, 1, 60, 3000}}
	for i, w := range want {
		g := d.ByModelLoadMode[i]
		if g.LoadMode == nil || *g.LoadMode != w.mode || g.Steps != w.steps || g.Nodes == nil || *g.Nodes != 2 || !close2(g.Metrics["load_s"].Mean, w.load) {
			t.Fatalf("group %d: %+v, want %+v", i, g, w)
		}
		lm := g.Metrics["lowest_available_mib"]
		if (w.low == 0) != (lm == nil) || lm != nil && lm.Min != w.low {
			t.Fatalf("group %d lowest %+v, want %v", i, lm, w.low)
		}
	}
	if d.ByModelNodes[0].LoadMode != nil {
		t.Fatalf("the by-nodes grouping carries no load mode")
	}
}
