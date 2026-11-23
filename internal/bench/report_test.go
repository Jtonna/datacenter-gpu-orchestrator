package bench

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Reports are made from the run and its measurements: the summary of a group
// run (headline, issues first, a table per metric, the index), the benchmark
// report of a step (inputs, a section per phase) and a section. The same data
// gives the same report.
func TestReports(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.consoles = consoles()
	tm := newTestMgr(t, api)
	tm.bench(coldLoadRecipe)
	tm.suite(`{"id":"g","cooldown_s":0,"only_when_idle":false,"benchmarks":[
		{"benchmark":"cold-load","inputs":{"model":"a.gguf"},"matrix":{"placement":["any:2","nodes:ps5-3"]}},
		{"benchmark":"cold-load","inputs":{"model":"a.gguf","placement":"any:2"}}]}`)
	r := tm.finished(tm.run("g").ID)
	ms, err := tm.store.RunMeasurements(r.ID, MeasureQuery{})
	if err != nil {
		t.Fatal(err)
	}
	tr := map[int]bool{0: true, 2: true}

	sum, err := BuildReport(r, ms, ReportOptions{Transcripts: tr})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Kind != ReportSummary || sum.Headline == nil || sum.Headline.Steps != 3 || sum.Headline.Completed != 2 || sum.Headline.Refused != 1 || sum.Headline.Ran != 3 {
		t.Fatalf("headline %+v", sum.Headline)
	}
	if len(sum.Issues) != 1 || sum.Issues[0].Kind != IssuePlacementRefused || !strings.Contains(sum.Issues[0].Text, `console "ps5-3" cannot take a load now (paused)`) {
		t.Fatalf("issues %+v", sum.Issues)
	}
	if len(sum.Index) != 3 || sum.Index[1].Outcome != OutcomeRefused || sum.Index[2].Report != "/api/bench/runs/"+r.ID+"/report?step=2" {
		t.Fatalf("index %+v", sum.Index)
	}
	var load *SummaryTable
	for i := range sum.Tables {
		if sum.Tables[i].Metric == "load_s" {
			load = &sum.Tables[i]
		}
	}
	// Steps 0 and 2 are the same use of the recipe (same inputs): one row, two values.
	if load == nil || len(load.Rows) != 1 || load.Rows[0].N != 2 || load.Rows[0].Placement != "any:2" || load.Rows[0].Model != "a.gguf" {
		t.Fatalf("load_s table %+v", load)
	}
	again, _ := BuildReport(r, ms, ReportOptions{Transcripts: tr})
	a, _ := json.Marshal(sum)
	b, _ := json.Marshal(again)
	if string(a) != string(b) || ReportMarkdown(sum) != ReportMarkdown(again) {
		t.Fatal("the same data gave another report")
	}
	md := ReportMarkdown(sum)
	for _, want := range []string{"# Summary: g", "## Headline", "3 steps. Ran 3: 2 completed, 0 failed, 1 refused.", "## Issues", "the placement was refused", "### load_s (s)",
		"## Benchmark reports", "[report](/api/bench/runs/" + r.ID + "/report?step=1)"} {
		if !strings.Contains(md, want) {
			t.Errorf("summary markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Index(md, "## Issues") > strings.Index(md, "## Measurements") {
		t.Error("the issues come first")
	}

	zero := 0
	br, err := BuildReport(r, ms, ReportOptions{Step: &zero, Transcripts: tr})
	if err != nil {
		t.Fatal(err)
	}
	bm := br.Benchmark
	if br.Kind != ReportBenchmark || bm == nil || bm.Recipe != "cold-load" || bm.Inputs == nil || bm.Inputs.Ctx != 8192 || len(bm.Sections) != 3 || bm.Transcript == "" {
		t.Fatalf("benchmark report %+v", bm)
	}
	load0, req0 := bm.Sections[0], bm.Sections[1]
	if load0.Title != "Load" || load0.Transcript != "" || len(load0.Figures) == 0 || req0.Transcript != "/api/bench/runs/"+r.ID+"/steps/0/transcript?format=md#phase-1" {
		t.Fatalf("sections %+v %+v", load0, req0)
	}
	bmd := ReportMarkdown(br)
	for _, want := range []string{"# Cold load (a.gguf, 8k, any:2)", "| Recipe | `cold-load` |", "| Context | 8192 |", "| Placement | any:2 |", "## 1. Load", "## 2. Requests"} {
		if !strings.Contains(bmd, want) {
			t.Errorf("benchmark markdown lacks %q:\n%s", want, bmd)
		}
	}

	two, one := 2, 1
	sec, err := BuildReport(r, ms, ReportOptions{Step: &two, Phase: &one})
	if err != nil || sec.Kind != ReportSection || sec.Section.Idx != 1 {
		t.Fatalf("%v %+v", err, sec)
	}
	for _, rp := range []*Report{sum, br, sec} {
		page, err := ReportHTML(rp, "/x?format=md")
		if err != nil || !strings.Contains(string(page), "Download as PDF") || !strings.Contains(string(page), `href="/x?format=md"`) || !strings.Contains(string(page), "@media print") {
			t.Fatalf("html %s: %v %s", rp.Kind, err, page)
		}
	}
	page, _ := ReportHTML(sum, "/x")
	if !strings.Contains(string(page), "report?format=html&step=1") || !strings.Contains(string(page), "the placement was refused") {
		t.Fatalf("summary html: %s", page)
	}
	page, _ = ReportHTML(br, "/x")
	if !strings.Contains(string(page), "<th>Placement</th><td>any:2</td>") || !strings.Contains(string(page), `id="phase-1"`) {
		t.Fatalf("benchmark html: %s", page)
	}
	nine := 9
	if _, err := BuildReport(r, ms, ReportOptions{Step: &nine}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no step: %v", err)
	}

	// A direct run's own report is the benchmark report of its step.
	no := false
	q, err := tm.m.EnqueueBenchmark("cold-load", BenchRunOptions{OnlyWhenIdle: &no, Inputs: nil})
	if err == nil {
		t.Fatalf("a recipe without a model ran: %+v", q)
	}
	tm.bench(`{"id":"plain","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	q, err = tm.m.EnqueueBenchmark("plain", BenchRunOptions{OnlyWhenIdle: &no})
	if err != nil {
		t.Fatal(err)
	}
	d := tm.finished(q.ID)
	dms, _ := tm.store.RunMeasurements(d.ID, MeasureQuery{})
	dr, err := BuildReport(d, dms, ReportOptions{})
	if err != nil || dr.Kind != ReportBenchmark || dr.Benchmark.BenchmarkID != "plain" || len(dr.Issues) != 0 {
		t.Fatalf("%v %+v", err, dr)
	}
}

func TestIssuesAreFacts(t *testing.T) {
	r := &Run{ID: "r", State: RunFailed, Error: "failed (interrupted)", Steps: []Step{
		{Idx: 0, Name: "A", Outcome: OutcomeFailed, Error: "timed out after 10m0s (timeout_s)"},
		{Idx: 1, Name: "B", Outcome: OutcomeError, Error: "interrupted"},
		{Idx: 2, Name: "C", Outcome: OutcomeReady, Phases: []PhaseResult{{Idx: 0, Type: "tasks", State: PhaseDone, Tasks: &TasksResult{Failed: 1, NotSent: 2}}},
			Requests: []RequestResult{{PhaseIdx: 0, Name: "q", Status: 500, Error: "boom"}}},
		{Idx: 3, Name: "D", State: StepSkipped, Outcome: OutcomeSkipped},
	}}
	rep, err := BuildReport(r, nil, ReportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, i := range rep.Issues {
		kinds = append(kinds, i.Kind)
	}
	if strings.Join(kinds, ",") != "interrupted,timed_out,interrupted,request_failed,tasks_failed" {
		t.Fatalf("%v", kinds)
	}
	if h := rep.Headline; h.Ran != 3 || h.Failed != 2 || h.Completed != 1 || h.Skipped != 1 {
		t.Fatalf("%+v", h)
	}
	if !strings.Contains(rep.Issues[4].Text, "1 task(s) failed, 0 timed out, 2 not sent") {
		t.Fatal(rep.Issues[4].Text)
	}
}
