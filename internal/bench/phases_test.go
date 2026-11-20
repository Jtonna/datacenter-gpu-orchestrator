package bench

import (
	"strings"
	"testing"
	"time"
)

// runOne runs a one-benchmark suite and returns the finished run.
func (tm *testMgr) runOne(benchJSON string) *Run {
	tm.t.Helper()
	tm.bench(benchJSON)
	tm.suite(`{"id":"s","benchmarks":["b"],"cooldown_s":0,"only_when_idle":false}`)
	return tm.finished(tm.run("s").ID)
}

func phaseTypesOf(st Step) string {
	var s []string
	for _, p := range st.Phases {
		s = append(s, p.Type+":"+p.State)
	}
	return strings.Join(s, ",")
}

func TestLoadOnlyUsesApiLoadAndSendsNoChat(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{loadDelay: 30 * time.Millisecond, nodes: []string{"ps5-a", "ps5-b"}})
	api.unloadMS = 40 * time.Millisecond
	api.insts["gemma"] = &fakeInst{state: "ready"} // loaded by someone: the load phase makes it cold first
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","description":"just the load","phases":[{"type":"load"},{"type":"unload"}],"expect":{"max_load_s":120,"max_unload_s":5}}`)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
	st := r.Steps[0]
	calls := strings.Join(api.callList(), " ")
	if strings.Contains(calls, "chat:") {
		t.Fatalf("a load-only benchmark sent a chat request: %s", calls)
	}
	if strings.Index(calls, "unload:gemma force=false") > strings.Index(calls, "load:gemma") || !strings.Contains(calls, "load:gemma") {
		t.Fatalf("expected unload, then /api/load: %s", calls)
	}
	if st.Outcome != OutcomeReady || len(st.Requests) != 0 || st.Expect == nil || !st.Expect.Pass {
		t.Fatalf("%+v", st)
	}
	if phaseTypesOf(st) != "load:done,unload:done" {
		t.Fatalf("%s", phaseTypesOf(st))
	}
	l := st.Phases[0].Load
	if l == nil || l.LoadS != 66.1 || l.Bytes != 6700000000 || l.AvgMBs != 101.4 || len(l.Nodes) != 2 || *l.Nodes[1].Bytes != 6700000000 || l.MemoryMiB != 7900 {
		t.Fatalf("load phase %+v", l)
	}
	if st.Load == nil || st.Load.LoadS != 66.1 || st.ModelFile != "gemma.gguf" || st.NodeCount != 2 || len(st.Plan) == 0 {
		t.Fatalf("step %+v", st)
	}
	u := st.Phases[1].Unload
	if u == nil || !u.Loaded || !u.Gone || u.UnloadS < 0.03 {
		t.Fatalf("unload phase %+v", u)
	}
	if api.has("gemma") {
		t.Fatal("left loaded")
	}
	if r.Env == nil || r.Env.SchedulerVersion != "test" || r.Env.LlamaBinary != "llama-server-rpc" || len(r.Env.Workers) != 3 || r.Env.Workers[0].RPCServerVersion != "b4567" {
		t.Fatalf("environment %+v", r.Env)
	}
	for _, w := range r.Env.Workers {
		if w.Node == "host-1" {
			t.Fatal("the local node is not a worker")
		}
	}
}

func TestShorthandRunsAsTheSamePhases(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","repeats":2,"requests":[{"name":"q","prompt":"hi","max_tokens":4}]}`)
	if phaseTypesOf(r.Steps[0]) != "load:done,warmup:done,requests:done,unload:done" {
		t.Fatalf("%s", phaseTypesOf(r.Steps[0]))
	}
	if n := r.Steps[0].Phases[2].Stats.Requests; n != 2 {
		t.Fatalf("requests phase stats %+v", r.Steps[0].Phases[2].Stats)
	}
	if w := r.Steps[0].Phases[1].Stats; w == nil || w.Requests != 1 {
		t.Fatalf("warmup phase stats %+v", w)
	}
}

func TestConversationCarriesHistoryAndRecordsReuse(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","expect":{"min_reused_tokens":5},"phases":[{"type":"load"},
	  {"type":"conversation","system":"Be brief.","max_tokens":20,"stream":true,"turns":[
	    {"content":"First question about hash tables and how they work in detail, please."},
	    {"content":"second question that builds on the first answer you gave me just now"},
	    {"content":"third and last question to wrap up the whole conversation for today"}]},{"type":"unload"}]}`)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
	st := r.Steps[0]
	api.mu.Lock()
	bodies := append([]string(nil), api.bodies...)
	api.mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("%d chat requests", len(bodies))
	}
	if !strings.Contains(bodies[0], `"role":"system"`) || strings.Contains(bodies[0], "assistant") {
		t.Fatalf("turn 1: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], "First question") || !strings.Contains(bodies[1], `"role":"assistant"`) || !strings.Contains(bodies[1], "hello from gemma") || !strings.Contains(bodies[1], "second question") {
		t.Fatalf("turn 2 must carry the first turn and its reply: %s", bodies[1])
	}
	if !strings.Contains(bodies[2], "hello from gemma (turn two)") || !strings.Contains(bodies[2], "third and last") {
		t.Fatalf("turn 3 must carry both earlier exchanges: %s", bodies[2])
	}
	if !strings.Contains(bodies[1], `"cache_prompt":true`) || !strings.Contains(bodies[1], `"stream":true`) {
		t.Fatalf("a conversation caches the prompt by default: %s", bodies[1])
	}
	conv := st.Phases[1]
	if len(conv.Turns) != 3 || conv.Turns[0].CacheN != 0 || conv.Turns[1].CacheN == 0 || conv.Turns[2].CacheN <= conv.Turns[1].CacheN {
		t.Fatalf("turns %+v", conv.Turns)
	}
	if conv.Turns[1].PromptTokens != conv.Turns[1].PromptN+conv.Turns[1].CacheN || conv.Turns[0].TTFTMS == nil || conv.Turns[0].DecodeTPS == 0 || conv.Turns[0].PrefillTPS == 0 {
		t.Fatalf("turn metrics %+v", conv.Turns[0])
	}
	if st.Expect == nil || !st.Expect.Pass {
		t.Fatalf("expect %+v", st.Expect)
	}
	// the step speaks with its conversation rows when it has no measured requests
	if st.Stats == nil || st.Stats.Decode == nil || st.Stats.Decode.N != 3 || st.Stats.TTFT == nil {
		t.Fatalf("stats %+v", st.Stats)
	}
	for _, q := range st.Requests {
		if q.Phase != PhaseConversation || q.PhaseIdx != 1 || q.Turn < 1 {
			t.Fatalf("%+v", q)
		}
	}
}

func TestConversationWithoutCacheReusesNothing(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","expect":{"min_reused_tokens":1},"phases":[{"type":"conversation","cache_prompt":false,
	  "turns":[{"content":"one one one one one one one one one one one one one one one"},{"content":"two two two two two two two two two two two two two two two two two"}]}]}`)
	st := r.Steps[0]
	if st.Phases[0].Turns[1].CacheN != 0 || st.Expect.Pass {
		t.Fatalf("%+v %+v", st.Phases[0].Turns, st.Expect)
	}
}

func TestHoldKeepsTheModelIdleThenSendsOneRequest(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	started := time.Now()
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"hold","seconds":1,"request":{"name":"after-idle","prompt":"hi","max_tokens":4}},{"type":"unload"}]}`)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
	if time.Since(started) < time.Second {
		t.Fatal("the hold did not wait")
	}
	st := r.Steps[0]
	h := st.Phases[1]
	if h.HeldS == nil || *h.HeldS < 1 || h.LoadedAfterHold == nil || !*h.LoadedAfterHold || h.DurationS < 1 {
		t.Fatalf("%+v", h)
	}
	if len(st.Requests) != 1 || st.Requests[0].Phase != PhaseHold || st.Requests[0].Name != "after-idle" {
		t.Fatalf("%+v", st.Requests)
	}
	calls := strings.Join(api.callList(), " ")
	if strings.Index(calls, "load:gemma") > strings.Index(calls, "chat:gemma#1") || strings.Count(calls, "chat:") != 1 {
		t.Fatalf("%s", calls)
	}
}

func TestConcurrentRequestsFireTogetherAndRecordWaitAndErrors(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatDelay: 60 * time.Millisecond})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","expect":{"max_errors":0,"max_concurrent_wall_s":5},"phases":[{"type":"load"},{"type":"concurrent_requests","n":4,"request":{"name":"c","prompt":"hi","max_tokens":4}},{"type":"unload"}]}`)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
	st := r.Steps[0]
	c := st.Phases[1].Concurrent
	if c == nil || c.N != 4 || len(c.Items) != 4 || c.Errors != 0 || c.WallMS < 50 || c.WallMS > 2000 {
		t.Fatalf("%+v", c)
	}
	for _, it := range c.Items {
		if it.Status != 200 || it.WaitMS == nil || it.DecodeTPS == 0 || it.Index < 1 {
			t.Fatalf("%+v", it)
		}
	}
	api.mu.Lock()
	ts := api.chats["gemma"]
	api.mu.Unlock()
	if len(ts) != 4 {
		t.Fatalf("%d requests", len(ts))
	}
	first, last := ts[0], ts[0]
	for _, x := range ts {
		if x.Before(first) {
			first = x
		}
		if x.After(last) {
			last = x
		}
	}
	if gap := last.Sub(first); gap > 40*time.Millisecond {
		t.Fatalf("the requests were %v apart", gap)
	}
	if len(st.Requests) != 4 || st.Requests[0].Phase != PhaseConcurrent || st.Requests[0].WaitMS == nil {
		t.Fatalf("%+v", st.Requests)
	}
	if st.Expect == nil || !st.Expect.Pass {
		t.Fatalf("%+v", st.Expect)
	}
}

func TestConcurrentErrorsAreCountedNotFatal(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatCode: 503})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","expect":{"max_errors":1},"phases":[{"type":"concurrent_requests","n":3,"request":{"prompt":"hi","max_tokens":4}}]}`)
	st := r.Steps[0]
	if st.Outcome != OutcomeReady || st.Phases[0].Concurrent.Errors != 3 || st.Expect.Pass {
		t.Fatalf("%+v %+v", st.Phases[0].Concurrent, st.Expect)
	}
	if !strings.Contains(st.Expect.Checks[len(st.Expect.Checks)-1].Detail, "3 requests failed") {
		t.Fatalf("%+v", st.Expect)
	}
}

func TestUnloadPhaseTimesTheDisappearance(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.unloadMS = 120 * time.Millisecond
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","expect":{"max_unload_s":0.05},"phases":[{"type":"load"},{"type":"unload"}]}`)
	st := r.Steps[0]
	u := st.Phases[1].Unload
	if u == nil || u.UnloadS < 0.1 || !u.Gone {
		t.Fatalf("%+v", u)
	}
	if st.Expect.Pass {
		t.Fatalf("a slow unload should fail max_unload_s: %+v", st.Expect)
	}
}

func TestFailureSkipsLaterPhasesButStillUnloads(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatCode: 500})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"warmup","request":{"prompt":"hi"}},{"type":"requests","requests":[{"prompt":"hi"}]},{"type":"unload"}]}`)
	st := r.Steps[0]
	if st.Outcome != OutcomeFailed || st.HTTPStatus != 500 {
		t.Fatalf("%+v", st)
	}
	if phaseTypesOf(st) != "load:done,warmup:failed,requests:skipped,unload:done" {
		t.Fatalf("%s", phaseTypesOf(st))
	}
	if !strings.Contains(strings.Join(api.callList(), " "), "unload:gemma force=true") || api.has("gemma") {
		t.Fatalf("a failed step unloads with force: %v", api.callList())
	}
}

func TestRequestsOnAnUnloadedModelLoadItFirst(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"requests","requests":[{"prompt":"hi","max_tokens":4}]}]}`)
	calls := strings.Join(api.callList(), " ")
	if r.State != RunCompleted || strings.Index(calls, "load:gemma") < 0 || strings.Index(calls, "load:gemma") > strings.Index(calls, "chat:gemma#1") {
		t.Fatalf("%s %v", r.State, calls)
	}
	if r.Steps[0].Load == nil {
		t.Fatal("the implicit load is recorded on the step")
	}
	if api.has("gemma") {
		t.Fatal("what the run loaded is unloaded when the run ends")
	}
}

func TestSoftUnloadLeavesSomeoneElsesModelAlone(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.insts["gemma"] = &fakeInst{state: "ready"}
	tm := newTestMgr(t, api)
	// the simple form: cold false, unload_after default true; the model was not loaded by the run
	r := tm.runOne(`{"id":"b","profile":"gemma","cold":false,"requests":[{"prompt":"hi","max_tokens":4}]}`)
	if r.State != RunCompleted || !api.has("gemma") || r.Steps[0].Phases[len(r.Steps[0].Phases)-1].State != PhaseSkipped {
		t.Fatalf("%s %s", r.State, phaseTypesOf(r.Steps[0]))
	}
}

func TestLoadFailureIsReportedWithTheSchedulersReason(t *testing.T) {
	api := newFake().prof("bad", fakeProf{fail: true})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"b","profile":"bad","phases":[{"type":"load"},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["b"],"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	st := r.Steps[0]
	if st.Outcome != OutcomeFailed || !strings.Contains(st.Reason, "child exited") || st.Load == nil || st.Load.Outcome != "failed" {
		t.Fatalf("%+v", st)
	}
	if st.Phases[0].State != PhaseFailed || st.Phases[0].Load == nil {
		t.Fatalf("%+v", st.Phases[0])
	}
}
