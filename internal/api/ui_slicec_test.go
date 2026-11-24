package api

import "testing"

// Settings, rebuilt (slice C): a menu of the groups, one group in view at a
// time, one row per setting (its name and one-line help, its control, its
// default and unit); a search looks through every group; an edit is staged
// in the pending bar and saved only through Review changes. Drawn from a
// real scheduler's GET /api/config and /api/config/schema.
func TestUINextSettings(t *testing.T) {
	e, _, _ := cfgEnv(t)
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": e.front.URL, "cases": []any{
		map[string]any{"name": "settings: the first group", "route": "#/settings", "status": "fetch",
			"page":    []string{"Settings", "Timing", "Capacity", "Safety", "Queue timeout", "queue_timeout", "Longest a request may wait in the queue without being given a model.", "Default"},
			"pageHas": []string{"nav.smenu button.smi.on[data-group=timing]", "div.srow[data-key='settings.queue_timeout'] input.input", "input[data-search]"},
			"notOnPage": []string{"Max pool", "Forced fit check"}},
		map[string]any{"name": "settings: another group", "route": "#/settings", "status": "fetch", "click": []string{"button[data-group=capacity]"},
			"page": []string{"Capacity", "Max pool", "nodes", "Most nodes a single model may be split across."}, "notOnPage": []string{"Queue timeout"},
			"pageHas": []string{"button.smi.on[data-group=capacity]"}, "hash": "#/settings?group=capacity"},
		map[string]any{"name": "settings: the address names the group", "route": "#/settings?group=safety", "status": "fetch",
			"page": []string{"Forced fit check", "If you loosen it:"}, "pageHas": []string{"div.srow[data-key='settings.forced_fit_check'] select.select"}},
		map[string]any{"name": "settings: a search looks through every group", "route": "#/settings", "status": "fetch",
			"click": []any{map[string]any{"type": "input[data-search]", "value": "timeout"}},
			"page": []string{"Timing", "Queue timeout", "Command load timeout"}, "pageHas": []string{"section.sgrp[data-sec=timing]"}, "pageNone": []string{"button.smi.on"}},
		map[string]any{"name": "settings: a search with no hit", "route": "#/settings", "status": "fetch",
			"click": []any{map[string]any{"type": "input[data-search]", "value": "zzzz-nothing"}}, "page": []string{"No setting matches"}},
		map[string]any{"name": "settings: an edit is staged, not saved", "route": "#/settings", "status": "fetch",
			"click": []any{map[string]any{"type": "div.srow[data-key='settings.queue_timeout'] input.input", "value": "7m"}},
			"page":    []string{"1 unsaved change", "Review changes", "Unsaved", "Reset"},
			"pageHas": []string{"button[data-reset=queue_timeout]"}, "notFetched": []string{"PATCH"}},
		map[string]any{"name": "settings: a wrong value says why", "route": "#/settings", "status": "fetch",
			"click": []any{map[string]any{"type": "div.srow[data-key='settings.queue_timeout'] input.input", "value": "soon"}},
			"page": []string{"Enter a duration such as 90s, 15m or 1h30m."}},
	}})
}

// Placement preview, rebuilt (slice C): a form (the profile; anywhere the
// scheduler chooses, or only on chosen zones) and a result card with the
// verdict, what it needs and where it would go. POST /api/plan is a dry run.
func TestUINextPlacement(t *testing.T) {
	e, _, _ := cfgEnv(t)
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": e.front.URL, "cases": []any{
		map[string]any{"name": "placement: the first profile", "route": "#/placement", "status": "fetch",
			"page":    []string{"Placement preview", "Profile", "Where", "Anywhere the scheduler chooses", "Only on chosen zones", "Needs", "anywhere the scheduler chooses"},
			"pageHas": []string{"section.pl-form select[data-pl=profile]", "section.pl-res[data-result]", "input[data-where=any][checked]"}},
		map[string]any{"name": "placement: another profile", "route": "#/placement/gamma", "status": "fetch",
			"page": []string{"gamma", "g.gguf"}, "pageHas": []string{"select[data-pl=profile] option[value=gamma][selected]"}},
		map[string]any{"name": "placement: only on chosen zones asks for the zones", "route": "#/placement/alpha?where=chosen", "status": "fetch",
			"page": []string{"Choose the zones"}, "pageHas": []string{"div[data-pl=zones]"}},
		map[string]any{"name": "placement: on a chosen zone", "route": "#/placement/alpha?where=chosen&zones=host-1", "status": "fetch",
			"page": []string{"only on 1 chosen zone"}, "pageHas": []string{"input[data-zone=host-1][checked]", "section.pl-res[data-result]"}},
	}})
}

// Profiles, cleaned up (slice C): one row per profile, a search, and the
// generated (benchmark) profiles hidden until asked for.
func TestUINextProfilesList(t *testing.T) {
	e, _, _ := cfgEnv(t)
	for _, p := range []string{`{"id":"alpha","model":"a.gguf","ctx":4096,"needs_mib":1000}`, `{"id":"gamma","model":"g.gguf","ctx":4096,"needs_mib":500}`,
		`{"id":"bench-alpha-2048","model":"a.gguf","ctx":2048,"needs_mib":100}`} {
		if st, m := e.cdo("POST", "/api/config/profiles", p, map[string]string{"Content-Type": "application/json"}); st != 201 {
			t.Fatalf("create a profile %s: %d %v", p, st, m)
		}
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": e.front.URL, "cases": []any{
		map[string]any{"name": "profiles: made by hand, the generated hidden", "route": "#/profiles", "status": "fetch",
			"page":      []string{"Profiles", "alpha", "gamma", "Made by hand", "1 generated profile hidden", "Show them"},
			"pageHas":   []string{"tr[data-profile=alpha]", "tr[data-profile=gamma]", "div.pf-foot[data-hidden-generated='1']"},
			"pageNone":  []string{"tr[data-profile=bench-alpha-2048]", "tr.grp"}},
		map[string]any{"name": "profiles: show the generated", "route": "#/profiles", "status": "fetch", "click": []string{"button[data-show=generated]"},
			"pageHas": []string{"tr[data-profile=bench-alpha-2048]"}, "pageNone": []string{"tr[data-profile=alpha]"}, "hash": "#/profiles?show=generated"},
		map[string]any{"name": "profiles: a search finds the generated too", "route": "#/profiles", "status": "fetch",
			"click":   []any{map[string]any{"type": "section.card input[type=search]", "value": "alpha"}},
			"pageHas": []string{"tr[data-profile=alpha]", "tr[data-profile=bench-alpha-2048]"}, "pageNone": []string{"tr[data-profile=gamma]"}},
		map[string]any{"name": "profiles: the editor is unchanged", "route": "#/profiles/alpha", "status": "fetch", "page": []string{"alpha", "Context size"}},
	}})
}
