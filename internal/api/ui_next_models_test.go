package api

import "testing"

// the fixture's model files (scripts/ui-fixture/models.mjs)
const (
	mdlLoaded  = "gemma-4-27b-it-Q4_K_M.gguf" // loaded, 3 profiles (one generated)
	mdlRecent  = "qwen3-14b-Q5_K_M.gguf"      // used 48 s ago
	mdlOlder   = "gemma-4-12b-it-Q4_K_M.gguf" // used 4 h ago, two contexts
	mdlNever   = "qwen3-4b-Q8_0.gguf"         // never used
	mdlNoProfs = "nomic-embed-text-v2.gguf"   // no profiles, never used
)

// The Models list as the board draws it (phase 7), on the fixture's catalog:
// one row per file with its size, "N profiles" and when it was last used
// (Now, a time, Never), sorted by last used; search over files, names and
// profiles; the foot "N model files · X of Y GB used" from the disk block,
// amber on a nearly full disk; the selected row and its side panel (what is
// loaded where, the configurations with their context, the memory each
// needs and a Now pill); and the controls whose backend does not exist yet
// drawn but unavailable, each with its reason on hover and doing nothing.
func TestUINextModels(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	// a control drawn but not available: disabled, with its reason on hover (the first word of it here: the selectors take no spaces)
	off := func(sel, reason string) string { return sel + "[aria-disabled=true][data-off][title^=" + reason + "]" }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: the list", "route": "#/models", "status": st,
			"expect": []string{"Models", "Sort: last used", "Add a model", "Downloading", "Pinned", "All models",
				"5 model files · 50.3 of 240 GB used", mdlLoaded, "16.5 GB", "2 profiles", "Now", mdlRecent, "10.1 GB", "1 profile",
				mdlNoProfs, "0.3 GB", "No profiles", "Never", "Deleted models", "Needs", "17.1 GB", "measured", "5.1 GB", "estimated"},
			"order": []string{mdlLoaded, mdlRecent, mdlOlder, mdlNoProfs, mdlNever, "Deleted models"},
			"has": []string{off("button.btn.pri[data-act=add]", "Adding"), "button.star[aria-pressed=true]", off("button.tbar[data-act=trash]", "Deleted"),
				"div.sech[data-sec=downloading][title^=Downloads]", "div.sech[data-sec=pinned]", "label.sx input",
				"span.mdl-disk[data-disk]", "select.fs[aria-label=Sort]", "select.fs[aria-label=Show]",
				"div.mr[data-model='" + mdlNoProfs + "'] span.mdl-needs span.mut"},
			"none": []string{"div.mr.sel", "span.mdl-disk.full"}, "absent": designSamples},
		// the profile count does not count generated profiles out: the server's profile_count is what the row says
		map[string]any{"name": "models: the loaded row says Now", "route": "#/models", "status": st,
			"has": []string{"div.mr[data-model='" + mdlLoaded + "'] time.mdl-now", "div.mr[data-model='" + mdlRecent + "'] time[title]"}},
		map[string]any{"name": "models: Add a model does nothing", "route": "#/models", "status": st, "click": []string{"button[data-act=add]"}, "hash": "#/models",
			"pageNone": []string{"aside.side", "dialog"}},
		map[string]any{"name": "models: the star does not open the row", "route": "#/models", "status": st,
			"click": []string{"div.mr[data-model='" + mdlRecent + "'] button.star"}, "hash": "#/models", "pageNone": []string{"aside.side"}},
		map[string]any{"name": "models: a row opens its panel", "route": "#/models", "status": st, "click": []string{"div.mr[data-model='" + mdlRecent + "']"},
			"hash": "#/models?model=" + mdlRecent},
		map[string]any{"name": "models: search by file", "route": "#/models?q=nomic", "status": st, "expect": []string{mdlNoProfs}, "absent": []string{mdlLoaded, mdlRecent}},
		map[string]any{"name": "models: search by profile", "route": "#/models?q=12b:64k", "status": st, "expect": []string{mdlOlder}, "absent": []string{mdlLoaded, mdlNoProfs}},
		map[string]any{"name": "models: no match", "route": "#/models?q=zzz", "status": st, "expect": []string{"No model matches", `Nothing matches "zzz"`}},
		// the Pinned group (gemma-4-27b, qwen3-14b) comes first, each group in the asked order
		map[string]any{"name": "models: sort by name", "route": "#/models?sort=name", "status": st, "order": []string{mdlLoaded, mdlRecent, mdlOlder, mdlNoProfs, mdlNever}},
		map[string]any{"name": "models: sort by size", "route": "#/models?sort=size", "status": st, "order": []string{mdlLoaded, mdlRecent, mdlOlder, mdlNever, mdlNoProfs}},
		map[string]any{"name": "models: show loaded", "route": "#/models?show=loaded", "status": st, "expect": []string{mdlLoaded}, "absent": []string{mdlRecent, mdlNoProfs}},

		// the panel of the loaded model
		map[string]any{"name": "models: the loaded model's panel", "route": "#/models?model=" + mdlLoaded, "status": st,
			"page": []string{"Selected model", mdlLoaded, "16.5 GB", "loaded now", "Benchmarks", "3 reports", "Pinned", "Delete model",
				"Loaded now", "on ps5-01, ps5-02", "2 requests in flight", "Runs on", "gemma-4-27b", "Google",
				"Configurations", "2 profiles", "Profile", "Context", "Memory needed", "8,192", "32,768", "17,480 MiB", "20,890 MiB", "measured",
				"1 generated configuration", "Add profile", "Open the model page", "Edit under Profiles"},
			"pageHas": []string{"aside.side tr[data-profile='gemma-4-27b'] span.mdl-pill", "aside.side a.go[href='#/profiles/gemma-4-27b']",
				"aside.side a.btn[href='#/profiles/new']", "aside.side select.fs[aria-label=Context]", "aside.side label.sx",
				"aside.side button.btn[data-act=pin][aria-pressed=true]", off("aside.side button.btn.red[data-act=delete]", "Deleting")},
			"pageNone":  []string{"aside.side tr[data-profile='gemma-4-27b:long'] span.mdl-pill", "aside.side tr[data-profile='bench-gemma-4-27b-c16k-n2']"},
			"has":       []string{"div.mr.sel[aria-current=true][data-model='" + mdlLoaded + "']"},
			"notOnPage": []string{"4 runs"}},
		map[string]any{"name": "models: Delete does nothing", "route": "#/models?model=" + mdlLoaded, "status": st, "click": []string{"button[data-act=delete]"},
			"hash": "#/models?model=" + mdlLoaded, "pageNone": []string{"dialog"}},
		map[string]any{"name": "models: the context filter", "route": "#/models?model=" + mdlOlder, "status": st,
			"click":   []any{map[string]any{"type": "aside.side select[aria-label=Context]", "value": "65536"}},
			"pageHas": []string{"aside.side tr[data-profile='gemma-4-12b:64k']"}, "pageNone": []string{"aside.side tr[data-profile='gemma-4-12b']"}},
		map[string]any{"name": "models: a never-used model", "route": "#/models?model=" + mdlNever, "status": st,
			"page": []string{mdlNever, "4.3 GB", "never used", "1 profile", "5,200 MiB", "estimated"}, "notOnPage": []string{"Loaded now"}},
		map[string]any{"name": "models: a file with no profiles", "route": "#/models?model=" + mdlNoProfs, "status": st,
			"page": []string{mdlNoProfs, "No profiles", "No configuration uses this file yet", "Add profile"}, "pageNone": []string{"aside.side table"}},
		map[string]any{"name": "models: closing the panel", "route": "#/models?model=" + mdlNever, "status": st, "click": []string{"aside.side button.x"}, "hash": "#/models"},
		// the page rule: an automatic profile says how many nodes, a forced one names its own
		map[string]any{"name": "models: an automatic profile's plan gives a count", "route": "#/models/" + mdlOlder, "status": st,
			"expect": []string{"would load on 1 node"}, "has": []string{"span[data-would='1']"}, "absent": []string{"ps5-03"}},
		map[string]any{"name": "models: a forced profile's plan names its node", "route": "#/models/" + mdlRecent, "status": st,
			"expect": []string{"ps5-04"}, "none": []string{"span[data-would]"}},
		map[string]any{"name": "models: a model that is not there", "route": "#/models?model=no-such-model.gguf", "status": st, "page": []string{"This model is not in the catalog"}},
	}})

	full := fixtureAnswers(t, []string{"--models", "full"}, "/api/models")
	full["/api/status"] = st
	none := fixtureAnswers(t, []string{"--models", "none"}, "/api/models")
	none["/api/status"] = st
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": full, "cases": []any{
		map[string]any{"name": "models: a nearly full disk", "route": "#/models", "status": st,
			"expect": []string{"5 model files · 228.4 of 240 GB used · nearly full"}, "has": []string{"span.mdl-disk.full[title*=free]"}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": none, "cases": []any{
		map[string]any{"name": "models: no models", "route": "#/models", "status": st,
			"expect": []string{"No models yet", "0 model files · 1.2 of 240 GB used", "Add a model"}, "none": []string{"div.mr", "select.fs[aria-label=Show]"}},
	}})
}
