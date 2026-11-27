package api

import "testing"

// The editors' screens (board bench-editors, five states); the fixture is
// benchEditors (ui_next_bench_edit_test.go).
func TestUINextBenchEditors(t *testing.T) {
	f := benchEditors(t)
	st := f["/api/status"]
	typeIn := func(sel, v string) map[string]any { return map[string]any{"type": sel, "value": v} }
	comment := func(v string) map[string]any { return typeIn(".ed-savebar input[aria-label=Comment]", v) }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		// the list: New benchmark, New group and a benchmark's Edit are links now
		map[string]any{"name": "editors: the list's links", "route": "#/benchmarks/all", "status": st,
			"has": []string{"a.btn.pri[data-act=new-benchmark][href='#/benchmarks/new']", "a.btn[data-act=new-group][href='#/benchmarks/group/new']",
				"div.bp-row[data-bench=qwen4b-2con] a[data-act=edit][href='#/benchmarks/benchmark/qwen4b-2con/edit']",
				// difference 11: a benchmark row's Edit is drawn on the row, not only on hover
				"div.bp-row[data-bench=qwen4b-2con] a.bp-edit[data-act=edit]"},
			"none": []string{"[data-notyet=edit]", "div.bp-row a.bp-hover[data-act=edit]"}},
		map[string]any{"name": "editors: the group panel's Edit", "route": "#/benchmarks/all/s3a", "status": st,
			"pageHas": []string{"aside.side a[data-act=edit][href='#/benchmarks/group/s3a/edit']", "aside.side button[data-act=duplicate]"}},
		// 1 · what a new benchmark starts as: the board's cards, the server's other templates, Copy, Empty
		map[string]any{"name": "editors 1: New benchmark, start from", "route": "#/benchmarks/new", "status": st,
			"has": []string{"button.ed-card-tpl.on[data-tpl=cold-reload-loaded]", "button[data-tpl=load-only]", "button[data-tpl=tasks]", "button[data-tpl=copy]", "button[data-tpl=empty]",
				"button[data-tpl=load-and-conversation]", ".bp-notyet [data-tpl=context-fill-recipe]"},
			"none": []string{"button[data-tpl=cold-load-recipe]"},
			"expect": []string{"New benchmark", "Pick what it starts as. Everything can be changed on the next page.", "Load and speed",
				"Cold load, first generation, steady state, unload, reload, unload. The usual performance run.", "Load only", "Coding tasks", "Copy of an existing one", "Empty",
				"Nothing is saved until you press Save on the next page.", "Continue"}},
		map[string]any{"name": "editors 1: Continue opens the editor from the template", "route": "#/benchmarks/new", "status": st,
			"click": []any{"button[data-tpl=load-only]", "button[data-act=continue]"}, "hash": "#/benchmarks/new/load-only",
			"has": []string{"[data-phase='0'][data-kind=load]", "[data-phase='1'][data-kind=unload]"}},
		map[string]any{"name": "editors 1: Copy of an existing one", "route": "#/benchmarks/new", "status": st,
			"click": []any{"button[data-tpl=copy]", typeIn("select[data-from]", "qwen4b-2con"), "button[data-act=continue]"}, "hash": "#/benchmarks/new/copy?from=qwen4b-2con",
			"has": []string{"input[name=id]:not([disabled])"}, "expect": []string{"New benchmark"}},
		// 2 · the editor of a stored benchmark
		map[string]any{"name": "editors 2: a stored benchmark", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"has": []string{"input[name=id][disabled]", "select[name=profile] option[value=test-s3-qwen4b-2con][selected]", "[data-would-load] [data-plan=start] .b.b-use",
				"[data-phase='0'][data-kind=load]", "[data-phase='1'][data-kind=warmup]", "[data-phase='2'][data-kind=requests]", "[data-phase='3'][data-kind=unload]",
				".ed-savebar button[data-act=save][disabled]"},
			"count": map[string]any{".ed-phase[data-open]": 0},
			"expect": []string{"Edit ", "Qwen 4B, 2 consoles", "What it is", "(used in addresses; cannot change after saving)", "What it loads", "One profile. The run uses it exactly as it is stored.",
				"Production and test profiles are both listed; test ones show as TEST.", "TEST - s3-qwen4b-2con · Qwen 3.5 4B · 16k · forced on 2 zones",
				"Would it load now", "Fits now", " on ps5-01 and ps5-02", "1,700 MiB each", "Need 3,400 MiB, measured, needs_mib. Host on ps5-01.",
				"Phases ", "4 · run in this order", "Warm-up", "1 request", "Requests", "1 prompt × 3 repeats", "Unload", "Add a phase", "One phase is open at a time; the others show as one line.",
				"What counts as passing", "Expected outcome", "It runs to the end", "Give up after", "minutes", "the cluster's limit is 1,440", "No changes yet.", "Cancel", "Save"}},
		map[string]any{"name": "editors 2: a phase opens with the server's fields for its kind", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click": []any{"[data-phase='2'] button[data-act=edit-phase]"},
			"has": []string{".ed-phase[data-phase='2'][data-open] select[data-opt=type] option[value=requests][selected]", "[data-phase='2'] input[data-opt=label]", "[data-phase='2'] input[data-opt=repeats]",
				"[data-phase='2'] [data-opt=stream] input[type=checkbox]", "[data-phase='2'] [data-opt=cache_prompt]", "[data-phase='2'] input[data-opt=max_tokens]", "[data-phase='2'] input[data-opt=temperature]",
				"[data-phase='2'] input[data-opt=seed]", "[data-phase='2'] [data-prompts] [data-prompt='0'] textarea[aria-label=Prompt]", "[data-phase='2'] button[data-act=add-prompt]",
				"[data-phase='2'] button[data-act=done-phase]"},
			"count":  map[string]any{".ed-phase[data-open]": 1},
			"expect": []string{"Kind", "Label", "Repeats", "Stream the answers", "(measures time to the first token)", "Prompts · 1", "Name", "Prompt", "Max tokens", "Add a prompt", "3 requests in all: 1 prompt × 3 repeats.", "Done"}},
		map[string]any{"name": "editors 2: every kind the server has is offered", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click": []any{"[data-phase='2'] button[data-act=edit-phase]"},
			"has": []string{"select[data-opt=type] option[value=load]", "select[data-opt=type] option[value=warmup]", "select[data-opt=type] option[value=conversation]", "select[data-opt=type] option[value=context_fill]",
				"select[data-opt=type] option[value=hold]", "select[data-opt=type] option[value=concurrent_requests]", "select[data-opt=type] option[value=tasks]", "select[data-opt=type] option[value=unload]"}},
		map[string]any{"name": "editors 2: a changed kind takes that kind's fields", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click": []any{"[data-phase='2'] button[data-act=edit-phase]", typeIn("[data-phase='2'] select[data-opt=type]", "context_fill")},
			"has": []string{"[data-phase='2'][data-kind=context_fill] input[data-opt=fill_pct]", "[data-phase='2'] input[data-opt=marks_pct]", "[data-phase='2'] input[data-opt=stop_available_mib]",
				"[data-phase='2'] [data-opt=refuse_shared_shape]"},
			"none": []string{"[data-phase='2'] [data-prompts]"}},
		map[string]any{"name": "editors 2: a change, the comment, then Save is on", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click":  []any{"[data-phase='2'] button[data-act=edit-phase]", typeIn("[data-phase='2'] [data-prompt='0'] textarea[aria-label=Prompt]", "Explain a suspension bridge."), comment("longer prompt")},
			"has":    []string{".ed-savebar button[data-act=save]:not([disabled])", ".ed-savebar button[data-act=diff]"},
			"expect": []string{"Changed: the prompts of phase 3. ", "See what changed"}},
		map[string]any{"name": "editors 2: See what changed is the audit page's view", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click":   []any{"[data-phase='2'] button[data-act=edit-phase]", typeIn("[data-phase='2'] [data-prompt='0'] textarea[aria-label=Prompt]", "Explain a suspension bridge."), "button[data-act=diff]"},
			"pageHas": []string{"div.dlg [data-dlg=diff] .diff .diff-row"}, "page": []string{"What changed", "phases[2].requests[0].prompt", "Explain a suspension bridge."}},
		map[string]any{"name": "editors 2: Save sends the definition and the comment, then the list", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click":        []any{"[data-phase='2'] button[data-act=edit-phase]", typeIn("[data-phase='2'] input[data-opt=repeats]", "5"), comment("five repeats"), ".ed-savebar button[data-act=save]"},
			"fetchedQuery": []string{`^PUT /api/bench/benchmarks/qwen4b-2con \{"id":"qwen4b-2con","name":"Qwen 4B, 2 consoles","profile":"test-s3-qwen4b-2con","phases":\[.*"repeats":5.*\],"comment":"five repeats"\}$`},
			"notFetched":   []string{`^PUT .*"source"`, `^PUT .*"created_at"`}, "hash": "#/benchmarks/all?saved=qwen4b-2con", "pageNone": []string{"div.dlg"},
			// the list says what was saved, and its row is marked
			"has":    []string{"p.bp-savedline[role=status][data-saved-line=qwen4b-2con]", "div.bp-row.saved[data-bench=qwen4b-2con][data-saved]"},
			"count":  map[string]any{".bp-row.saved": 1},
			"expect": []string{"Saved benchmark Qwen 4B, 2 consoles. Its row is marked below."}},
		map[string]any{"name": "editors: a group's save names it and marks its row", "route": "#/benchmarks/all/s3a?saved=s3a", "status": st,
			"has":    []string{"p.bp-savedline[data-saved-line=s3a]", "a.bp-row.saved.sel[data-group=s3a]"},
			"expect": []string{"Saved group S3a console workers only, 1 to 5 consoles, Qwen 4B. Its row is marked below."}},
		map[string]any{"name": "editors: no save, no line", "route": "#/benchmarks/all", "status": st,
			"none": []string{"p.bp-savedline", ".bp-row.saved"}},
		map[string]any{"name": "editors 2: moving and removing a phase act at once, nothing is written", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click": []any{"[data-phase='1'] button[data-act=down]", "[data-phase='3'] button[data-act=remove-phase]"},
			"has":   []string{"[data-phase='1'][data-kind=requests]", "[data-phase='2'][data-kind=warmup]"}, "none": []string{"[data-phase='3']"},
			"expect": []string{"Changed: the phases."}, "notFetched": []string{`^PUT `}},
		// 3 · the server refuses: "Not saved." and its sentence, the phase marked
		map[string]any{"name": "editors 3: the server's refusal, verbatim, the part marked", "route": "#/benchmarks/new/cold-reload-loaded", "status": st,
			"click": []any{typeIn("input[name=name]", "Qwen new"), typeIn("select[name=profile]", "test-s3-qwen4b-3con"),
				"[data-phase='2'] button[data-act=edit-phase]", typeIn("[data-phase='2'] [data-prompt='0'] textarea[aria-label=Prompt]", ""), comment("new"), ".ed-savebar button[data-act=save]"},
			"has": []string{".ed-savebar.bad [data-refused]", ".ed-phase.bad[data-phase='2'] [data-problem]"},
			// the save bar puts the place first in the page's words (phases counted from 1), the sentence verbatim
			"expect": []string{"Not saved.", `Phase 3, prompt "explain": messages or prompt is required`},
			"hash":   "#/benchmarks/new/cold-reload-loaded"},
		map[string]any{"name": "editors 2: a new benchmark's id follows its name, then it is saved", "route": "#/benchmarks/new/load-only", "status": st,
			"click":        []any{typeIn("input[name=name]", "Qwen new"), typeIn("select[name=profile]", "test-s3-qwen4b-3con"), comment("for S3"), ".ed-savebar button[data-act=save]"},
			"fetchedQuery": []string{`^POST /api/bench/benchmarks \{.*"id":"qwen-new".*"profile":"test-s3-qwen4b-3con","comment":"for S3"\}$`, `^POST /api/plan \{"model":"test-s3-qwen4b-3con"\}$`},
			"hash":         "#/benchmarks/all?saved=qwen-new"},
		// 4 · the group editor: each entry's dry read
		map[string]any{"name": "editors 4: the group editor", "route": "#/benchmarks/group/s3a/edit", "status": st,
			"has": []string{"input[name=id][disabled]", "[data-entry='0'][data-fits=fits] .b.b-use", "[data-entry='4'][data-fits=fits]",
				"[data-opt=only_when_idle] input:checked", "[data-opt=evict_others] input:checked", "[data-opt=stop_on_failure] input:not(:checked)", "input[data-opt=cooldown_s]", "input[data-opt=idle_wait_s]",
				".bp-notyet .ed-check", "input[data-add]"},
			"fetchedQuery": []string{`^POST /api/bench/benchmarks/qwen4b-1con/run\?dry_run=1 \{\}$`},
			"expect": []string{"Edit group ", "S3a console workers only, 1 to 5 consoles, Qwen 4B", "What it runs ", "5 entries · in this order", "Qwen 4B, 2 consoles", "TEST - s3-qwen4b-2con", "Fits now",
				"A group inside a group runs its entries in place. ", "5 benchmarks, 20 steps in all.", "How it runs", "Wait until idle", "Stop at the first failure",
				"Unload other people's idle models", "Pause between entries", "seconds", "Cold loads", " · not available yet"}},
		map[string]any{"name": "editors 4: an entry not startable here, and what that means", "route": "#/benchmarks/group/s3a/edit", "status": st,
			"click":  []any{typeIn("input[data-add]", "host"), "button[data-add-id=host-gpu-alone]", comment("add the reference")},
			"has":    []string{"[data-entry='5'][data-fits=off] .b.b-aside", "[data-entry='5'] [data-off]", "[data-cannot-start]", ".ed-savebar button[data-act=save]:not([disabled])"},
			"expect": []string{"Not startable here", "nothing of the run was started. A group with this entry cannot be started from Potluck.", "Changed: entry 6 added."}},
		map[string]any{"name": "editors 4: a refused group save names the entry", "route": "#/benchmarks/group/s3a/edit", "status": st,
			"fetch": withAnswer(f, "/api/bench/groups/s3a", withGroupEntries(f["/api/bench/groups/s3a"], []any{"nope"})),
			"click": []any{comment("x"), typeIn("input[data-opt=cooldown_s]", "20"), ".ed-savebar button[data-act=save]"},
			"has":   []string{".ed-savebar.bad", ".ed-phase.bad[data-entry='0'] [data-problem]"}, "expect": []string{"Not saved.", `Entry 1: "nope" is not a benchmark`}},
		// a refusal's place, in the page's words; a path the page cannot name keeps the raw sentence
		map[string]any{"name": "editors 3: each refused part is named first, an unnamed one stays as sent", "route": "#/benchmarks/new/cold-reload-loaded", "status": st,
			"fetch": withAnswer(f, "POST /api/bench/benchmarks", map[string]any{"$status": 422, "$body": map[string]any{
				"error": map[string]any{"message": "x", "code": 422, "reason": "invalid"},
				"problems": []any{
					map[string]any{"path": "phases[1]", "message": "phases[1] (warmup): count must be 0 to 8"},
					map[string]any{"path": "phases[2].requests[0]", "message": "phases[2].requests[0] (explain): messages or prompt is required"},
					map[string]any{"path": "expect.outcome", "message": `expect.outcome: "maybe" is not one of ready, refused, failed`},
					map[string]any{"path": "", "message": "the definition is larger than 1 MiB"}}}}),
			"click": []any{comment("x"), ".ed-savebar button[data-act=save]"},
			// several: the bar says how many and one of them (the one no part shows), the parts keep their own sentences
			"has":    []string{".ed-savebar.bad [data-refused] [data-line='0'][data-count='4']", ".ed-phase.bad[data-phase='1'] [data-problem]", ".ed-phase.bad[data-phase='2'] [data-problem]"},
			"none":   []string{".ed-savebar [data-line='1']"},
			"expect": []string{"Not saved. 4 problems; the first: the definition is larger than 1 MiB", `expect.outcome: "maybe" is not one of ready, refused, failed`},
			"titles": []string{`Phase 3, prompt "explain": messages or prompt is required`}},
		map[string]any{"name": "editors 3: one problem, the place first", "route": "#/benchmarks/new/cold-reload-loaded", "status": st,
			"fetch": withAnswer(f, "POST /api/bench/benchmarks", map[string]any{"$status": 422, "$body": map[string]any{
				"error":    map[string]any{"message": "x", "code": 422, "reason": "invalid"},
				"problems": []any{map[string]any{"path": "phases[1]", "message": "phases[1] (warmup): count must be 0 to 8"}}}}),
			"click":  []any{comment("x"), ".ed-savebar button[data-act=save]"},
			"expect": []string{"Not saved. Phase 2: count must be 0 to 8"}},
		// the host is named by its zone's name: the plan's own name for the node when the zones list does not carry it, never its id
		map[string]any{"name": "editors 2: the host by its zone's name, also without the zones list", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": withoutZones(st),
			"expect": []string{"Need 3,400 MiB, measured, needs_mib. Host on ps5-01."}, "absent": []string{"Host on 02:"}},
		// a new benchmark from a template never says it keeps settings (only a stored one can)
		map[string]any{"name": "editors: a new benchmark from a template has no kept line", "route": "#/benchmarks/new/cold-reload-loaded", "status": st,
			"none": []string{".ed-kept"}},
		map[string]any{"name": "editors 4: a new group", "route": "#/benchmarks/group/new", "status": st,
			"has": []string{"input[name=id]:not([disabled])"}, "expect": []string{"New group", "No entries yet.", "New: nothing is saved yet."}},
		// what a save does to past runs: the run keeps what it ran and says the definition changed since
		map[string]any{"name": "editors: a run's definition changed since it ran", "route": "#/benchmarks/runs/51", "status": st,
			"pageHas": []string{"aside.side [data-definitions-changed] a[data-def=qwen4b-2con][href='#/benchmarks/benchmark/qwen4b-2con/edit']"},
			"page":    []string{"Definition changed since this run: ", "Qwen 4B, 2 consoles", ". The run keeps what it ran."}},
		// a cancelled run: nothing failed; the cancelled step is a neutral "not run" tile, the red banner says who and why
		map[string]any{"name": "editors: a cancelled run's tiles", "route": "#/benchmarks/runs/51", "status": st,
			"pageHas":  []string{"aside.side .bp-kpi.notrun[data-kpi=not-run]", "aside.side .bp-kpi[data-kpi=failed]", "aside.side .banner.err"},
			"pageNone": []string{"aside.side .bp-kpi.bad"},
			"page":     []string{"not run", "No step failed or was refused.", `cancelled at step 1 of 1 by anonymous: "wrong zones"`}},
		map[string]any{"name": "editors: a run whose definitions did not change says nothing", "route": "#/benchmarks/runs/52", "status": st,
			"pageNone": []string{"aside.side [data-definitions-changed]"}},
		// 5 · leaving with unsaved changes
		map[string]any{"name": "editors 5: Leave without saving? (Stay)", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click":   []any{typeIn("input[name=name]", "Qwen 4B, two consoles"), ".ed-savebar button[data-act=cancel]"},
			"pageHas": []string{"div.dlg [data-dlg=leave]", "div.dlg .dlg-foot button.dng"},
			"page":    []string{"Leave without saving?", "Your changes to ", "Qwen 4B, two consoles", " are not saved: the name.", "Stay", "Leave"},
			"hash":    "#/benchmarks/benchmark/qwen4b-2con/edit"},
		map[string]any{"name": "editors 5: Leave goes", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click": []any{typeIn("input[name=name]", "Qwen 4B, two consoles"), ".ed-savebar button[data-act=cancel]", "div.dlg .dlg-foot button.dng"},
			"hash":  "#/benchmarks/all", "pageNone": []string{"div.dlg"}},
		map[string]any{"name": "editors 5: nothing changed, nothing asked", "route": "#/benchmarks/benchmark/qwen4b-2con/edit", "status": st,
			"click": []any{".ed-savebar button[data-act=cancel]"}, "hash": "#/benchmarks/all", "pageNone": []string{"div.dlg"}},
		map[string]any{"name": "editors 5: Duplicate", "route": "#/benchmarks/all/s3a", "status": st,
			"click":   []any{"aside.side button[data-act=duplicate]"},
			"pageHas": []string{"div.dlg [data-dlg=duplicate] input[name=name]", "div.dlg [data-dlg=duplicate] input[name=id]", "div.dlg .dlg-foot button.pri[disabled]"},
			"page":    []string{"Duplicate ", "S3a console workers only, 1 to 5 consoles, Qwen 4B", "Name of the copy", "Id", "It cannot change later.", "Comment (required)", "Create the copy"}},
		map[string]any{"name": "editors 5: Duplicate creates the copy with the comment", "route": "#/benchmarks/all/s3a", "status": st,
			"click":        []any{"aside.side button[data-act=duplicate]", typeIn("div.dlg textarea[aria-label=Comment]", "a copy to try 3 consoles"), "div.dlg .dlg-foot button.pri"},
			"fetchedQuery": []string{`^POST /api/bench/groups \{"id":"s3a-copy","name":"S3a console workers only, 1 to 5 consoles, Qwen 4B \(copy\)","benchmarks":\["qwen4b-1con",.*\],.*"evict_others":true,"comment":"a copy to try 3 consoles"\}$`},
			"hash":         "#/benchmarks/all/s3a-copy"},
	}})
}

// withGroupEntries: a group's GET with other entries.
func withGroupEntries(g any, entries []any) any {
	m := map[string]any{}
	for k, v := range g.(map[string]any) {
		m[k] = v
	}
	m["benchmarks"] = entries
	return m
}

// withoutZones(status): the status with no zones in its view (the page then
// cannot look a zone's name up by id)
func withoutZones(st any) any {
	m := map[string]any{}
	for k, v := range st.(map[string]any) {
		m[k] = v
	}
	if v, ok := m["view"].(map[string]any); ok {
		vv := map[string]any{}
		for k, x := range v {
			vv[k] = x
		}
		vv["nodes"] = []any{}
		m["view"] = vv
	}
	return m
}
