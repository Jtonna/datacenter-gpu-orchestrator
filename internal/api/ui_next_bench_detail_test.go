package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The benchmark pages (25 Nov; boards Bench-Overview, Bench, Bench-Group,
// Bench-Runs, Bench-Runs-Group, Bench-Report) in the new shell, drawn from the
// fixture's answers in the server's shapes (scripts/ui-fixture/bench.mjs:
// --bench boards for the boards' content, --bench long for long texts and a
// failed run). The layout (no sideways scroll at 1536 and 1280, no squeezed
// long text) is measured in Chrome on the same fixture: the pictures under
// potluck-notes/ui-audit/compare/phase8/bench-pages/.

const (
	benchSuite = "cold-warm-consoles"
	benchID    = "qwen4b-2con"
)

// The checkbox rule is scoped to inputs; the card body keeps no width of its own.
func TestUINextCardBodyIsNotACheckbox(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	if m := regexp.MustCompile(`(^|[\s},])\.cb\s*\{[^}]*\bwidth\s*:`).FindString(css); m != "" {
		t.Errorf("app.css gives every .cb (the card body too) a width: %q", m)
	}
	if !regexp.MustCompile(`input\.cb\s*\{[^}]*width:16px`).MatchString(css) {
		t.Error("app.css: the 16 px checkbox rule is not on input.cb")
	}
	// the benchmark tables scroll inside their own box, never the page
	if !regexp.MustCompile(`\.bp-tablewrap\{[^}]*overflow-x:auto`).MatchString(css) {
		t.Error("app.css: a benchmark table does not scroll inside its card")
	}
}

// benchBoards is the fixture's --bench boards answers, the runs list by query.
func benchBoards(t *testing.T) map[string]any {
	t.Helper()
	q := func(s string) string { return "/api/bench/runs?" + s }
	runsQ := []string{q("suite=all-load&limit=5"), q("suite=full-pass&limit=5"), q("suite=planned-long-context&limit=5"), q("limit=4"), q("limit=1"),
		q("limit=12&offset=0"), q("limit=12&offset=12"), q("limit=12&offset=0&kind=benchmark"), q("limit=12&offset=0&result=has_failed"), q("suite=every-model&limit=5")}
	paths := append([]string{"/api/status", "/api/models", "/api/profiles", "/api/bench/state", "/api/bench/groups", "/api/bench/benchmarks",
		"/api/bench/groups/all-load/expand", "/api/bench/groups/full-pass/expand", "/api/bench/groups/planned-long-context/expand",
		"/api/bench/runs/46", "/api/bench/runs/47", "/api/bench/runs/43", "/api/bench/runs/run-47/report", "/api/bench/results/models", "/api/bench/runs/49",
		"/api/bench/results/models?run=47", "/api/bench/results/models?ctx=4096", "/api/bench/groups/every-model/expand"}, runsQ...)
	f := fixtureAnswers(t, []string{"--bench", "boards"}, paths...)
	// GET /api/bench/runs answers by its query (the harness ignores a query otherwise)
	f["/api/bench/runs"] = map[string]any{"$match": []any{
		[]any{`suite=all-load`, f[runsQ[0]]}, []any{`suite=full-pass`, f[runsQ[1]]}, []any{`suite=planned-long-context`, f[runsQ[2]]}, []any{`suite=every-model`, f[runsQ[9]]},
		[]any{`kind=benchmark`, f[runsQ[7]]}, []any{`result=has_failed`, f[runsQ[8]]}, []any{`offset=12`, f[runsQ[6]]},
		[]any{`limit=4\b`, f[runsQ[3]]}, []any{`limit=1\b`, f[runsQ[4]]}, []any{`.`, f[runsQ[5]]}}}
	f["/api/bench/runs/run-47"] = f["/api/bench/runs/47"]
	f["/api/bench/runs/run-49"] = f["/api/bench/runs/49"]
	// the results route answers by its query (the From and Context filters)
	f["/api/bench/results/models"] = map[string]any{"$match": []any{
		[]any{`run=47`, f["/api/bench/results/models?run=47"]}, []any{`ctx=4096`, f["/api/bench/results/models?ctx=4096"]}, []any{`.`, f["/api/bench/results/models"]}}}
	f["/api/bench/runs/run-46"] = f["/api/bench/runs/46"]
	f["PUT /api/bench/runs/run-46/pin"] = map[string]any{"id": "run-46", "pinned": true}
	f["DELETE /api/bench/runs/run-46/pin"] = map[string]any{"id": "run-46", "pinned": false}
	f["POST /api/bench/groups"] = map[string]any{"id": "all-load-copy"}
	return f
}

// The group page first (#/benchmarks/all/<group>, board Bench-Group): the
// list narrows beside the group's panel; the panel has every element of the
// board, the ones without data drawn greyed with their reason.
func TestUINextBenchGroupPage(t *testing.T) {
	f := benchBoards(t)
	st := f["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "group page: the list beside the panel", "route": "#/benchmarks/all/all-load", "status": st, "width": 1536,
			"expect": []string{"Running now · 1", "Full pass", "10 of 96", "Groups", "All models: load and response", "Benchmarks", "New benchmark",
				"Load cold · Warm-up · Requests · Unload · Reload · Requests · Unload", "Load cold · Unload", "Never run", "planned: waits for the 128k profiles"},
			"absent": []string{"New group"},
			"has":    []string{"div.bp-list.narrow", "a.bp-row.sel[data-group=all-load][aria-current=true]"},
			"page": []string{"Group", "Run group", "Edit", "Duplicate", "Contains", "24 runs", "Cold load, response and repeat", "12 models", "2 placements",
				"Smallest that fits", "placed by the scheduler, and all", "context 8,192", "carries on after a failure", "waits until idle", "cold loads",
				"Runs", "22", "Compare two", "All 22 runs ›", "cold: caches dropped and verified on every load (24 loads)"},
			"pageHas": []string{"aside.side .bp-run[data-run='46'] button.bp-pin.on[aria-pressed=true]", "aside.side a.bp-all[href='#/benchmarks/runs?suite=all-load']",
				"aside.side [data-notyet=b7]", "aside.side [data-notyet=b10]", "aside.side [data-notyet=edit]", "aside.side [data-notyet=run]",
				"aside.side .bp-entry .bp-opts .bp-notyet[data-notyet=b6]"},
			"titles":    []string{`Not available yet: a group cannot ask for "the smallest that fits" yet: the placements are auto, all, any N or named zones (benchmarks slice 7: B6)`},
			"pageCount": map[string]any{"aside.side .bp-run": 5},
			// the list draws the run itself: no run card in the strip above the page (board Bench-Group)
			"pageNone": []string{".topslots a.run-ind", ".topslots .run-ind"}},
		map[string]any{"name": "group page: 24 entries, all inside the panel's scrolling body", "route": "#/benchmarks/all/every-model", "status": st, "width": 1536,
			"page":      []string{"Every model, one at a time", "Contains", "24 runs", "mistral-small-24b"},
			"pageHas":   []string{"aside.side .side-body .bp-entries .bp-entry[data-entry='23']", "aside.side .side-body .bp-entry[data-entry='0']"},
			"pageCount": map[string]any{"aside.side .side-body .bp-entry": 24, "aside.side .side-body .bp-entry .bp-notyet[data-notyet=b6]": 24}},
		map[string]any{"name": "group page: a group of groups", "route": "#/benchmarks/all/full-pass", "status": st,
			"page": []string{"Full pass", "Contains", "96 runs", "All models: load and response", "All models: coding", "Running now"}},
		map[string]any{"name": "group page: a planned group says why it cannot run", "route": "#/benchmarks/all/planned-long-context", "status": st,
			"page": []string{"This group is planned: waits for the 128k profiles of the four smallest models. It cannot run until that is settled.", "This group has not run yet."}},
		map[string]any{"name": "group page: an unknown group", "route": "#/benchmarks/all/nope", "status": st, "page": []string{"There is no such group"}},
		map[string]any{"name": "group page: closing the panel goes back to the list", "route": "#/benchmarks/all/all-load", "status": st, "keys": []string{"Escape"},
			"hash": "#/benchmarks/all", "expect": []string{"New group"}, "pageNone": []string{"aside.side"}},
		map[string]any{"name": "group page: Duplicate asks a new id and a comment, then posts the copy", "route": "#/benchmarks/all/all-load", "status": st,
			"click": []any{"aside.side button[data-act=duplicate]", map[string]any{"type": "div.dlg input[name=id]", "value": "all-load-copy"},
				map[string]any{"type": "div.dlg textarea.dlg-comment", "value": "try a third placement"}, "div.dlg .dlg-foot button.pri"},
			"fetchedQuery": []string{`^POST /api/bench/groups \{"id":"all-load-copy","name":"All models: load and response \(copy\)",.*"benchmarks":\[\{"benchmark":"cold-response-repeat"`},
			"sentHeader":   []string{"^X-Comment: try a third placement$"}, "hash": "#/benchmarks/all/all-load-copy"},
		map[string]any{"name": "group page: the pin asks why, then unpins", "route": "#/benchmarks/all/all-load", "status": st,
			"click":        []any{"aside.side .bp-run[data-run='46'] button.bp-pin", map[string]any{"type": "div.dlg textarea.dlg-comment", "value": "not needed"}, "div.dlg .dlg-foot button.pri"},
			"fetchedQuery": []string{`^DELETE /api/bench/runs/run-46/pin`}, "sentHeader": []string{"^X-Comment: not needed$"}},
		map[string]any{"name": "the list: every group and benchmark, the last runs", "route": "#/benchmarks/all", "status": st,
			"expect": []string{"New group", "New benchmark", "Gemma 4 12B: everything", "4 benchmarks · any 2 zones", "1 benchmark × 12 models × 2 placements · 24 runs",
				"2 groups · 96 runs", "1 benchmark × 12 models · 72 tasks", "Running now", "Fill the context", "Load · Conversation to 90% · Unload", "Eight at once", "Load · 8 at once · Unload", "Coding tasks", "Load · Tasks · Unload"},
			"has": []string{"div.bp-row[data-bench=cold-load] a[href='#/benchmarks/runs/43/0']", "a.bp-row[data-group=full-pass] .bp-now"}},
		map[string]any{"name": "the list: the search", "route": "#/benchmarks/all", "status": st, "click": []any{map[string]any{"type": "label.sx input", "value": "gemma"}},
			"expect": []string{"Gemma 4 12B: everything", "No benchmark matches the search."}, "none": []string{"a.bp-row[data-group=full-pass]", "a.bp-row[data-group=all-load]"}},
		map[string]any{"name": "the menu: Overview, Benchmarks, Runs, Settings with the counts", "route": "#/benchmarks/all", "status": st,
			"pageHas":   []string{"a.ni[href='#/benchmarks']", "a.ni.on[href='#/benchmarks/all'] span.nb.cnt", "a.ni[href='#/benchmarks/runs'] span.nb.cnt", "a.ni[href='#/settings/bench']"},
			"notOnPage": []string{"Aggregates"}, "page": []string{"11", "49"}},
	}})
}

// The Overview, the runs, a run's panel and a benchmark's report.
func TestUINextBenchPages(t *testing.T) {
	f := benchBoards(t)
	st := f["/api/status"]
	slug := "cold-response-repeat-qwen2.5-72b-instruct-q2_k-all"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "overview", "route": "#/benchmarks", "status": st,
			"expect": []string{"Full pass", "10 of 96", "Requests, step 3 of 7", "about 6 h left", "Run a benchmark", "Latest result for each model", "Smallest fit", "Cold load",
				"Reload", "Generation, tok/s", "First token", "Coding, of 102", "Recent runs", "All 49", "Gemma 4 12B: everything", "Cold load, response and repeat · qwen2.5-72b-instruct-q2_k · all"},
			"has":  []string{"div.bp-recent[data-run], a.bp-recent[data-run='48']", "section.bp-latest [data-notyet=coding]"},
			"page": []string{"about 6 h left"}, "pageHas": []string{"a.run-ind"}},
		map[string]any{"name": "runs", "route": "#/benchmarks/runs", "status": st,
			"expect": []string{"RUNS", "Load times", "Generation speed", "First token", "Memory", "Coding", "Started", "Run", "Result", "Took", "1–12 of 49",
				"Full pass", "9 passed", "87 to go", "All models: load and response · cancelled", "Cold load · mixtral-8x7b-q4_k_m · any 4 zones"},
			"has": []string{"tr[data-run='46'] button.bp-pin.on", "tr.direct[data-run='44'] td.bp-name .bp-sub", "tr.direct[data-run='44'] td.bp-name .bp-kind", "tr[data-run='46'] td.bp-start [data-result=with_issues]", "a.bp-pg.on[aria-current=page]", "select.fs[aria-label=Kind]", "select.fs[aria-label=Result]",
				"select.fs[aria-label=Model]", "[data-notyet=coding]", "a.bp-view[data-view=load-times][href='#/benchmarks/runs?show=load-times']"},
			"count": map[string]any{"table.bp-runs tbody tr": 12}},
		// the results per model (GET /api/bench/results/models): figures at the
		// smallest fit, a failed load said in the first figure column, a figure
		// not measured is a dash with its hover, Coding greyed (slice 4)
		map[string]any{"name": "overview: the latest result for each model", "route": "#/benchmarks", "status": st,
			"expect": []string{"Smallest fit · context 8,192", "qwen3.5-4b-q4_k_m", "1 zone", "26.8 s", "2.0 s", "96.2", "0.41 s", "qwen2.5-72b-instruct-q2_k", "4 zones", "72.2 s", "40.4 s",
				"8.2", "1.51 s", "mixtral-8x7b-q4_k_m", "failed", "llama-3.3-8b-q4_k_m", "6 zones"},
			"has": []string{"section.bp-latest tr[data-model='mixtral-8x7b-q4_k_m.gguf'] a.bp-out[data-outcome=failed][href='#/benchmarks/runs/run-46/16']",
				"section.bp-latest tr[data-model='phi-4-14b-q4_k_m.gguf'] [data-missing=ttft_ms][title*=measured]",
				"section.bp-latest tr[data-model='llama-3.3-8b-q4_k_m.gguf'] [data-missing=reload_s]",
				"section.bp-latest tr[data-model='qwen3.5-4b-q4_k_m.gguf'] a.bp-fv[href='#/benchmarks/runs/run-46/0']"},
			"count": map[string]any{"section.bp-latest tbody tr": 12, "section.bp-latest [data-notyet=coding]": 12},
			// a model file no longer in the models folder: said so, listed last
			// (reviewer's ruling B: "file no longer present", or "in deleted models"; never failed)
			"order": []string{"llama-3.3-8b-q4_k_m", "mixtral-8x7b-instruct-v0.1.Q4_K_M", "file no longer present", "phi-4-14b-q4_k_m", "in deleted models"}},
		map[string]any{"name": "overview: a model whose file is gone", "route": "#/benchmarks", "status": st,
			"has": []string{"section.bp-latest tr.gone[data-model='mixtral-8x7b-instruct-v0.1.Q4_K_M.gguf'] [data-gone=missing]",
				"section.bp-latest tr.gone[data-model='phi-4-14b-q4_k_m.gguf'] a[data-gone=trash][href='#/models/deleted?item=a1b2c3d4e5f6']"},
			"none":  []string{"section.bp-latest tr.gone [data-outcome]"},
			"count": map[string]any{"section.bp-latest [data-gone]": 2}},
		// reviewer's ruling A: each column from the newest step that measured
		// it; the hover names the run and date; "several runs" on a mixed row
		map[string]any{"name": "overview: columns from different runs", "route": "#/benchmarks", "status": st,
			"expect": []string{"29.4", "several runs"},
			"has": []string{"section.bp-latest tr[data-model='gemma-4-12b-q4_k_m.gguf'] [data-mixed='2']",
				"section.bp-latest tr[data-model='gemma-4-12b-q4_k_m.gguf'] a.bp-fv[data-run=run-47][href='#/benchmarks/runs/run-47/0']",
				"section.bp-latest tr[data-model='gemma-4-12b-q4_k_m.gguf'] a.bp-fv[data-run=run-46]"},
			"titles": []string{"run 47, ", "run 46, ", "come from several runs"},
			"count":  map[string]any{"section.bp-latest [data-mixed]": 1}, "none": []string{"section.bp-latest [data-notyet=b5]"}},
		map[string]any{"name": "runs: a view marks a row whose columns come from different runs", "route": "#/benchmarks/runs?show=generation", "status": st,
			"has": []string{"table.bp-res-t tr[data-model='gemma-4-12b-q4_k_m.gguf'] [data-mixed]"}, "count": map[string]any{"table.bp-res-t [data-mixed]": 1}},
		map[string]any{"name": "runs: a view of one figure from one run has no mark", "route": "#/benchmarks/runs?show=load-times", "status": st,
			"none": []string{"table.bp-res-t [data-mixed]"}},
		map[string]any{"name": "runs: the Load times view", "route": "#/benchmarks/runs?show=load-times", "status": st,
			"expect": []string{"LOAD TIMES", "Model", "Smallest fit", "Cold, smallest", "Cold, all 6", "qwen3.5-4b-q4_k_m", "26.8 s", "11.3 s", "mixtral-8x7b-q4_k_m", "failed", "not run"},
			"has":    []string{"table.bp-res-t[data-view=load-times]", "a.bp-view[data-view=runs]", "[data-notyet=coding]"},
			"none":   []string{"select.fs[aria-label=Kind]", "div.bp-pager"}, "count": map[string]any{"table.bp-res-t tbody tr": 12}},
		// board Bench-Runs-Load: the Reload columns and the From and Context filters
		map[string]any{"name": "load times: the Reload columns", "route": "#/benchmarks/runs?show=load-times", "status": st,
			"expect": []string{"Reload, smallest", "Reload, all 6", "2.0 s", "From: latest run of each", "Context: any", "Context: 8,192", "Context: 4,096"},
			"has":   []string{"select.fs[aria-label=From]", "select.fs[aria-label=Context]", "tr[data-model='qwen3.5-4b-q4_k_m.gguf'] td[data-fig='reload_s:s'] a.bp-fv"},
			"order": []string{"Cold, smallest", "Cold, all 6", "Reload, smallest", "Reload, all 6"}},
		map[string]any{"name": "load times: From one run", "route": "#/benchmarks/runs?show=load-times&from=47", "status": st,
			"fetchedQuery": []string{`^GET /api/bench/results/models\?run=47$`}, "count": map[string]any{"table.bp-res-t tbody tr": 1}},
		map[string]any{"name": "load times: pick a run in From", "route": "#/benchmarks/runs?show=load-times", "status": st,
			"click": []any{map[string]any{"type": "select.fs[aria-label=From]", "value": "47"}}, "hash": "#/benchmarks/runs?show=load-times&from=47"},
		map[string]any{"name": "load times: a context with no results", "route": "#/benchmarks/runs?show=load-times&ctx=4096", "status": st,
			"fetchedQuery": []string{`^GET /api/bench/results/models\?ctx=4096$`}, "expect": []string{"No results yet"}},
		// board Bench-Report #2: the view beside the report keeps its place,
		// narrows to its first figure, and outlines the cell whose report is open
		map[string]any{"name": "report beside load times: the selected cell", "route": "#/benchmarks/runs/run-46/0?show=load-times&fig=cold_load_s:s", "status": st, "width": 1536,
			"has":    []string{"table.bp-res-t[data-view=load-times] tr[data-model='qwen3.5-4b-q4_k_m.gguf'] td.sel[data-fig='cold_load_s:s'] a.bp-fv.sel[aria-current=true]",
				"a.bp-fv[href='#/benchmarks/runs/run-46/2?show=load-times&fig=cold_load_s%3As']"},
			"count":  map[string]any{"table.bp-res-t td.sel": 1},
			"absent": []string{"Reload, smallest"}, "pageHas": []string{"aside.side"}},
		map[string]any{"name": "load times: a figure link keeps the view", "route": "#/benchmarks/runs?show=load-times", "status": st,
			"has": []string{"tr[data-model='qwen3.5-4b-q4_k_m.gguf'] td[data-fig='cold_load_s:s'] a.bp-fv[href='#/benchmarks/runs/run-46/0?show=load-times&fig=cold_load_s%3As']"}},
		map[string]any{"name": "runs: the Generation speed view", "route": "#/benchmarks/runs?show=generation", "status": st,
			"expect": []string{"GENERATION SPEED", "tok/s, smallest", "tok/s, all 6", "96.2", "92.2"}},
		map[string]any{"name": "runs: the First token view", "route": "#/benchmarks/runs?show=first-token", "status": st,
			"expect": []string{"FIRST TOKEN", "First token, smallest", "0.41 s"}, "has": []string{"tr[data-model='phi-4-14b-q4_k_m.gguf'] [data-missing=ttft_ms]"}},
		map[string]any{"name": "runs: the Memory view: a gone file is not failed", "route": "#/benchmarks/runs?show=memory", "status": st,
			"has":  []string{"table.bp-res-t tr.gone[data-model='mixtral-8x7b-instruct-v0.1.Q4_K_M.gguf'] [data-missing=memory_mib]"},
			"none": []string{"table.bp-res-t tr.gone [data-outcome]"}},
		map[string]any{"name": "runs: the Memory view", "route": "#/benchmarks/runs?show=memory", "status": st,
			"expect": []string{"MEMORY", "Memory, smallest", "Memory, all 6", "Lowest available", "3,100 MiB", "960 MiB"},
			// the lowest available figure names its machine (one the step used)
			"titles": []string{"zone-6, the lowest of the machines the step used"}},
		// every phase type has its word: no raw type id ("context_fill") on the report
		map[string]any{"name": "benchmark report: a context fill phase in words", "route": "#/benchmarks/runs/46/2", "status": st, "width": 1536,
			"page": []string{"Context fill"}, "notOnPage": []string{"context_fill"}, "notOnPageRe": []string{`[a-z]{3,}_[a-z]{3,}`}},
		map[string]any{"name": "runs: the second page", "route": "#/benchmarks/runs?page=2", "status": st, "expect": []string{"13–24 of 49"}, "fetchedQuery": []string{`offset=12`}},
		map[string]any{"name": "runs: direct runs only", "route": "#/benchmarks/runs?kind=benchmark", "status": st, "expect": []string{"1–4 of 4", "Fill the context · gemma-4-12b-q4_k_m · any 2 zones · 131,072"},
			"fetchedQuery": []string{`kind=benchmark`}, "absent": []string{"Full pass"}},
		map[string]any{"name": "runs: something failed", "route": "#/benchmarks/runs?result=has_failed", "status": st, "fetchedQuery": []string{`result=has_failed`}},
		map[string]any{"name": "run panel (Group run)", "route": "#/benchmarks/runs/46", "status": st, "width": 1536,
			"page": []string{"Group run", "All models: load and response", "release 0.1.9", "cold: caches dropped and verified on every load (24 loads)", "ran", "passed", "failed", "refused", "total",
				"Compare", "Download PDF", "Run again", "Issues", "Failed", "child exited during load: exit code 1, after 2.2 s", "Refused", "Does not fit on 1 zone",
				"Reports", "All 24 reports", "26.8 s load · 96.2 tok/s"},
			"pageHas":   []string{"aside.side .bp-acts button.bp-pin.on", "aside.side a.bp-rep[href='#/benchmarks/runs/46/" + "cold-response-repeat-qwen3.5-4b-q4_k_m-auto']"},
			"pageCount": map[string]any{"aside.side .bp-issue": 2, "aside.side a.bp-rep": 8}},
		// board Bench-Runs-Group #20 (the sub-heading by recipe), #17 (the
		// short refusal; the scheduler's sentence on hover), #26 (Cancel on a
		// run still going: run control, slice 6)
		map[string]any{"name": "run panel: reports by recipe, the short refusal", "route": "#/benchmarks/runs/46", "status": st, "width": 1536,
			"pageHas":   []string{"aside.side .bp-reports .bp-rsub[data-recipe=cold-response-repeat]", "aside.side .bp-issue [data-short]"},
			"page":      []string{"Cold load, response and repeat", "Does not fit on 1 zone"},
			"notOnPage": []string{"by the memory reservation rule"}, "titles": []string{"does not fit ra-01 by the memory reservation rule"},
			// the sub-heading is the recipe's title, not the first step's generated name
			"notOnPageRe": []string{`Cold load, response and repeat \(`},
			"pageNone":  []string{"aside.side [data-act=cancel]"}},
		map[string]any{"name": "run panel: Cancel on a running run", "route": "#/benchmarks/runs/49", "status": st, "width": 1536,
			"pageHas": []string{"aside.side .bp-acts .bp-notyet[data-notyet=run] [data-act=cancel]"}},
		map[string]any{"name": "run panel: All reports", "route": "#/benchmarks/runs/46", "status": st, "click": []string{"aside.side button.bp-all"},
			"pageCount": map[string]any{"aside.side a.bp-rep": 24}},
		map[string]any{"name": "run panel: an unknown run", "route": "#/benchmarks/runs/999", "status": st, "page": []string{"There is no such run"}},
		map[string]any{"name": "benchmark report", "route": "#/benchmarks/runs/47/" + slug, "status": st, "width": 1536,
			"page": []string{"Benchmark report", "qwen2.5-72b-instruct-q2_k", "Cold load, response and repeat · all", "context 8,192", "cold load", "reload", "tok/s steady", "first token",
				"Passed", "Steps", "took 3 min 52 s", "Load cold", "60.5 s", "32,643 MiB", "cold verified", "Warm-up", "2.34 s to first token", "Requests, 2 × 3", "8.18 tok/s · output identical",
				"Unload", "2.0 s", "Reload", "5.8 s · 32,643 MiB · file fully cached", "Transcript", "Measured", "Mean", "Median", "Min", "Max", "Generation, tok/s", "Prompt, tok/s",
				"First token, s", "Total time, s", "Open full report", "Download PDF", "Run again", "The whole run"},
			"pageHas": []string{"aside.side a[href='/api/bench/runs/run-47/report?step=0&format=html']", "aside.side .bp-head[data-head=cold]", "aside.side .bp-head[data-head=reload]",
				"aside.side .bp-head[data-head=tps]", "aside.side .bp-head[data-head=ttft]"},
			"pageNone":  []string{"aside.side .bp-heads [data-notyet]", "aside.side .bp-head[data-missing]"},
			// the runner's generated name ("... (qwen2.5-72b-instruct-q2_k.gguf, 8k, all)") is not printed: its words are beside it
			"notOnPageRe": []string{`\(qwen2\.5-72b-instruct-q2_k\.gguf`, `8k, all\)`},
			"pageCount":   map[string]any{"aside.side table.bp-stats": 2, "aside.side li.bp-step": 7}},
		map[string]any{"name": "benchmark report: a failed direct run", "route": "#/benchmarks/runs/43/0", "status": st,
			"page": []string{"Failed", "child exited during load: exit code 1, after 2.2 s"}, "pageCount": map[string]any{"aside.side [data-reason]": 1}},
		map[string]any{"name": "benchmark report: an unknown step", "route": "#/benchmarks/runs/47/nope", "status": st, "page": []string{"There is no such report"}},
		map[string]any{"name": "classic addresses open the new pages", "route": "#/benchmarks/suite/all-load", "status": st, "hash": "#/benchmarks/all/all-load", "page": []string{"Contains"}},
		map[string]any{"name": "classic aggregates address opens the list", "route": "#/benchmarks/trends", "status": st, "hash": "#/benchmarks/all"},
	}})
}

// Long texts and a failed run (--bench long): the failure's reason is printed
// ONCE although the step's reason, its error, its load's reason and its
// phase's error all carry it (it was printed three times on the classic run
// page); long names stay on their line with an ellipsis and their hover.
func TestUINextBenchDetailLongTexts(t *testing.T) {
	failed := "run-20261124T071800-a0c008" // FAILED_AT 8 in bench.mjs
	paths := []string{"/api/status", "/api/models", "/api/profiles", "/api/bench/state", "/api/bench/groups", "/api/bench/groups/" + benchSuite + "/expand",
		"/api/bench/benchmarks", "/api/bench/runs", "/api/bench/runs/" + failed}
	f := fixtureAnswers(t, []string{"--bench", "long"}, paths...)
	st := f["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "failed run: the reason once", "route": "#/benchmarks/runs/" + failed + "/qwen4b-2con-any-2", "status": st,
			"page":      []string{"Failed", "out of memory; the other workers had loaded their shares; nothing was served"},
			"pageCount": map[string]any{"aside.side [data-reason]": 1, "aside.side .bp-reason": 0}},
		map[string]any{"name": "failed run: the run's panel lists it once", "route": "#/benchmarks/runs/" + failed, "status": st,
			"pageCount": map[string]any{"aside.side .bp-issue": 1, "aside.side .bp-issue .bp-reason": 1}},
		map[string]any{"name": "long group: the name keeps its hover in the list", "route": "#/benchmarks/all/" + benchSuite, "status": st,
			"page": []string{"Load times on the consoles, cold and warm, every consoles pair, programme blocks 1 and 2", "Measures load times and answer speed"},
			"has":  []string{"a.bp-row[data-group=" + benchSuite + "] span.bp-name"}},
		map[string]any{"name": "long runs list", "route": "#/benchmarks/runs", "status": st, "count": map[string]any{"table.bp-runs tbody tr": 25},
			"titles": []string{"Load times on the consoles, cold and warm, every consoles pair, programme blocks 1 and 2"}},
	}})
}

// Beside a docked run or report panel (1536 and 1280) every view tab of the
// Runs page stays reachable: no rule hides a tab or clips the tab row; the
// row wraps (the Chrome pictures measure it: Bench-Runs__ui-panel-*.png).
func TestUINextBenchViewTabsNotHidden(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("ui", "next", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range regexp.MustCompile(`(?s)([^{}]*)\{([^{}]*)\}`).FindAllStringSubmatch(string(css), -1) {
		sel, body := rule[1], strings.ReplaceAll(rule[2], " ", "")
		if !strings.Contains(sel, "bp-view") {
			continue
		}
		if strings.Contains(body, "display:none") || strings.Contains(body, "overflow:hidden") || strings.Contains(body, "visibility:hidden") {
			t.Errorf("a rule hides or clips the view tabs: %s{%s}", strings.TrimSpace(sel), rule[2])
		}
		if strings.Contains(sel, "bp-views") && strings.Contains(body, "flex-wrap:nowrap") {
			t.Errorf("the view tabs may not wrap: %s{%s}", strings.TrimSpace(sel), rule[2])
		}
	}
}

// A group's panel scrolls INSIDE: the side panel is one viewport high and
// its body (the entries, the runs) scrolls, so the last of a long group's
// entries (live's longest group has 24) is reached by scrolling the panel,
// never cut off below the window. The fake DOM has no layout: this pins the
// rules the scrolling rests on, and the case "group page: 24 entries" puts
// every entry inside .side-body. Chrome measures it at 1536 x 864 (the
// picture group24-scrolled-1536.png: the body scrolled to its end, entry 23
// inside the body and the window, the window itself not scrolled).
func TestUINextBenchGroupPanelScrollsInside(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("ui", "next", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]string{}
	plain := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(css), "")
	for _, rule := range regexp.MustCompile(`(?s)([^{}]*)\{([^{}]*)\}`).FindAllStringSubmatch(plain, -1) {
		sel, body := strings.TrimSpace(rule[1]), strings.ReplaceAll(rule[2], " ", "")
		if _, seen := rules[sel]; !seen {
			rules[sel] = body
		}
		// nothing between the body and an entry may clip it, fix its height or take it out of the flow
		if strings.Contains(sel, "bp-panel") || strings.Contains(sel, "bp-entries") || strings.Contains(sel, "bp-entry") || strings.Contains(sel, "bp-opts") {
			for _, bad := range []string{"overflow:hidden", "overflow-y:hidden", "max-height:", "height:", "position:fixed", "position:absolute", "display:none"} {
				if strings.Contains(body, bad) && !strings.Contains(body, "min-"+bad) && !strings.Contains(body, "line-"+bad) {
					t.Errorf("a rule on the group's entries has %s: %s{%s}", bad, sel, rule[2])
				}
			}
		}
	}
	for sel, wants := range map[string][]string{
		".side":        {"display:flex", "flex-direction:column", "overflow:hidden"},
		".side.docked": {"height:100vh"},
		".side-body":   {"flex:11auto", "min-height:0", "overflow-y:auto"},
	} {
		body, ok := rules[sel]
		if !ok {
			t.Errorf("no rule %s", sel)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s{%s}: want %s", sel, body, w)
			}
		}
	}
}

// One open of a benchmark page reads each thing once: the Runs page's list
// is read once (not once per draw while the answer is on its way), the
// Overview's recent runs and the results per model only where they are drawn.
// (On live v83 one open of the Runs page sent GET /api/bench/runs 8 times.)
func TestUINextBenchPageOpenReads(t *testing.T) {
	f := benchBoards(t)
	st := f["/api/status"]
	tick := []any{map[string]any{"type": "status", "data": st}, map[string]any{"type": "status", "data": st}, map[string]any{"type": "status", "data": st}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "runs: the list read once, the menu's total once", "route": "#/benchmarks/runs", "status": st, "ws": tick,
			"expect": []string{"1–12 of 49"},
			"fetchedCount": map[string]any{`^GET /api/bench/runs\?(.*&)?offset=`: 1, `^GET /api/bench/runs\?limit=1$`: 1, `^GET /api/bench/runs\b`: 2, `^GET /api/bench/results/models`: 0}},
		// the list's answer still on its way while live messages draw the page again
		map[string]any{"name": "runs: the list asked once while its answer is on its way", "route": "#/benchmarks/runs", "status": st, "ws": tick,
			"fetch":        map[string]any{"/api/bench/runs": map[string]any{"$match": []any{[]any{`offset=`, map[string]any{"$hang": true}}, []any{`.`, f["/api/bench/runs"]}}}},
			"fetchedCount": map[string]any{`^GET /api/bench/runs\?(.*&)?offset=`: 1}},
		map[string]any{"name": "overview: the recent runs and the menu's total", "route": "#/benchmarks", "status": st, "ws": tick,
			"expect": []string{"Recent runs"},
			"fetchedCount": map[string]any{`^GET /api/bench/runs\?limit=4$`: 1, `^GET /api/bench/runs\b`: 2, `^GET /api/bench/results/models`: 1}},
		map[string]any{"name": "groups: no runs list but the menu's total", "route": "#/benchmarks/all", "status": st, "ws": tick,
			"fetchedCount": map[string]any{`^GET /api/bench/runs\b`: 1, `^GET /api/bench/results/models`: 0}},
	}})
}

// Panels and dialogs that scroll inside keep the wheel to themselves at
// their end (overscroll-behavior: contain): on live the wheel went on to
// scroll the list behind the group panel by 2,956 px once the panel reached
// its last entry. The scrolling bodies, on Models, Zones and Benchmarks
// alike (core/panel.js, core/ui.js):
//   - .side-body: the side panel's body (a model, a deleted model, a zone, a
//     group, a run, a report);
//   - .drawer: the drawer (core/panel.js);
//   - .dlg: every dialog;
//   - .hp: the System health popup (it scrolls on a narrow window).
// A box that scrolls inside one of them (a list in a dialog) is left to
// chain to it, so the wheel over it still moves the panel.
func TestUINextScrollingPanelsKeepTheWheel(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("ui", "next", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	plain := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(css), "")
	contain := map[string]bool{}
	for _, rule := range regexp.MustCompile(`(?s)([^{}]*)\{([^{}]*)\}`).FindAllStringSubmatch(plain, -1) {
		if !strings.Contains(strings.ReplaceAll(rule[2], " ", ""), "overscroll-behavior:contain") {
			continue
		}
		for _, sel := range strings.Split(rule[1], ",") {
			contain[strings.TrimSpace(sel)] = true
		}
	}
	for _, sel := range []string{".side-body", ".drawer", ".dlg", ".hp"} {
		if !contain[sel] {
			t.Errorf("%s scrolls inside but lets the wheel go on to the page behind: want overscroll-behavior:contain", sel)
		}
	}
}
