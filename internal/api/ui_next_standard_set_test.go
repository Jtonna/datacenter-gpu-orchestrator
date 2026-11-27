package api

import (
	"strings"
	"testing"
)

// The TEST / PRODUCTION pages (profiles plan, section 9; the owner's note:
// "test profiles get their own page … all visible but moved out of the way
// visually … named TEST - <name> … when promoted it loses the TEST - part
// automatically"). The shown name is never sent: ids stay in the
// addresses, the data-* keys and the writes.

// Settings > Profiles: production by default under their shown names;
// "Show test profiles too"; the panel's stage, Promote and Demote with a
// required comment, sent by id.
func TestUINextProfilesStage(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles", "/api/models", "/api/config", "/api/config/schema", "/api/status")
	st := fetch["/api/status"]
	fetch["POST /api/config/profiles/qwen3-14b-laptop/promote"] = map[string]any{"revision": 413, "changed": true, "id": "qwen3-14b-laptop", "stage": "production"}
	fetch["POST /api/config/profiles/gemma-4-27b/demote"] = map[string]any{"revision": 413, "changed": true, "id": "gemma-4-27b", "stage": "test"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "profiles: production by default, shown names", "route": "#/profiles", "status": st,
			"expect": []string{"10 production", "302 test", "Gemma chat", "gemma-4-27b", "qwen3-32b-split", "Show test profiles too", "10 profiles", "Test profiles page"},
			"has":    []string{"tr[data-profile=gemma-4-27b] [data-shown]", "a[data-test-page][href='#/test-profiles']"},
			"absent": []string{"qwen3-14b-laptop", "Qwen 128k trial", "TEST - ", "made by hand"}},
		map[string]any{"name": "profiles: test profiles too, named TEST -", "route": "#/profiles?show=all&q=qwen3-14b-128k", "status": st,
			"expect": []string{"TEST - Qwen 128k trial", "qwen3-14b-128k", "Held back", "Hide test profiles"}, "has": []string{"tr[data-profile=qwen3-14b-128k] span.plc-gen"}},
		map[string]any{"name": "profiles: a forced one is test by the rule", "route": "#/profiles?show=all&q=laptop", "status": st,
			"expect": []string{"TEST - qwen3-14b-laptop"}, "has": []string{"tr[data-profile=qwen3-14b-laptop] span.plc-gen"}},
		map[string]any{"name": "profiles: the demote dialog says the name after", "route": "#/profiles?profile=gemma-4-27b", "status": st,
			"click": []any{"aside.side button[data-act=demote]"}, "pageHas": []string{"div.dlg [data-dlg=demote] [data-after]"}, "page": []string{"Gemma chat", "TEST - Gemma chat"}},
		map[string]any{"name": "profiles: the panel of a test profile", "route": "#/profiles?show=all&profile=qwen3-14b-laptop", "status": st,
			"page":    []string{"TEST - qwen3-14b-laptop", "Stage", "Test (forced zones)", "Promote"},
			"pageHas": []string{"aside.side button[data-act=promote]"}, "pageNone": []string{"aside.side button[data-act=demote]"}},
		map[string]any{"name": "profiles: promote asks a comment and sends the id", "route": "#/profiles?show=all&profile=qwen3-14b-laptop", "status": st,
			"click":        []any{"aside.side button[data-act=promote]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "in use every day"}, "div.dlg-foot button.pri"},
			"fetchedQuery": []string{`^POST /api/config/profiles/qwen3-14b-laptop/promote \{"comment":"in use every day"\}$`},
			"notFetched":   []string{`TEST - `}},
		map[string]any{"name": "profiles: the promote dialog without a comment sends nothing", "route": "#/profiles?show=all&profile=qwen3-14b-laptop", "status": st,
			"click":   []any{"aside.side button[data-act=promote]"},
			"pageHas": []string{"div.dlg [data-dlg=promote]", "div.dlg-foot button.pri[disabled]"}, "page": []string{"qwen3-14b-laptop", "after"},
			"notFetched": []string{`/promote`}},
		map[string]any{"name": "profiles: demote a production profile", "route": "#/profiles?profile=gemma-4-27b", "status": st,
			"click":        []any{"aside.side button[data-act=demote]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "trial only"}, "div.dlg-foot button.pri"},
			"fetchedQuery": []string{`^POST /api/config/profiles/gemma-4-27b/demote \{"comment":"trial only"\}$`}},
		map[string]any{"name": "profiles: the editor has the display name, not the stage", "route": "#/profiles/gemma-4-27b", "status": st,
			"expect": []string{"Display name"}, "absent": []string{"Whether the profile is a test or a production profile"}},
	}})
}

// The Test profiles page: one row per test profile with its shown name and
// id, the context it really runs at, why it is test, who refers to it, its
// last use and Promote; filters by model and reason; a pager.
func TestUINextTestProfilesPage(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles/test", "/api/models", "/api/status")
	st := fetch["/api/status"]
	fetch["POST /api/config/profiles/qwen3-14b-128k/promote"] = map[string]any{"revision": 413, "changed": true, "id": "qwen3-14b-128k", "stage": "production"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "test profiles: the page", "route": "#/test-profiles", "status": st,
			"expect": []string{"Test profiles", "302", "10", "production", "302 test profiles", "1–25 of 302", "Why test", "Referred to by", "Last used", "Promote"},
			"count":  map[string]any{"tbody tr[data-profile]": 25}},
		map[string]any{"name": "test profiles: in the gear menu under Profiles", "route": "#/test-profiles", "status": st,
			"pageHas": []string{"a[href='#/test-profiles']"}, "order": []string{"Test profiles"}},
		map[string]any{"name": "test profiles: a marked one, shown name and id", "route": "#/test-profiles?reason=marked", "status": st,
			"expect": []string{"TEST - Qwen 128k trial", "qwen3-14b-128k", "Marked test", "1 test profile"},
			"has":    []string{"tr[data-profile=qwen3-14b-128k][data-reason=marked] [data-shown]"}},
		map[string]any{"name": "test profiles: the real context, not the label", "route": "#/test-profiles?q=bench-qwen3-14b-c64k-n2", "status": st,
			"expect": []string{"40k", "written 64k, runs at 40k"},
			"has":    []string{"tr[data-profile=bench-qwen3-14b-c64k-n2] td[data-ctx-real='40960']"}},
		map[string]any{"name": "test profiles: by model", "route": "#/test-profiles?model=GLM-4.5-Air-Q2_K.gguf", "status": st,
			"expect": []string{"50 test profiles", "1–25 of 50"}, "absent": []string{"Qwen 128k trial"}},
		map[string]any{"name": "test profiles: forced zones", "route": "#/test-profiles?reason=forced%20zones", "status": st,
			"expect": []string{"TEST - qwen3-14b-laptop", "Forced zones", "1 test profile"}},
		map[string]any{"name": "test profiles: nothing matches", "route": "#/test-profiles?q=zzzz", "status": st, "expect": []string{"No test profile matches"}},
		map[string]any{"name": "test profiles: promote sends the id", "route": "#/test-profiles?reason=marked", "status": st,
			"click":        []any{"tr[data-profile=qwen3-14b-128k] button[data-act=promote]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "it works"}, "div.dlg-foot button.pri"},
			"page":         []string{"Qwen 128k trial"},
			"fetchedQuery": []string{`^POST /api/config/profiles/qwen3-14b-128k/promote \{"comment":"it works"\}$`}},
	}})
}

// The Models panel: production profiles by context under their shown names,
// one "and N test profiles" line to the Test profiles page; "Make standard
// profiles" and its dialog's states (checking, ready, named, nothing,
// refused, error, written), with the reviewer's rulings A to G: the name
// prefilled from the family and editable, a refused name, the default that
// exists at another context with what it carries, the tick boxes and the
// count, an estimate row ticked, the estimate-does-not-fit row unticked,
// the max_pool words, the 5% margin named.
func TestUINextModelsStandardSet(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	path := "/api/models/" + mdlLoaded + "/standard-set"
	std := fixtureAnswers(t, []string{"--stdset", "ready"}, path)[path]
	mine := fixtureAnswers(t, []string{"--stdset", "ready"}, path+"?short=my-gemma")[path+"?short=my-gemma"]
	none := fixtureAnswers(t, []string{"--stdset", "nothing"}, path)[path]
	taken := map[string]any{"$status": 400, "$body": map[string]any{"error": map[string]any{"message": "the name qwen3-14b is taken: the profile qwen3-14b is of another file (qwen3-14b-Q5_K_M.gguf)"}}}
	// the read by its query: ?short=qwen3-14b refused, ?short=my-gemma, else the prefill
	reads := map[string]any{"$match": []any{
		[]any{`\?short=qwen3-14b`, taken},
		[]any{`\?short=my-gemma`, mine},
		[]any{`^GET `, std},
	}}
	with := func(get, post any) map[string]any {
		f := map[string]any{}
		for k, v := range fetch {
			f[k] = v
		}
		if get != nil {
			f[path] = get
		}
		if post != nil {
			f["POST "+path] = post
		}
		return f
	}
	open := []any{"button[data-act=standard]"}
	comment := map[string]any{"type": "div.dlg textarea[aria-label=Comment]", "value": "standard set for gemma"}
	written := map[string]any{"revision": 413, "changed": true, "written": []any{"gemma-4-27b:16k", "gemma-4-27b:64k", "gemma-4-27b:128k", "gemma-4-27b:256k"},
		"skipped": []any{map[string]any{"id": "gemma-4-27b", "reason": "gemma-4-27b exists at 8k, kept as it is; the standard default would be 32k"}}}
	refused := map[string]any{"$status": 409, "$body": map[string]any{"error": map[string]any{"message": "configuration changed since revision 412 (now 413): read it again", "code": "stale"}}}
	failed := map[string]any{"$status": 503, "$body": map[string]any{"error": map[string]any{"message": "the scheduler is shutting down", "code": "shutting_down"}}}
	tr := func(ctx string) string { return "tr[data-std='gemma-4-27b" + ctx + "']" }
	notes := func(ctx string) string { return "tr[data-std-notes='gemma-4-27b" + ctx + "']" }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: a test profile is one line to the Test profiles page", "route": "#/models?model=qwen3-14b-Q5_K_M.gguf", "status": st,
			"page":     []string{"1 test profile"},
			"pageHas":  []string{"aside.side p.mdl-tests[data-tests='1'] a[href^='#/test-profiles?model=']"},
			"pageNone": []string{"aside.side details.mdl-tests", "aside.side table.mdl-ptbl tr[data-profile=qwen3-14b]"}},
		map[string]any{"name": "models: the button", "route": "#/models/" + mdlLoaded, "status": st,
			"pageHas": []string{"button[data-act=standard]"}, "page": []string{"Make standard profiles"}},
		map[string]any{"name": "standard: checking", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(map[string]any{"$hang": true}, nil),
			"click": open, "pageHas": []string{"div.dlg[data-dlg=standard][data-state=checking]"}, "page": []string{"Planning each context with nothing loaded"}},
		// A, B, C, D, F, G on the read
		map[string]any{"name": "standard: ready, the read call's table", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(reads, nil),
			"click": open,
			"pageHas": []string{"div.dlg[data-dlg=standard][data-state=ready]",
				// A: the name, prefilled from the family
				"div.dlg div.std-namef[data-name-source=family] input.std-name", "div.dlg [data-name-from=family]",
				// B: the default exists at another context, not tickable
				tr("") + "[data-exists=true][data-ticked=false]", notes("") + " [data-exists-note]",
				// C: ticked by default, the estimate rows' states
				tr(":16k") + "[data-state=fits-derived][data-ticked=true] input[data-tick]", notes(":256k") + " [data-zones-need-note]", tr(":64k") + "[data-state=fits-by-estimate][data-ticked=true] input[data-tick]",
				tr(":128k") + "[data-ticked=true]", tr(":256k") + "[data-state=estimate-does-not-fit][data-ticked=false] input[data-tick]",
				notes(":256k") + " [data-why=estimate-does-not-fit]", notes(":64k") + " [data-need-note]", notes(":256k") + " [data-shape-note]",
				notes(":128k") + " [data-every-zone=max-pool]", notes(":64k") + " [data-now=evict]", notes(":128k") + " [data-host-memory=not-counted]",
				"[data-to-write='3']", "div.dlg-foot button[data-go][disabled]"},
			"pageNone": []string{tr("") + " input[data-tick]"},
			"page": []string{"from the existing family", "gemma-4-27b exists at 8k, kept as it is; the standard default would be 32k",
				"It has what a standard row would not: KV type q8_0, flash attention, thinking off (the chat-template argument), a hand-written need of 15,900 MiB, aliases (gemma-27b)",
				"Fits, estimated", "Not measured", "Estimate only: not measured: by the file estimate it does not fit even with nothing loaded",
				"the fit uses 13,751 MiB: the estimate with its 5% margin", "so that figure is not its own",
				"every zone it may use (4: the setting max_pool)", "the default context", "measured for this shape", "derived from other contexts of this model", "Fits, derived", "Two figures: /api/plan shows 25,384 MiB",
				"Write 3 profiles", "Comment (required)", "now it would first unload gemma-4-12b", "every standard context is at or below it"}},
		// C: a tick changes the count; untick everything and nothing can be written
		map[string]any{"name": "standard: ticking the estimate row counts it", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(reads, nil),
			"click":   append(append([]any{}, open...), tr(":256k")+" input[data-tick]", comment),
			"pageHas": []string{tr(":256k") + "[data-ticked=true]", "[data-to-write='4']", "div.dlg-foot button[data-go]:not([disabled])"}, "page": []string{"Write 4 profiles"}},
		map[string]any{"name": "standard: nothing ticked, nothing to write", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(reads, nil),
			"click":   append(append([]any{}, open...), tr(":16k")+" input[data-tick]", tr(":64k")+" input[data-tick]", tr(":128k")+" input[data-tick]", comment),
			"pageHas": []string{"[data-to-write='0']", "div.dlg-foot button[data-go][disabled]"}, "page": []string{"No row is ticked", "Write 0 profiles"}},
		// the write sends the comment, the name and the ticked ids
		map[string]any{"name": "standard: the write sends the ticked ids", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(reads, written),
			"click":        append(append([]any{}, open...), tr(":256k")+" input[data-tick]", comment, "div.dlg-foot button[data-go]"),
			"fetchedQuery": []string{`^POST /api/models/` + mdlLoaded + `/standard-set \{"comment":"standard set for gemma","short":"gemma-4-27b","ids":\["gemma-4-27b:16k","gemma-4-27b:64k","gemma-4-27b:128k","gemma-4-27b:256k"\]\}$`},
			"pageHas":      []string{"div.dlg[data-dlg=standard][data-state=written] [data-written='4']"}, "page": []string{"Wrote 4 profiles", "gemma-4-27b: gemma-4-27b exists at 8k"}},
		// A: a name of one's own, read again with ?short=
		map[string]any{"name": "standard: another name reads again", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(reads, nil),
			"click":        append(append([]any{}, open...), map[string]any{"type": "div.dlg input.std-name", "value": "my-gemma"}, "div.dlg button[data-go=name]"),
			"fetchedQuery": []string{`^GET /api/models/` + mdlLoaded + `/standard-set\?short=my-gemma$`},
			"pageHas":      []string{"div.dlg[data-state=ready] [data-name-from=asked]", "tr[data-std='my-gemma'][data-ticked=true]", "tr[data-std='my-gemma:16k']"},
			"page":         []string{"the name you gave"}},
		// A: a name of another file is refused, with its reason; nothing can be written
		map[string]any{"name": "standard: a taken name is refused", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(reads, nil),
			"click":    append(append([]any{}, open...), map[string]any{"type": "div.dlg input.std-name", "value": "qwen3-14b"}, "div.dlg button[data-go=name]"),
			"pageHas":  []string{"div.dlg[data-dlg=standard][data-state=named] [data-name-refused]", "div.dlg-foot button[data-go][disabled]"},
			"pageNone": []string{"table.std-tbl"},
			"page":     []string{"This name cannot be used.", "the profile qwen3-14b is of another file (qwen3-14b-Q5_K_M.gguf)"}},
		map[string]any{"name": "standard: refused", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(reads, refused),
			"click":   append(append([]any{}, open...), map[string]any{"type": "div.dlg textarea[aria-label=Comment]", "value": "x"}, "div.dlg-foot button[data-go]"),
			"pageHas": []string{"div.dlg[data-dlg=standard][data-state=refused] [data-refused]"}, "page": []string{"Not written: nothing changed.", "read it again", "Check again"}},
		map[string]any{"name": "standard: nothing to write", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(none, nil),
			"click": open, "pageHas": []string{"div.dlg[data-dlg=standard][data-state=nothing] [data-to-write='0']"}, "pageNone": []string{"div.dlg-foot button[data-go]", "input[data-tick]"},
			"page": []string{"Nothing to write"}},
		map[string]any{"name": "standard: the read fails", "route": "#/models/" + mdlLoaded, "status": st, "fetch": with(failed, nil),
			"click": open, "pageHas": []string{"div.dlg[data-dlg=standard][data-state=error]"}, "page": []string{"could not be worked out", "shutting down", "Check again"}},
	}})
}

// The placement preview lists production first and the test profiles under
// their shown names; the result's title is the shown name, the id beside it.
func TestUINextPlacementShownNames(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles", "/api/models", "/api/status")
	st := fetch["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "placement: production first, test named TEST -", "route": "#/placement?q=qwen3-14b", "status": st,
			"order": []string{"qwen3-14b", "TEST - Qwen 128k trial"}, "expect": []string{"TEST - qwen3-14b-laptop"},
			"has": []string{"button.plc-opt[data-profile=qwen3-14b-128k] [data-shown]"}},
		map[string]any{"name": "placement: no test profile until asked", "route": "#/placement", "status": st,
			"absent": []string{"TEST - "}, "expect": []string{"Show test profiles too"}},
	}})
}

// The owner's note: "when promoted it loses the TEST - part automatically".
// After Promote the page reads its list again (no page reload: the same
// address, the same view) and the shown name drops "TEST - "; the Models
// panel redraws from the catalog the server pushes after the change.
func TestUINextPromoteDropsTheTestName(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles", "/api/profiles/test", "/api/models", "/api/config", "/api/config/schema", "/api/status")
	st := fetch["/api/status"]
	promoted := func(doc any, key, id string, drop bool) map[string]any {
		d := mcpCopy(t, doc)
		var keep []any
		for _, r := range d[key].([]any) {
			m := r.(map[string]any)
			if m["id"] == id {
				if drop {
					continue
				}
				m["stage"], m["stage_set"], m["shown_name"] = "production", true, id
			}
			keep = append(keep, m)
		}
		d[key] = keep
		return d
	}
	after := func(post string, before, then any) map[string]any {
		return map[string]any{"$after": "^POST " + post, "$before": before, "$then": then}
	}
	laptop := "/api/config/profiles/qwen3-14b-laptop/promote"
	trial := "/api/config/profiles/qwen3-14b-128k/promote"
	fetch["POST "+laptop] = map[string]any{"revision": 413, "changed": true, "id": "qwen3-14b-laptop", "stage": "production"}
	fetch["POST "+trial] = map[string]any{"revision": 413, "changed": true, "id": "qwen3-14b-128k", "stage": "production"}
	profs := fetch["/api/profiles"]
	fetch["/api/profiles"] = after(laptop, profs, promoted(profs, "profiles", "qwen3-14b-laptop", false))
	tests := fetch["/api/profiles/test"]
	fetch["/api/profiles/test"] = after(trial, tests, promoted(tests, "profiles", "qwen3-14b-128k", true))
	confirm := func(sel string) []any {
		return []any{sel, map[string]any{"type": "textarea[aria-label=Comment]", "value": "in use"}, "div.dlg-foot button.pri"}
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "promote: Settings > Profiles names it without TEST - at once", "route": "#/profiles?show=all&q=laptop&profile=qwen3-14b-laptop", "status": st,
			"click": confirm("aside.side button[data-act=promote]"), "hash": "#/profiles?show=all&q=laptop&profile=qwen3-14b-laptop",
			"fetchedQuery": []string{`^POST ` + laptop + ` `, `^GET /api/profiles$`},
			"expect":       []string{"qwen3-14b-laptop"}, "none": []string{"tr[data-profile=qwen3-14b-laptop] span.plc-gen"},
			"pageHas": []string{"aside.side [data-stage=production]", "aside.side button[data-act=demote]"}, "notOnPage": []string{"TEST - qwen3-14b-laptop"}},
		map[string]any{"name": "promote: the Test profiles page drops the row at once", "route": "#/test-profiles?q=qwen3-14b-128k", "status": st,
			"click": confirm("tr[data-profile=qwen3-14b-128k] button[data-act=promote]"), "hash": "#/test-profiles?q=qwen3-14b-128k",
			"expect": []string{"No test profile matches"}, "notOnPage": []string{"TEST - Qwen 128k trial"}, "page": []string{"Promoted qwen3-14b-128k"}},
	}})

	// the Models panel: the catalog the server pushes after the promote
	mf := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	cat := mcpCopy(t, mf["/api/models"])
	for _, m := range cat["models"].([]any) {
		mm := m.(map[string]any)
		for _, p := range mm["profiles"].([]any) {
			if pp := p.(map[string]any); pp["id"] == "qwen3-14b" {
				pp["stage"], pp["stage_set"], pp["shown_name"] = "production", true, "qwen3-14b"
			}
		}
	}
	cat["version"] = 1 << 30
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": mf, "cases": []any{
		// before first: the catalog the live channel delivers stays for the cases after it
		map[string]any{"name": "promote: before it, the Models panel counts it as a test profile", "route": "#/models?model=qwen3-14b-Q5_K_M.gguf", "status": mf["/api/status"],
			"pageHas": []string{"aside.side p.mdl-tests[data-tests='1']"}, "pageNone": []string{"aside.side table.mdl-ptbl tr[data-profile=qwen3-14b]"}},
		map[string]any{"name": "promote: the Models panel moves it to the profiles, no TEST -", "route": "#/models?model=qwen3-14b-Q5_K_M.gguf", "status": mf["/api/status"],
			"wsAfter": []any{map[string]any{"type": "models", "data": cat}},
			"pageHas": []string{"aside.side table.mdl-ptbl tr[data-profile=qwen3-14b] [data-shown]"}, "pageNone": []string{"aside.side p.mdl-tests"},
			"notOnPage": []string{"TEST - qwen3-14b", "1 test profile"}},
	}})
}

// The standard dialog on rows as long as live's gemma-4-12b (every row with
// four notes; the v85 copy drew a 1,467 px dialog): only the rows scroll, the
// name and the intro stay above them, and what is written, the comment and
// the buttons stay below them, so Write is on screen without scrolling. The
// fake DOM has no layout: this checks the structure and the rules that pin
// it; the real browser's boxes at 1536 x 864 and 1280 x 800 are measured by
// the picture script (potluck-notes .../standard-set/shots-pin.cjs).
func TestUINextStandardDialogPinnedFooter(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	for _, rule := range []string{
		".dlg[data-dlg=standard]{overflow:hidden}",
		".dlg .std-body{flex:1 1 auto;min-height:0}",
		".std-body>*{flex:none}",
		".std-body>.std-rows{flex:0 1 auto;min-height:120px;overflow:auto;overscroll-behavior:contain;",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("app.css lacks %q", rule)
		}
	}
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	path := "/api/models/" + mdlLoaded + "/standard-set"
	fetch[path] = fixtureAnswers(t, []string{"--stdset", "long"}, path)[path]
	open := []any{"button[data-act=standard]"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "standard: long rows scroll, the footer does not", "route": "#/models/" + mdlLoaded, "status": fetch["/api/status"],
			"click": append(append([]any{}, open...), map[string]any{"type": "div.dlg textarea[aria-label=Comment]", "value": "x"}),
			"pageHas": []string{"div.dlg[data-state=ready] div.std-body div.std-rows[data-std-scroll] table.std-tbl", "div.std-body div.std-foot[data-std-foot] [data-to-write='1']",
				"div.std-foot textarea[aria-label=Comment]", "div.std-foot div.dlg-foot button[data-go]:not([disabled])", "div.std-body div.std-namef input.std-name",
				"tr[data-std-notes='gemma-4-27b:256k'] [data-need-note]", "tr[data-std-notes='gemma-4-27b:256k'] [data-shape-note]", "tr[data-std-notes='gemma-4-27b:256k'] [data-exists-note]", "tr[data-std-notes='gemma-4-27b:256k'] [data-host-memory]"},
			"pageNone": []string{"div.std-rows button", "div.std-rows textarea", "div.std-rows input.std-name", "div.std-rows [data-to-write]"},
			"page":     []string{"Write 1 profile"}},
	}})
}
