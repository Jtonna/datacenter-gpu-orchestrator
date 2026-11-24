package api

import "testing"

// Tiers and zones, rebuilt (next/views/tiers.js; no mockup): the tiers as a
// table in check order with the rule in words, the zones in each tier now,
// ↑ ↓ in the row and the tier in the docked panel; Excluded zones and Named
// only as lists; Node overrides (one line with agents); one save bar and
// one save dialog (dry run first, the list of changes, the required
// comment); the server's sentence on a refusal. Fixture: --tiers live (the
// live pool's shape) and --zones live (its eight machines).
func TestUINextTiers(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--tiers", "live", "--zones", "live"}, "/api/config", "/api/config/schema", "/api/status")
	st := fetch["/api/status"]
	noKey := []string{"X-Actor", "X-Agent-Key", "Authorization"}
	comment := func(text string) []any {
		return []any{"button[data-save]", map[string]any{"type": "textarea.dlg-comment", "value": text}, "div.dlg-foot button.pri"}
	}
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
	run := func(f map[string]any, cases ...any) {
		t.Helper()
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": cases})
	}

	run(fetch,
		map[string]any{"name": "tiers: the page", "route": "#/tiers", "status": st, "width": 1536,
			"expect": []string{"Tiers and zones", "4 tiers", "2 excluded", "1 named only",
				"Checked from the top", "#", "Matches", "Priority", "Headroom", "Memory", "Link", "Settle", "Zones now", "Add a tier",
				"Every zone (no rule)", "Host name matches alienware*", "Only the local zone", "Host name matches laptop-*",
				"1,024 MiB", "Shared", "100 MB/s", "3s", "256 MiB", "Dedicated", "60 MB/s", "8s",
				"Excluded zones", "laptop-02", "02:00:00:00:00:77", "Named only", "laptop-01",
				"Node overrides", "Not used in this mode", "Zones and their tier", "ps5-01", "Tier 1"},
			"has":  []string{"tr[data-tier='1']", "tr[data-tier='4']", "tr[data-tier='1'] button[data-mv=up][disabled]", "tr[data-tier='4'] button[data-mv=down][disabled]", "td[data-zones]"},
			"none": []string{"Forced only", "Classic", "div.trs-save", "button[data-act=add-override]"}},
		map[string]any{"name": "tiers: a row opens the tier", "route": "#/tiers", "status": st, "width": 1536, "click": []string{"tr[data-tier='2']"}, "hash": "#/tiers?tier=2"},
		map[string]any{"name": "tiers: the tier in the panel", "route": "#/tiers?tier=1", "status": st, "width": 1536,
			"page":    []string{"Tier 1 · 8 zones", "Every zone (no rule)", "1st of 4", "Shared with the system", "Zones in this tier now", "ps5-01", "laptop-02", "Edit", "Move up", "Move down", "Remove"},
			"pageHas": []string{"aside.side.docked", "aside.side button[data-act=up][disabled]", "tr.sel[data-tier='1']"}},
		map[string]any{"name": "tiers: Add a tier asks every rule field", "route": "#/tiers", "status": st, "click": []string{"button[data-act=add-tier]"},
			"page": []string{"Add a tier", "Host name pattern", "Role", "Source", "Platform", "Device name pattern", "Which zones", "Priority", "Headroom", "Link speed", "Settle time", "Add the tier"}},
		map[string]any{"name": "tiers: a move collects in the save bar", "route": "#/tiers", "status": st, "click": []string{"tr[data-tier='1'] button[data-mv=down]"},
			"expect": []string{"1 change", "Nothing is applied until you save", "Discard", "Save"}, "has": []string{"div.trs-save", "tr[data-tier='2'] td.trs-rule"}},
	)
	// each edit in a run of its own: unsaved edits survive between screens
	run(fetch, map[string]any{"name": "tiers: stop excluding", "route": "#/tiers", "status": st, "click": []string{"button[data-rm=laptop-02]"},
		"expect": []string{"1 change", "1 excluded"}})
	run(fetch, map[string]any{"name": "tiers: named only, add", "route": "#/tiers", "status": st,
		"click":  []any{map[string]any{"type": "input[data-add=noauto]", "value": "ps5-03"}, "button[data-add-btn=noauto]"},
		"expect": []string{"2 named only", "ps5-03", "Unsaved", "1 change"}})
	run(fetch, map[string]any{"name": "tiers: named only, already there", "route": "#/tiers", "status": st,
		"click":  []any{map[string]any{"type": "input[data-add=noauto]", "value": "laptop-01"}, "button[data-add-btn=noauto]"},
		"expect": []string{"laptop-01 is already named only."}})

	// the save: a dry run of each part, then one dialog with the changes and the comment, then the writes
	saved := with(map[string]any{"PUT /api/config/tiers": map[string]any{"revision": 413}, "PUT /api/config/exclude-nodes": map[string]any{"revision": 414}})
	run(saved, map[string]any{"name": "tiers: save, the dialog", "route": "#/tiers", "status": st,
		"click": []any{"tr[data-tier='1'] button[data-mv=down]", "button[data-rm=laptop-02]", "button[data-save]"},
		"page":  []string{"Save 2 changes?", "on top of revision 412", "Host name matches alienware*", "New order", "Stop excluding ", "laptop-02", "Comment (required)", "Save 2 changes"}})
	run(saved, map[string]any{"name": "tiers: save, the writes", "route": "#/tiers", "status": st,
		"click": append([]any{"tr[data-tier='1'] button[data-mv=down]", "button[data-rm=laptop-02]"}, comment("laptop-02 is back")...),
		"fetchedQuery": []string{
			`^PUT /api/config/tiers\?dry_run=1 \{"tiers":\[\{"headroom_mib":256,"link_mb_s":60,"match":\{"hostname":"alienware\*"\},"priority":2,"settle":"8s"\},\{"headroom_mib":1024,`,
			`^PUT /api/config/exclude-nodes\?dry_run=1 \{"exclude_nodes":\["02:00:00:00:00:77"\]\}$`,
			`^PUT /api/config/tiers \{"tiers":\[\{"headroom_mib":256,"link_mb_s":60,"match":\{"hostname":"alienware\*"\}.*\],"comment":"laptop-02 is back"\}$`,
			`^PUT /api/config/exclude-nodes \{"exclude_nodes":\["02:00:00:00:00:77"\],"comment":"laptop-02 is back"\}$`},
		"sentHeader": []string{`^X-Comment: laptop-02 is back$`, `^If-Match: 412$`},
		"noHeader":   noKey,
		"none":       []string{"div.trs-save"}})

	// a refusal: the server's sentence, nothing saved, the edits kept
	refused := with(map[string]any{"PUT /api/config/tiers": map[string]any{"$status": 400,
		"$body": map[string]any{"error": map[string]any{"message": "tier 2: settle must be a duration such as 8s", "messages": []any{"tier 2: settle must be a duration such as 8s"}}}}})
	run(refused, map[string]any{"name": "tiers: refused", "route": "#/tiers", "status": st, "click": []any{"tr[data-tier='1'] button[data-mv=down]", "button[data-save]"},
		"expect": []string{"The server did not accept the tier list. Nothing was saved.", "tier 2: settle must be a duration such as 8s", "1 change"},
		"none":   []string{"Save 1 change?"}})

	// membership fleet: overrides can be added
	fleet := mcpCopy(t, st)
	fleet["membership"] = "fleet"
	run(fetch, map[string]any{"name": "tiers: fleet, overrides", "route": "#/tiers", "status": fleet,
		"expect": []string{"Node overrides", "0 overrides", "No overrides: every zone uses the address the fleet console reports."},
		"has":    []string{"button[data-act=add-override]"}, "none": []string{"Not used in this mode"}})
}
