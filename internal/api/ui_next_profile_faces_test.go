package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shared profile panel (views/profiles.js openProfilePanel, on Profiles
// and on Test profiles): mono is for ids, file names, flags and figures; every
// other value is sans at the body weight (the reviewer, build-92). The values
// sit in dl.prf-facts, whose dd is sans at --w-body; a figure or an id is a
// span.mono of its own.
func TestUINextProfilePanelFaces(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles/test", "/api/profiles", "/api/models", "/api/status")
	st := fetch["/api/status"]
	const tp, prod = "qwen3-14b-128k", "gemma-4-27b"
	for _, x := range jlist(jmap(fetch["/api/profiles"])["profiles"]) {
		if jstr(jmap(x)["id"]) == tp {
			jmap(x)["ttl_s"] = 900
		}
	}
	fetch["/api/config/profiles/"+tp] = map[string]any{"revision": 412, "id": tp, "profile": map[string]any{"kind": "llama", "model": "qwen3-14b-Q5_K_M.gguf", "ctx": 131072,
		"stage": "test", "extra_args": []string{"--ctx-checkpoints", "3", "--jinja"}}}
	fetch["/api/config/profiles/"+prod] = map[string]any{"revision": 412, "id": prod, "profile": map[string]any{"kind": "llama", "model": "gemma-4-27b-it-Q4_K_M.gguf", "ctx": 32768}}
	f := "aside.side dl.prf-facts "
	cases := []any{}
	for _, c := range []struct{ name, route string }{
		{"Test profiles", "#/test-profiles?q=" + tp + "&profile=" + tp},
		{"Profiles", "#/profiles?profile=" + prod},
	} {
		cases = append(cases, map[string]any{"name": "faces on " + c.name + ": ids, files, flags and figures mono; prose sans", "route": c.route, "status": st,
			"pageHas": []string{
				// ids, file names, flags and figures
				f + "span.mono[data-fact=id]", f + "span.mono[data-fact=file]", f + "span.mono[data-fact=ctx]",
				// prose: no mono on the value
				f + "span[data-fact=generated]", f + "span[data-stage]"},
			"pageNone": []string{f + "span.mono[data-fact=generated]", f + "span.mono[data-stage]", f + "span.mono[data-ttl]", f + "dd.mono"}})
	}
	cases = append(cases, map[string]any{"name": "faces: flags mono, when it unloads in words", "route": "#/test-profiles?q=" + tp + "&profile=" + tp, "status": st,
		"pageHas": []string{f + "span.mono[data-extra-args]", f + "span[data-ttl]"}, "page": []string{"after 15 min", "no, made by hand"}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": cases})

	// the face of a dd in the panel: sans at the body weight, by the tokens
	css, err := os.ReadFile(filepath.Join("ui", "next", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".prf-facts dd{font-family:var(--sans);font-weight:var(--w-body)}") {
		t.Error("app.css: .prf-facts dd is not sans at --w-body")
	}
	if !strings.Contains(string(css), ".mono{font-family:var(--mono);") {
		t.Error("app.css: .mono is not the mono face")
	}
}
