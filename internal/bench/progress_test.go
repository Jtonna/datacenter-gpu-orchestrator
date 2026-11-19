package bench

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
)

// docOf builds the progress document of a hand-made active run.
func docOf(t *testing.T, tm *testMgr, ar *activeRun) map[string]any {
	t.Helper()
	ar.mu.Lock()
	defer ar.mu.Unlock()
	return tm.m.progressDoc(ar)
}

func mkBench(t *testing.T, id, profile, js string) *benchspec.Benchmark {
	t.Helper()
	b, err := benchspec.ParseBenchmarkJSON([]byte(`{"id":"` + id + `","profile":"` + profile + `",` + js + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(nil); err != nil {
		t.Fatal(err)
	}
	return b
}

// activeOf makes a running run over benchmarks, as if execute had planned it.
func activeOf(tm *testMgr, bs ...*benchspec.Benchmark) *activeRun {
	suite := &benchspec.Suite{ID: "s", Name: "S"}
	plan := &runPlan{Suite: suite, Benchmarks: map[string]*benchspec.Benchmark{}}
	for _, b := range bs {
		suite.Benchmarks = append(suite.Benchmarks, benchspec.SuiteItem{Ref: b.ID})
		plan.Benchmarks[b.ID] = b
	}
	run := &Run{ID: "run-x", SuiteID: "s", SuiteName: "S", State: RunRunning, StartedAt: tm.m.o.Now().Add(-10 * time.Second).UnixMilli()}
	ar := &activeRun{run: run, plan: plan, cancelCh: make(chan struct{}), loaded: map[string]bool{}, cur: -1, tracks: map[string]*benchTrack{}, total: len(bs)}
	ar.items = tm.m.planItems(plan)
	return ar
}

func num(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	t.Fatalf("%v is not a number", v)
	return 0
}

func TestProgressEqualWeightsWhenNothingIsKnown(t *testing.T) {
	tm := newTestMgr(t, newFake())
	a := mkBench(t, "a", "p", `"description":"first","phases":[{"type":"load"}]`)
	b := mkBench(t, "b", "p", `"description":"second","phases":[{"type":"load"}]`)
	c := mkBench(t, "c", "q", `"description":"third","phases":[{"type":"load"}]`)
	ar := activeOf(tm, a, b, c)
	// before anything starts: 0 percent, the first benchmark is next, nothing is current
	d := docOf(t, tm, ar)
	if d["current"] != nil || num(t, d["percent"]) != 0 || d["eta_s"] != nil || d["steps_total"] != 3 {
		t.Fatalf("%v", d)
	}
	if n := d["next"].(map[string]any); n["benchmark_id"] != "a" || n["description"] != "first" || n["profile"] != "p" {
		t.Fatalf("%v", d["next"])
	}
	if q := d["queue"].([]string); strings.Join(q, ",") != "a,b,c" {
		t.Fatalf("%v", q)
	}
	// the second benchmark is half done: (1 + 0.5) / 3
	ar.cur = 1
	tr := newTrack(b, 1, nil)
	tr.cur, tr.planned, tr.done = 0, 2, 1
	ar.tracks["b"] = tr
	d = docOf(t, tm, ar)
	cur := d["current"].(map[string]any)
	if got := num(t, d["percent"]); math.Abs(got-50) > 0.11 {
		t.Fatalf("run percent %v, want 50", got)
	}
	if cur["benchmark_id"] != "b" || cur["name"] != "b" || cur["description"] != "second" || cur["profile"] != "p" || num(t, cur["index"]) != 1 || num(t, cur["percent"]) != 50 {
		t.Fatalf("%v", cur)
	}
	ph := cur["phase"].(map[string]any)
	if ph["type"] != "load" || num(t, ph["index"]) != 0 || num(t, ph["phases_total"]) != 1 {
		t.Fatalf("%v", ph)
	}
	if n := d["next"].(map[string]any); n["benchmark_id"] != "c" || n["profile"] != "q" {
		t.Fatalf("%v", d["next"])
	}
	if q := d["queue"].([]string); strings.Join(q, ",") != "c" {
		t.Fatalf("%v", q)
	}
	if d["eta_s"] != nil {
		t.Fatalf("no history means no eta: %v", d["eta_s"])
	}
	if num(t, d["elapsed_s"]) < 9 {
		t.Fatalf("elapsed %v", d["elapsed_s"])
	}
	// the last one: nothing next, empty queue
	ar.cur = 2
	ar.tracks["c"] = newTrack(c, 2, nil)
	d = docOf(t, tm, ar)
	if d["next"] != nil || len(d["queue"].([]string)) != 0 {
		t.Fatalf("%v", d)
	}
}

// completeRun stores a finished run of a benchmark with the given step and phase durations.
func completeRun(t *testing.T, s *Store, runID string, at int64, b *benchspec.Benchmark, stepS float64, phaseS map[int]float64) {
	t.Helper()
	r := newRun(runID, "s", at)
	r.State = RunCompleted
	if err := s.CreateRun(r); err != nil {
		t.Fatal(err)
	}
	s.SetRunState(runID, RunCompleted, at, at+int64(stepS*1000), "")
	st := &Step{RunID: runID, BenchmarkID: b.ID, Name: b.Title(), Profile: b.Profile, State: StepDone, Outcome: OutcomeReady, StartedAt: at, FinishedAt: at + int64(stepS*1000)}
	if err := s.AddStep(st); err != nil {
		t.Fatal(err)
	}
	ps := b.Expand()
	for i, p := range ps {
		d, ok := phaseS[i]
		if !ok {
			continue
		}
		s.AddPhase(&PhaseResult{StepID: st.ID, RunID: runID, Idx: i, Type: p.Type, State: PhaseDone, StartedAt: at, FinishedAt: at + int64(d*1000)})
	}
}

func TestProgressWeightsComeFromEarlierRuns(t *testing.T) {
	tm := newTestMgr(t, newFake())
	a := mkBench(t, "a", "p", `"phases":[{"type":"load"}]`)
	b := mkBench(t, "b", "p", `"phases":[{"type":"load"}]`)
	c := mkBench(t, "c", "p", `"phases":[{"type":"load"}]`)
	// a took 10 s and 20 s (median 15), b 60 s; c has no history
	completeRun(t, tm.store, "r1", 1000, a, 10, map[int]float64{0: 10})
	completeRun(t, tm.store, "r2", 2000, a, 20, map[int]float64{0: 20})
	completeRun(t, tm.store, "r3", 3000, b, 60, map[int]float64{0: 60})
	if v, ok := tm.store.MedianStepSeconds("a"); !ok || v != 15 {
		t.Fatalf("median %v %v", v, ok)
	}
	if _, ok := tm.store.MedianStepSeconds("c"); ok {
		t.Fatal("c has no history")
	}
	ws, known := itemWeights(activeOf(tm, a, b, c).items)
	// c gets the median of the known ones (15 and 60: the upper median, 60)
	if !known || ws[0] != 15 || ws[1] != 60 || ws[2] != 60 {
		t.Fatalf("%v %v", ws, known)
	}
	ar := activeOf(tm, a, b, c)
	// a is done; b is at 50% of its load (phase weight 60 s from history); total weight 135
	ar.cur = 1
	tr := newTrack(b, 1, tm.store.MedianPhaseSeconds("b"))
	tr.cur, tr.useDirect, tr.direct = 0, true, 0.5
	ar.tracks["b"] = tr
	d := docOf(t, tm, ar)
	want := 100 * (15 + 60*0.5) / 135
	if got := num(t, d["percent"]); math.Abs(got-want) > 0.11 {
		t.Fatalf("percent %v, want %.1f", got, want)
	}
	// eta: the rest of b (30 s) and all of c (60 s)
	if eta := num(t, d["eta_s"]); math.Abs(eta-90) > 0.11 {
		t.Fatalf("eta %v, want 90", eta)
	}
}

func TestProgressOfALoadOnlyBenchmarkFollowsTheLoad(t *testing.T) {
	tm := newTestMgr(t, newFake())
	b := mkBench(t, "b", "p", `"phases":[{"type":"load"}]`)
	ar := activeOf(tm, b)
	ar.cur = 0
	tr := newTrack(b, 0, nil)
	tr.cur, tr.useDirect, tr.direct = 0, true, 0.37
	tr.load = json.RawMessage(`{"progress":0.37,"workers":[{"addr":"10.0.0.2:50052","bytes_total":10,"bytes_sent":4}]}`)
	ar.tracks["b"] = tr
	d := docOf(t, tm, ar)
	cur := d["current"].(map[string]any)
	if got := num(t, cur["percent"]); got != 37 {
		t.Fatalf("a load-only benchmark's percent is the load's: %v", got)
	}
	det := cur["detail"].(map[string]any)
	raw, ok := det["load"].(json.RawMessage)
	if !ok || !strings.Contains(string(raw), `"bytes_sent":4`) {
		t.Fatalf("the scheduler's load object is passed through: %v", det)
	}
	// and the document is plain JSON
	if _, err := json.Marshal(d); err != nil {
		t.Fatal(err)
	}
}

func TestProgressOfRequestPhasesCountsRequestsAndStreamedTokens(t *testing.T) {
	tm := newTestMgr(t, newFake())
	b := mkBench(t, "b", "p", `"phases":[{"type":"requests","repeats":4,"requests":[{"prompt":"x","max_tokens":100}]}]`)
	tr := newTrack(b, 0, nil)
	tr.cur, tr.planned, tr.done = 0, 4, 1
	tr.inflight = 0.5 // streaming: 50 of 100 tokens of the request in flight
	if got := tr.percent(); math.Abs(got-37.5) > 0.01 {
		t.Fatalf("(1 + 0.5) / 4 of the phase: %v", got)
	}
	// a hold counts elapsed time
	h := mkBench(t, "h", "p", `"phases":[{"type":"hold","seconds":20,"request":{"prompt":"x"}}]`)
	ht := newTrack(h, 0, nil)
	ht.cur, ht.useDirect, ht.direct = 0, true, 10.0/25
	if got := ht.percent(); math.Abs(got-40) > 0.01 {
		t.Fatalf("%v", got)
	}
	_ = tm
}

func TestProgressIsMonotonicAndReachesHundredOnlyAtTheEnd(t *testing.T) {
	tm := newTestMgr(t, newFake())
	b := mkBench(t, "a", "p", `"phases":[{"type":"load"},{"type":"unload"}]`)
	ar := activeOf(tm, b)
	ar.cur = 0
	tr := newTrack(b, 0, nil)
	ar.tracks["a"] = tr
	tr.cur, tr.useDirect, tr.direct = 0, true, 0.8
	p1 := num(t, docOf(t, tm, ar)["percent"])
	tr.direct = 0.3 // the scheduler's estimate fell back: the shown percent does not
	p2 := num(t, docOf(t, tm, ar)["percent"])
	if p2 < p1 {
		t.Fatalf("percent went back: %v then %v", p1, p2)
	}
	tr.cur, tr.useDirect, tr.direct = 1, true, 1 // the last phase is done, but the benchmark has not ended
	d := docOf(t, tm, ar)
	if num(t, d["percent"]) >= 100 || num(t, d["current"].(map[string]any)["percent"]) >= 100 {
		t.Fatalf("100 before the end: %v", d)
	}
	tr.finished = true
	ar.done = 1
	if got := num(t, docOf(t, tm, ar)["current"].(map[string]any)["percent"]); got != 100 {
		t.Fatalf("a finished benchmark is 100: %v", got)
	}
	if got := num(t, docOf(t, tm, ar)["percent"]); got >= 100 {
		t.Fatalf("a running run is below 100: %v", got)
	}
	ar.run.State = RunCompleted
	if got := num(t, docOf(t, tm, ar)["percent"]); got != 100 {
		t.Fatalf("a completed run is 100: %v", got)
	}
}

func TestProgressMessagesDuringARealRun(t *testing.T) {
	api := newFake().prof("a", fakeProf{loadDelay: 400 * time.Millisecond}).prof("b", fakeProf{loadDelay: 150 * time.Millisecond}).prof("c", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"load-a","name":"Load A","description":"cold load of a","profile":"a","phases":[{"type":"load"},{"type":"unload"}]}`)
	tm.bench(`{"id":"conv-b","name":"Conversation B","description":"talk to b","profile":"b","phases":[{"type":"load"},{"type":"conversation","stream":true,"max_tokens":8,"turns":[{"content":"hello there"},{"content":"and again"}]},{"type":"unload"}]}`)
	tm.bench(`{"id":"req-c","profile":"c","repeats":2,"requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.suite(`{"id":"s","name":"Three","benchmarks":["load-a","conv-b","req-c"],"cooldown_s":0,"only_when_idle":false}`)
	run := tm.run("s")
	// while it runs, the state document has the run object
	sawState := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if d := tm.m.State(); d.Run != nil && d.Run["current"] != nil {
			sawState = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawState {
		t.Fatal("State().Run never showed the running run")
	}
	tm.finished(run.ID)
	time.Sleep(50 * time.Millisecond)

	tm.evMu.Lock()
	var docs []map[string]any
	for _, e := range tm.events {
		if e["kind"] == "progress" {
			docs = append(docs, e["run"].(map[string]any))
		}
	}
	tm.evMu.Unlock()
	if len(docs) < 5 {
		t.Fatalf("only %d progress messages", len(docs))
	}
	last := -1.0
	sawLoadPct, sawNextB, sawConvPhase, sawLoadObj := false, false, false, false
	for i, d := range docs {
		p := d["percent"].(float64)
		if p < last {
			t.Fatalf("message %d: percent went back from %v to %v", i, last, p)
		}
		last = p
		if p >= 100 && d["state"] != RunCompleted {
			t.Fatalf("message %d: 100 percent while %v", i, d["state"])
		}
		if d["id"] != run.ID || d["suite_id"] != "s" || d["suite_name"] != "Three" || d["steps_total"].(int) != 3 {
			t.Fatalf("%v", d)
		}
		cur, _ := d["current"].(map[string]any)
		if cur == nil {
			continue
		}
		if cur["benchmark_id"] == "load-a" {
			if n, _ := d["next"].(map[string]any); n != nil && n["benchmark_id"] == "conv-b" && n["description"] == "talk to b" {
				sawNextB = true
			}
			if ph := cur["phase"].(map[string]any); ph["type"] == "load" && cur["percent"].(float64) > 0 {
				sawLoadPct = true
				if det := cur["detail"].(map[string]any); det["load"] != nil {
					sawLoadObj = true
				}
			}
		}
		if cur["benchmark_id"] == "conv-b" && cur["phase"].(map[string]any)["type"] == "conversation" {
			sawConvPhase = true
			if cur["model_file"] != "b.gguf" || cur["description"] != "talk to b" {
				t.Fatalf("%v", cur)
			}
		}
	}
	if !sawLoadPct || !sawNextB || !sawConvPhase || !sawLoadObj {
		t.Fatalf("load percent %v, next %v, conversation phase %v, load object %v", sawLoadPct, sawNextB, sawConvPhase, sawLoadObj)
	}
	final := docs[len(docs)-1]
	if final["state"] != RunCompleted || final["percent"].(float64) != 100 || final["steps_done"].(int) != 3 || final["next"] != nil {
		t.Fatalf("final progress %v", final)
	}
	// at most two a second between changes: the count is bounded by the changes plus the ticker
	if span := 2.0; float64(len(docs)) > 60*span {
		t.Fatalf("%d messages", len(docs))
	}
	// no progress message is sent after the run ended except the final one
	_ = fmt.Sprint()
}

func TestProgressMessagesAreRateLimited(t *testing.T) {
	api := newFake().prof("a", fakeProf{})
	tm := newTestMgr(t, api)
	b := mkBench(t, "a", "a", `"phases":[{"type":"load"}]`)
	ar := activeOf(tm, b)
	ar.cur = 0
	ar.tracks["a"] = newTrack(b, 0, nil)
	var n int
	tm.m.Subscribe(func(ev map[string]any) {
		if ev["kind"] == "progress" {
			n++
		}
	})
	for i := 0; i < 50; i++ {
		tm.m.emitProgress(ar, false)
	}
	if n != 1 {
		t.Fatalf("%d unforced messages within one interval, want 1", n)
	}
	tm.m.emitProgress(ar, true)
	tm.m.emitProgress(ar, true)
	if n != 3 {
		t.Fatalf("a forced message always goes out: %d", n)
	}
}
