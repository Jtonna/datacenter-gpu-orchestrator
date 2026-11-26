package bench

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// Run and Cancel from the Benchmarks pages: who started a run and why, who
// cancelled it and why, at which step; a cancel while the run waits for idle;
// the set of other people's models a run may unload; the start gate.

func (tm *testMgr) waitState(id, state string) *Run {
	tm.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := tm.store.GetRun(id)
		if err == nil && r.State == state {
			return r
		}
		if time.Now().After(deadline) {
			tm.t.Fatalf("run %s never became %s (%+v)", id, state, r)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestStartRecordsWhoAndWhy(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"b","profile":"gemma","requests":[{"prompt":"hi","max_tokens":8}]}`)
	r, err := tm.m.EnqueueBenchmark("b", BenchRunOptions{Start: StartInfo{By: "ann", Comment: "after the driver update"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.StartedBy != "ann" || r.StartComment != "after the driver update" {
		t.Fatalf("the queued run: %q %q", r.StartedBy, r.StartComment)
	}
	done := tm.finished(r.ID)
	if done.StartedBy != "ann" || done.StartComment != "after the driver update" {
		t.Fatalf("read back: %q %q", done.StartedBy, done.StartComment)
	}
}

func TestCancelQueuedRunSaysWhoAndWhy(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{loadDelay: 10 * time.Second})
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false}`)
	first := tm.run("s")
	second := tm.run("s")
	if err := tm.m.CancelBy(second.ID, "ann", "wrong zones"); err != nil {
		t.Fatal(err)
	}
	r := tm.finished(second.ID)
	if r.State != RunCancelled || r.Error != `cancelled before it started by ann: "wrong zones"` {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	tm.m.CancelBy(first.ID, "ann", "done")
	tm.finished(first.ID)
}

func TestCancelWhileLoadingSaysAtWhichStep(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{loadDelay: 10 * time.Second}).prof("second", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.bench(`{"id":"second","profile":"second","requests":[{"prompt":"hi","max_tokens":8}]}`)
	tm.suite(`{"id":"s","benchmarks":["gemma","second"],"cooldown_s":0,"only_when_idle":false}`)
	run := tm.run("s")
	for !api.has("gemma") {
		time.Sleep(2 * time.Millisecond)
	}
	if err := tm.m.CancelBy(run.ID, "ann", "wrong zones"); err != nil {
		t.Fatal(err)
	}
	r := tm.finished(run.ID)
	if r.State != RunCancelled || r.Error != `cancelled at step 1 of 2 by ann: "wrong zones"` {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	if api.has("gemma") || strings.Contains(strings.Join(api.callList(), " "), "load:second") {
		t.Fatalf("after the cancel: %v", api.callList())
	}
	if r.Steps[0].Outcome != OutcomeCancelled || r.Steps[1].State != StepSkipped {
		t.Fatalf("steps: %+v / %+v", r.Steps[0], r.Steps[1])
	}
	found := false
	for _, ev := range tm.histEvents() {
		if ev.Type == history.TBenchRunFinished && ev.Data["cancelled_by"] == "ann" && ev.Data["cancel_comment"] == "wrong zones" {
			found = true
		}
	}
	if !found {
		t.Fatal("the finished event does not say who cancelled and why")
	}
}

// A run that waits until idle (another user's model is loaded) is cancelled
// before it loads anything: every step is skipped, nothing is loaded.
func TestCancelWhileWaitingForIdle(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.external = map[string]bool{"theirs": true}
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":true,"idle_wait_s":600}`)
	run := tm.run("s")
	tm.waitState(run.ID, RunPaused)
	if err := tm.m.CancelBy(run.ID, "ann", "not now"); err != nil {
		t.Fatal(err)
	}
	r := tm.finished(run.ID)
	if r.State != RunCancelled || !strings.Contains(r.Error, `by ann: "not now"`) {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	for _, c := range api.callList() {
		if strings.HasPrefix(c, "load:") {
			t.Fatalf("a load was sent: %v", api.callList())
		}
	}
	if len(r.Steps) != 1 || r.Steps[0].State != StepSkipped {
		t.Fatalf("%+v", r.Steps)
	}
}

// evict_others with a named set: a model of someone else that the person was
// not shown is waited for, never unloaded; a named one is not waited for.
func TestEvictOthersOnlyTheNamedModels(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.external = map[string]bool{"named": true, "unnamed": true}
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	yes := true
	r, err := tm.m.EnqueueBenchmark("gemma", BenchRunOptions{EvictOthers: &yes, IdleWaitS: ptrInt(600),
		Start: StartInfo{By: "ann", Comment: "x", EvictLimited: true, EvictOnly: []string{"named"}}})
	if err != nil {
		t.Fatal(err)
	}
	tm.waitState(r.ID, RunPaused)
	var why string
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(why, "unnamed") && time.Now().Before(deadline) {
		why = tm.pausedReason()
		time.Sleep(2 * time.Millisecond)
	}
	if !strings.HasPrefix(why, "unnamed is ready") || strings.Contains(why, "; named") {
		t.Fatalf("the wait reason: %q (only the model not named is waited for)", why)
	}
	api.mu.Lock()
	delete(api.external, "unnamed")
	api.mu.Unlock()
	done := tm.finished(r.ID)
	if done.State != RunCompleted {
		t.Fatalf("%s %q", done.State, done.Error)
	}
}


func (tm *testMgr) pausedReason() string {
	tm.m.mu.Lock()
	cur := tm.m.cur
	tm.m.mu.Unlock()
	if cur == nil {
		return ""
	}
	cur.mu.Lock()
	defer cur.mu.Unlock()
	return cur.paused
}

func (tm *testMgr) histEvents() []history.Event {
	tm.evMu.Lock()
	defer tm.evMu.Unlock()
	return append([]history.Event(nil), tm.hist...)
}

// The start gate decides on the whole expanded run before anything is queued:
// a refusal creates no run and loads nothing, for a scheduled run as well.
func TestStartGateRefusesTheWholeRun(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{}).prof("second", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.bench(`{"id":"second","profile":"second","requests":[{"prompt":"hi","max_tokens":8}]}`)
	tm.suite(`{"id":"s","benchmarks":["gemma","second"],"cooldown_s":0,"only_when_idle":false}`)
	var saw []string
	tm.m.SetStartGate(func(g *GateRun) error {
		saw = nil
		for _, b := range g.Leaves {
			saw = append(saw, b.ID+"@"+b.Profile)
		}
		return &StartRefusal{Code: "not_startable", Msg: `"second" may not be started (bench_not_startable)`}
	})
	_, err := tm.m.Enqueue("s", EnqueueOptions{})
	var ref *StartRefusal
	if !errors.As(err, &ref) || ref.Code != "not_startable" {
		t.Fatalf("%v", err)
	}
	if strings.Join(saw, ",") != "gemma@gemma,second@second" {
		t.Fatalf("the gate saw %v", saw)
	}
	runs, _ := tm.store.ListRuns(RunFilter{})
	if len(runs) != 0 || len(api.callList()) != 0 {
		t.Fatalf("runs %d, calls %v", len(runs), api.callList())
	}
	tm.m.SetStartGate(nil)
	tm.finished(tm.run("s").ID)
}
