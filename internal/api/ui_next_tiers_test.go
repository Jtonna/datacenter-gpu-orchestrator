package api

import (
	"strings"
	"testing"
)

var tiersPaths = []string{"/api/config", "/api/config/schema", "/api/config/tiers", "/api/nodes", "/api/status"}

// Tiers and zones, rebuilt (next/views/tiers.js; no mockup): the tiers as a
// table in check order with the rule in words, the zones in each tier now,
// ↑ ↓ in the row and the tier in the docked panel; Excluded zones and Named
// only as lists; Node overrides (one line with agents); one save bar and
// one save dialog (dry run first, the list of changes, the required
// comment); the server's sentence on a refusal. Fixture: --tiers live (the
// real instance's shape) and --zones live (its eight machines).
//
// Membership comes ONLY from the server: tier_zones on GET
// /api/config/tiers, tier / tier_reason on GET /api/nodes.
func TestUINextTiers(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--tiers", "live", "--zones", "live"}, tiersPaths...)
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
				"Platform is linux-amd64-vulkan", "Host name matches alienware*", "Only the local zone", "Host name matches laptop-* and platform is linux-amd64-cuda",
				"1,024 MiB", "Shared", "100 MB/s", "3s", "256 MiB", "Dedicated", "60 MB/s", "8s",
				"Excluded zones", "devbox-02", "02:00:00:00:00:77", "Named only", "alienware-01",
				"Node overrides", "Not used in this mode", "Zones and their tier", "ps5-01", "scheduler-host", "Tier 1", "Tier 2", "Tier 3"},
			"has": []string{"tr[data-tier='1']", "tr[data-tier='4']", "tr[data-tier='1'] button[data-mv=up][disabled]", "tr[data-tier='4'] button[data-mv=down][disabled]",
				// the counts are the server's tier_zones
				"tr[data-tier='1'] td[data-zones='6']", "tr[data-tier='2'] td[data-zones='1']", "tr[data-tier='3'] td[data-zones='1']", "tr[data-tier='4'] td[data-zones='0']",
				// Tier now is the server's tier; a zone fitting none: a dash with the reason
				"tr[data-zone='ps5-01'] td[data-tier-now='1']", "tr[data-zone='alienware-01'] td[data-tier-now='2']", "tr[data-zone='scheduler-host'] td[data-tier-now='3']",
				"tr[data-zone='devbox-02'] td[data-tier-now='-'][title*='matches']"},
			"absent": []string{"Every zone (no rule)", "Never reached", "With your unsaved tiers", "ps5-old"},
			"none":   []string{"Forced only", "Classic", "div.trs-save", "button[data-act=add-override]", "[data-never]"}},
		// the excluded zone keeps the server's tier and reads Excluded
		map[string]any{"name": "tiers: an excluded zone", "route": "#/tiers", "status": st,
			"has": []string{"tr[data-zone='devbox-02'] span.b-dash", "tr[data-zone='alienware-01'] span.b-n"}},
		map[string]any{"name": "tiers: a row opens the tier", "route": "#/tiers", "status": st, "width": 1536, "click": []string{"tr[data-tier='2']"}, "hash": "#/tiers?tier=2"},
		// one source: the panel's count is the table's count
		map[string]any{"name": "tiers: the tier in the panel", "route": "#/tiers?tier=1", "status": st, "width": 1536,
			"page": []string{"Tier 1 · 6 zones", "Platform is linux-amd64-vulkan", "1st of 4", "Shared with the system", "Zones in this tier now", "ps5-01", "ps5-06"},
			"pageHas": []string{"aside.side.docked", "aside.side button[data-act=up][disabled]", "tr.sel[data-tier='1']", "aside.side [data-panel-zones='6']", "tr[data-tier='1'] td[data-zones='6']",
				// docked beside the page, the panel closes the section menu so the table keeps its columns
				".app.menu-closed"},
			"absent": []string{"0 zones"}},
		map[string]any{"name": "tiers: an empty tier in the panel", "route": "#/tiers?tier=4", "status": st, "width": 1536,
			"page": []string{"Tier 4 · 0 zones", "No zone is in this tier now."}, "pageHas": []string{"aside.side [data-panel-zones='0']"}},
		map[string]any{"name": "tiers: Add a tier asks every rule field", "route": "#/tiers", "status": st, "click": []string{"button[data-act=add-tier]"},
			"page": []string{"Add a tier", "Host name pattern", "Role", "Source", "Platform", "Device name pattern", "Which zones", "Priority", "Headroom", "Link speed", "Settle time", "Add the tier"}},
		map[string]any{"name": "tiers: a move collects in the save bar", "route": "#/tiers", "status": st, "click": []string{"tr[data-tier='1'] button[data-mv=down]"},
			"expect": []string{"1 change", "Nothing is applied until you save", "Discard", "Save"},
			// a moved tier keeps the server's zones: they follow the row
			"has": []string{"div.trs-save", "tr[data-tier='2'] td.trs-rule", "tr[data-tier='2'] td[data-zones='6']", "tr[data-tier='1'] td[data-zones='1']"}},
	)

	// membership is read, never worked out: a tier_zones / tier the rules
	// would not give is drawn as given; no answer: a dash, "not known"
	tz := mcpCopy(t, fetch["/api/config/tiers"])
	tzs := tz["tier_zones"].([]any)
	tzs[0].(map[string]any)["zones"] = []any{map[string]any{"id": "02:00:00:00:00:01", "name": "ps5-01"}}
	tzs[3].(map[string]any)["zones"] = []any{map[string]any{"id": "02:00:00:00:00:02", "name": "ps5-02"}}
	nodes := mcpCopy(t, fetch["/api/nodes"])
	for _, n := range nodes["nodes"].([]any) {
		if m := n.(map[string]any); m["name"] == "ps5-03" {
			delete(m, "tier")
		}
	}
	run(with(map[string]any{"/api/config/tiers": tz, "/api/nodes": nodes}),
		map[string]any{"name": "tiers: membership from the server only", "route": "#/tiers?tier=4", "status": st, "width": 1536,
			"pageHas": []string{"tr[data-tier='1'] td[data-zones='1']", "tr[data-tier='4'] td[data-zones='1']", "aside.side [data-panel-zones='1']",
				"tr[data-zone='ps5-03'] td[data-tier-now='-'][title*='known']"},
			"page": []string{"Tier 4 · 1 zone", "ps5-02"}})
	noTZ := mcpCopy(t, fetch["/api/config/tiers"])
	delete(noTZ, "tier_zones")
	run(with(map[string]any{"/api/config/tiers": noTZ}),
		map[string]any{"name": "tiers: no tier_zones", "route": "#/tiers?tier=1", "status": st, "width": 1536,
			"pageHas": []string{"tr[data-tier='1'] td[data-zones='-'][title^='Not']"},
			"page":    []string{"Tier 1 · – zones"}})

	// each edit in a run of its own: unsaved edits survive between screens
	run(fetch, map[string]any{"name": "tiers: stop excluding", "route": "#/tiers", "status": st, "click": []string{"button[data-rm=devbox-02]"},
		"expect": []string{"1 change", "1 excluded"}})
	run(fetch, map[string]any{"name": "tiers: named only, add", "route": "#/tiers", "status": st,
		"click":  []any{map[string]any{"type": "input[data-add=noauto]", "value": "ps5-03"}, "button[data-add-btn=noauto]"},
		"expect": []string{"2 named only", "ps5-03", "Unsaved", "1 change"}})
	run(fetch, map[string]any{"name": "tiers: named only, already there", "route": "#/tiers", "status": st,
		"click":  []any{map[string]any{"type": "input[data-add=noauto]", "value": "alienware-01"}, "button[data-add-btn=noauto]"},
		"expect": []string{"alienware-01 is already named only."}})

	// the save: a dry run of each part, then one dialog with the changes and the comment, then the writes
	saved := with(map[string]any{"PUT /api/config/tiers": map[string]any{"revision": 413}, "PUT /api/config/exclude-nodes": map[string]any{"revision": 414}})
	run(saved, map[string]any{"name": "tiers: save, the dialog", "route": "#/tiers", "status": st,
		"click":    []any{"tr[data-tier='1'] button[data-mv=down]", "button[data-rm=devbox-02]", "button[data-save]"},
		"page":     []string{"Save 2 changes?", "on top of revision 412", "Host name matches alienware*", "New order", "Stop excluding ", "devbox-02", "Comment (required)", "Save 2 changes"},
		"pageNone": []string{"[data-never]"}})
	run(saved, map[string]any{"name": "tiers: save, the writes", "route": "#/tiers", "status": st,
		"click": append([]any{"tr[data-tier='1'] button[data-mv=down]", "button[data-rm=devbox-02]"}, comment("devbox-02 is back")...),
		"fetchedQuery": []string{
			`^PUT /api/config/tiers\?dry_run=1 \{"tiers":\[\{"headroom_mib":256,"link_mb_s":60,"match":\{"hostname":"alienware\*"\},"priority":2,"settle":"8s"\},\{"headroom_mib":1024,`,
			`^PUT /api/config/exclude-nodes\?dry_run=1 \{"exclude_nodes":\["02:00:00:00:00:77"\]\}$`,
			`^PUT /api/config/tiers \{"tiers":\[\{"headroom_mib":256,"link_mb_s":60,"match":\{"hostname":"alienware\*"\}.*\],"comment":"devbox-02 is back"\}$`,
			`^PUT /api/config/exclude-nodes \{"exclude_nodes":\["02:00:00:00:00:77"\],"comment":"devbox-02 is back"\}$`},
		"sentHeader": []string{`^X-Comment: devbox-02 is back$`, `^If-Match: 412$`},
		"noHeader":   noKey,
		"none":       []string{"div.trs-save"}})
	// nothing is saved without the comment
	run(saved, map[string]any{"name": "tiers: no comment, no save", "route": "#/tiers", "status": st,
		"click":      []any{"tr[data-tier='1'] button[data-mv=down]", "button[data-save]", "div.dlg-foot button.pri"},
		"notFetched": []string{`^PUT /api/config/tiers {`},
		"expect":     []string{"1 change", "Nothing is applied until you save"}})

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
		"expect": []string{"Node overrides", "0 overrides", "No overrides: every zone uses the address the legacy fleet feed reports.",
			"Use a fixed rpc-server address for a zone instead of the one the legacy fleet feed reports."},
		"has": []string{"button[data-act=add-override]"}, "none": []string{"Not used in this mode"}})
}

// The Matches column: every field of a tier's match in words, joined with
// " and "; a key the page does not know as "<key> is <value>"; "Every zone
// (no rule)" only for a truly empty match.
func TestUINextTiersRuleWords(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--tiers", "live", "--zones", "live"}, tiersPaths...)
	cfg := mcpCopy(t, fetch["/api/config"])
	cfg["tiers"] = []any{
		map[string]any{"match": map[string]any{"platform": "linux-amd64-vulkan"}, "priority": 1},
		map[string]any{"match": map[string]any{"hostname": "alienware*"}, "priority": 2},
		map[string]any{"match": map[string]any{"local": true}, "priority": 3},
		map[string]any{"match": map[string]any{"platform": "linux-amd64-cuda", "hostname": "laptop-*"}, "priority": 4},
		map[string]any{"match": map[string]any{"role": "worker", "source": "agent", "device": "*RTX*"}, "priority": 5},
		map[string]any{"match": map[string]any{"local": false, "rack": "a"}, "priority": 6},
	}
	fetch["/api/config"] = cfg
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "tiers: the rule in words", "route": "#/tiers", "status": fetch["/api/status"], "width": 1536,
			"expect": []string{"Platform is linux-amd64-vulkan", "Host name matches alienware*", "Only the local zone",
				"Host name matches laptop-* and platform is linux-amd64-cuda",
				"Role is worker and source is agent and a device name matches *RTX*",
				"Only remote zones and rack is a"},
			"absent": []string{"Every zone (no rule)", "no rule"}},
	}})
}

// --tiers catchall: a truly empty match first. "Every zone (no rule)", and
// the tiers below it carry the server's never_reached: "Never reached: tier
// 1 above matches every zone", in the row and in the panel. A move that
// puts a tier below an empty match says the same in the save dialog.
func TestUINextTiersNeverReached(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--tiers", "catchall", "--zones", "live"}, tiersPaths...)
	st := fetch["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "tiers: catch-all first", "route": "#/tiers", "status": st, "width": 1536,
			"expect": []string{"Every zone (no rule)", "5 tiers", "Never reached: tier 1 above matches every zone"},
			"has":    []string{"tr[data-tier='1'] td[data-zones='9']", "tr[data-tier='2'] [data-never='2']", "tr[data-tier='5'] [data-never='5']", "tr[data-tier='2'] td[data-zones='0']"},
			"none":   []string{"tr[data-tier='1'] [data-never]"}},
		map[string]any{"name": "tiers: catch-all, the panel", "route": "#/tiers?tier=3", "status": st, "width": 1536,
			"page": []string{"Tier 3 · 0 zones", "Never reached: tier 1 above matches every zone"}, "pageHas": []string{"aside.side [data-never]"}},
	}})

	// the live list with an empty match added last; moving it up leaves tier 5 below it
	live := fixtureAnswers(t, []string{"--tiers", "live", "--zones", "live"}, tiersPaths...)
	cfg := mcpCopy(t, live["/api/config"])
	cfg["tiers"] = append(cfg["tiers"].([]any), map[string]any{"match": map[string]any{}, "priority": 5})
	tz := mcpCopy(t, live["/api/config/tiers"])
	tz["tiers"] = cfg["tiers"]
	tz["tier_zones"] = append(tz["tier_zones"].([]any), map[string]any{"index": 5, "zones": []any{map[string]any{"id": "02:00:00:00:00:08", "name": "devbox-02"}}})
	live["/api/config"], live["/api/config/tiers"] = cfg, tz
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": live, "cases": []any{
		map[string]any{"name": "tiers: a move that creates it", "route": "#/tiers", "status": live["/api/status"], "width": 1536,
			"click":   []any{"tr[data-tier='5'] button[data-mv=up]", "button[data-save]"},
			"page":    []string{"Save 1 change?", "Tier 5: Never reached: tier 4 above matches every zone"},
			"pageHas": []string{"[data-never='5']"}},
	}})
	if !strings.Contains(uiText(t, uiNextDir+"views/tiers.js"), "never_reached") {
		t.Error("next/views/tiers.js does not read never_reached")
	}
}

// Long pages: the rail and the section menu stay in place for the full
// height while the page scrolls (sticky, top 0, the window's height; checked
// in Chromium by scrolling: their rectangles stay 0..window height), and the
// .app background repeats their columns so a full-page picture, which draws
// a sticky element once, shows them full height too.
func TestUIRailAndMenuForLongPages(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	rule := func(sel string) string {
		i := strings.Index(css, "\n"+sel+"{")
		if i < 0 {
			t.Fatalf("app.css has no rule %s", sel)
		}
		j := strings.Index(css[i:], "}")
		return css[i : i+j]
	}
	for _, sel := range []string{".rail", ".menu"} {
		r := rule(sel)
		for _, want := range []string{"position:sticky", "top:0", "height:100vh"} {
			if !strings.Contains(r, want) {
				t.Errorf("app.css %s: no %s (it must stay in place while a long page scrolls)", sel, want)
			}
		}
	}
	for _, want := range []string{".app{background:linear-gradient(to right,var(--surface)", "var(--menu-bg) var(--rail-w)", ".app.menu-closed{background:linear-gradient("} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css: the .app background no longer repeats the rail and menu columns (%s)", want)
		}
	}
}
