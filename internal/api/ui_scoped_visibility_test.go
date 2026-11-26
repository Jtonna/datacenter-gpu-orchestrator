package api

import (
	"encoding/json"
	"testing"
)

// deepCopy is v through JSON (the fixture answers are plain JSON values).
func deepCopy(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	json.Unmarshal(b, &out)
	return out
}

// A run-scoped instance of a model counts only for itself on the Models
// page: the model whose only instance is a test run keeps production's
// state (Not loaded, never "Ready on 1 zone"), its row says "test run e2 on
// ps5-03" and no "in use on", and its panel lists the instance under Loaded
// now, marked as a test run of its scope. The Loaded filter still shows it.
func TestUIScopedInstanceCountsForItself(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	models := deepCopy(t, fetch["/api/models"]).(map[string]any)
	st := deepCopy(t, fetch["/api/status"]).(map[string]any)
	var prod string
	for _, x := range models["models"].([]any) {
		m := x.(map[string]any)
		if m["key"] != mdlNever && m["file"] != mdlNever {
			continue
		}
		prod = m["profiles"].([]any)[0].(map[string]any)["id"].(string)
		m["instances"] = []any{map[string]any{"profile": prod + "@e2", "scope": "e2", "production": prod, "state": "ready", "nodes": []any{"ps5-03"},
			"memory_mib": 3071, "in_flight": 0, "waiters": 0, "requests_served": 2}}
	}
	if prod == "" {
		t.Fatal("no never-used model in the fixture")
	}
	var node any
	for _, x := range st["view"].(map[string]any)["nodes"].([]any) {
		if n := x.(map[string]any); n["hostname"] == "ps5-03" {
			node = n["id"]
		}
	}
	insts, _ := st["instances"].([]any)
	st["instances"] = append(insts, map[string]any{"profile": prod + "@e2", "production": prod, "scope": "e2", "state": "ready", "model_file": mdlNever,
		"nodes": []any{map[string]any{"id": node}}, "in_flight": 0, "waiters": 0, "requests_served": 2})
	fetch["/api/models"], fetch["/api/status"] = models, st
	row := "div.mr[data-model='" + mdlNever + "']"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "scoped: the row says the test run, not in use", "route": "#/models", "status": st,
			"has":  []string{row + " span.mdl-inuse[data-inuse]"},
			"none": []string{row + " span.badge.ready"}},
		map[string]any{"name": "scoped: the row's words", "route": "#/models?q=" + mdlNever, "status": st,
			"expect": []string{"test run e2 on ps5-03"}, "absent": []string{"in use on ps5-03", "Ready on"}},
		map[string]any{"name": "scoped: the Loaded filter shows it", "route": "#/models?show=loaded", "status": st, "expect": []string{mdlNever}},
		map[string]any{"name": "scoped: the panel", "route": "#/models?model=" + mdlNever, "status": st,
			"page":      []string{"Loaded now", prod + "@e2", "Test run e2", "Not loaded"},
			"pageHas":   []string{"aside.side div.mdl-inst[data-profile='" + prod + "@e2'] span.mdl-test[data-scope=e2]"},
			"notOnPage": []string{"Ready on"},
			"pageNone":  []string{"aside.side tr[data-profile='" + prod + "'] span.mdl-pill"}},
	}})
}
