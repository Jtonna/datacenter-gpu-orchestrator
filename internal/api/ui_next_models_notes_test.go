package api

import "testing"

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
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": cases})
}

// Note 1: "Downloading" and "Pinned" show only when something is
// downloading or pinned: no empty group, no "not built yet" line.
func TestUINextNotesOnlyGroupsWithRows(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "notes 1: two pinned models, nothing downloading", "route": "#/models", "status": st,
			"has": []string{"div.sech[data-sec=pinned]", "div.mdl-rows[data-pinned-rows]", "div.sech[data-sec=all]"},
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
			"has":    []string{row(mdlLoaded) + " span[data-basis=measured][title*=measured]", row(mdlNever) + " span[data-basis=estimated][title*=estimated]"},
			"none":   []string{row(mdlLoaded) + " span.sub"}},
		map[string]any{"name": "notes 7: the panel says how it is known", "route": "#/models?model=" + mdlLoaded, "status": st,
			"page": []string{"needs ~17.1 GB", "(measured)"}},
	}})
}
