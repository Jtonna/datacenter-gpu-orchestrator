package api

import "testing"

// The Profiles page (phase 5, no mockup): one row per profile (name and
// description, the model's short name, context, zones, the plan's pill),
// no placement lists in the table; the generated benchmark profiles hidden
// until "Show generated (N)"; a pager; the search, filter, page and open
// profile in the address; the placements in the side panel with Edit,
// Duplicate and Delete; the editor and its save through the review dialog
// (dry run, a comment, then the write).
func TestUINextProfiles(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles", "/api/models", "/api/config", "/api/config/schema", "/api/status")
	st := fetch["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "profiles: the list, generated hidden", "route": "#/profiles", "status": st,
			"expect": []string{"Profiles", "made by hand", "generated", "gemma-4-27b-long", "qwen3-32b-split", "Show generated", "300", "12 profiles",
				"Would make room", "Cannot be placed", "Would wait", "Held back", "Forced on 3", "Automatic", "128k", "gemma-4-12b"},
			"has":    []string{"tr[data-profile=gemma-4-27b-long]", "button[data-gen][aria-pressed=false]"},
			"absent": []string{"bench-gemma-4-12b-c16k-n1", "1–16 of"}},
		// no placement lists in the table: the zones of a forced profile are only counted
		map[string]any{"name": "profiles: no placements in the table", "route": "#/profiles?q=qwen3-32b-split", "status": st,
			"expect": []string{"qwen3-32b-split", "Forced on 3"}, "absent": []string{"ps5-01", "ps5-02", "Tensor split"}},
		map[string]any{"name": "profiles: generated shown, with a pager", "route": "#/profiles?show=all", "status": st,
			"expect": []string{"bench-gemma-4-12b-c16k-n1", "Hide generated", "312 profiles", "1–16 of 312"}, "has": []string{"button[data-gen][aria-pressed=true]"}},
		map[string]any{"name": "profiles: page 2 from the address", "route": "#/profiles?show=all&page=2", "status": st, "expect": []string{"17–32 of 312"}},
		map[string]any{"name": "profiles: search counts the hidden ones", "route": "#/profiles?q=glm", "status": st,
			"expect": []string{"glm-4.5-air", "generated match too"}, "absent": []string{"gemma-4-27b-long"}},
		map[string]any{"name": "profiles: nothing matches", "route": "#/profiles?q=zzzz", "status": st, "expect": []string{"No profile matches"}},
		map[string]any{"name": "profiles: a row opens the panel", "route": "#/profiles", "status": st, "click": []string{"tr[data-profile=qwen3-32b-split]"},
			"hash": "#/profiles?profile=qwen3-32b-split"},
		map[string]any{"name": "profiles: the panel holds the placements", "route": "#/profiles?profile=qwen3-32b-split", "status": st,
			"page":    []string{"Placement", "Forced on 3 zones", "Tensor split", "Where it would go now", "Open in Placement preview", "Edit", "Duplicate", "Delete"},
			"pageHas": []string{"aside.side a[data-act=edit]", "aside.side span.nodes"},
			"has":     []string{"tr.sel[data-profile=qwen3-32b-split]"}},
		map[string]any{"name": "profiles: the editor", "route": "#/profiles/gemma-4-12b", "status": st,
			"expect": []string{"Edit profile", "gemma-4-12b", "Context size", "Placement", "Back to the list", "Save"}, "none": []string{"tr[data-profile=gemma-4-12b]"}},
		map[string]any{"name": "profiles: a copy", "route": "#/profiles/new?from=gemma-4-12b", "status": st, "expect": []string{"New profile, copied from", "Name"}},
		map[string]any{"name": "profiles: a profile that does not exist", "route": "#/profiles/nope", "status": st, "expect": []string{"There is no profile nope"}},
	}})

	// a save: the dry run, the dialog with a comment, the write (no actor header)
	save := map[string]any{}
	for k, v := range fetch {
		save[k] = v
	}
	save["PUT /api/config/profiles/gemma-4-12b"] = map[string]any{"changed": true, "revision": 413, "diff": []any{map[string]any{"path": "profiles.gemma-4-12b.ctx", "old": 65536, "new": 32768}}}
	save["GET /api/config/profiles/qwen3-32b"] = map[string]any{"revision": 412, "profile": map[string]any{"model": "qwen3-32b-Q4_K_M.gguf"}}
	save["DELETE /api/config/profiles/qwen3-32b"] = map[string]any{"changed": true, "revision": 413, "diff": []any{map[string]any{"path": "profiles.qwen3-32b", "old": map[string]any{}}}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": save, "cases": []any{
		map[string]any{"name": "profiles: save with a comment", "route": "#/profiles/gemma-4-12b", "status": st,
			"click":        []any{"button[data-save]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "shorter context"}, "div.dlg-foot button.pri"},
			"fetchedQuery": []string{`^PUT /api/config/profiles/gemma-4-12b\?dry_run=1 \{"profile":\{.*\}\}$`, `^PUT /api/config/profiles/gemma-4-12b \{"profile":\{.*\},"comment":"shorter context"\}$`},
			"noHeader":     []string{"X-Actor"}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": save, "cases": []any{
		map[string]any{"name": "profiles: delete from the panel", "route": "#/profiles?profile=qwen3-32b", "status": st,
			"click":        []any{"button[data-act=delete]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "not used"}, "div.dlg-foot button.pri"},
			"fetchedQuery": []string{`^DELETE /api/config/profiles/qwen3-32b\?dry_run=1`, `^DELETE /api/config/profiles/qwen3-32b$`},
			"noHeader":     []string{"X-Actor"}},
	}})
}
