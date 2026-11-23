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

// The shared components of SH-6 (S16) on the fixture's components page: the
// numbered pager on its first, a middle and its last page, both of its
// counts ("Page 1 of 15", "1–12 of 49"), its page in the address; the
// anchored popover (opens beside its anchor, Escape closes it and gives the
// focus back); the 500 px dialog with its close button and the optional
// comment field that holds the main action until it is filled; the search
// field with its magnifier, small and large.
func TestUINextSharedComponents(t *testing.T) {
	now := time.Now()
	comp := map[string]any{"shell": "next", "app": true, "modules": []string{"_fixture/components.js"}, "fetch": fixtureRuns(now)}
	comp["cases"] = []any{
		map[string]any{"name": "pager: the first page", "route": "#/components?page=1",
			"pageHas":  []string{"nav.pager[aria-label=Zones] button.pg[aria-current=page][data-page=1]", "nav.pager[aria-label=Zones] button.pg[data-page=prev][disabled]", "nav.pager[aria-label=Zones] button.pg[data-page=3]", "nav.pager[aria-label=Zones] button.pg[data-page=15]", "nav.pager[aria-label=Zones] span.pager-gap"},
			"pageNone": []string{"nav.pager[aria-label=Zones] button.pg[data-page=4]", "nav.pager[aria-label=Zones] button.pg[data-page=next][disabled]"},
			"page": []string{"Page 1 of 15", "1–12 of 49"}},
		map[string]any{"name": "pager: the count of a range", "route": "#/components?page=3",
			"pageHas": []string{"nav.pager[aria-label=Runs] button.pg[aria-current=page][data-page=3]", "nav.pager[aria-label=Runs] button.pg[data-page=5]"},
			"pageNone": []string{"nav.pager[aria-label=Runs] span.pager-gap"}, "page": []string{"25–36 of 49"}},
		map[string]any{"name": "pager: a middle page", "route": "#/components?page=8",
			"pageHas":  []string{"nav.pager[aria-label=Zones] button.pg[aria-current=page][data-page=8]", "nav.pager[aria-label=Zones] button.pg[data-page=7]", "nav.pager[aria-label=Zones] button.pg[data-page=9]", "nav.pager[aria-label=Zones] button.pg[data-page=1]", "nav.pager[aria-label=Zones] button.pg[data-page=15]"},
			"pageNone": []string{"nav.pager[aria-label=Zones] button.pg[data-page=2]", "nav.pager[aria-label=Zones] button.pg[data-page=14]", "nav.pager[aria-label=Zones] button.pg[disabled]"},
			"page":     []string{"Page 8 of 15"}},
		map[string]any{"name": "pager: the last page", "route": "#/components?page=15",
			"pageHas":  []string{"nav.pager[aria-label=Zones] button.pg[aria-current=page][data-page=15]", "nav.pager[aria-label=Zones] button.pg[data-page=next][disabled]", "nav.pager[aria-label=Zones] button.pg[data-page=13]"},
			"pageNone": []string{"nav.pager[aria-label=Zones] button.pg[data-page=prev][disabled]"},
			"page":     []string{"Page 15 of 15"}},
		// the page is in the address: it replaces it (no history entry)
		map[string]any{"name": "pager: next writes the address", "route": "#/components", "click": []string{"nav.pager[aria-label=Zones] button.pg[data-page=next]"},
			"pageHas": []string{"nav.pager[aria-label=Zones] button.pg[aria-current=page][data-page=2]"}, "hash": "#/components?page=2"},
		map[string]any{"name": "popover: opens beside its anchor", "route": "#/components", "click": []string{"button[data-pop=open]"},
			"pageHas": []string{"div.pop[role=dialog]", "button[data-pop=open][aria-expanded=true]"}, "page": []string{"Anchored popover"}},
		map[string]any{"name": "popover: Escape closes it, the focus returns", "route": "#/components", "click": []string{"button[data-pop=open]"}, "keys": []any{"Escape"},
			"pageNone": []string{"div.pop"}, "pageHas": []string{"button[data-pop=open][aria-expanded=false]"}, "focused": "button[data-pop=open]"},
		map[string]any{"name": "dialog: 500 wide, its close button, the comment", "route": "#/components", "click": []string{"button[data-dlg=comment]"},
			"pageHas": []string{"div.backdrop div.dlg[role=dialog] div.dlg-head h2", "div.dlg div.dlg-head button.x[aria-label=Close]", "div.dlg textarea.dlg-comment", "div.dlg-foot button.dng[disabled]"},
			"page":    []string{"Comment (required)"}},
		map[string]any{"name": "dialog: the close button", "route": "#/components", "click": []string{"button[data-dlg=comment]", "div.dlg button.x"},
			"pageNone": []string{"div.dlg"}, "focused": "button[data-dlg=comment]"},
		map[string]any{"name": "dialog: without the comment", "route": "#/components", "click": []string{"button[data-dlg=plain]"},
			"pageHas": []string{"div.dlg div.dlg-foot button.dng"}, "pageNone": []string{"textarea.dlg-comment", "div.dlg-foot button[disabled]"}},
		map[string]any{"name": "search: the magnifier inside the box", "route": "#/components",
			"pageHas": []string{"label.sx .ic", "label.sx input[type=search]", "label.sx.lg input[type=search]"}},
	}
	uiExec(t, comp)
}
