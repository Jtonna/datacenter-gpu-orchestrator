package api

import "testing"

// A command profile (kind command: it starts a program, it loads no model
// file) reads "command profile" in the Model column of the Profiles list,
// with no context size, instead of two dashes.
func TestUINextProfilesCommandRow(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles", "/api/models", "/api/config", "/api/config/schema", "/api/status")
	st := fetch["/api/status"]
	doc := fetch["/api/profiles"].(map[string]any)
	list := append([]any{}, doc["profiles"].([]any)...)
	list = append(list, map[string]any{"id": "image-gen", "kind": "command", "model": "", "ctx": 0, "parallel": 1, "aliases": []any{}, "force_nodes": []any{},
		"plan": map[string]any{"action": "start", "nodes": []any{}}})
	fetch["/api/profiles"] = map[string]any{"profiles": list}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "profiles: a command profile row", "route": "#/profiles?q=image-gen", "status": st,
			"expect": []string{"image-gen", "command profile"}, "has": []string{"tr[data-profile=image-gen] td[data-cmdprof]"},
			"titles": []string{"A command profile starts a program on a machine"}},
	}})
}
