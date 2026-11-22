package bench

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Every step that does not end ready leaves nothing of its own loaded: it
// ends with a forced unload of its profile before the next step starts,
// whether a phase failed, the step timed out or the run was cancelled.

// unloadedBefore reports whether a forced unload of profile comes before the
// first call that starts with next (or anywhere, when next is "").
func unloadedBefore(calls []string, profile, next string) bool {
	for _, c := range calls {
		if c == "unload:"+profile+" force=true" {
			return true
		}
		if next != "" && strings.HasPrefix(c, next) {
			return false
		}
	}
	return false
}

func twoSteps(t *testing.T, tm *testMgr, first string) *Run {
	t.Helper()
	tm.bench(first)
	tm.bench(`{"id":"next","profile":"second","phases":[{"type":"load"},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["b","next"],"cooldown_s":0,"only_when_idle":false}`)
	return tm.finished(tm.run("s").ID)
}

func TestFailingPhaseUnloadsBeforeTheNextStep(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatCode: 500}).prof("second", fakeProf{})
	tm := newTestMgr(t, api)
	r := twoSteps(t, tm, `{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"requests","requests":[{"prompt":"hi","max_tokens":8}]}]}`)
	if r.Steps[0].Outcome == OutcomeReady {
		t.Fatalf("%+v", r.Steps[0])
	}
	if calls := api.callList(); !unloadedBefore(calls, "gemma", "load:second") || api.has("gemma") {
		t.Fatalf("the failed step's model was still loaded when the next step began: %v", calls)
	}
}

func TestTimedOutStepUnloadsBeforeTheNextStep(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatDelay: 3 * time.Second}).prof("second", fakeProf{})
	tm := newTestMgr(t, api)
	r := twoSteps(t, tm, `{"id":"b","profile":"gemma","timeout_s":1,"phases":[{"type":"load"},{"type":"requests","requests":[{"prompt":"hi","max_tokens":8}]}]}`)
	if r.Steps[0].Outcome != OutcomeFailed || !strings.Contains(r.Steps[0].Error, "timed out") {
		t.Fatalf("%+v", r.Steps[0])
	}
	if calls := api.callList(); !unloadedBefore(calls, "gemma", "load:second") || api.has("gemma") {
		t.Fatalf("the timed-out step's model was still loaded when the next step began: %v", calls)
	}
}

func TestCancelledStepWithoutUnloadPhaseUnloads(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatDelay: 30 * time.Millisecond})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"requests","repeats":50,"requests":[{"prompt":"hi","max_tokens":8}]}]}`)
	tm.suite(`{"id":"s","benchmarks":["b"],"only_when_idle":false}`)
	run := tm.run("s")
	for api.nChatOf("gemma") < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	tm.m.Cancel(run.ID)
	r := tm.finished(run.ID)
	if r.State != RunCancelled || api.has("gemma") || !unloadedBefore(api.callList(), "gemma", "") {
		t.Fatalf("%s: %v", r.State, api.callList())
	}
}

// The scheduler accepted the load but the answer never arrived (a reset
// connection): the run did not know it had loaded anything and left it.
func TestLoadAnswerLostAfterAcceptStillUnloads(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{loadErr: true, loadDelay: 20 * time.Millisecond})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"requests","requests":[{"prompt":"hi","max_tokens":8}]}]}`)
	if r.Steps[0].Outcome != OutcomeError {
		t.Fatalf("%+v", r.Steps[0])
	}
	time.Sleep(40 * time.Millisecond) // the accepted load finishes on its own
	if !unloadedBefore(api.callList(), "gemma", "") {
		t.Fatalf("nothing unloaded the accepted load: %v", api.callList())
	}
}

// An unload that does not finish in time still counts as loaded, so the run
// tries again with force.
func TestUnloadThatDoesNotFinishIsRetriedWithForce(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.unloadMS = time.Second
	tm := newTestMgr(t, api, func(o *ManagerOptions) { o.GoneTimeout = 50 * time.Millisecond })
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	if r.Steps[0].Outcome != OutcomeError {
		t.Fatalf("%+v", r.Steps[0])
	}
	if !unloadedBefore(api.callList(), "gemma", "") {
		t.Fatalf("the lingering unload was not retried with force: %v", api.callList())
	}
}

// A successful step that leaves its model loaded on purpose keeps it.
func TestReadyStepWithoutUnloadKeepsItsModelUntilTheRunEnds(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{}).prof("second", fakeProf{})
	tm := newTestMgr(t, api)
	r := twoSteps(t, tm, `{"id":"b","profile":"gemma","phases":[{"type":"load"}]}`)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
	if unloadedBefore(api.callList(), "gemma", "load:second") {
		t.Fatalf("a ready step's model was unloaded early: %v", api.callList())
	}
}

func TestEvictOthersRunsBesideAnIdleOutsideModel(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.external["other"] = true // someone else's model, loaded and idle
	tm := newTestMgr(t, api)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"idle_wait_s":0,"evict_others":true}`)
	if r := tm.finished(tm.run("s").ID); r.State != RunCompleted {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	// off (the default) the same scheduler is busy: see TestOnlyWhenIdleSkipsAfterIdleWait
	api.extReq = 1 // someone else's request in flight: busy either way
	tm.suite(`{"id":"s2","benchmarks":["gemma"],"idle_wait_s":0,"evict_others":true}`)
	r := tm.finished(tm.run("s2").ID)
	if r.State != RunSkipped || !strings.Contains(r.Error, "not idle") {
		t.Fatalf("%s %q", r.State, r.Error)
	}
}

func TestBusyReasonWithEvictOthers(t *testing.T) {
	api := newFake()
	api.external["other"] = true
	st, _ := api.Status(context.Background())
	if busyReason(st, nil, false) == "" {
		t.Fatal("off: an outside model makes the scheduler busy")
	}
	if why := busyReason(st, nil, true); why != "" {
		t.Fatalf("on: an idle outside model is ignored, got %q", why)
	}
	st.QueueLen = 1
	if busyReason(st, nil, true) == "" {
		t.Fatal("on: a queue entry still waits")
	}
}
