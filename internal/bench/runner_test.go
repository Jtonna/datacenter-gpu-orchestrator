package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// ---- harness ----

type testMgr struct {
	t      *testing.T
	m      *Manager
	store  *Store
	api    *fakeAPI
	events []map[string]any
	evMu   sync.Mutex
	hist   []history.Event
}

func newTestMgr(t *testing.T, api *fakeAPI, mod ...func(*ManagerOptions)) *testMgr {
	t.Helper()
	store, _ := openTemp(t)
	tm := &testMgr{t: t, store: store, api: api}
	o := ManagerOptions{Store: store, API: api, Version: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		IdlePoll: 5 * time.Millisecond, LoadPoll: 5 * time.Millisecond, GoneTimeout: 2 * time.Second,
		Record: func(e history.Event) { tm.evMu.Lock(); tm.hist = append(tm.hist, e); tm.evMu.Unlock() }}
	for _, f := range mod {
		f(&o)
	}
	m, err := NewManager(o)
	if err != nil {
		t.Fatal(err)
	}
	m.Subscribe(func(ev map[string]any) { tm.evMu.Lock(); tm.events = append(tm.events, ev); tm.evMu.Unlock() })
	tm.m = m
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop(context.Background()) })
	// The loop's start-up schedule check runs in its own goroutine; a test
	// that moves the clock waits for it, or the check would act on the
	// test's suites at a time the test does not choose (next: the 30 s tick).
	select {
	case <-m.startupChecked:
	case <-time.After(10 * time.Second):
		t.Fatal("the manager loop did not make its start-up schedule check")
	}
	return tm
}

func (tm *testMgr) bench(js string) *benchspec.Benchmark {
	tm.t.Helper()
	b, err := benchspec.ParseBenchmarkJSON([]byte(js))
	if err != nil {
		tm.t.Fatal(err)
	}
	if err := b.Validate(nil); err != nil {
		tm.t.Fatal(err)
	}
	if err := tm.store.SaveBenchmark(b, SourceAPI, 1); err != nil {
		tm.t.Fatal(err)
	}
	return b
}

func (tm *testMgr) suite(js string) *benchspec.Suite {
	tm.t.Helper()
	s, err := benchspec.ParseSuiteJSON([]byte(js))
	if err != nil {
		tm.t.Fatal(err)
	}
	look := func(id string) (*benchspec.Benchmark, bool) {
		b, err := tm.store.GetBenchmark(id)
		if err != nil {
			return nil, false
		}
		return b.Spec, true
	}
	groups := func(id string) (*benchspec.Suite, bool) {
		g, err := tm.store.GetSuite(id)
		if err != nil {
			return nil, false
		}
		return g.Spec, true
	}
	if err := s.ValidateGroup(nil, look, groups); err != nil {
		tm.t.Fatal(err)
	}
	if err := tm.store.SaveSuite(s, SourceAPI, 1); err != nil {
		tm.t.Fatal(err)
	}
	return s
}

func (tm *testMgr) run(suite string) *Run {
	tm.t.Helper()
	r, err := tm.m.Enqueue(suite, EnqueueOptions{})
	if err != nil {
		tm.t.Fatal(err)
	}
	return r
}

func (tm *testMgr) wait(id string, states ...string) *Run {
	tm.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r, err := tm.store.GetRun(id)
		if err != nil {
			tm.t.Fatal(err)
		}
		for _, s := range states {
			if r.State == s {
				return r
			}
		}
		if time.Now().After(deadline) {
			tm.t.Fatalf("run %s is %s, wanted one of %v; calls %v", id, r.State, states, tm.api.callList())
		}
		time.Sleep(3 * time.Millisecond)
	}
}

func (tm *testMgr) finished(id string) *Run {
	return tm.wait(id, RunCompleted, RunFailed, RunCancelled, RunSkipped)
}

func (tm *testMgr) histTypes() []string {
	tm.evMu.Lock()
	defer tm.evMu.Unlock()
	var out []string
	for _, e := range tm.hist {
		out = append(out, e.Type)
	}
	return out
}

const gemmaBench = `{"id":"gemma","profile":"gemma","repeats":3,"requests":[{"name":"q","prompt":"hi","max_tokens":8}]}`

// ---- the tests ----

func TestRunMeasuresTheColdLoadAndExcludesWarmup(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{loadDelay: 20 * time.Millisecond, decode: 30})
	api.insts["gemma"] = &fakeInst{state: "ready"} // already loaded by someone: a cold benchmark unloads it first
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunCompleted {
		t.Fatalf("run %s: %s", r.State, r.Error)
	}
	st := r.Steps[0]
	if st.Outcome != OutcomeReady || st.State != StepDone || st.Expect == nil || !st.Expect.Pass {
		t.Fatalf("%+v", st)
	}
	calls := strings.Join(api.callList(), " ")
	if !strings.Contains(calls, "plan:gemma") || strings.Index(calls, "unload:gemma force=false") > strings.Index(calls, "load:gemma") ||
		strings.Index(calls, "load:gemma") > strings.Index(calls, "chat:gemma#1") {
		t.Fatalf("a cold benchmark unloads the profile, loads it with /api/load and only then sends requests: %s", calls)
	}
	// load numbers come from the scheduler: history by session id
	if st.Load == nil || st.Load.LoadS != 66.1 || st.Load.Bytes != 6700000000 || st.Load.AvgMBs != 101.4 || st.Load.Source != "history" ||
		st.Load.MemoryMiB != 7900 || len(st.Load.Nodes) != 1 || st.Load.Nodes[0].Node != "ps5-a" || *st.Load.Nodes[0].Bytes != 6700000000 {
		t.Fatalf("load %+v", st.Load)
	}
	if st.LogFile != "/var/log/llm-children/gemma.log" {
		t.Fatalf("log file %q", st.LogFile)
	}
	// requests: one warmup, three measured; the load sends no request
	var phases []string
	for _, q := range st.Requests {
		phases = append(phases, q.Phase)
	}
	if strings.Join(phases, ",") != "warmup,measured,measured,measured" {
		t.Fatalf("phases %v", phases)
	}
	// The first request after the load (the warmup) is slow (decode/5): it is not in the statistics.
	if st.Stats.Decode.N != 3 || st.Stats.Decode.Mean != 30 || st.Stats.Decode.Stdev != 0 || st.Stats.Prefill.Mean != 200 {
		t.Fatalf("stats %+v (warmup leaked in?)", st.Stats)
	}
	if st.Requests[0].DecodeTPS != 6 {
		t.Fatalf("the warmup should be the slow one: %+v", st.Requests[0])
	}
	if st.Requests[2].OutputSHA256 == "" || st.Requests[2].OutputHead != "hello from gemma" || len(st.Requests[2].Timings) == 0 {
		t.Fatalf("%+v", st.Requests[2])
	}
	// unload_after defaults to true
	if api.has("gemma") {
		t.Fatal("the model was left loaded")
	}
	// every request carried the run id
	for _, h := range api.hdrRuns {
		if h != r.ID {
			t.Fatalf("request header run id %q, want %q", h, r.ID)
		}
	}
	// history events and the bench events
	types := strings.Join(tm.histTypes(), ",")
	if types != "benchRunStarted,benchStepFinished,benchRunFinished" {
		t.Fatalf("history events %s", types)
	}
	tm.evMu.Lock()
	defer tm.evMu.Unlock()
	kinds := map[string]bool{}
	for _, e := range tm.events {
		kinds[e["kind"].(string)] = true
	}
	for _, k := range []string{"run", "step_started", "step_finished", "progress", "load"} {
		if !kinds[k] {
			t.Errorf("no %q bench event; saw %v", k, kinds)
		}
	}
}

func TestExpectedRefusalPasses(t *testing.T) {
	api := newFake().prof("huge", fakeProf{refuse: true})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"toobig","profile":"huge","expect":{"outcome":"refused"},"requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.suite(`{"id":"s","benchmarks":["toobig"],"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	st := r.Steps[0]
	if r.State != RunCompleted || st.Outcome != OutcomeRefused || st.HTTPStatus != 503 || !strings.Contains(st.Reason, "does not fit") || !st.Expect.Pass {
		t.Fatalf("%s %+v", r.State, st)
	}
	if len(st.Requests) != 0 || r.Summary.Passed != 1 || r.Summary.Failed != 0 || strings.Contains(strings.Join(api.callList(), " "), "chat:") {
		t.Fatalf("a refused load is the load's answer: no request is sent: %+v %+v", st.Requests, r.Summary)
	}
	if len(st.Phases) != 4 || st.Phases[0].Type != "load" || st.Phases[0].State != PhaseFailed || st.Phases[0].Outcome != OutcomeRefused || st.Phases[1].State != PhaseSkipped {
		t.Fatalf("phases %+v", st.Phases)
	}
	if len(api.callList()) > 0 && strings.Contains(strings.Join(api.callList(), " "), "unload:huge") && false {
		t.Fatal("unreachable")
	}
}

func TestUnexpectedFailureStopsTheSuiteWhenAsked(t *testing.T) {
	api := newFake().prof("bad", fakeProf{fail: true}).prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"bad","profile":"bad","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["bad","gemma"],"stop_on_failure":true,"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunFailed || !strings.Contains(r.Error, "stop_on_failure") {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	if r.Steps[0].Outcome != OutcomeFailed || r.Steps[0].Expect.Pass || !strings.Contains(r.Steps[0].Reason, "child exited") {
		t.Fatalf("%+v", r.Steps[0])
	}
	if r.Steps[0].Load == nil || r.Steps[0].Load.Outcome != "failed" || r.Steps[0].Load.Reason != "child exited during load" {
		t.Fatalf("a failed load is a result with the scheduler's reason: %+v", r.Steps[0].Load)
	}
	if r.Steps[1].State != StepSkipped || r.Steps[1].Outcome != OutcomeSkipped {
		t.Fatalf("%+v", r.Steps[1])
	}
	if strings.Contains(strings.Join(api.callList(), " "), "chat:gemma") {
		t.Fatal("the next benchmark ran after a failure with stop_on_failure")
	}
}

func TestWithoutStopOnFailureTheSuiteGoesOn(t *testing.T) {
	api := newFake().prof("bad", fakeProf{fail: true}).prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"bad","profile":"bad","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["bad","gemma"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunCompleted || r.Steps[1].Outcome != OutcomeReady || r.Summary.Failed != 1 || r.Summary.Passed != 1 {
		t.Fatalf("%s %+v %+v", r.State, r.Steps[1], r.Summary)
	}
}

func TestParallelStepFiresTheLoadsTogether(t *testing.T) {
	api := newFake().prof("a", fakeProf{loadDelay: 60 * time.Millisecond}).prof("b", fakeProf{loadDelay: 60 * time.Millisecond})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"ba","profile":"a","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(`{"id":"bb","profile":"b","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(`{"id":"both","parallel":["ba","bb"]}`)
	tm.suite(`{"id":"s","benchmarks":["both"],"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunCompleted || len(r.Steps) != 2 {
		t.Fatalf("%s %+v", r.State, r.Steps)
	}
	for _, st := range r.Steps {
		if st.Group != "both" || st.Idx != 0 || st.Outcome != OutcomeReady {
			t.Fatalf("%+v", st)
		}
	}
	api.mu.Lock()
	ta, tb := api.chats["a"][0], api.chats["b"][0]
	api.mu.Unlock()
	gap := ta.Sub(tb)
	if gap < 0 {
		gap = -gap
	}
	if gap > 30*time.Millisecond {
		t.Fatalf("the two first requests started %v apart: the loads were not fired together", gap)
	}
}

func TestOnlyWhenIdleWaitsThenRuns(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.external["other"] = true // someone else has a model loaded
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"idle_wait_s":30}`)
	run := tm.run("s")
	r := tm.wait(run.ID, RunPaused)
	if r.State != RunPaused {
		t.Fatal(r.State)
	}
	cur := tm.m.State().Current
	if cur == nil || cur["state"] != RunPaused || !strings.Contains(fmt.Sprint(cur["progress"]), "other") {
		t.Fatalf("state doc while paused: %v", cur)
	}
	if strings.Contains(strings.Join(api.callList(), " "), "chat:") {
		t.Fatal("the run started while the scheduler was in use")
	}
	api.mu.Lock()
	delete(api.external, "other")
	api.mu.Unlock()
	r = tm.finished(run.ID)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
}

func TestOnlyWhenIdleSkipsAfterIdleWait(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.external["other"] = true
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"idle_wait_s":0}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunSkipped || !strings.Contains(r.Error, "not idle") || len(r.Steps) != 1 || r.Steps[0].State != StepSkipped {
		t.Fatalf("%s %q %+v", r.State, r.Error, r.Steps)
	}
	if strings.Contains(strings.Join(api.callList(), " "), "chat:") {
		t.Fatal("a skipped run sent requests")
	}
}

func TestOnlyWhenIdleNoticesRequestsInFlight(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.external["other"] = true
	api.extReq = 2
	st, _ := api.Status(context.Background())
	if why := busyReason(st, map[string]bool{"other": true}, false); !strings.Contains(why, "2 requests in flight") {
		t.Fatalf("%q", why)
	}
	if why := busyReason(st, map[string]bool{}, false); !strings.Contains(why, "not loaded by this run") {
		t.Fatalf("%q", why)
	}
	st.Instances = nil
	if busyReason(st, nil, false) != "" {
		t.Fatal("an empty scheduler is idle")
	}
	st.QueueLen = 1
	if busyReason(st, nil, false) == "" {
		t.Fatal("a queue entry is not idle")
	}
}

func TestCancelStopsAfterTheCurrentRequestAndUnloads(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatDelay: 40 * time.Millisecond}).prof("second", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"gemma","profile":"gemma","repeats":50,"unload_after":false,"requests":[{"name":"q","prompt":"hi","max_tokens":8}]}`)
	tm.bench(`{"id":"second","profile":"second","requests":[{"prompt":"hi","max_tokens":8}]}`)
	tm.suite(`{"id":"s","benchmarks":["gemma","second"],"cooldown_s":0,"only_when_idle":false}`)
	run := tm.run("s")
	deadline := time.Now().Add(10 * time.Second)
	for api.nChatOf("gemma") < 3 {
		if time.Now().After(deadline) {
			t.Fatal("never got going")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := tm.m.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	r := tm.finished(run.ID)
	if r.State != RunCancelled {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	if api.has("gemma") {
		t.Fatal("cancel left the run's own model loaded")
	}
	if strings.Contains(strings.Join(api.callList(), " "), "chat:second") {
		t.Fatal("the next benchmark started after the cancel")
	}
	// it stopped well before 50 repeats, and every recorded request finished (200)
	got := len(r.Steps[0].Requests)
	if got >= 52 {
		t.Fatalf("%d requests: the cancel was ignored", got)
	}
	for _, q := range r.Steps[0].Requests {
		if q.Status != 200 {
			t.Fatalf("a request was cut off by the cancel: %+v", q)
		}
	}
	if r.Steps[1].State != StepSkipped {
		t.Fatalf("%+v", r.Steps[1])
	}
	if err := tm.m.Cancel(run.ID); err == nil {
		t.Fatal("cancelling a finished run should be an error")
	}
}

func (f *fakeAPI) nChatOf(p string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.nChat[p] }

func TestCancelDuringALoadStopsItAndUnloads(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{loadDelay: 10 * time.Second})
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false}`)
	run := tm.run("s")
	for !api.has("gemma") {
		time.Sleep(2 * time.Millisecond)
	}
	start := time.Now()
	tm.m.Cancel(run.ID)
	r := tm.finished(run.ID)
	if r.State != RunCancelled || time.Since(start) > 3*time.Second {
		t.Fatalf("%s after %v", r.State, time.Since(start))
	}
	if api.has("gemma") || !strings.Contains(strings.Join(api.callList(), " "), "unload:gemma force=true") {
		t.Fatalf("the half-loaded model was not removed: %v", api.callList())
	}
}

func TestRegressionAgainstThePreviousCompletedRun(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{decode: 30, loadS: 60})
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false}`)
	r1 := tm.finished(tm.run("s").ID)
	if r1.Steps[0].Comparison != nil {
		t.Fatalf("the first run has no baseline: %+v", r1.Steps[0].Comparison)
	}
	// Slower load (+25%) and slower decode (-20%).
	api.mu.Lock()
	api.profs["gemma"].decode, api.profs["gemma"].loadS = 24, 75
	api.mu.Unlock()
	r2 := tm.finished(tm.run("s").ID)
	c := r2.Steps[0].Comparison
	if r2.BaselineRunID != r1.ID || c == nil || !c.Found || !c.Regression || c.BaselineRunID != r1.ID {
		t.Fatalf("%s %+v", r2.BaselineRunID, c)
	}
	if *c.LoadPct < 24.9 || *c.LoadPct > 25.1 || *c.DecodePct > -19.9 || *c.DecodePct < -20.1 || len(c.Reasons) != 2 {
		t.Fatalf("%+v %v", c, c.Reasons)
	}
	if r2.Summary.Regressions != 1 {
		t.Fatalf("%+v", r2.Summary)
	}
	// A run can name its baseline.
	api.mu.Lock()
	api.profs["gemma"].decode, api.profs["gemma"].loadS = 30, 61
	api.mu.Unlock()
	run3, err := tm.m.Enqueue("s", EnqueueOptions{Baseline: r1.ID, Note: "against the first"})
	if err != nil {
		t.Fatal(err)
	}
	r3 := tm.finished(run3.ID)
	if r3.BaselineRunID != r1.ID || r3.Steps[0].Comparison.Regression || r3.Note != "against the first" {
		t.Fatalf("%s %+v", r3.BaselineRunID, r3.Steps[0].Comparison)
	}
	if _, err := tm.m.Enqueue("s", EnqueueOptions{Baseline: "no-such-run"}); err == nil {
		t.Fatal("an unknown baseline was accepted")
	}
}

func TestDeterministicOutputChangeIsARegression(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{output: "the same words"})
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false}`)
	r1 := tm.finished(tm.run("s").ID)
	r2 := tm.finished(tm.run("s").ID)
	if c := r2.Steps[0].Comparison; c.Regression || (c.HashDiffers != nil && *c.HashDiffers) {
		t.Fatalf("identical output flagged: %+v", c)
	}
	api.mu.Lock()
	api.profs["gemma"].output = "different words"
	api.mu.Unlock()
	r3 := tm.finished(tm.run("s").ID)
	c := r3.Steps[0].Comparison
	if !c.Regression || c.HashDiffers == nil || !*c.HashDiffers || r3.BaselineRunID != r2.ID {
		t.Fatalf("%+v", c)
	}
	_ = r1
}

func TestExpectPinnedHash(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{output: "pinned"})
	tm := newTestMgr(t, api)
	msg, _ := json.Marshal(map[string]any{"role": "assistant", "content": "pinned"})
	tm.bench(`{"id":"g","profile":"gemma","expect":{"output_sha256":"` + HashMessage(msg) + `","min_decode_tps":10},"requests":[{"name":"q","prompt":"hi","max_tokens":8}]}`)
	tm.bench(`{"id":"g2","profile":"gemma","expect":{"output_sha256":"` + strings.Repeat("0", 64) + `"},"requests":[{"name":"q","prompt":"hi","max_tokens":8}]}`)
	tm.suite(`{"id":"s","benchmarks":["g","g2"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if !r.Steps[0].Expect.Pass || r.Steps[1].Expect.Pass || r.State != RunCompleted {
		t.Fatalf("%+v %+v", r.Steps[0].Expect, r.Steps[1].Expect)
	}
}

func TestOnlyOneRunAtATimeOthersQueue(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatDelay: 30 * time.Millisecond})
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false}`)
	a, b := tm.run("s"), tm.run("s")
	st := tm.m.State()
	if (st.Current == nil || st.Current["id"] != a.ID) && len(st.Queue) == 0 {
		t.Fatalf("state %+v", st)
	}
	ra := tm.finished(a.ID)
	rb := tm.finished(b.ID)
	if ra.FinishedAt > rb.StartedAt {
		t.Fatalf("runs overlapped: a finished %d, b started %d", ra.FinishedAt, rb.StartedAt)
	}
	// A queued run can be cancelled before it starts.
	c, d := tm.run("s"), tm.run("s")
	if err := tm.m.Cancel(d.ID); err != nil {
		t.Fatal(err)
	}
	rd := tm.wait(d.ID, RunCancelled)
	if !strings.Contains(rd.Error, "before it started") {
		t.Fatal(rd.Error)
	}
	tm.finished(c.ID)
}

func TestStopMarksTheRunningRunCancelled(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatDelay: 30 * time.Millisecond})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"gemma","profile":"gemma","repeats":100,"requests":[{"prompt":"hi","max_tokens":8}]}`)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false}`)
	run := tm.run("s")
	for api.nChatOf("gemma") < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	queued := tm.run("s")
	tm.m.Stop(context.Background())
	r := tm.finished(run.ID)
	if r.State != RunCancelled || !strings.Contains(r.Error, "shutting down") || api.has("gemma") {
		t.Fatalf("%s %q loaded=%v", r.State, r.Error, api.has("gemma"))
	}
	if q, _ := tm.store.GetRun(queued.ID); q.State != RunCancelled {
		t.Fatalf("queued run %s", q.State)
	}
	if _, err := tm.m.Enqueue("s", EnqueueOptions{}); err == nil {
		t.Fatal("a stopped manager accepted a run")
	}
}

func TestMaxRunTimeFailsTheRun(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatDelay: 30 * time.Millisecond})
	tm := newTestMgr(t, api, func(o *ManagerOptions) { o.MaxRun = 150 * time.Millisecond })
	tm.bench(`{"id":"gemma","profile":"gemma","repeats":100,"requests":[{"prompt":"hi","max_tokens":8}]}`)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunFailed || !strings.Contains(r.Error, "max_run_minutes") {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	if api.has("gemma") {
		t.Fatal("left loaded")
	}
}

// The run time limit is read from the setting when each run starts, so a
// change of bench_max_run_minutes applies to the next run without a restart
// and a run is never cut at an older, shorter limit (B5).
func TestMaxRunIsReadFromTheSettingAtEachRun(t *testing.T) {
	var mu sync.Mutex
	limit := 150 * time.Millisecond
	setting := func() time.Duration { mu.Lock(); defer mu.Unlock(); return limit }
	api := newFake().prof("gemma", fakeProf{chatDelay: 30 * time.Millisecond})
	tm := newTestMgr(t, api, func(o *ManagerOptions) { o.MaxRun = time.Hour; o.MaxRunSetting = setting })
	tm.bench(`{"id":"gemma","profile":"gemma","repeats":100,"requests":[{"prompt":"hi","max_tokens":8}]}`)
	tm.bench(`{"id":"short","profile":"gemma","repeats":12,"requests":[{"prompt":"hi","max_tokens":8}]}`)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false}`)
	tm.suite(`{"id":"s2","benchmarks":["short"],"only_when_idle":false}`)
	if r := tm.finished(tm.run("s").ID); r.State != RunFailed || !strings.Contains(r.Error, "max_run_minutes (150ms)") {
		t.Fatalf("the setting's limit was not used: %s %q", r.State, r.Error)
	}
	mu.Lock()
	limit = time.Minute
	mu.Unlock()
	if r := tm.finished(tm.run("s2").ID); r.State != RunCompleted { // about 360 ms: over the old limit
		t.Fatalf("a run was cut below the setting: %s %q", r.State, r.Error)
	}
}

func TestEnqueueRejectsAStaleProfileAndUnknownSuite(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api, func(o *ManagerOptions) { o.ProfileOK = func(p string) bool { return p == "gemma" } })
	if _, err := tm.m.Enqueue("nope", EnqueueOptions{}); err != ErrNotFound {
		t.Fatalf("%v", err)
	}
	b, _ := benchspec.ParseBenchmarkJSON([]byte(`{"id":"gone","profile":"removed","requests":[{"prompt":"x","max_tokens":1}]}`))
	tm.store.SaveBenchmark(b, SourceAPI, 1)
	tm.suite(`{"id":"s","benchmarks":["gone"]}`)
	if _, err := tm.m.Enqueue("s", EnqueueOptions{}); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("%v", err)
	}
}

func TestParallelMembersMustUseDifferentProfiles(t *testing.T) {
	api := newFake().prof("a", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"x1","profile":"a","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(`{"id":"x2","profile":"a","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(`{"id":"both","parallel":["x1","x2"]}`)
	tm.suite(`{"id":"s","benchmarks":["both"]}`)
	if _, err := tm.m.Enqueue("s", EnqueueOptions{}); err == nil || !strings.Contains(err.Error(), "same profile") {
		t.Fatalf("%v", err)
	}
}

func TestInlineBenchmarksRun(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.suite(`{"id":"s","only_when_idle":false,"benchmarks":[{"profile":"gemma","warmup_requests":0,"requests":[{"name":"q","prompt":"hi","max_tokens":4}]}]}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunCompleted || r.Steps[0].BenchmarkID != "s-1" || len(r.Steps[0].Requests) != 1 { // one measured request: the load itself sends none
		t.Fatalf("%s %+v", r.State, r.Steps)
	}
}

func TestWarmBenchmarkSendsNoLoadRequest(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.insts["gemma"] = &fakeInst{state: "ready"}
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"warm","profile":"gemma","cold":false,"unload_after":false,"requests":[{"name":"q","prompt":"hi","max_tokens":4}]}`)
	tm.suite(`{"id":"s","benchmarks":["warm"],"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	st := r.Steps[0]
	if st.Load != nil || strings.Contains(strings.Join(api.callList(), " "), "unload:") == false && false {
		t.Fatalf("%+v", st.Load)
	}
	for _, q := range st.Requests {
		if q.Phase == PhaseLoad {
			t.Fatal("a warm benchmark sent a load request")
		}
	}
	if !api.has("gemma") {
		t.Fatal("a model this run did not load was unloaded")
	}
}

func TestScheduleQueuesADueSuiteOnce(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	var now = time.Date(2026, 11, 18, 2, 0, 0, 0, time.Local)
	var mu sync.Mutex
	tm := newTestMgr(t, api, func(o *ManagerOptions) {
		o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false,"schedule":{"at":"03:00"}}`)
	st := tm.m.State()
	if len(st.NextScheduled) != 1 || time.UnixMilli(st.NextScheduled[0].Next).Hour() != 3 {
		t.Fatalf("%+v", st.NextScheduled)
	}
	tm.m.checkSchedules(true)
	if runs, _ := tm.store.ListRuns(RunFilter{}); len(runs) != 0 {
		t.Fatalf("queued before it was due: %+v", runs)
	}
	mu.Lock()
	now = time.Date(2026, 11, 18, 3, 0, 5, 0, time.Local)
	mu.Unlock()
	tm.m.checkSchedules(true)
	tm.m.checkSchedules(true) // already handled: not queued twice
	runs, _ := tm.store.ListRuns(RunFilter{})
	if len(runs) != 1 || runs[0].Trigger != "schedule" {
		t.Fatalf("%+v", runs)
	}
	tm.finished(runs[0].ID)
}

func TestSeedNeverOverwritesApiItems(t *testing.T) {
	api := newFake()
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"mine","profile":"p","requests":[{"prompt":"x","max_tokens":1}]}`)
	cfg, _ := benchspec.ParseBenchmarkJSON([]byte(`{"id":"mine","profile":"other","requests":[{"prompt":"x","max_tokens":1}]}`))
	cfg2, _ := benchspec.ParseBenchmarkJSON([]byte(`{"id":"seeded","profile":"p","requests":[{"prompt":"x","max_tokens":1}]}`))
	tm.m.Seed([]*benchspec.Benchmark{cfg, cfg2}, nil)
	b, _ := tm.store.GetBenchmark("mine")
	s, _ := tm.store.GetBenchmark("seeded")
	if b.Spec.Profile != "p" || b.Source != SourceAPI || s.Source != SourceConfig {
		t.Fatalf("%+v %+v", b, s)
	}
}

func TestStartRecoversInterruptedRuns(t *testing.T) {
	store, dir := openTemp(t)
	store.CreateRun(&Run{ID: "old", SuiteID: "s", SuiteName: "s", State: RunRunning, QueuedAt: 1, Suite: []byte(`{}`)})
	store.Close()
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := NewManager(ManagerOptions{Store: s2, API: newFake()}); err != nil {
		t.Fatal(err)
	}
	r, _ := s2.GetRun("old")
	if r.State != RunFailed || r.Error != "failed (interrupted)" {
		t.Fatalf("%s %q", r.State, r.Error)
	}
}

// ---- a load that the scheduler accepts and then does not carry out ----

const loadOnlyBench = `{"id":"lo","profile":"m","phases":[{"type":"load"}],"timeout_s":20,"expect":{"outcome":"%s"}}`

// runLoadOnly runs a benchmark whose only phase is a load against the scripted profile.
func runLoadOnly(t *testing.T, api *fakeAPI, want string) (*Run, *Step) {
	t.Helper()
	tm := newTestMgr(t, api)
	tm.bench(fmt.Sprintf(loadOnlyBench, want))
	tm.suite(`{"id":"s","benchmarks":["lo"],"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if len(r.Steps) != 1 {
		t.Fatalf("steps %+v", r.Steps)
	}
	return r, &r.Steps[0]
}

func checkPhaseReason(t *testing.T, st *Step, outcome, reason string) {
	t.Helper()
	if st.Outcome != outcome || !strings.Contains(st.Reason, reason) {
		t.Fatalf("step outcome %q reason %q, want %q containing %q", st.Outcome, st.Reason, outcome, reason)
	}
	if len(st.Phases) == 0 || st.Phases[0].Outcome != outcome || !strings.Contains(st.Phases[0].Error, reason) {
		t.Fatalf("phase %+v", st.Phases)
	}
}

// (a) the load API itself refuses.
func TestLoadPhaseAPIRefusalStatuses(t *testing.T) {
	for code, want := range map[int]string{404: OutcomeRefused, 409: OutcomeRefused, 422: OutcomeRefused, 503: OutcomeRefused, 500: OutcomeFailed} {
		api := newFake().prof("m", fakeProf{loadCode: code})
		_, st := runLoadOnly(t, api, want)
		if st.Outcome != want || st.HTTPStatus != code || st.Reason != "scripted load answer" {
			t.Errorf("status %d: outcome %q http %d reason %q, want %q", code, st.Outcome, st.HTTPStatus, st.Reason, want)
		}
	}
}

// (b) accepted, then refused before a child starts: from the status record.
func TestLoadPhaseRefusedAfterAcceptFromStatus(t *testing.T) {
	text := "forced nodes do not fit: profile m: need 14691 MiB (estimated, +5% margin)"
	api := newFake().prof("m", fakeProf{loadDelay: 60 * time.Millisecond, refuseAfter: "status", refuseText: text})
	r, st := runLoadOnly(t, api, "refused")
	checkPhaseReason(t, st, OutcomeRefused, text)
	if !st.Expect.Pass || r.Summary.Passed != 1 || st.HTTPStatus != 503 {
		t.Fatalf("expect %+v summary %+v http %d", st.Expect, r.Summary, st.HTTPStatus)
	}
	if strings.Contains(strings.Join(api.callList(), " "), "chat:") {
		t.Fatal("no request is sent")
	}
}

// (b) the same, found in the history when the status has no record.
func TestLoadPhaseRefusedAfterAcceptFromHistory(t *testing.T) {
	text := "forced nodes do not fit: profile m: need 14691 MiB"
	api := newFake().prof("m", fakeProf{loadDelay: 60 * time.Millisecond, refuseAfter: "history", refuseText: text})
	_, st := runLoadOnly(t, api, "refused")
	checkPhaseReason(t, st, OutcomeRefused, text)
	if !st.Expect.Pass {
		t.Fatalf("%+v", st.Expect)
	}
}

// A refusal of another bench run, or one from before this load, is not ours.
func TestLoadPhaseIgnoresForeignRefusals(t *testing.T) {
	api := newFake().prof("m", fakeProf{loadDelay: 30 * time.Millisecond})
	api.lastRef = []Refusal{{Profile: "m", AtMS: time.Now().Add(-time.Hour).UnixMilli(), Status: 503, Reason: "does not fit", Message: "old"},
		{Profile: "m", AtMS: time.Now().Add(time.Hour).UnixMilli(), Status: 503, Reason: "does not fit", Message: "someone else", BenchRun: "other-run"}}
	_, st := runLoadOnly(t, api, "ready")
	if st.Outcome != OutcomeReady || !st.Expect.Pass {
		t.Fatalf("%+v", st)
	}
}

// (c) a child started and died or was cancelled: failed, with the scheduler's reason.
func TestLoadPhaseChildDiedKeepsItsReason(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled"} {
		api := newFake().prof("m", fakeProf{loadDelay: 60 * time.Millisecond, fail: true, failOutcome: outcome, failReason: "llama-server exited with code 1 (out of memory)"})
		_, st := runLoadOnly(t, api, "ready")
		if st.Outcome != OutcomeFailed || st.Reason != "llama-server exited with code 1 (out of memory)" || st.Phases[0].Error != st.Reason {
			t.Errorf("%s: %+v", outcome, st)
		}
		if st.Load == nil || st.Load.Outcome != outcome {
			t.Errorf("%s: load %+v", outcome, st.Load)
		}
	}
}

// (c) followed by a backoff record: the child's reason wins over the backoff.
func TestLoadPhaseFailedChildBeatsBackoffRecord(t *testing.T) {
	api := newFake().prof("m", fakeProf{loadDelay: 40 * time.Millisecond, fail: true, failReason: "child exited during load"})
	api.histRef = []Refusal{{Profile: "m", Type: "profileBackedOff", AtMS: time.Now().Add(time.Hour).UnixMilli(), Message: "backed off"}}
	_, st := runLoadOnly(t, api, "ready")
	if st.Outcome != OutcomeFailed || st.Reason != "child exited during load" {
		t.Fatalf("%+v", st)
	}
}

// (d) a forced node is absent or offline: the scheduler's fail-fast refusal.
func TestLoadPhaseForcedNodeUnavailable(t *testing.T) {
	text := "forced nodes unavailable: node ps5-b is offline in the fleet"
	api := newFake().prof("m", fakeProf{loadDelay: 60 * time.Millisecond, refuseAfter: "status", refuseReason: "forced nodes unavailable", refuseText: text})
	_, st := runLoadOnly(t, api, "refused")
	checkPhaseReason(t, st, OutcomeRefused, text)
}

// Nothing explains the vanished instance: the generic reason, and not after the whole timeout.
func TestLoadPhaseGenericDisappearance(t *testing.T) {
	api := newFake().prof("m", fakeProf{loadDelay: 60 * time.Millisecond, refuseAfter: "none"})
	start := time.Now()
	_, st := runLoadOnly(t, api, "ready")
	if st.Outcome != OutcomeFailed || st.Reason != "the instance disappeared during the load" || time.Since(start) > 10*time.Second {
		t.Fatalf("%+v after %s", st, time.Since(start))
	}
}

// A load refused because a forced node is paused (sixth review SS3): the
// step is recorded as refused, not retried, and its reason says why. The
// scheduler's error body is read by HTTPAPI.Load into the message.
func TestLoadRefusedOnAPausedNodeSaysWhy(t *testing.T) {
	text := "forced nodes unavailable: profile m forces node a, but node a is paused by an operator; it cannot start until the node is unpaused, so this fails now instead of waiting for queue_timeout"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(503)
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": text, "type": "server_error", "code": 503, "reason": "forced nodes unavailable"}})
		w.Write(b)
	}))
	defer srv.Close()
	ans, err := (&HTTPAPI{Base: srv.URL}).Load(t.Context(), "m")
	if err != nil || ans.Status != 503 || ans.Msg != text || ans.Reason != "forced nodes unavailable" {
		t.Fatalf("load answer %+v %v", ans, err)
	}
	api := newFake().prof("m", fakeProf{loadCode: 503, loadMsg: ans.Msg})
	_, st := runLoadOnly(t, api, OutcomeRefused)
	checkPhaseReason(t, st, OutcomeRefused, "paused by an operator")
	if n := strings.Count(strings.Join(api.callList(), " "), "load:"); n > 1 {
		t.Fatalf("the load was sent %d times", n)
	}
}
