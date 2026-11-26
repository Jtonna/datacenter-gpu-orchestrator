package api

import "testing"

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
// profiles" and its dialog's states (checking, ready, nothing, refused,
// error, written).
func TestUINextModelsStandardSet(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	std := fixtureAnswers(t, []string{"--stdset", "ready"}, "/api/models/"+mdlLoaded+"/standard-set")["/api/models/"+mdlLoaded+"/standard-set"]
	none := fixtureAnswers(t, []string{"--stdset", "nothing"}, "/api/models/"+mdlLoaded+"/standard-set")["/api/models/"+mdlLoaded+"/standard-set"]
	path := "/api/models/" + mdlLoaded + "/standard-set"
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
	open := []any{"aside.side button[data-act=standard]"}
	written := map[string]any{"revision": 413, "changed": true, "written": []any{"gemma-4-27b:16k", "gemma-4-27b:64k", "gemma-4-27b:128k"},
		"skipped": []any{map[string]any{"id": "gemma-4-27b", "reason": "exists, kept as it is"}, map[string]any{"id": "gemma-4-27b:256k", "reason": "not kept: memory need unknown"}}}
	refused := map[string]any{"$status": 409, "$body": map[string]any{"error": map[string]any{"message": "configuration changed since revision 412 (now 413): read it again", "code": "stale"}}}
	failed := map[string]any{"$status": 503, "$body": map[string]any{"error": map[string]any{"message": "the scheduler is shutting down", "code": "shutting_down"}}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: a test profile is one line to the Test profiles page", "route": "#/models?model=qwen3-14b-Q5_K_M.gguf", "status": st,
			"page":     []string{"1 test profile"},
			"pageHas":  []string{"aside.side p.mdl-tests[data-tests='1'] a[href^='#/test-profiles?model=']"},
			"pageNone": []string{"aside.side details.mdl-tests", "aside.side table.mdl-ptbl tr[data-profile=qwen3-14b]"}},
		map[string]any{"name": "models: the button", "route": "#/models?model=" + mdlLoaded, "status": st,
			"pageHas": []string{"aside.side button[data-act=standard]"}, "page": []string{"Make standard profiles"}},
		map[string]any{"name": "standard: checking", "route": "#/models?model=" + mdlLoaded, "status": st, "fetch": with(map[string]any{"$hang": true}, nil),
			"click": open, "pageHas": []string{"div.dlg[data-dlg=standard][data-state=checking]"}, "page": []string{"Planning each context"}},
		map[string]any{"name": "standard: ready, the read call's table", "route": "#/models?model=" + mdlLoaded, "status": st, "fetch": with(std, nil),
			"click": open,
			"pageHas": []string{"div.dlg[data-dlg=standard][data-state=ready]", "tr[data-std='gemma-4-27b'][data-exists=true]", "tr[data-std='gemma-4-27b:256k'][data-kept=false]", "tr[data-std-notes='gemma-4-27b:256k'] [data-why]",
				"tr[data-std-notes='gemma-4-27b:128k'] [data-every-zone]", "tr[data-std-notes='gemma-4-27b:64k'] [data-now=evict]", "tr[data-std-notes='gemma-4-27b:128k'] [data-host-memory=not-counted]", "[data-to-write='3']", "div.dlg-foot button[data-go][disabled]"},
			"page": []string{"exists, kept as it is", "Not kept", "fits only by using every zone", "up to 10,240 MiB, not counted",
				"the default context", "measured for this shape", "derived from another context", "Write 3 profiles", "Comment (required)", "now it would first unload gemma-4-12b"}},
		map[string]any{"name": "standard: the write sends the comment", "route": "#/models?model=" + mdlLoaded, "status": st, "fetch": with(std, written),
			"click":        append(append([]any{}, open...), map[string]any{"type": "div.dlg textarea[aria-label=Comment]", "value": "standard set for gemma"}, "div.dlg-foot button[data-go]"),
			"fetchedQuery": []string{`^POST /api/models/` + mdlLoaded + `/standard-set \{"comment":"standard set for gemma"\}$`},
			"pageHas":      []string{"div.dlg[data-dlg=standard][data-state=written] [data-written='3']"}, "page": []string{"Wrote 3 profiles", "gemma-4-27b: exists, kept as it is"}},
		map[string]any{"name": "standard: refused", "route": "#/models?model=" + mdlLoaded, "status": st, "fetch": with(std, refused),
			"click":   append(append([]any{}, open...), map[string]any{"type": "div.dlg textarea[aria-label=Comment]", "value": "x"}, "div.dlg-foot button[data-go]"),
			"pageHas": []string{"div.dlg[data-dlg=standard][data-state=refused] [data-refused]"}, "page": []string{"Not written: nothing changed.", "read it again", "Check again"}},
		map[string]any{"name": "standard: nothing to write", "route": "#/models?model=" + mdlLoaded, "status": st, "fetch": with(none, nil),
			"click": open, "pageHas": []string{"div.dlg[data-dlg=standard][data-state=nothing] [data-to-write='0']"}, "pageNone": []string{"div.dlg-foot button[data-go]"},
			"page": []string{"Nothing to write"}},
		map[string]any{"name": "standard: the read fails", "route": "#/models?model=" + mdlLoaded, "status": st, "fetch": with(failed, nil),
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
