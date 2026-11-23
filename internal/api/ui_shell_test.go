package api

import (
	"testing"
	"time"
)

// The shell's shared components (Phase 2, SH-4, SH-5, SH-6): run on the
// fixture's components page (scripts/ui-fixture/components.js, loaded before
// app.js as ui-fixture-server.mjs serves it) and on the pages that use them.

// fixtureRuns are GET /api/requests/{id} answers for the components page.
func fixtureRuns(now time.Time) map[string]any {
	return map[string]any{
		"/api/requests/101": map[string]any{"id": 101, "status": "done", "profile": "alpha", "queued_at": now.Add(-time.Minute).UnixMilli(), "wait_ms": 120, "duration_ms": 22600,
			"tokens_in": 1200, "tokens_out": 340, "nodes": []any{map[string]any{"id": "02:00:00:00:00:01", "name": "node-a", "role": "host", "share": 0.6}}},
		"/api/requests/102": map[string]any{"id": 102, "status": "failed", "profile": "beta", "queued_at": now.Add(-2 * time.Minute).UnixMilli(), "wait_ms": 80, "duration_ms": 900, "nodes": []any{}},
	}
}

// The side panel (S15, Q72): beside the page at 1400 px and wider (in the
// app's row, no scrim, not modal), over it below that (a scrim, a modal
// dialog); a header with its label and title, a footer with its actions;
// Escape closes it and the focus returns to the button that opened it; one
// panel at a time; a resize moves an open panel between the two places.
func TestUINextSidePanel(t *testing.T) {
	now := time.Now()
	fetch := fixtureRuns(now)
	open := []string{"button[data-run=101]"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "modules": []string{"_fixture/components.js"}, "fetch": fetch, "cases": []any{
		map[string]any{"name": "panel: beside the page at 1440", "route": "#/components", "width": 1440, "click": open,
			"pageHas":  []string{"div.app aside.side.docked", "aside.side div.side-head h2.side-title", "aside.side div.side-label", "aside.side div.sf a.btn", "aside.side button.x[aria-label^=Close]"},
			"pageNone": []string{"div.side-back", "aside.side[aria-modal]"},
			"page":     []string{"Run", "#101", "alpha", "Tokens out", "340", "All runs"}, "hash": "#/components/run/101"},
		map[string]any{"name": "panel: over the page at 1200", "route": "#/components", "width": 1200, "click": open,
			"pageHas":  []string{"div.side-back aside.side[role=dialog][aria-modal=true]"},
			"pageNone": []string{"aside.side.docked"}},
		map[string]any{"name": "panel: Escape closes it and the focus returns", "route": "#/components", "width": 1440, "click": open, "keys": []any{"Escape"},
			"pageNone": []string{"aside.side"}, "focused": "button[data-run=101]", "hash": "#/components"},
		map[string]any{"name": "panel: the close button", "route": "#/components", "width": 1200, "click": []string{"button[data-run=101]", "aside.side button.x"},
			"pageNone": []string{"aside.side", "div.side-back"}, "focused": "button[data-run=101]"},
		map[string]any{"name": "panel: one at a time", "route": "#/components", "width": 1440, "click": []string{"button[data-run=101]", "button[data-run=102]"},
			"pageHas": []string{"aside.side h2.side-title"}, "page": []string{"beta"}, "notOnPage": []string{"alpha"}, "hash": "#/components/run/102"},
		map[string]any{"name": "panel: a resize moves it beside the page", "route": "#/components", "width": 1200, "click": open, "widthAfter": 1440,
			"pageHas": []string{"div.app aside.side.docked"}, "pageNone": []string{"div.side-back"}},
		map[string]any{"name": "panel: a resize moves it over the page", "route": "#/components", "width": 1440, "click": open, "widthAfter": 1000,
			"pageHas": []string{"div.side-back aside.side[aria-modal=true]"}, "pageNone": []string{"aside.side.docked"}},
		// opened from the address alone: no opener to return to, the panel still opens
		map[string]any{"name": "panel: from the address", "route": "#/components/run/102", "width": 1440,
			"pageHas": []string{"aside.side.docked"}, "page": []string{"#102", "beta"}},
	}})
}

// A page that already has a panel gets the docked one: the zone panel sits
// beside the Zones page at 1440 and over it at 1200.
func TestUINextZonesPanelDocks(t *testing.T) {
	now := time.Now()
	st := ovStatus(now)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "zones: docked at 1440", "route": "#/zones?zone=" + ovA, "status": st, "width": 1440,
			"pageHas": []string{"div.app aside.side.docked"}, "pageNone": []string{"div.side-back"}},
		map[string]any{"name": "zones: over at 1200", "route": "#/zones?zone=" + ovA, "status": st, "width": 1200,
			"pageHas": []string{"div.side-back aside.side"}},
	}})
}

// Deep links (S17, SH-5): the design's addresses inside the hash. A zone's
// panel is #/zones/<zone name>; the old ?zone=<id> still opens it and the
// page then writes the path; opening a panel adds a history entry, so Back
// closes it and Forward opens it again; a name the status does not know yet
// is looked up again when the status arrives; the server's own links
// (#/monitor/queue, #/zones?filter=down) open their pages.
func TestUINextDeepLinks(t *testing.T) {
	now := time.Now()
	st := ovStatus(now)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "deep link: a zone by name", "route": "#/zones/node-a", "status": st, "width": 1440,
			"pageHas": []string{"aside.side.docked", "tr.sel[aria-label^=node-a]"}, "page": []string{"node-a"}},
		map[string]any{"name": "deep link: the old form", "route": "#/zones?zone=" + ovA, "status": st,
			"pageHas": []string{"aside.side"}},
		// a row click writes the design's address and adds an entry; Back closes the panel
		map[string]any{"name": "deep link: a row opens the path", "route": "#/zones", "status": st, "click": []string{"tr[aria-label^=node-b]"},
			"pageHas": []string{"aside.side"}, "hash": "#/zones/node-b"},
		map[string]any{"name": "deep link: Back closes the panel", "route": "#/zones", "status": st, "click": []string{"tr[aria-label^=node-b]"}, "back": 1,
			"pageNone": []string{"aside.side"}, "hash": "#/zones"},
		// closing it adds an entry too: Back opens it again
		map[string]any{"name": "deep link: Back after a close opens it", "route": "#/zones/node-b", "status": st, "click": []string{"aside.side button.x"}, "back": 1,
			"pageHas": []string{"aside.side"}, "hash": "#/zones/node-b"},
		// a filter replaces the address (no entry), keeping the panel in it
		map[string]any{"name": "deep link: the server's Zones filter", "route": "#/zones?filter=down", "status": st,
			"expect": []string{"node-e"}, "absent": []string{"node-b"}, "notOnPage": []string{"There is no such screen"}},
		map[string]any{"name": "deep link: #/monitor/queue", "route": "#/monitor/queue", "status": st,
			"pageHas": []string{"button.rb[title=Monitor][aria-current=page]"}, "notOnPage": []string{"There is no such screen"}},
	}})
	// a reload: the address names a zone before the first status
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "deep link: the name resolves when the status comes", "route": "#/zones/node-c",
			"ws": []any{map[string]any{"type": "status", "data": st}}, "pageHas": []string{"aside.side", "tr.sel[aria-label^=node-c]"}},
	}})
	// the components page: a run's panel by address, Back and Forward
	uiExec(t, map[string]any{"shell": "next", "app": true, "modules": []string{"_fixture/components.js"}, "fetch": fixtureRuns(now), "cases": []any{
		map[string]any{"name": "deep link: Back closes a run's panel", "route": "#/components", "click": []string{"button[data-run=101]"}, "back": 1,
			"pageNone": []string{"aside.side"}, "hash": "#/components"},
	}})
}
