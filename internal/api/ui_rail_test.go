package api

import (
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

// The health popup (Health board): the rows whose data exists, the ones
// that wait for theirs, the heart's dot and count.
func TestUINextHealthPopup(t *testing.T) {
	now := time.Now()
	st := ovStatus(now) // node-e set aside: one zone down; one request waiting
	st["queue"] = []any{map[string]any{"profile": "beta", "waiters": 2, "age": 348.0, "claim": []any{ovC}}}
	fetch := ovFetch(now, ovRequests(now), true)
	fetch["/api/requests/summary"] = map[string]any{"on": true, "totals": map[string]any{"requests": 9, "failed": 1, "refused": 0}, "series": []any{}}
	fetch["/api/system"] = map[string]any{"version": "v1.2.3", "uptime_s": 1036800, "read_panics": 0,
		"tasks":   []any{map[string]any{"name": "loop", "policy": "exit", "state": "running"}},
		"storage": map[string]any{"database": map[string]any{"bytes": 40000000}, "history": map[string]any{"bytes": 3000000}, "usage": map[string]any{"size_bytes": 7000000}}}
	fetch["/api/agents"] = map[string]any{"agents": []any{map[string]any{"id": "a1"}, map[string]any{"id": "a2"}, map[string]any{"id": "a3", "revoked_at": 1}}}
	fetch["/api/config/settings"] = map[string]any{"settings": map[string]any{"queue_timeout": "10m0s"}}
	fetch["/api/history"] = map[string]any{"events": []any{
		map[string]any{"ts": now.Add(-10 * time.Minute).UnixMilli(), "seq": 7, "type": "nodeQuarantined", "severity": "warn", "nodes": []any{"node-e"}, "data": map[string]any{"for_s": 600}},
		map[string]any{"ts": now.Add(-50 * time.Minute).UnixMilli(), "seq": 6, "type": "nodeLost", "severity": "error", "nodes": []any{"node-x"}},
	}}
	open := []string{"button.rb[title^=System]"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "health: the heart's dot", "route": "#/overview", "status": st,
			"pageHas": []string{"button.rb[title^=System] span.dotmark.bad"}},
		map[string]any{"name": "health: the popup's rows", "route": "#/overview", "status": st, "click": open,
			"pageHas": []string{"div.hp[role=dialog]", "button.rb[title^=System][aria-expanded=true]",
				"div.hp-row[data-health=zones] span.hd.bad", "div.hp-row[data-health=runs] span.hd.warn", "div.hp-row[data-health=scheduler] span.hd.ok",
				"div.hp-row[data-health=mcp] span.hd.ok", "div.hp-row[data-health=storage] span.hd.wait", "div.hp-row[data-health=queue] span.hd.warn", "div.hp-row.nolink[data-health=alerts] span.hd.wait"},
			"page": []string{"System health", "3 things need attention", "Zones", "5 of 6 reachable · 1 down", "Runs", "1 failed in the last hour",
				"Scheduler", "Running · release ", "v1.2.3", " · up 12 d", "MCP server", "Running · 2 keys active", "Storage", "of Potluck data · disk: waiting for data",
				"Queue", "2 waiting · longest 5 min 48 s", "Alerts", "Not built yet: waiting for data",
				"Last 2 hours", "All activity", "failed a load and was set aside until", "went offline", "Alert settings"}},
		map[string]any{"name": "health: a row opens its page", "route": "#/overview", "status": st, "click": []string{"button.rb[title^=System]", "div.hp-row[data-health=queue]"},
			"pageNone": []string{"div.hp"}},
		map[string]any{"name": "health: the close button", "route": "#/overview", "status": st, "click": []string{"button.rb[title^=System]", "div.hp button.hp-x"},
			"pageNone": []string{"div.hp"}},
	}})
	// nothing read yet: every row waits, the heart has no dot
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "health: before any data", "route": "#/overview", "click": []string{"button.rb[title^=System]"},
			"page": []string{"Waiting for the first status", "Waiting for data"}, "pageNone": []string{"span.dotmark"}},
	}})
}
