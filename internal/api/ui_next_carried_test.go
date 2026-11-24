package api

import (
	"strings"
	"testing"
	"time"
)

// Small fixes on the carried-over screens (About, Activity, Queue, Machine
// details, the 24-hour report).

// About, Activity and Queue draw two columns with .cols: a grid on desktop
// widths, so both columns start on the same line, and the right-hand column
// (.cols>.side) has none of the docked side panel's look (.side: white
// ground, left border, shadow, 620 px), which drew it as a white box.
// Checked in Chromium at 1280 and 1536: both columns' tops at the same y.
func TestUITwoColumnsAlign(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	for _, want := range []string{
		".cols>.side{width:auto;background:none;border-left:0;box-shadow:none;overflow:visible;font-size:inherit}",
		".cols{display:grid;grid-template-columns:minmax(0,1fr) clamp(320px,30%,420px);align-items:start}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %s", want)
		}
	}
	for _, f := range []string{"views/about.js", "views/activity.js", "views/queue.js"} {
		if s := uiText(t, f); !strings.Contains(s, "h('div.cols'") {
			t.Errorf("%s no longer draws its two columns with .cols", f)
		}
	}
}

// The report's token chart fills its card and stays inside it (it was a
// narrow strip: .tokens did not grow in the report's flex box).
func TestUIReportChartFillsItsCard(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	if !strings.Contains(css, ".tokens{display:flex;gap:18px;padding:2px var(--pad-panel) 14px;flex:1 1 auto;min-width:0;box-sizing:border-box;max-width:100%}") {
		t.Error("app.css: .tokens does not grow and shrink inside its card")
	}
}

// Activity: the learnedNeed events (memory need, host and per-device
// overhead, readings not used, the higher reading kept, a need raised) read
// as plain sentences, never "it needs –"; an unmeasured MB/s is a dash,
// never 0. Machine details is titled so.
func TestUIActivitySentencesAndMachineDetails(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--zones", "live"}, "/api/status")
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).UnixMilli()
	ev := func(ago int, typ, profile string, nodes []string, data map[string]any) map[string]any {
		return map[string]any{"ts": now - int64(ago)*60000, "seq": ago, "type": typ, "severity": "info", "profile": profile, "nodes": nodes,
			"summary": "server words for " + typ, "data": data}
	}
	events := []any{
		ev(1, "learnedNeed", "qwen3-14b", []string{"ps5-01"}, map[string]any{"measured_mib": 9216}),
		ev(2, "learnedNeed", "qwen3-14b", []string{"ps5-01"}, map[string]any{"node": "ps5-01", "host_overhead_mib": 412, "in_force_mib": 450, "devices": 2}),
		ev(3, "learnedNeed", "qwen3-14b", []string{"ps5-02"}, map[string]any{"node": "ps5-02", "device_overhead_mib": 96, "in_force_mib": 96, "devices": 1}),
		ev(4, "learnedNeed", "qwen3-14b", []string{"ps5-03"}, map[string]any{"node": "ps5-03", "host_overhead_reading_mib": 99999}),
		ev(5, "learnedNeed", "qwen3-14b", []string{"ps5-04"}, map[string]any{"node": "ps5-04", "kept_mib": 480, "was_mib": 410, "devices": 2}),
		ev(6, "learnedNeed", "qwen3-14b", []string{"ps5-05"}, map[string]any{"from_mib": 8192, "to_mib": 9216, "exit": "oom_killed", "node": "ps5-05"}),
		ev(7, "loadFinished", "qwen3-14b", []string{"ps5-01"}, map[string]any{"outcome": "ready", "load_s": 4, "avg_mb_s": 0}),
		ev(8, "loadFinished", "gemma-4-27b", []string{"ps5-02"}, map[string]any{"outcome": "ready", "load_s": 21, "avg_mb_s": 104.6}),
	}
	f := map[string]any{
		"/api/history/summary":  map[string]any{"counts": map[string]any{}},
		"/api/metrics/activity": map[string]any{"data": []any{}},
		"/api/history":          map[string]any{"history": "on", "events": events},
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "now": "2026-11-24T12:00:00Z", "tz": "UTC", "fetch": f, "cases": []any{
		map[string]any{"name": "activity: measured overhead in words", "route": "#/activity", "status": fetch["/api/status"], "width": 1536,
			"expect": []string{"Measured qwen3-14b: it needs 9.0 GB.",
				"Measured how much memory qwen3-14b takes on its host on ps5-01 with 2 devices beyond the model: 412 MiB. 450 MiB is used from now on.",
				"Measured how much extra memory qwen3-14b takes on each device on ps5-02 with 1 device: 96 MiB.",
				"A host memory reading of qwen3-14b on ps5-03 was 99,999 MiB, which cannot be right: it was not used.",
				"Kept the higher of the host memory readings on ps5-04 with 2 devices: 480 MiB, not 410 MiB.",
				"The memory need of qwen3-14b was raised from 8.0 GB to 9.0 GB after its worker on ps5-05 was ended (oom killed).",
				"qwen3-14b is ready: loaded in 4 s.", "at 105 MB/s"},
			"absent": []string{"it needs –", "at 0 MB/s", "server words for learnedNeed"}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "machine details: the title", "route": "#/nodes", "status": fetch["/api/status"],
			"expect": []string{"Machine details"}, "has": []string{"div.page-head h1"}},
	}})
	if s := uiText(t, "views/nodes.js"); strings.Contains(s, "pageHead('Nodes'") || !strings.Contains(s, "title: 'Machine details'") {
		t.Error("views/nodes.js: the page is still titled Nodes")
	}
}
