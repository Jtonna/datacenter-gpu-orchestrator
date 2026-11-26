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

// One open of the Models page costs one round of plans: the catalog is read
// once over REST, the disk figures with GET /api/models?only=disk (no plans),
// and the live channel's "models" subscribe waits for the REST answer (the
// server then reuses the plans it just computed; a subscribe sent first
// computed them once for the channel and the REST read once more).
func TestUIModelsOpenReadsCatalogOnce(t *testing.T) {
	e := newEnv(t, chatOK)
	st := jsonOf(t, body(t, e.get("/api/status")))
	cat := map[string]any{"version": 1, "models": []any{}}
	disk := map[string]any{"disk": map[string]any{"state": "ok", "total_bytes": 240e9, "used_bytes": 50e9}}
	both := map[string]any{"$match": []any{[]any{`only=disk`, disk}, []any{`.`, cat}}}
	// the REST answer still on its way: no models topic yet
	uiExec(t, map[string]any{"shell": "next", "app": true, "wsOpen": true, "fetch": map[string]any{"/api/profiles": map[string]any{"profiles": []any{}},
		"/api/models": map[string]any{"$match": []any{[]any{`only=disk`, disk}, []any{`.`, map[string]any{"$hang": true}}}}}, "cases": []any{
		map[string]any{"name": "models: the subscribe waits for the catalog's REST answer", "route": "#/models", "status": st,
			"subscribed": []string{"status"}, "notSubscribed": []string{"models"},
			"fetchedCount": map[string]any{`^GET /api/models$`: 1, `^GET /api/models\?only=disk$`: 1}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "wsOpen": true, "fetch": map[string]any{"/api/profiles": map[string]any{"profiles": []any{}}, "/api/models": both}, "cases": []any{
		map[string]any{"name": "models: one catalog read, the disk alone, then the models topic", "route": "#/models", "status": st,
			"subscribed":   []string{"status", "models"},
			"fetchedCount": map[string]any{`^GET /api/models$`: 1, `^GET /api/models\?only=disk$`: 1}},
	}})
}

// After the scheduler restarts (a new image), a tab that stayed open
// reconnects to a server whose catalog versions start again. The tab kept
// the old version, ignored the new server's full list as "older", and then
// read every change as not fitting its copy: each change made it read the
// whole catalog again over REST, a fresh round of every plan (live v84, the
// owner's tab open across the switch: one more round on every page open
// anywhere). A new server (its hello's started_at) resets the copy's
// version, so its full list is taken and its changes apply.
func TestUICatalogAfterAServerRestart(t *testing.T) {
	e := newEnv(t, chatOK)
	st := jsonOf(t, body(t, e.get("/api/status")))
	topics := []string{"status", "event", "bench", "config", "ping", "models", "requests", "health", "groups"}
	hello := func(at int) map[string]any {
		return map[string]any{"type": "hello", "data": map[string]any{"started_at": at, "topics": topics, "version": "v"}}
	}
	full := func(v int) map[string]any {
		return map[string]any{"type": "models", "data": map[string]any{"version": v, "models": []any{}}}
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "wsOpen": true, "fetch": map[string]any{"/api/profiles": map[string]any{"profiles": []any{}},
		"/api/models": map[string]any{"version": 500, "models": []any{}}}, "cases": []any{
		map[string]any{"name": "a change from the restarted server applies without a REST read", "route": "#/models", "status": st,
			"ws": []any{hello(1000), full(500), hello(2000), full(3),
				map[string]any{"type": "models", "data": map[string]any{"base": 3, "version": 4, "changed": []any{}, "removed": []any{}, "order": []any{}}}},
			"fetchedCount": map[string]any{`^GET /api/models$`: 1}},
	}})
}
