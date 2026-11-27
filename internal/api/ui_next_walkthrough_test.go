package api

import (
	"testing"
	"time"
)

// The gear opens the cluster's own Settings page, not Profiles (the owner's
// walkthrough, build-92 W4); its menu reads Settings, Profiles, Test
// profiles, Tiers and zones, Placement preview, Audit, About; the old
// addresses of every page under it still open that page.
func TestUINextSettingsGear(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--config", "normal"}, settingsPaths...)
	st := ovStatus(time.Now())
	menu := "nav[aria-label^=Settings] a.nd b"
	order := []string{"Settings", "Profiles", "Test profiles", "Tiers and zones", "Placement preview", "Audit", "About"}
	cases := []any{
		map[string]any{"name": "gear: opens Settings", "route": "#/overview", "status": st, "click": []string{"button.rb[data-section=settings]"},
			"hash":    "#/settings",
			"pageHas": []string{"button.rb[data-section=settings][aria-current=page]", "nav[aria-label^=Settings] a.nd.on[href='#/settings']"},
			"expect":  []string{"Timing"}},
		map[string]any{"name": "gear: the menu's order", "route": "#/settings", "status": st, "pageSeq": map[string]any{menu: order}},
	}
	// every page of the section keeps its address and stays in the gear's section
	for _, r := range []string{"#/settings", "#/profiles", "#/test-profiles", "#/tiers", "#/placement", "#/audit", "#/about"} {
		cases = append(cases, map[string]any{"name": "gear: old address " + r, "route": r, "status": st,
			"hash": r, "pageHas": []string{"button.rb[data-section=settings][aria-current=page]", "nav[aria-label^=Settings] a.nd.on[href='" + r + "']"},
			"notOnPage": []string{"There is no such screen"}})
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": cases})
}
