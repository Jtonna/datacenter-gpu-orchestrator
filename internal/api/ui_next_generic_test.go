package api

import (
	"fmt"
	"testing"
)

// Potluck is generic: the pages count machines as zones whatever their host
// names are (build-92, B3). A cluster whose every host name starts with ps5
// used to read "1 PS5"; it reads "1 zone" now, like any other cluster.
func TestUINextZonesCountWordIgnoresHostNames(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := mcpCopy(t, fetch["/api/status"])
	view, _ := st["view"].(map[string]any)
	nodes, _ := view["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("the fixture's status has no view.nodes")
	}
	for i, n := range nodes {
		if m, ok := n.(map[string]any); ok {
			m["hostname"] = fmt.Sprintf("ps5-%02d", i+1)
		}
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: every host named ps5-*, still zones", "route": "#/models/" + mdlOlder, "status": st,
			"expect": []string{"would load on 1 zone"}, "has": []string{"span[data-would='1']"},
			"notOnPageRe": []string{`\b1 PS5\b`, `PS5s`}},
	}})
}

// The Settings texts name the fleet node source as what it is: the legacy
// fleet feed (membership: fleet), never a "fleet console"; the memory floor's
// risk speaks of a shared-memory machine, not a console (build-92, B7).
func TestUINextSettingsGenericWords(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--config", "normal"}, settingsPaths...)
	row := func(key string) string { return "div.setp-row[data-key=" + key + "]" }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "settings: the fleet group with agents, not used", "route": "#/settings/fleet",
			"page":      []string{"Not used in this mode: the zones come from agent registrations, not from the legacy fleet feed."},
			"notOnPage": []string{"fleet console"}, "brandFree": true},
		map[string]any{"name": "settings: the memory floor's risk", "route": "#/settings/safety", "click": []string{row("load_memory_floor_mib") + " button.setp-more"},
			"page":      []string{"a load that is eating a shared-memory machine is no longer stopped in time, and the machine can crash."},
			"brandFree": true},
		map[string]any{"name": "settings: every group brand free", "route": "#/settings?q=e", "brandFree": true},
	}})
	// membership fleet: the group's own line names the legacy feed
	fleet := mcpCopy(t, fetch)
	set, _ := fleet["/api/config/settings"].(map[string]any)
	if set == nil {
		t.Fatal("the fixture has no /api/config/settings object")
	}
	if inner, ok := set["settings"].(map[string]any); ok {
		set = inner
	}
	set["membership"] = "fleet"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fleet, "cases": []any{
		map[string]any{"name": "settings: the fleet group, legacy feed", "route": "#/settings/fleet",
			"page": []string{"The legacy fleet feed that lists the zones (membership: fleet)."}, "notOnPage": []string{"Not used in this mode", "fleet console"},
			"brandFree": true},
	}})
}
