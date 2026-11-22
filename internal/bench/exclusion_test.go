package bench

import (
	"strings"
	"testing"
)

// 0.1.7: a step whose profile is refused because it forces an excluded node
// is recorded as refused_by_exclusion with the scheduler's reason. It is not
// a failure: it does not stop a stop_on_failure suite, and the summary
// counts it apart (1 passed, 1 refused, 0 failed, 1 ran).
func TestStepRefusedByExclusionIsNotAFailure(t *testing.T) {
	why := `profile x forces node host-1, but node host-1 is excluded by config (exclude_nodes entry "host-1"; excluding wins: change exclude_nodes or force_nodes)`
	api := newFake().prof("x", fakeProf{loadCode: 503, loadReason: "node excluded", loadMsg: why}).prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"x","profile":"x","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["x","gemma"],"stop_on_failure":true,"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunCompleted {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	st := r.Steps[0]
	if st.Outcome != OutcomeRefusedByExclusion || st.Reason != why || st.HTTPStatus != 503 || st.State != StepDone {
		t.Fatalf("%+v", st)
	}
	if got := StepResult(&st); got != ResultRefused {
		t.Fatalf("step result %q", got)
	}
	if r.Steps[1].Outcome != OutcomeReady {
		t.Fatalf("%+v", r.Steps[1])
	}
	if s := r.Summary; s.Steps != 2 || s.Ran != 1 || s.Passed != 1 || s.Refused != 1 || s.Failed != 0 {
		t.Fatalf("summary %+v", s)
	}
	if got := RunResult(r); got != ResultPass {
		t.Fatalf("run result %q", got)
	}
	if strings.Contains(strings.Join(api.callList(), " "), "chat:x") {
		t.Fatal("a request was sent to the refused profile")
	}
}
