package api

import (
	"fmt"
	"testing"
	"time"
)

// The rail as the design draws it (Phase 2, SH-1, SH-2, Q69, Q73): buttons
// with titles, the mark not a link, Ask Potluck and the heart at the foot,
// the gear holding the configuration pages and the classic UI link.
func TestUINextRail(t *testing.T) {
	now := time.Now()
	st := ovStatus(now)
	fetch := ovFetch(now, ovRequests(now), true)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "rail: buttons with titles, in the design's order", "route": "#/overview", "status": st,
			"pageHas": []string{"button.rb[title=Monitor][aria-current=page]", "button.rb[title=Zones]", "button.rb[title=Models]", "button.rb[title=Benchmarks]",
				"button.rb[title^=Ask]", "button.rb[title=MCP]", "button.rb[title^=System][aria-expanded=false]", "button.rb[title=Settings]", "span.mark"},
			"pageNone": []string{"a.rb", "a.mark", "a.uiswitch"},
			// the Monitor menu as drawn: Overview, Models, Queue, Activity
			"links":     []map[string]string{{"text": "Overview", "path": "/ui/"}, {"text": "Models", "path": "/ui/"}, {"text": "Activity", "path": "/ui/"}},
			"page":      []string{"Queue"},
			"notOnPage": []string{"Placement preview", "Configure", "Timing defaults"}},
		// a rail button opens its section's first page
		map[string]any{"name": "rail: Zones opens Zones", "route": "#/overview", "status": st, "click": []string{"button.rb[title=Zones]"}, "hash": "#/zones"},
		// the gear's menu (Q69): the five pages, the placement preview until it folds, the classic UI (Q73)
		map[string]any{"name": "rail: the Settings menu", "route": "#/profiles", "status": st,
			"pageHas": []string{"button.rb[title=Settings][aria-current=page]"},
			"page":    []string{"Profiles", "Tiers and zones", "Settings", "Audit", "About", "Placement preview"},
			"links":   []map[string]string{{"text": "Classic UI", "path": "/ui/classic/"}}},
		// Ask Potluck: a drawer that says it is not built yet
		map[string]any{"name": "rail: Ask Potluck", "route": "#/overview", "status": st, "click": []string{"button.rb[title^=Ask]"},
			"pageHas": []string{"button.rb[title^=Ask][aria-expanded=true]", "aside.drw2"}, "page": []string{"Ask Potluck is not built yet"}},
		map[string]any{"name": "rail: Ask Potluck closes", "route": "#/overview", "status": st, "click": []string{"button.rb[title^=Ask]", "button[aria-label^=Close]"},
			"pageNone": []string{"aside.drw2"}},
	}})
}

// healthDoc is a GET /api/health answer (health.go, HealthSummary) with the
// seven rows in the design's order; states maps a row id to its state and
// sentence, the others are ok.
func healthDoc(now time.Time, states map[string][2]string, recent []any) map[string]any {
	type row struct{ id, label, text, link string }
	rows := []row{
		{"zones", "Zones", "8 of 8 reachable", "#/zones"},
		{"runs", "Runs", "None failed in the last hour", "#/overview"},
		{"scheduler", "Scheduler", "Running · release v1-test · up 12 d", "#/about"},
		{"mcp", "MCP server", "Running · 3 keys active", "#/mcp"},
		{"storage", "Storage", "50.3 of 240 GB used", "#/about"},
		{"queue", "Queue", "Nothing waiting", "#/monitor/queue"},
		{"alerts", "Alerts", "Not set up yet", "#/health/alerts"},
	}
	out := []any{}
	issues, worst := 0, "ok"
	for _, r := range rows {
		state, text := "ok", r.text
		if r.id == "alerts" {
			state = "unknown"
		}
		if s, ok := states[r.id]; ok {
			state, text = s[0], s[1]
		}
		switch state {
		case "attention":
			issues++
			if worst == "ok" {
				worst = "attention"
			}
		case "problem":
			issues++
			worst = "problem"
		}
		out = append(out, map[string]any{"id": r.id, "label": r.label, "state": state, "text": text, "count": 0, "link": r.link, "facts": map[string]any{}})
	}
	sentence := map[int]string{0: "All good", 1: "1 thing needs attention"}[issues]
	if sentence == "" {
		sentence = fmt.Sprintf("%d things need attention", issues)
	}
	if recent == nil {
		recent = []any{}
	}
	return map[string]any{"state": worst, "color": map[string]string{"ok": "green", "attention": "amber", "problem": "red"}[worst],
		"issues": issues, "sentence": sentence, "updated_at": now.UTC().Format(time.RFC3339), "rows": out, "recent": recent,
		"recent_link": "#/activity?range=2h&sev=warn", "alerts_link": "#/health/alerts"}
}

// The health popup (Health board) draws GET /api/health and the "health"
// topic as the server judged them: the sentences as written, a dot per row
// (ok, attention, problem; a row the server cannot judge, Alerts "Not set
// up yet", gets the neutral ring), each row a link to its page, the heart's
// dot and issue count from the same document. Nothing is judged in the page.
func TestUINextHealthPopup(t *testing.T) {
	now := time.Now()
	st := ovStatus(now)
	open := []string{"button.rb[data-surface=health]"}
	good := healthDoc(now, nil, nil)
	two := healthDoc(now, map[string][2]string{
		"zones": {"problem", "7 of 8 reachable · 1 down"},
		"runs":  {"attention", "1 failed in the last hour"},
	}, []any{
		map[string]any{"at": now.Add(-10 * time.Minute).UTC().Format(time.RFC3339), "seq": 7, "type": "nodeQuarantined", "severity": "warn",
			"text": "node-e failed a load and was set aside", "node": "node-e", "until": now.Add(8 * time.Minute).UTC().Format(time.RFC3339)},
		map[string]any{"at": now.Add(-50 * time.Minute).UTC().Format(time.RFC3339), "seq": 6, "type": "nodeLost", "severity": "error",
			"text": "node-x went offline", "node": "node-x"},
		map[string]any{"at": now.Add(-70 * time.Minute).UTC().Format(time.RFC3339), "seq": 5, "type": "requestRefused", "severity": "warn",
			"text": "A request for beta was refused"},
	})
	hello := map[string]any{"type": "hello", "data": map[string]any{"topics": []any{"status", "event", "health"}}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{"/api/health": two}, "cases": []any{
		// two issues: the summary, the rows as the server wrote them, the recent lines
		map[string]any{"name": "health: two issues", "route": "#/overview", "status": st, "click": open,
			"pageHas": []string{"div.hp[role=dialog]", "button.rb[data-surface=health][aria-expanded=true]", "button.rb[data-surface=health][data-issues=2]",
				"button.rb[data-surface=health] span.dotmark", "div.hp-sum span.hd.bad",
				"a.hp-row[data-health=zones] span.hd.bad", "a.hp-row[data-health=runs] span.hd.warn", "a.hp-row[data-health=scheduler] span.hd.ok",
				"a.hp-row[data-health=mcp] span.hd.ok", "a.hp-row[data-health=storage] span.hd.ok", "a.hp-row[data-health=queue] span.hd.ok",
				"a.hp-row[data-health=alerts] span.hd.wait",
				"a.hp-row[data-health=zones][href='#/zones']", "a.hp-row[data-health=queue][href='#/monitor/queue']", "a.hp-row[data-health=alerts][href='#/health/alerts']",
				"div.hp-ev span.mono", "a.hp-foot[href='#/health/alerts']", "div.hp-last a[href='#/activity?range=2h&sev=warn']"},
			"pageNone": []string{"div.hp-row", "span.hr-count"},
			"page": []string{"System health", "2 things need attention", "Zones", "7 of 8 reachable · 1 down", "Runs", "1 failed in the last hour",
				"Scheduler", "Running · release v1-test · up 12 d", "MCP server", "Running · 3 keys active", "Storage", "50.3 of 240 GB used",
				"Queue", "Nothing waiting", "Alerts", "Not set up yet",
				"Last 2 hours", "All activity", "node-e", " failed a load and was set aside until ", "node-x", " went offline", "A request for beta was refused", "Alert settings"}},
		// a row is a link: following it closes the popup and goes there
		map[string]any{"name": "health: a row opens its page", "route": "#/overview", "status": st, "click": []string{"button.rb[data-surface=health]", "a.hp-row[data-health=mcp]"},
			"pageNone": []string{"div.hp"}, "hash": "#/mcp"},
		map[string]any{"name": "health: the close button", "route": "#/overview", "status": st, "click": []string{"button.rb[data-surface=health]", "div.hp button.hp-x"},
			"pageNone": []string{"div.hp"}},
		// all good: no dot on the heart, no count, the server's sentence
		map[string]any{"name": "health: no issues", "route": "#/overview", "status": st, "fetch": map[string]any{"/api/health": good}, "click": open,
			"pageHas":  []string{"div.hp-sum span.hd.ok", "button.rb[data-surface=health][data-issues=0]", "a.hp-row[data-health=alerts] span.hd.wait"},
			"pageNone": []string{"button.rb[data-surface=health] span.dotmark", "span.hd.bad", "span.hd.warn"},
			"page":     []string{"All good", "8 of 8 reachable", "Nothing to report in the last 2 hours."}, "notOnPage": []string{"need attention"}},
		// a frame on the health topic changes the heart and an open popup at once
		map[string]any{"name": "health: the topic's frame", "route": "#/overview", "status": st, "fetch": map[string]any{"/api/health": good}, "click": open,
			"ws": []any{hello}, "wsAfter": []any{map[string]any{"type": "health", "data": two}},
			"pageHas": []string{"button.rb[data-surface=health][data-issues=2]", "button.rb[data-surface=health] span.dotmark", "a.hp-row[data-health=zones] span.hd.bad"},
			"page":    []string{"2 things need attention", "7 of 8 reachable · 1 down"}},
	}})
	// a server that does not answer /api/health: the popup says it has no data, the heart has no dot
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "health: before any data", "route": "#/overview", "click": open,
			"page": []string{"System health", "Waiting for data"}, "pageNone": []string{"span.dotmark", "a.hp-row"}},
	}})
}

// The heart's dot is amber when the worst row needs attention, red for a
// problem; its label counts the issues ("System health, 1 issue").
func TestUINextHealthHeart(t *testing.T) {
	now := time.Now()
	one := healthDoc(now, map[string][2]string{"queue": {"attention", "2 waiting · longest 8 min"}}, nil)
	hello := map[string]any{"type": "hello", "data": map[string]any{"topics": []any{"status", "health"}}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "heart: attention", "route": "#/overview", "status": ovStatus(now),
			"ws":      []any{hello, map[string]any{"type": "health", "data": one}},
			"pageHas": []string{"button.rb[data-surface=health][data-issues=1] span.dotmark.warn", "button.rb[aria-label$=issue]"}},
	}})
}
