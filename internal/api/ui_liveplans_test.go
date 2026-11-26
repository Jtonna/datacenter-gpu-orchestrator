package api

import (
	"strings"
	"testing"
)

// The page subscribes the live channel to "models" (the model catalog, for
// which the server computes every profile's plan) only while the open screen
// draws the catalog live: Models, a model's page, Deleted models, Profiles,
// Placement. Overview, Requests and Benchmarks take one copy for the model
// names; every other screen none. Leaving a screen that asked lets the
// topic go.
func TestUILiveCatalogOnlyWhereShown(t *testing.T) {
	e := newEnv(t, chatOK)
	st := jsonOf(t, body(t, e.get("/api/status")))
	f := map[string]any{
		"/api/models":   map[string]any{"version": 1, "models": []any{}},
		"/api/profiles": map[string]any{"profiles": []any{}},
	}
	without := func(route string) map[string]any {
		return map[string]any{"name": route + ": no models topic", "route": route, "status": st,
			"subscribed": []string{"status", "event", "bench", "config"}, "notSubscribed": []string{"models"}}
	}
	with := func(route string) map[string]any {
		return map[string]any{"name": route + ": the models topic", "route": route, "status": st, "subscribed": []string{"status", "models"}}
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "wsOpen": true, "fetch": f, "cases": []any{
		without("#/zones"),
		with("#/models"),
		with("#/models?model=a.gguf"), // the same screen, a panel: still subscribed
		without("#/overview"),         // names only
		without("#/activity"),
		with("#/profiles"),
		without("#/requests"), // names only
		with("#/placement"),
		without("#/benchmarks"), // names only
		without("#/nodes"),
	}})
	for _, f := range []string{"next/views/overview.js", "next/views/requests.js", "next/views/benchmarks.js"} {
		if !strings.Contains(uiText(t, f), "needCatalog({ live: false })") {
			t.Errorf("%s asks for the live catalog: it draws only model names", f)
		}
	}
	if live := uiText(t, "next/core/live.js"); strings.Contains(live, "'config', 'models']") {
		t.Error("next/core/live.js subscribes to models on every screen")
	}
}
