package api

import (
	"regexp"
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
		q("limit=12&offset=0"), q("limit=12&offset=12"), q("limit=12&offset=0&kind=benchmark"), q("limit=12&offset=0&result=has_failed")}
	paths := append([]string{"/api/status", "/api/models", "/api/profiles", "/api/bench/state", "/api/bench/groups", "/api/bench/benchmarks",
		"/api/bench/groups/all-load/expand", "/api/bench/groups/full-pass/expand", "/api/bench/groups/planned-long-context/expand",
		"/api/bench/runs/46", "/api/bench/runs/47", "/api/bench/runs/43", "/api/bench/runs/run-47/report"}, runsQ...)
	f := fixtureAnswers(t, []string{"--bench", "boards"}, paths...)
	// GET /api/bench/runs answers by its query (the harness ignores a query otherwise)
	f["/api/bench/runs"] = map[string]any{"$match": []any{
		[]any{`suite=all-load`, f[runsQ[0]]}, []any{`suite=full-pass`, f[runsQ[1]]}, []any{`suite=planned-long-context`, f[runsQ[2]]},
		[]any{`kind=benchmark`, f[runsQ[7]]}, []any{`result=has_failed`, f[runsQ[8]]}, []any{`offset=12`, f[runsQ[6]]},
		[]any{`limit=4\b`, f[runsQ[3]]}, []any{`limit=1\b`, f[runsQ[4]]}, []any{`.`, f[runsQ[5]]}}}
	f["/api/bench/runs/run-47"] = f["/api/bench/runs/47"]
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
				"placed by the scheduler, and all", "context 8,192", "carries on after a failure", "waits until idle", "cold loads",
				"Runs", "22", "Compare two", "All 22 runs ›", "cold: caches dropped and verified on every load (24 loads)"},
			"pageHas": []string{"aside.side .bp-run[data-run='46'] button.bp-pin.on[aria-pressed=true]", "aside.side a.bp-all[href='#/benchmarks/runs?suite=all-load']",
				"aside.side [data-notyet=b7]", "aside.side [data-notyet=b10]", "aside.side [data-notyet=edit]", "aside.side [data-notyet=run]"},
			"pageCount": map[string]any{"aside.side .bp-run": 5},
			// the list draws the run itself: no run card in the strip above the page (board Bench-Group)
			"pageNone": []string{".topslots a.run-ind", ".topslots .run-ind"}},
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
				"2 groups · 96 runs", "Running now", "Fill the context", "Load · Conversation to 90% · Unload", "Eight at once", "Load · 8 at once · Unload", "Coding tasks", "Load · Tasks · Unload"},
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
			"has":  []string{"div.bp-recent[data-run], a.bp-recent[data-run='48']", "section.bp-latest [data-notyet=b5]"},
			"page": []string{"about 6 h left"}, "pageHas": []string{"a.run-ind"}},
		map[string]any{"name": "runs", "route": "#/benchmarks/runs", "status": st,
			"expect": []string{"RUNS", "Load times", "Generation speed", "First token", "Memory", "Coding", "Started", "Run", "Result", "Took", "1–12 of 49",
				"Full pass", "9 passed", "87 to go", "All models: load and response · cancelled", "Cold load · mixtral-8x7b-q4_k_m · any 4 zones"},
			"has": []string{"tr[data-run='46'] button.bp-pin.on", "tr.direct[data-run='44'] td.bp-name .bp-sub", "tr.direct[data-run='44'] td.bp-name .bp-kind", "tr[data-run='46'] td.bp-start [data-result=with_issues]", "a.bp-pg.on[aria-current=page]", "select.fs[aria-label=Kind]", "select.fs[aria-label=Result]",
				"select.fs[aria-label=Model]", "[data-notyet=b5]"},
			"count": map[string]any{"table.bp-runs tbody tr": 12}},
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
		map[string]any{"name": "run panel: All reports", "route": "#/benchmarks/runs/46", "status": st, "click": []string{"aside.side button.bp-all"},
			"pageCount": map[string]any{"aside.side a.bp-rep": 24}},
		map[string]any{"name": "run panel: an unknown run", "route": "#/benchmarks/runs/999", "status": st, "page": []string{"There is no such run"}},
		map[string]any{"name": "benchmark report", "route": "#/benchmarks/runs/47/" + slug, "status": st, "width": 1536,
			"page": []string{"Benchmark report", "qwen2.5-72b-instruct-q2_k", "Cold load, response and repeat · all", "context 8,192", "cold load", "reload", "tok/s steady", "first token",
				"Passed", "Steps", "took 3 min 52 s", "Load cold", "60.5 s", "32,643 MiB", "cold verified", "Warm-up", "2.34 s to first token", "Requests, 2 × 3", "8.18 tok/s · output identical",
				"Unload", "2.0 s", "Reload", "5.8 s · 32,643 MiB · file fully cached", "Transcript", "Measured", "Mean", "Median", "Min", "Max", "Generation, tok/s", "Prompt, tok/s",
				"First token, s", "Total time, s", "Open full report", "Download PDF", "Run again", "The whole run"},
			"pageHas":   []string{"aside.side a[href='/api/bench/runs/run-47/report?step=0&format=html']", "aside.side .bp-heads [data-notyet=b9]"},
			"pageCount": map[string]any{"aside.side table.bp-stats": 2, "aside.side li.bp-step": 7}},
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
