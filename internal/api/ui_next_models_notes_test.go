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
