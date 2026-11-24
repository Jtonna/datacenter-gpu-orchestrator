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
