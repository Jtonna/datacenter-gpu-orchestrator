package api

import (
	"testing"
)

// A cancelled run is a neutral grey dash with a neutral count wherever a run's
// result is shown, never the red cross or the red dot (the reviewer, build-92):
// the Benchmarks list (a benchmark's last use, a group's last run), the
// Overview's recent runs and its results per model, the model's side panel
// (its runs) and the model's page (its Benchmarks table). One helper draws
// them all (views/bench/common.js resultIcon, outcomeResult, resultTone).
// Failed runs stay red beside them.
func TestUINextBenchCancelledIsGreyEverywhere(t *testing.T) {
	f := benchRunCancel(t, "runcancel")
	st := f["/api/status"]

	// a benchmark whose last use was cancelled (run 51 cancelled it at its one step)
	bms := mcpCopy(t, f["/api/bench/benchmarks"])
	found := false
	for _, x := range bms["benchmarks"].([]any) {
		b := x.(map[string]any)
		if b["id"] == "qwen4b-2con" {
			b["last_use"] = map[string]any{"run_id": "run-51", "run_number": 51, "step": 0, "at": 1795772847086, "outcome": "cancelled", "duration_s": 3}
			found = true
		}
	}
	if !found {
		t.Fatal("the runcancel fixture has no benchmark qwen4b-2con")
	}
	// a group whose last run was cancelled part way
	grs := mcpCopy(t, f["/api/bench/groups"])
	found = false
	for _, x := range grs["groups"].([]any) {
		g := x.(map[string]any)
		if g["id"] == "full-pass" {
			lr := g["last_run"].(map[string]any)
			lr["state"], lr["result"] = "cancelled", "cancelled"
			lr["counts"] = map[string]any{"steps": 96, "done": 96, "to_go": 0, "completed": 10, "passed": 10, "with_issues": 0, "failed": 0, "refused": 0, "cancelled": 1, "skipped": 85}
			found = true
		}
	}
	if !found {
		t.Fatal("the runcancel fixture has no group full-pass")
	}
	// the results per model: a model whose latest step was cancelled
	res := mcpCopy(t, f["/api/bench/results/models"])
	found = false
	for _, x := range res["models"].([]any) {
		m := x.(map[string]any)
		if m["model"] == "mixtral-8x7b-q4_k_m.gguf" {
			if l, ok := m["latest"].(map[string]any); ok && l["outcome"] == "failed" {
				l["outcome"] = "cancelled"
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the runcancel fixture has no failed latest result for mixtral-8x7b-q4_k_m.gguf")
	}
	g := withAnswer(withAnswer(withAnswer(f, "/api/bench/benchmarks", bms), "/api/bench/groups", grs), "/api/bench/results/models", res)
	// a step report: the phase the cancel stopped is not a failed phase; a failed one stays red
	r51 := mcpCopy(t, f["/api/bench/runs/51"])
	step := r51["steps"].([]any)[0].(map[string]any)
	step["phases"] = []any{
		map[string]any{"idx": 0, "type": "load", "outcome": "ok", "state": "done", "duration_s": 2},
		map[string]any{"idx": 1, "type": "requests", "outcome": "cancelled", "state": "done", "duration_s": 1},
	}
	r51f := mcpCopy(t, r51)
	r51f["steps"].([]any)[0].(map[string]any)["phases"].([]any)[1].(map[string]any)["outcome"] = "failed"
	g = withAnswer(withAnswer(g, "/api/bench/runs/51", r51), "/api/bench/runs/run-51", r51)
	gf := withAnswer(withAnswer(g, "/api/bench/runs/51", r51f), "/api/bench/runs/run-51", r51f)

	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": g, "cases": []any{
		map[string]any{"name": "list: a benchmark's cancelled last use is grey, its count neutral", "route": "#/benchmarks/all", "status": st,
			"has": []string{"div.bp-row[data-bench=qwen4b-2con] .bp-last .bp-ri[data-result=cancelled] [stroke*=cancelled]",
				"div.bp-row[data-bench=qwen4b-2con] .bp-dot.notrun[data-result=cancelled][title*=run]",
				// a failed last use stays red
				"div.bp-row[data-bench=cold-load] .bp-last .bp-ri[data-result=failed]", "div.bp-row[data-bench=cold-load] .bp-dot.bad"},
			"none": []string{"div.bp-row[data-bench=qwen4b-2con] [fill*=failed]", "div.bp-row[data-bench=qwen4b-2con] .bp-dot.bad"}},
		map[string]any{"name": "list: a group's cancelled last run is grey, its counts neutral", "route": "#/benchmarks/all", "status": st,
			"has": []string{"a.bp-row[data-group=full-pass] .bp-last .bp-ri[data-result=cancelled] [stroke*=cancelled]", "a.bp-row[data-group=full-pass] .bp-dot.notrun",
				// a run with a failure stays red
				"a.bp-row[data-group=all-load] .bp-dot.bad"},
			"none": []string{"a.bp-row[data-group=full-pass] [fill*=failed]", "a.bp-row[data-group=full-pass] .bp-dot.bad"}},
		map[string]any{"name": "overview: a cancelled recent run is grey", "route": "#/benchmarks", "status": st,
			"has":  []string{"a.bp-recent[data-run='51'] .bp-ri[data-result=cancelled] [stroke*=cancelled]", "a.bp-recent[data-run='51'] .bp-dot.notrun"},
			"none": []string{"a.bp-recent[data-run='51'] [fill*=failed]", "a.bp-recent[data-run='51'] .bp-dot.bad"}},
		map[string]any{"name": "overview: a cancelled latest result is a neutral word", "route": "#/benchmarks", "status": st,
			"has":  []string{"section.bp-latest tr[data-model='mixtral-8x7b-q4_k_m.gguf'] a.bp-out.notrun[data-outcome=cancelled]"},
			"none": []string{"section.bp-latest tr[data-model='mixtral-8x7b-q4_k_m.gguf'] a.bp-out.bad"}},
		map[string]any{"name": "report: a cancelled phase is not red", "route": "#/benchmarks/runs/51/0", "status": st,
			"pageHas": []string{"li.bp-step[data-phase='1']"}, "pageNone": []string{"li.bp-step.bad"}},
		map[string]any{"name": "report: a failed phase stays red", "route": "#/benchmarks/runs/51/0", "status": st, "fetch": gf,
			"pageHas": []string{"li.bp-step.bad[data-phase='1']"}},
	}})

	// the model's side panel (its runs) and the model's page (its Benchmarks table)
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	mst := fetch["/api/status"]
	fetch["/api/bench/runs"] = map[string]any{"runs": []any{
		map[string]any{"id": "run-c1", "kind": "benchmark", "benchmark_id": "zone-01-cold", "state": "cancelled", "trigger": "manual",
			"queued_at": 1764061200000, "started_at": 1764061200000, "finished_at": 1764061300000, "summary": map[string]any{"steps": 3, "ran": 1, "passed": 1, "failed": 0}},
		map[string]any{"id": "run-f1", "kind": "benchmark", "benchmark_id": "zone-01-warm", "state": "failed", "trigger": "manual",
			"queued_at": 1763974800000, "started_at": 1763974800000, "finished_at": 1763975100000, "summary": map[string]any{"steps": 3, "ran": 3, "passed": 2, "failed": 1}},
	}}
	var entry map[string]any
	for _, x := range mcpCopy(t, fetch["/api/models"])["models"].([]any) {
		if m := x.(map[string]any); m["key"] == mdlLoaded {
			entry = m
		}
	}
	if entry == nil {
		t.Fatal("no catalog entry for " + mdlLoaded)
	}
	entry["bench"] = map[string]any{"benchmarks": 2, "runs": 3, "reports": 2, "detail": []any{
		map[string]any{"benchmark_id": "test-cold", "name": "Test cold", "profile": "gemma-4-27b", "runs": 2, "pass_rate": 1,
			"recent":      []any{map[string]any{"run_id": "run-p1", "at": 1763974800000, "result": "pass"}, map[string]any{"run_id": "run-c1", "at": 1764061200000, "result": "cancelled"}},
			"last_run_id": "run-c1", "last_at": 1764061200000},
		map[string]any{"benchmark_id": "test-warm", "name": "Test warm", "profile": "gemma-4-27b", "runs": 1, "pass_rate": 0,
			"recent":      []any{map[string]any{"run_id": "run-f1", "at": 1763974800000, "result": "fail"}},
			"last_run_id": "run-f1", "last_at": 1763974800000},
	}}
	fetch["/api/models/"+mdlLoaded] = entry
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "model panel: a cancelled run is grey", "route": "#/models?model=" + mdlLoaded, "status": mst, "width": 1536,
			"click": []string{"aside.side button[data-act=bench]"},
			"pageHas": []string{"aside.side a.mdl-run[href='#/benchmarks/runs/run-c1'] .bp-ri[data-result=cancelled] [stroke*=cancelled]",
				// a failed run stays red
				"aside.side a.mdl-run[href='#/benchmarks/runs/run-f1'] .bp-ri[data-result=failed]"},
			"pageNone": []string{"aside.side a.mdl-run[href='#/benchmarks/runs/run-c1'] [fill*=failed]"}},
		map[string]any{"name": "model page: a cancelled run's mark is neutral", "route": "#/models/" + mdlLoaded, "status": mst,
			"expect": []string{"Test cold", "Test warm"},
			"count":  map[string]any{"a.pd-cancelled": 1, "a.pd-fail": 1, "a.pd-pass": 1}},
	}})
}
