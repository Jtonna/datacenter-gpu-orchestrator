package api

import "testing"

// Models pins (v61, models slice 2), as the board draws them: a "Pinned"
// group above "All models" with the pinned files, a star on every row
// (filled when pinned) and the panel's "Pinned" button. Pin and unpin go
// through the comment dialog the other writes use (the comment is
// required: the dialog keeps its button off while the box is empty, so the
// server's 400 is never asked for), then PUT or DELETE
// /api/models/pin/{key} with the comment. A refusal (404) is shown as the
// server's sentence. A server without `pinned` (before v61) keeps the
// earlier drawing: star and group unavailable with the reason.
func TestUINextModelsPins(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	const pinnedA, pinnedB, free = "gemma-4-27b-it-Q4_K_M.gguf", "qwen3-14b-Q5_K_M.gguf", "qwen3-4b-Q8_0.gguf"
	with := func(extra map[string]any) map[string]any {
		f := map[string]any{}
		for k, v := range fetch {
			f[k] = v
		}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	ok := with(map[string]any{
		"PUT /api/models/pin/" + free:      map[string]any{"key": free, "pinned": true, "changed": true, "audit_id": 41},
		"DELETE /api/models/pin/" + pinnedA: map[string]any{"key": pinnedA, "pinned": false, "changed": true, "audit_id": 42},
	})
	run := func(f map[string]any, cases ...any) {
		t.Helper()
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": cases})
	}
	comment := func(text string) []any {
		return []any{map[string]any{"type": "textarea.dlg-comment", "value": text}, "div.dlg-foot button.pri"}
	}

	run(ok,
		map[string]any{"name": "pins: the Pinned group and the stars", "route": "#/models", "status": st,
			"has": []string{
				"[data-pinned-rows] div.mr[data-model='" + pinnedA + "']", "[data-pinned-rows] div.mr[data-model='" + pinnedB + "']",
				"div.mr[data-model='" + pinnedA + "'] button.star[aria-pressed=true]", "div.mr[data-model='" + free + "'] button.star[aria-pressed=false]"},
			"none": []string{"[data-pinned-rows] div.mr[data-model='" + free + "']", "button.star[data-off]", "div.sech[data-sec=pinned][title^=Pins]"},
			"order": []string{"Pinned", pinnedA, pinnedB, "All models", free}},
		map[string]any{"name": "pins: the panel's Pinned button", "route": "#/models?model=" + pinnedA, "status": st,
			"pageHas": []string{"aside.side button[data-act=pin][aria-pressed=true]"}, "pageNone": []string{"aside.side button[data-act=pin][data-off]"}},
	)
	run(ok, map[string]any{"name": "pins: pin with a comment", "route": "#/models", "status": st,
		"click":        append([]any{"div.mr[data-model='" + free + "'] button.star"}, comment("keep it handy")...),
		"fetchedQuery": []string{`^PUT /api/models/pin/qwen3-4b-Q8_0\.gguf \{"comment":"keep it handy"\}$`},
		"sentHeader":   []string{`^X-Comment: keep it handy$`},
		"hash":         "#/models"})
	run(ok, map[string]any{"name": "pins: unpin from the panel", "route": "#/models?model=" + pinnedA, "status": st,
		"click":        append([]any{"aside.side button[data-act=pin]"}, comment("not needed")...),
		"fetchedQuery": []string{`^DELETE /api/models/pin/gemma-4-27b-it-Q4_K_M\.gguf \{"comment":"not needed"\}$`}})
	run(ok, map[string]any{"name": "pins: no comment, nothing sent", "route": "#/models", "status": st,
		"click":      []any{"div.mr[data-model='" + free + "'] button.star", "div.dlg-foot button.pri"},
		"notFetched": []string{`^PUT /api/models/pin/`}})
	refused := with(map[string]any{"DELETE /api/models/pin/" + pinnedA: map[string]any{"$status": 404,
		"$body": map[string]any{"error": map[string]any{"message": "gemma-4-27b-it-Q4_K_M.gguf is not pinned"}}}})
	run(refused, map[string]any{"name": "pins: a refusal in words", "route": "#/models", "status": st,
		"click":  append([]any{"div.mr[data-model='" + pinnedA + "'] button.star"}, comment("tidy")...),
		"expect": []string{"Could not unpin gemma-4-27b-it-Q4_K_M.gguf: gemma-4-27b-it-Q4_K_M.gguf is not pinned"}})

	// before v61: no pinned field, the pins stay unavailable
	old := mcpCopy(t, fetch["/api/models"])
	for _, m := range old["models"].([]any) {
		delete(m.(map[string]any), "pinned")
		delete(m.(map[string]any), "pinned_at")
	}
	run(with(map[string]any{"/api/models": old}), map[string]any{"name": "pins: an older server", "route": "#/models", "status": st,
		"has": []string{"button.star[data-off][aria-disabled=true]", "div.sech[data-sec=pinned][title^=Pins]"}, "none": []string{"[data-pinned-rows]"}})
}
