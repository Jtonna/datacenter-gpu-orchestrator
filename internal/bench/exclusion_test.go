package bench

import (
	"strings"
	"testing"
	"time"
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
	if got := StepResult(&st); got != ResultNotJudged {
		t.Fatalf("step result %q", got)
	}
	if r.Steps[1].Outcome != OutcomeReady {
		t.Fatalf("%+v", r.Steps[1])
	}
	if s := r.Summary; s.Steps != 2 || s.Ran != 1 || s.Passed != 1 || s.RefusedByExclusion != 1 || s.Failed != 0 {
		t.Fatalf("summary %+v", s)
	}
	if got := RunResult(r); got != ResultPass {
		t.Fatalf("run result %q", got)
	}
	if strings.Contains(strings.Join(api.callList(), " "), "chat:x") {
		t.Fatal("a request was sent to the refused profile")
	}
	// 0.1.7 review, ES2: every place that renders or records the step says
	// "not judged", never "failed".
	if st.Expect == nil || !st.Expect.NotJudged || st.Expect.Pass {
		t.Fatalf("expect %+v", st.Expect)
	}
	if got := passText(&st); got != "not judged" {
		t.Fatalf("export says %q", got)
	}
	csv, err := ExportCSV(r)
	if err != nil {
		t.Fatal(err)
	}
	for format, out := range map[string]string{"markdown": ExportMarkdown(r), "csv": csv} {
		if !strings.Contains(out, "not judged") || strings.Contains(out, "refused_by_exclusion, failed") || strings.Contains(out, "refused_by_exclusion,503,"+why+",failed") {
			t.Fatalf("%s export:\n%s", format, out)
		}
	}
	base := &Step{Outcome: OutcomeReady}
	if c := Compare(&st, base, "run-0", Thresholds{LoadPct: 20, DecodePct: 10}); c.Regression {
		t.Fatalf("a step refused by an exclusion is a regression: %+v", c)
	}
	tm.evMu.Lock()
	defer tm.evMu.Unlock()
	seen := false
	for _, e := range tm.hist {
		if e.Type == "benchStepFinished" && e.Data["benchmark"] == "x" {
			seen = true
			if e.Severity != "info" || e.Data["result"] != ResultNotJudged || e.Data["pass"] != nil || !strings.Contains(e.Summary, "not judged") ||
				strings.Contains(e.Summary, "expectations not met") {
				t.Fatalf("history event %+v", e)
			}
		}
	}
	if !seen {
		t.Fatal("no step event")
	}
}

// 0.1.7 review, ES3: an automatic profile that finds no room because nodes
// are excluded is refused with an ordinary reason (the scheduler keeps
// "node excluded" for a profile that forces an excluded node). Such a step
// is an ordinary refusal: judged, a failure, in the pass rate.
func TestAutomaticStepShortOfRoomIsAnOrdinaryRefusal(t *testing.T) {
	why := "node ps5-a is excluded by config; 1 node excluded by config"
	api := newFake().prof("auto", fakeProf{loadCode: 503, loadReason: "cluster busy", loadMsg: why}).prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"auto","profile":"auto","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["auto","gemma"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	st := r.Steps[0]
	if st.Outcome != OutcomeRefused || st.Expect == nil || st.Expect.Pass || StepResult(&st) != ResultFail {
		t.Fatalf("%+v", st)
	}
	if s := r.Summary; s.Failed != 1 || s.Passed != 1 || s.RefusedByExclusion != 0 || s.Ran != 2 {
		t.Fatalf("summary %+v", s)
	}
}

// 0.1.7 review, ES2: steps refused by an exclusion stay out of a benchmark's
// and a suite's pass rate, and out of the trends.
func TestRefusedByExclusionLeavesThePassRates(t *testing.T) {
	s, _ := openTemp(t)
	addRun(t, s, "run-a", "nightly", 1000, nil, RunCompleted, fakeStep{bench: "f", profile: "f", pass: true, loadS: 10})
	addRun(t, s, "run-b", "nightly", 2000, nil, RunCompleted, fakeStep{bench: "f", profile: "f", outcome: OutcomeFailed})
	for _, id := range []string{"run-c", "run-d"} {
		r := newRun(id, "nightly", 3000)
		r.State = RunCompleted
		if err := s.CreateRun(r); err != nil {
			t.Fatal(err)
		}
		st := &Step{RunID: id, BenchmarkID: "f", Name: "f", Profile: "f", State: StepDone, Outcome: OutcomeRefusedByExclusion,
			Expect: &ExpectResult{NotJudged: true}}
		if err := s.AddStep(st); err != nil {
			t.Fatal(err)
		}
		s.SetRunSummary(id, &RunSummary{Steps: 1, RefusedByExclusion: 1}, "")
		s.SetRunState(id, RunCompleted, 3000, 4000, "")
	}
	doc, err := s.Aggregate(AggFilter{}, th, time.UnixMilli(10_000))
	if err != nil {
		t.Fatal(err)
	}
	b := doc.Benchmarks[0]
	if b.Outcomes[OutcomeRefusedByExclusion] != 2 || !close2(b.PassRate, 0.5) {
		t.Fatalf("benchmark %+v", b)
	}
	if len(doc.Suites) != 1 || !close2(doc.Suites[0].PassRate, 0.5) {
		t.Fatalf("suite %+v", doc.Suites)
	}
	pts, err := s.Trend("f", "load_s", 10)
	if err != nil || len(pts) != 1 {
		t.Fatalf("trend %+v %v", pts, err)
	}
}
