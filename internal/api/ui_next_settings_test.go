package api

import (
	"testing"
)

// fixtureAnswers starts the fixture server (scripts/ui-fixture-server.mjs)
// with args and reads the answers of these GET paths, for ui-exec's "fetch".
func fixtureAnswers(t *testing.T, args []string, paths ...string) map[string]any {
	t.Helper()
	get := fixtureServer(t, args...)
	out := map[string]any{}
	for _, p := range paths {
		var v any
		get(p, &v)
		out[p] = v
	}
	return out
}

var settingsPaths = []string{"/api/config/schema", "/api/config/settings", "/api/config/recovery", "/api/config/fleet", "/api/system"}

// The Settings page (phase 5, no mockup): one group of settings at a time,
// picked in the group menu and carried by the address; each setting one row
// with its control, its default, the "differs from the default" dot and
// "Reset to default"; the search across groups; the save bar; the dry run,
// the one comment and the PATCH; the server's refusal on the row it names;
// the fields of the stored configuration this version does not know.
func TestUINextSettings(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--config", "normal"}, settingsPaths...)
	row := func(key string) string { return "div.setp-row[data-key=" + key + "]" }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		// the first group: timing, and nothing of another group
		map[string]any{"name": "settings: the first group", "route": "#/settings",
			"expect": []string{"Settings", "Timing", "TTL", "ttl", "Default 5m", "Capacity", "Safety", "Recovery", "Fleet"},
			"has":    []string{row("ttl") + " span.changed", "a.setp-gi[aria-current=page][data-group=timing]", row("queue_timeout"), "button[data-reset=ttl]"},
			"none":   []string{row("max_pool"), row("forced_fit_check"), "div.setp-save"}, "absent": designSamples},
		// one group by address; every control type of it
		map[string]any{"name": "settings: safety", "route": "#/settings/safety",
			"expect": []string{"Safety", "Forced fit check", "Default off", "More", "split_skew_margin"},
			"has":    []string{row("forced_fit_check") + " select.select", row("placement_uses_reserved") + " button.switch[aria-checked=true]", row("load_memory_floor_mib") + " div.unit", "a.setp-gi[aria-current=page][data-group=safety]"},
			"none":   []string{row("ttl")}},
		// a long help unfolds, with the risk of loosening it
		map[string]any{"name": "settings: a long help", "route": "#/settings/safety", "click": []string{row("forced_fit_check") + " button.setp-more"},
			"expect": []string{"Less", "If you loosen it: "}},
		map[string]any{"name": "settings: a restart", "route": "#/settings/retention", "expect": []string{"Needs restart", "child_log_keep"}},
		map[string]any{"name": "settings: recovery, a list of actions", "route": "#/settings/recovery",
			"expect": []string{"recovery.actions", "restart_rpc_server", "restart_docker", "Not used in this mode"}, "has": []string{row("actions") + " input.cb"}},
		// the search finds settings in every group
		map[string]any{"name": "settings: search", "route": "#/settings?q=timeout",
			"expect": []string{"match", "queue_timeout", "stall_timeout", "worker_start_timeout", "Timing", "Agents"}, "has": []string{"h3.setp-sub"}, "none": []string{row("ttl")}},
		map[string]any{"name": "settings: search, nothing", "route": "#/settings?q=zzzz", "expect": []string{"No setting matches"}},
	}})
	// edits, each on a page of its own (unsaved edits survive a visit to another screen)
	for _, c := range []map[string]any{
		// an edit: the save bar counts it
		{"name": "settings: an edit", "route": "#/settings/capacity", "click": []any{map[string]any{"type": row("max_pool") + " input", "value": "8"}},
			"expect": []string{"1 change", "Nothing is applied until you save.", "Unsaved", "Discard"}, "has": []string{"div.setp-save button[data-save]", row("max_pool") + ".edited"}},
		{"name": "settings: a value that does not parse", "route": "#/settings/capacity", "click": []any{map[string]any{"type": row("min_share_mib") + " input", "value": "lots"}},
			"expect": []string{"Type a number.", "1 value to fix"}, "has": []string{row("min_share_mib") + ".bad"}},
		{"name": "settings: reset to default", "route": "#/settings/timing", "click": []string{"button[data-reset=ttl]"},
			"expect": []string{"1 change", "Undo"}},
		// save: a dry run first, then one comment
		{"name": "settings: save asks for one comment", "route": "#/settings/capacity",
			"click":        []any{map[string]any{"type": row("max_pool") + " input", "value": "8"}, "button[data-save]"},
			"page":         []string{"Save 1 change?", "Comment (required)", "Max pool", "6 nodes", "8 nodes"},
			"fetchedQuery": []string{`^PATCH /api/config/settings\?dry_run=1 \{"max_pool":8\}$`}},
	} {
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{c}})
	}

	// the server's refusal, on the row it names; the save itself
	refuse := map[string]any{}
	for k, v := range fetch {
		refuse[k] = v
	}
	refuse["PATCH /api/config/settings"] = map[string]any{"$status": 400, "$body": map[string]any{"error": map[string]any{"type": "invalid_config", "message": "settings: max_pool: must be at most 16",
		"messages": []string{"settings: max_pool: must be at most 16"}}}}
	ok := map[string]any{}
	for k, v := range fetch {
		ok[k] = v
	}
	ok["PATCH /api/config/settings"] = map[string]any{"revision": 413, "changed": true, "diff": []any{map[string]any{"path": "settings.max_pool", "old": 6, "new": 8}}, "warnings": []string{}, "restart_required": []string{}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": refuse, "cases": []any{
		map[string]any{"name": "settings: the server refuses", "route": "#/settings/capacity",
			"click":  []any{map[string]any{"type": row("max_pool") + " input", "value": "99"}, "button[data-save]"},
			"expect": []string{"The server did not accept this change. Nothing was saved.", "max_pool: must be at most 16"}, "has": []string{row("max_pool") + ".bad"},
			"notOnPage": []string{"Comment (required)"}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": ok, "cases": []any{
		map[string]any{"name": "settings: saved", "route": "#/settings/capacity",
			"click": []any{map[string]any{"type": row("max_pool") + " input", "value": "8"}, "button[data-save]",
				map[string]any{"type": "textarea.dlg-comment", "value": "more zones per model"}, "div.dlg-foot button.pri"},
			"page":         []string{"Saved as revision 413"},
			"fetchedQuery": []string{`^PATCH /api/config/settings \{"max_pool":8,"comment":"more zones per model"\}$`},
			"noHeader":     []string{"X-Actor"}},
	}})

	// the fields of the stored configuration this version does not know
	unknown := fixtureAnswers(t, []string{"--config", "unknown"}, settingsPaths...)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": unknown, "cases": []any{
		map[string]any{"name": "settings: unknown fields", "route": "#/settings/unknown",
			"expect": []string{"Not known to this version", "settings.gpu_affinity_hint", "written by a newer version", "the server refuses every change on this page", "Read-only."}},
	}})

	// nothing configured yet
	fresh := fixtureAnswers(t, []string{"--config", "fresh"}, settingsPaths...)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fresh, "cases": []any{
		map[string]any{"name": "settings: nothing configured", "route": "#/settings", "expect": []string{"No configuration has been saved yet.", "revision 0"}, "none": []string{"div.setp-row span.changed"}},
	}})
}
