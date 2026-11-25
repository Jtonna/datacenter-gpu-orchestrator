package api

import (
	"strings"
	"testing"
	"time"
)

// Small fixes for the UI image on v59: zone names beside stored ids on the
// Tiers page, "Named only" on Machine details, and no MB/s figure for a load
// that uploaded almost nothing.

// Tiers page, Excluded zones and Named only: a stored entry that is the id of
// a known zone shows the zone's name beside it (name · id, the name from GET
// /api/nodes); a stored name or pattern stays as it is; an id no zone has
// stays bare. The stored value is never changed: Remove sends the list
// without exactly that value, and the other entries as stored.
func TestUINextTiersZoneNamesBesideIds(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--tiers", "live", "--zones", "live"}, tiersPaths...)
	st := fetch["/api/status"]
	idOf := map[string]string{}
	for _, n := range fetch["/api/nodes"].(map[string]any)["nodes"].([]any) {
		m := n.(map[string]any)
		idOf[m["name"].(string)] = m["id"].(string)
	}
	id3, id4 := idOf["ps5-03"], idOf["ps5-04"]
	if id3 == "" || id4 == "" || id3 == "ps5-03" {
		t.Fatalf("the fixture's /api/nodes has no ids for ps5-03 / ps5-04: %v", idOf)
	}
	cfg := mcpCopy(t, fetch["/api/config"])
	cfg["exclude_nodes"] = []any{"devbox-02", "02:00:00:00:00:77", id4}
	cfg["no_auto_nodes"] = []any{"alienware-01", id3, "ps5-test*"}
	f := map[string]any{}
	for k, v := range fetch {
		f[k] = v
	}
	f["/api/config"] = cfg
	f["PUT /api/config/exclude-nodes"] = map[string]any{"revision": 413}
	f["PUT /api/config/no-auto-nodes"] = map[string]any{"revision": 414}
	run := func(cases ...any) {
		t.Helper()
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": cases})
	}
	run(map[string]any{"name": "tiers: a known id shows its zone's name", "route": "#/tiers", "status": st, "width": 1536,
		"expect": []string{"ps5-03 · " + id3, "ps5-04 · " + id4, "3 excluded", "3 named only"},
		"has": []string{
			"[data-list=noauto] li[data-entry='" + id3 + "'] [data-name='ps5-03']",
			"[data-list=exclude] li[data-entry='" + id4 + "'] [data-name='ps5-04']",
			"[data-list=noauto] li[data-entry='alienware-01']", "[data-list=noauto] li[data-entry='ps5-test*']",
			"[data-list=exclude] li[data-entry='02:00:00:00:00:77']",
			"button[data-rm='" + id3 + "']"},
		"none": []string{
			"li[data-entry='alienware-01'] [data-name]", "li[data-entry='ps5-test*'] [data-name]",
			"li[data-entry='devbox-02'] [data-name]", "li[data-entry='02:00:00:00:00:77'] [data-name]"}})
	run(map[string]any{"name": "tiers: Remove sends the stored values", "route": "#/tiers", "status": st,
		"click": []any{"button[data-rm='" + id4 + "']", "button[data-rm=alienware-01]", "button[data-save]",
			map[string]any{"type": "textarea.dlg-comment", "value": "names"}, "div.dlg-foot button.pri"},
		"fetchedQuery": []string{
			`^PUT /api/config/exclude-nodes\?dry_run=1 \{"exclude_nodes":\["devbox-02","02:00:00:00:00:77"\]\}$`,
			`^PUT /api/config/no-auto-nodes\?dry_run=1 \{"no_auto_nodes":\["` + id3 + `","ps5-test\*"\]\}$`,
			`^PUT /api/config/no-auto-nodes \{"no_auto_nodes":\["` + id3 + `","ps5-test\*"\],"comment":"names"\}$`}})
}

// Machine details: a zone in no_auto_nodes reads "Named only", the owner's
// word and the Tiers page's; no "Forced only" left in the new UI for it.
func TestUINextNamedOnlyWord(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--zones", "live", "--named-only", "ps5-02"}, "/api/status")
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "machine details: Named only", "route": "#/nodes", "status": fetch["/api/status"], "width": 1536,
			"expect": []string{"Named only"}, "absent": []string{"Forced only", "Forced-only", "forced only"}},
	}})
	for _, f := range []string{"views/nodes.js", "core/configdata.js"} {
		if s := uiText(t, f); strings.Contains(s, "Forced only") || strings.Contains(s, "Forced-only") {
			t.Errorf("%s still shows Forced only for no_auto_nodes", f)
		}
	}
}

// Activity: a load that uploaded less than 1 MiB (a cached load), or whose
// rate is absent, 0 or not a number, shows a dash for MB/s in the Loads
// table and no "at N MB/s" in the ready sentence; a real upload keeps it.
func TestUIActivityTinyUploadHasNoRate(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--zones", "live"}, "/api/status")
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).UnixMilli()
	ev := func(ago int, profile string, data map[string]any) map[string]any {
		return map[string]any{"ts": now - int64(ago)*60000, "seq": ago, "type": "loadFinished", "severity": "info", "profile": profile, "nodes": []string{"ps5-01"},
			"summary": "server words", "data": data}
	}
	events := []any{
		ev(1, "qwen3-14b", map[string]any{"outcome": "ready", "load_s": 6.3, "bytes": 414000, "avg_mb_s": 0.066}),
		ev(2, "gemma-4-27b", map[string]any{"outcome": "ready", "load_s": 21, "bytes": 2200000000, "avg_mb_s": 104.6}),
		ev(3, "phi-4", map[string]any{"outcome": "ready", "load_s": 9, "bytes": 900000000, "avg_mb_s": "n/a"}),
		ev(4, "mistral-7b", map[string]any{"outcome": "ready", "load_s": 2, "bytes": 1048575, "avg_mb_s": 0.5}),
	}
	f := map[string]any{
		"/api/history/summary":  map[string]any{"counts": map[string]any{}},
		"/api/metrics/activity": map[string]any{"data": []any{}},
		"/api/history":          map[string]any{"history": "on", "events": events},
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "now": "2026-11-24T12:00:00Z", "tz": "UTC", "fetch": f, "cases": []any{
		map[string]any{"name": "activity: a tiny upload has no rate", "route": "#/activity", "status": fetch["/api/status"], "width": 1536,
			"expect": []string{"gemma-4-27b is ready: loaded in 21 s at 105 MB/s."},
			"titles": []string{"Less than 1 MiB uploaded"},
			"absent": []string{"0.1 MB/s", "0.5 MB/s", "at 0.1", "at 0.5", "n/a MB/s"}},
	}})
}

// A card whose zone rows (.nr: name, bar, figure, sentence) do not fit their
// four columns stacks them, as on a narrow window: beside the docked Models
// panel the instance card's "No memory was reported for this node" ran out
// of the card under the panel. Checked in Chromium at 1536 with the panel.
func TestUINodeRowsStackInANarrowCard(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	for _, want := range []string{".card:has(>.nr){container-type:inline-size}",
		"@container (max-width:800px){.nr{grid-template-columns:minmax(0,1fr) auto}.nr>.bar{grid-column:1/-1;order:3}.nr>:nth-child(4){grid-column:1/-1;order:4}"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %s", want)
		}
	}
}

// Models with v60's read additions (models slice 1): the panel's "added
// <date>" from added_at (the Benchmarks count is the runs its list shows:
// TestUINextNotesPanelDeeper), the
// configurations' "Unload when idle" from ttl_s (0 = Never), and the disk
// line's hover "… GB free on <host>" from disk.host. A server without them
// (an older image) keeps the earlier drawing: no added date, no
// idle column, no host.
func TestUINextModelsSliceOneFields(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	const loaded = "gemma-4-27b-it-Q4_K_M.gguf"
	uiExec(t, map[string]any{"shell": "next", "app": true, "tz": "UTC", "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: slice 1 fields in the panel", "route": "#/models?model=" + loaded, "status": st,
			"page": []string{"16.5 GB · added 4 Nov 2026 · loaded now", "Benchmarks", "Unload when idle", "10 min", "Never"},
			"pageHas": []string{"aside.side tr[data-profile='gemma-4-27b'] td[data-ttl='600']", "aside.side tr[data-profile='gemma-4-27b:long'] td[data-ttl='0']",
				"aside.side button.btn[data-act=bench]"},
			"notOnPage": []string{"4 runs", "3 reports"}},
		map[string]any{"name": "models: the disk's host", "route": "#/models", "status": st, "titles": []string{"GB free on scheduler-host"}},
	}})

	old := mcpCopy(t, fetch["/api/models"])
	delete(old["disk"].(map[string]any), "host")
	for _, m := range old["models"].([]any) {
		e := m.(map[string]any)
		delete(e, "added_at")
		delete(e["bench"].(map[string]any), "reports")
		for _, p := range e["profiles"].([]any) {
			delete(p.(map[string]any), "ttl_s")
		}
	}
	f := map[string]any{"/api/models": old, "/api/status": st}
	uiExec(t, map[string]any{"shell": "next", "app": true, "tz": "UTC", "fetch": f, "cases": []any{
		map[string]any{"name": "models: an older server", "route": "#/models?model=" + loaded, "status": st,
			"page": []string{"16.5 GB · loaded now"}, "notOnPage": []string{"added ", "reports", "Unload when idle"},
			"pageNone": []string{"aside.side td[data-ttl]"}},
	}})
}

// Models: the side panel, docked beside the page (1400 px and wider), closes
// the section menu as the Tiers and MCP panels do; the board draws no menu
// beside it, and the list keeps its Profiles column.
func TestUINextModelsPanelClosesMenu(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: docked panel, menu closed", "route": "#/models?model=gemma-4-27b-it-Q4_K_M.gguf", "status": fetch["/api/status"], "width": 1536,
			"pageHas": []string{"aside.side.docked", ".app.menu-closed"}},
		map[string]any{"name": "models: no panel, menu open", "route": "#/models", "status": fetch["/api/status"], "width": 1536,
			"pageNone": []string{".app.menu-closed"}},
	}})
}

// The model page's "Show the N generated configurations" is a text button
// at the foot of the Configurations card. A later rule for the Zones fold
// icon (.more: 20 px square, centred) made it 20 px wide, so its words ran
// out of the card to the left, under the section menu. The icon's rule is
// now scoped to its place (.gn .more); the text button keeps its own.
func TestUIMoreButtonStaysInItsCard(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	if strings.Contains(css, "\n.more{width:20px") {
		t.Error("app.css: a bare .more rule makes every Show/Hide text button 20 px wide")
	}
	if !strings.Contains(css, ".gn .more{width:20px;height:20px") {
		t.Error("app.css: the Zones fold icon (.gn .more) lost its size")
	}
	if s := uiText(t, "views/models/detail.js"); !strings.Contains(s, "h('div.cs', more(showGenerated") {
		t.Error("views/models/detail.js: the generated-configurations button is no longer in the card's foot")
	}
}

// Profiles side panel, "Where it would go now": an automatic profile's plan
// says how many machines ("would load on 1 …"), no names; a forced profile
// names its own zones; a loaded profile names the machines it is loaded on.
func TestUINextProfilesPanelPlanNames(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles", "/api/models", "/api/config", "/api/config/schema", "/api/status")
	st := mcpCopy(t, fetch["/api/status"])
	idOf := map[string]string{}
	for _, n := range st["view"].(map[string]any)["nodes"].([]any) {
		m := n.(map[string]any)
		idOf[m["hostname"].(string)] = m["id"].(string)
	}
	st["instances"] = append(st["instances"].([]any), map[string]any{"profile": "gemma-4-27b", "state": "ready", "in_flight": 0, "waiters": 0, "parallel": 1,
		"nodes": []any{map[string]any{"id": idOf["ps5-03"], "role": "host", "share": 0.5}, map[string]any{"id": idOf["ps5-04"], "role": "helper", "share": 0.5}}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "profiles panel: automatic, a count", "route": "#/profiles?profile=qwen3-14b", "status": st,
			"page": []string{"Where it would go now", "would load on 1 "}, "pageHas": []string{"aside.side [data-would='1']"},
			"pageNone": []string{"aside.side span.nodes"}, "notOnPage": []string{"ps5-05"}},
		map[string]any{"name": "profiles panel: forced, its names", "route": "#/profiles?profile=qwen3-32b-split", "status": st,
			"page": []string{"ps5-03", "ps5-04", "ps5-05"}, "pageHas": []string{"aside.side span.nodes"}, "pageNone": []string{"aside.side [data-would]"}},
		map[string]any{"name": "profiles panel: loaded, where it is loaded", "route": "#/profiles?profile=gemma-4-27b", "status": st,
			"page": []string{"Loaded on", "ps5-03", "ps5-04"}, "pageHas": []string{"aside.side [data-loaded-on] span.nodes"}, "pageNone": []string{"aside.side [data-would]"}},
	}})
	// the plan says it is loaded (serve) but the status has no instance of it yet: a count, never "would load"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "profiles panel: loaded, no instance in the status", "route": "#/profiles?profile=gemma-4-27b", "status": fetch["/api/status"],
			"pageHas": []string{"aside.side [data-would='2']"}, "pageNone": []string{"aside.side span.nodes"}, "notOnPage": []string{"would load on"}},
	}})
}
