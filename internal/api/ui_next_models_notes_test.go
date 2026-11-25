package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The owner's notes on the Models page image (25 Nov 2026). They overrule
// the board where they differ. One test per note.

// Note 2: the section menu has no health card ("8 of 8 reachable ·
// Updated …") on any page: the heart in the rail carries it. The menu's
// foot keeps its slot (the Benchmarks run card).
func TestUINextNotesMenuHasNoHealthCard(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	cases := []any{}
	for _, r := range []string{"#/overview", "#/models", "#/zones", "#/benchmarks", "#/profiles", "#/settings"} {
		cases = append(cases, map[string]any{"name": "menu without the health card: " + r, "route": r, "status": st,
			"pageNone": []string{".menu .fleet", ".fleet"}, "notOnPage": []string{"reachable · ", "Updated 0 s ago"},
			"pageHas": []string{".menu .menu-foot .slotbox", "button.rb[data-surface=health]"}})
	}
	// the Benchmarks run card stays at the menu's foot while a run goes
	running := map[string]any{}
	for k, v := range fetch {
		running[k] = v
	}
	running["/api/bench/state"] = map[string]any{"run": map[string]any{"id": "run-1", "state": "running", "suite_id": "cold-warm", "suite_name": "Cold and warm loads", "percent": 40,
		"started_at": 1764057600000, "current": map[string]any{"name": "Load on two consoles"}}}
	cases = append(cases, map[string]any{"name": "menu: the run card stays on Benchmarks", "route": "#/benchmarks", "status": st, "fetch": running,
		"pageHas": []string{".menu .menu-foot .slotbox a.run-ind"}, "pageNone": []string{".fleet"}, "page": []string{"Benchmark running", "Cold and warm loads"}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": cases})
}

// Note 1: "Downloading" and "Pinned" show only when something is
// downloading or pinned: no empty group, no "not built yet" line.
func TestUINextNotesOnlyGroupsWithRows(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 1: two pinned models, nothing downloading", "route": "#/models", "status": st,
			"has":  []string{"div.sech[data-sec=pinned]", "div.mdl-rows[data-pinned-rows]", "div.sech[data-sec=all]"},
			"none": []string{"div.sech[data-sec=downloading]"}, "absent": []string{"Downloading", "not built yet"}},
		map[string]any{"name": "notes 1: no pinned model in the search", "route": "#/models?q=nomic", "status": st,
			"none": []string{"div.sech[data-sec=pinned]", "div.sech[data-sec=downloading]"}, "absent": []string{"No pinned model", "The star on a row pins it"}},
	}})
}

// Note 4: no page header line ("Models · 0 ready · 0 loading · 16 in the
// catalog"); the page shows its content only. A screen reader still gets
// the page's name.
func TestUINextNotesNoPageHeader(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 4: no header line", "route": "#/models", "status": st,
			"none": []string{"div.page-head"}, "has": []string{"h1.sr"}, "absent": []string{"in the catalog", " ready · "}},
	}})
}

// Note 5: the Models entry of the section menu says plainly what is there.
func TestUINextNotesMenuWords(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 5: the Models entry", "route": "#/models", "status": fetch["/api/status"],
			"pageHas": []string{".menu a.nd.on"}, "page": []string{"The models available to you"}, "notOnPage": []string{"Model files, what is loaded, and their profiles"}},
	}})
}

// Note 7: Needs is written "~18.0 GB" (a tilde: approximate; one decimal)
// with no "measured" under it in the row; how it is known is on hover and
// in the panel.
func TestUINextNotesNeedsTilde(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	row := func(k string) string { return "div.mr[data-model='" + k + "'] span.mdl-needs" }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 7: the rows", "route": "#/models", "status": st,
			"expect": []string{"~17.1 GB", "~5.1 GB"}, "absent": []string{"measured", "estimated"},
			"has":  []string{row(mdlLoaded) + " span[data-basis=measured][title*=measured]", row(mdlNever) + " span[data-basis=estimated][title*=estimated]"},
			"none": []string{row(mdlLoaded) + " span.sub"}},
		map[string]any{"name": "notes 7: the panel says how it is known", "route": "#/models?model=" + mdlLoaded, "status": st,
			"page": []string{"needs ~17.1 GB", "(measured)"}},
	}})
	// One decimal only: the two-decimal choice drawn for the owner is gone,
	// switch and all (the reviewer, 25 Nov).
	js, err := os.ReadFile(filepath.Join("ui", "next", "views", "models.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), "NEED_DECIMALS") || strings.Contains(string(js), "18.48") {
		t.Error("models.js still carries the two-decimal Needs choice")
	}
}

// Note 9: a model's profiles are its PRODUCTION profiles (the interim rule
// until the stored TEST / PRODUCTION field: written by hand and placed
// automatically). The rest, pinned to zones or generated by a benchmark,
// is one collapsed line, never in the count: in the row, the panel and the
// delete dialog (which names the production profiles and counts the rest).
func TestUINextNotesProductionProfiles(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--trash", "two"}, "/api/models", "/api/models/trash", "/api/status")
	st := fetch["/api/status"]
	doc := mcpCopy(t, fetch["/api/models"])
	ids := []string{"gemma-4-12b", "gemma-4-12b:64k"}
	for _, m := range doc["models"].([]any) {
		mm := m.(map[string]any)
		if mm["key"] != mdlOlder {
			continue
		}
		ps := mm["profiles"].([]any)
		for i := 0; i < 17; i++ {
			id := "bench-gemma-4-12b-c" + string(rune('a'+i))
			ps = append(ps, map[string]any{"id": id, "ctx": 8192, "parallel": 1, "generated": true, "force_nodes": []any{}, "needs_mib": 8400, "plan": map[string]any{"would_start": true}})
			ids = append(ids, id)
		}
		ps = append(ps, map[string]any{"id": "gemma-4-12b:ps5-03", "ctx": 8192, "parallel": 1, "generated": false, "force_nodes": []any{"ps5-03"}, "placement": "forced", "needs_mib": 8400, "plan": map[string]any{"would_start": true}})
		ids = append(ids, "gemma-4-12b:ps5-03")
		mm["profiles"], mm["profile_count"] = ps, len(ps)
	}
	fetch["/api/models"] = doc
	fetch["POST /api/models/delete/"+mdlOlder] = map[string]any{"$match": []any{[]any{reDryPlain, trashDry(mdlOlder, 7300000000, ids, nil)}}}
	row := "div.mr[data-model='" + mdlOlder + "']"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 9: the row counts production profiles", "route": "#/models", "status": st,
			"has":    []string{row + " span.mdl-pc[data-prod='2']", "div.mr[data-model='" + mdlRecent + "'] span.mdl-pc[data-prod='0']"},
			"absent": []string{"20 profiles", "19 profiles"}},
		map[string]any{"name": "notes 9: the panel lists production, the rest collapsed", "route": "#/models?model=" + mdlOlder, "status": st,
			"page": []string{"2 profiles", "18 test and benchmark configurations"},
			"pageHas": []string{"aside.side table.mdl-ptbl tr[data-profile='gemma-4-12b']", "aside.side details.mdl-tests summary",
				"aside.side details.mdl-tests:not([open])", "aside.side details.mdl-tests tr[data-profile='gemma-4-12b:ps5-03']"},
			"pageNone":  []string{"aside.side table.mdl-ptbl tr[data-profile='gemma-4-12b:ps5-03']", "aside.side table.mdl-ptbl tr[data-profile='bench-gemma-4-12b-ca']"},
			"notOnPage": []string{"20 profiles", "generated configuration"}},
		map[string]any{"name": "notes 9: a model with only test configurations", "route": "#/models?model=" + mdlRecent, "status": st,
			"page": []string{"No profiles", "1 test and benchmark configuration"}, "pageNone": []string{"aside.side table.mdl-ptbl"}},
		map[string]any{"name": "notes 9: the delete dialog names production, counts the rest", "route": "#/models?model=" + mdlOlder, "status": st,
			"click":     []any{"aside.side button[data-act=delete]"},
			"pageHas":   []string{"div.dlg[data-dlg=delete][data-state=ready] [data-profiles]"},
			"page":      []string{"2 profiles", "gemma-4-12b:64k", "18 test and benchmark configurations"},
			"notOnPage": []string{"all 20 profiles", "20 profiles come back"}},
	}})
}

// Note 3: no "Nothing is loaded right now" banner; a model's row says when
// it is in use and on which zones, each zone a link to Zones with that
// zone selected.
func TestUINextNotesRowInUse(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	idle := mcpCopy(t, st)
	idle["instances"] = []any{}
	row := "div.mr[data-model='" + mdlLoaded + "']"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 3: no banner when nothing is loaded", "route": "#/models", "status": idle,
			"absent": []string{"Nothing is loaded right now", "A model loads when a request asks for it"}},
		map[string]any{"name": "notes 3: the row says in use and where", "route": "#/models", "status": st,
			"has":  []string{row + " [data-inuse] a.mdl-zone[href='#/zones/ps5-01']", row + " [data-inuse] a.mdl-zone[href='#/zones/ps5-02']"},
			"none": []string{"div.mr[data-model='" + mdlRecent + "'] [data-inuse]"}, "expect": []string{"in use on"}},
		// (the fake DOM neither follows a link nor bubbles a click: that the link opens Zones and not the row's panel is checked in Chrome)
	}})
}

// Note 6: the shared side panel is resizable by its edge while it is docked
// (1400 px and wider): a focusable handle, arrow keys resize, a double
// click resets; the width is remembered per panel kind; within a minimum
// and a maximum. Over the page (below 1400 px) there is no handle.
func TestUINextNotesPanelResize(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	grip := "aside.side.docked .side-grip[role=separator][tabindex='0'][aria-orientation=vertical]"
	open := "#/models?model=" + mdlRecent
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 6: docked, a handle at the default width", "route": open, "status": st, "width": 1536,
			"pageHas": []string{grip + "[aria-valuenow='620'][aria-valuemin='520']", "aside.side[data-w='620']"}},
		map[string]any{"name": "notes 6: arrow keys resize (left widens)", "route": open, "status": st, "width": 1536,
			"click": []string{"aside.side .side-grip"}, "keys": []any{"ArrowLeft", "ArrowLeft", "ArrowLeft", "ArrowRight"},
			"pageHas": []string{"aside.side[data-w='652']", grip + "[aria-valuenow='652']"}},
		map[string]any{"name": "notes 6: the width is remembered for the next panel of the kind", "route": open, "status": st, "width": 1536,
			"pageHas": []string{"aside.side[data-w='652']"}},
		map[string]any{"name": "notes 6: never below the minimum", "route": open, "status": st, "width": 1536,
			"click": []string{"aside.side .side-grip"}, "keys": []any{"Home", "ArrowRight", "ArrowRight"},
			"pageHas": []string{"aside.side[data-w='520']"}},
		map[string]any{"name": "notes 6: never above the maximum", "route": open, "status": st, "width": 1536,
			"click": []string{"aside.side .side-grip"}, "keys": []any{"End", "ArrowLeft"},
			"pageHas": []string{"aside.side[data-w='916']", grip + "[aria-valuemax='916']"}},
		map[string]any{"name": "notes 6: a double click resets", "route": open, "status": st, "width": 1536,
			"click": []any{map[string]any{"dbl": "aside.side .side-grip"}}, "pageHas": []string{"aside.side[data-w='620']"}},
		map[string]any{"name": "notes 6: over the page, no handle", "route": open, "status": st, "width": 1280,
			"pageHas": []string{"aside.side"}, "pageNone": []string{".side-grip", "aside.side[data-w]"}},
	}})
}

// Note 8: the side panel goes deeper without leaving it. (a) The Benchmarks
// count opens the model's runs and reports in the panel, each opening its
// report page, with a way back. (b) A configuration is edited in the panel
// with the profile editor (views/profiles.js), saved through the review
// (dry run, a required comment, If-Match), then its plan is asked again
// (POST /api/plan: it cannot preview unsaved values, which the panel says).
func TestUINextNotesPanelDeeper(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status", "/api/config", "/api/config/schema")
	st := fetch["/api/status"]
	fetch["/api/bench/runs"] = map[string]any{"runs": []any{
		map[string]any{"id": "run-20261124T0900-aa01", "kind": "suite", "suite_id": "cold-warm", "suite_name": "Cold and warm loads", "state": "completed", "trigger": "manual",
			"queued_at": 1764061200000, "started_at": 1764061200000, "finished_at": 1764061800000, "summary": map[string]any{"steps": 6, "ran": 6, "passed": 6, "failed": 0}},
		map[string]any{"id": "run-20261123T0900-aa02", "kind": "benchmark", "benchmark_id": "gemma27-2con", "suite_name": "", "state": "failed", "trigger": "schedule",
			"queued_at": 1763974800000, "started_at": 1763974800000, "finished_at": 1763975100000, "error": "a step failed", "summary": map[string]any{"steps": 3, "ran": 3, "passed": 2, "failed": 1}},
	}}
	fetch["PUT /api/config/profiles/gemma-4-12b"] = map[string]any{"changed": true, "revision": 413, "diff": []any{map[string]any{"path": "profiles.gemma-4-12b.ctx", "old": 8192, "new": 16384}}}
	fetch["POST /api/plan"] = map[string]any{"model": "gemma-4-12b", "action": "start", "nodes": []any{"ps5-03"}, "reason": ""}
	open := "#/models?model=" + mdlLoaded
	older := "#/models?model=" + mdlOlder
	editBtn := "aside.side tr[data-profile='gemma-4-12b'] button[data-act=edit]"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 8a: Benchmarks opens the runs in the panel", "route": open, "status": st, "width": 1536,
			"click":        []string{"aside.side button[data-act=bench]"},
			"fetchedQuery": []string{`^GET /api/bench/runs\?model=gemma-4-27b-it-Q4_K_M\.gguf`},
			"hash":         open,
			"page":         []string{"Benchmarks", "2 runs", "Cold and warm loads", "gemma27-2con", "Back to " + mdlLoaded},
			"pageHas": []string{"aside.side a.mdl-run[href='#/benchmarks/runs/run-20261124T0900-aa01']", "aside.side a.mdl-run[href='#/benchmarks/runs/run-20261123T0900-aa02']",
				"aside.side button[data-back]"},
			"pageNone": []string{"aside.side section.mdl-cfg"}},
		map[string]any{"name": "notes 8a: back to the panel's main view", "route": open, "status": st, "width": 1536,
			"click": []string{"aside.side button[data-act=bench]", "aside.side button[data-back]"}, "pageHas": []string{"aside.side section.mdl-cfg"}, "pageNone": []string{"aside.side a.mdl-run"}},
		map[string]any{"name": "notes 8b: Edit opens the editor in the panel", "route": older, "status": st, "width": 1536,
			"click": []string{editBtn}, "hash": older,
			"page":     []string{"Edit gemma-4-12b", "Context size", "Parallel slots", "Description", "Save", "Cancel", "cannot show unsaved values"},
			"pageHas":  []string{"aside.side section.prf-editor.prf-inpanel", "aside.side [data-plan-preview]", "aside.side button[data-back]"},
			"pageNone": []string{"aside.side a[href^='#/profiles/']", "aside.side section.mdl-cfg"}},
		map[string]any{"name": "notes 8b: Cancel goes back", "route": older, "status": st, "width": 1536,
			"click": []string{editBtn, "aside.side button[data-cancel]"}, "pageHas": []string{"aside.side section.mdl-cfg"}, "pageNone": []string{"aside.side section.prf-editor"}},
		map[string]any{"name": "notes 8b: no save without a comment", "route": older, "status": st, "width": 1536,
			"click":        []any{editBtn, "aside.side button[data-save]", "div.dlg-foot button.pri"},
			"fetchedQuery": []string{`^PUT /api/config/profiles/gemma-4-12b\?dry_run=1 \{"profile":\{.*\}\}$`},
			"notFetched":   []string{`^PUT /api/config/profiles/gemma-4-12b \{`}},
		map[string]any{"name": "notes 8b: save with a comment, then the plan", "route": older, "status": st, "width": 1536,
			"click": []any{editBtn, "aside.side button[data-save]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "longer context"}, "div.dlg-foot button.pri"},
			"fetchedQuery": []string{`^PUT /api/config/profiles/gemma-4-12b\?dry_run=1 \{"profile":\{.*\}\}$`, `^PUT /api/config/profiles/gemma-4-12b \{"profile":\{.*\},"comment":"longer context"\}$`,
				`^POST /api/plan \{"model":"gemma-4-12b"\}$`},
			"hash": older, "page": []string{"Saved gemma-4-12b", "would load on 1 zone"}, "pageHas": []string{"aside.side [data-saved]", "aside.side section.mdl-cfg"}},
	}})
}
