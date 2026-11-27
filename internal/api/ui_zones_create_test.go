package api

import (
	"testing"
	"time"
)

// Zones-Create: the Create new zone dialog wired to the server's pending
// zones (POST /api/zones/pending, GET /api/zones/pending/{id}, DELETE).
// The form reads the setting default_model_source and shows it; the person
// may change it for this zone (only a change is sent). The answer shows the
// command the server wrote, with Copy, and the state read again from the
// server: waiting (with its expiry), joined, expired, cancelled, and "not
// found" after a restart. The server's 400 and 409 sentences are shown as
// they come; with no model source the 400 links to Settings, Agents. Install
// over SSH, Windows and USB are drawn, not available, with the reason.
func TestUINextZonesCreate(t *testing.T) {
	fleet := znFleet()
	st := znStatus(fleet)
	base := znFetch(fleet, true)
	const src = "share:192.0.2.10:/models"
	const cmd = "curl -sSL 'http://192.0.2.1:8080/install.sh?zone=pz-0000000000000001' | sudo sh"
	pz := func(state, text string) map[string]any {
		z := map[string]any{"id": "pz-0000000000000001", "name": "zone-9", "group": "rack-a", "model_source": src, "mode": "bare",
			"state": state, "state_text": text, "created_at": znIso(0), "expires_at": znIso(46 * time.Minute),
			"command": cmd, "script_path": "/install.sh?zone=pz-0000000000000001", "download_path": "/install.sh?zone=pz-0000000000000001&download=1",
			"download_name": "potluck-zone-zone-9.sh"}
		if state == "joined" {
			z["joined_node"], z["joined_at"] = znMac(9), "2026-05-04T10:20:00Z"
		}
		return z
	}
	with := func(def string, extra map[string]any) map[string]any {
		f := map[string]any{}
		for k, v := range base {
			f[k] = v
		}
		f["/api/config/settings"] = map[string]any{"settings": map[string]any{"default_model_source": def}}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	err := func(code int, msg string) map[string]any {
		return map[string]any{"$status": code, "$body": map[string]any{"error": map[string]any{"code": code, "message": msg, "type": "invalid_request_error"}}}
	}
	typed := func(sel, v string) map[string]any { return map[string]any{"type": sel, "value": v} }
	open := "button[data-act=create]"
	fill := []any{open, typed("input[data-cz=name]", "zone-9"), typed("select[data-cz=group]", "rack-a"), typed("textarea[data-cz=comment]", "a new box")}
	made := func(more ...any) []any { return append(append(append([]any{}, fill...), "button[data-go]"), more...) }
	waiting := pz("waiting", "Waiting for the machine to check in")
	answers := func(now map[string]any) map[string]any {
		return map[string]any{"POST /api/zones/pending": map[string]any{"$status": 201, "$body": waiting}, "GET /api/zones/pending/pz-0000000000000001": now}
	}
	const noSource = "no model source: set the default model source for new machines or give one for this zone"

	uiExec(t, znSpec(with(src, answers(waiting)),
		map[string]any{"name": "create: the form", "route": "#/zones", "status": st, "click": []any{open},
			"page": []string{"Create new zone", "Zone name", "Group", "No group", "Run a command", "Download a script", "Install over SSH",
				"Model source", "The default for new machines is " + src, "Change it here for this zone only",
				"The command works for", "At most an hour",
				"It refuses to run on the scheduler's own machine and on a machine whose system runs from memory (use the RAM-only starter there)", "restarts no machine", "Comment (required)", "Create zone"},
			"brandFree":    true,
			"pageHas":      []string{"input[data-cz=source]", "select[data-cz=group] option[value=rack-a]", "button[data-cz-tab=ssh][disabled]", "button[data-go][disabled]"},
			"fetchedQuery": []string{`^GET /api/config/settings`},
			"notFetched":   []string{`^POST /api/zones/pending`}},
		map[string]any{"name": "create: SSH says why it is not there", "route": "#/zones", "status": st, "click": []any{open},
			"titles": []string{"Potluck never connects out to a machine and holds no SSH credentials"}},
		map[string]any{"name": "create: the default is not sent back", "route": "#/zones", "status": st, "click": made(),
			"fetchedQuery": []string{`^POST /api/zones/pending \{"name":"zone-9","ttl_min":60,"comment":"a new box","group":"rack-a"\}$`},
			"noHeader":     []string{"X-Actor"}},
		map[string]any{"name": "create: a source changed for this zone is sent", "route": "#/zones", "status": st,
			"click":        append(append([]any{}, fill...), typed("input[data-cz=source]", "dir:/models"), typed("select[data-cz=ttl]", "15"), "button[data-go]"),
			"fetchedQuery": []string{`^POST /api/zones/pending \{"name":"zone-9","ttl_min":15,"comment":"a new box","group":"rack-a","model_source":"dir:/models"\}$`}},
		map[string]any{"name": "create: the command and the wait", "route": "#/zones", "status": st, "click": made(),
			"page": []string{"Run this on the machine", "Linux", cmd, "Copy", "Waiting for the machine to check in", "the command works until",
				"Model files from " + src, "Windows: not available yet, there is no Windows installer", "Cancel this zone", "Close"},
			"pageHas":      []string{"[data-cz=state][data-state=waiting]", "button.copy"},
			"notOnPage":    []string{"Create zone"},
			"fetchedQuery": []string{`^GET /api/zones/pending/pz-0000000000000001$`}},
		map[string]any{"name": "create: download a script", "route": "#/zones", "status": st, "click": made("button[data-cz-tab=dl]"),
			"page":    []string{"potluck-zone-zone-9.sh", "Download", "USB drive", "Not available yet: there is no USB drive image"},
			"pageHas": []string{"a[data-cz=download][href='/install.sh?zone=pz-0000000000000001&download=1']"}},
		map[string]any{"name": "create: cancel asks for a comment, then DELETE", "route": "#/zones", "status": st,
			"click":        made("button[data-cz=cancel]", typed("textarea[data-cz=cancel-comment]", "wrong box"), "button[data-cz=cancel-go]"),
			"fetchedQuery": []string{`^DELETE /api/zones/pending/pz-0000000000000001$`}, "noHeader": []string{"X-Actor"}},
	))
	uiExec(t, znSpec(with(src, answers(pz("joined", "Joined"))),
		map[string]any{"name": "create: joined", "route": "#/zones", "status": st, "click": made(),
			"page": []string{"Joined", "as zone-9 in rack-a"}, "pageHas": []string{"[data-cz=state][data-state=joined]"},
			"pageNone": []string{"button[data-cz=cancel]", "button.copy"}},
	))
	uiExec(t, znSpec(with(src, answers(pz("expired", "Expired: the machine did not check in in time"))),
		map[string]any{"name": "create: expired", "route": "#/zones", "status": st, "click": made(),
			"page":     []string{"Expired: the machine did not check in in time", "The command no longer works; create the zone again"},
			"pageNone": []string{"button[data-cz=cancel]", "button.copy"}},
	))
	uiExec(t, znSpec(with(src, answers(pz("cancelled", "Cancelled"))),
		map[string]any{"name": "create: cancelled elsewhere", "route": "#/zones", "status": st, "click": made(),
			"page": []string{"Cancelled", "The command no longer works"}, "pageHas": []string{"[data-cz=state][data-state=cancelled]"}},
	))
	uiExec(t, znSpec(with(src, answers(err(404, "no pending zone pz-0000000000000001"))),
		map[string]any{"name": "create: not found after a restart", "route": "#/zones", "status": st, "click": made(),
			"page":     []string{"This zone was not found: create it again.", "a restart of the scheduler forgets them"},
			"pageHas":  []string{"[data-cz=state][data-state=gone]"},
			"pageNone": []string{"button[data-cz=cancel]", "button.copy", "a[data-cz=download]"}},
	))
	// never WAITING unless the server says waiting before the expiry: a
	// waiting zone read after its expiry is drawn expired; no state, "unknown"
	late := pz("waiting", "Waiting for the machine to check in")
	late["expires_at"] = znIso(-time.Minute)
	uiExec(t, znSpec(with(src, answers(late)),
		map[string]any{"name": "create: waiting past its expiry", "route": "#/zones", "status": st, "click": made(),
			"page":     []string{"Expired: the machine did not check in in time", "The command no longer works"},
			"pageHas":  []string{"[data-cz=state][data-state=expired]"},
			"pageNone": []string{"[data-state=waiting]", "button[data-cz=cancel]", "button.copy"}},
	))
	noState := pz("", "")
	delete(noState, "state")
	delete(noState, "state_text")
	uiExec(t, znSpec(with(src, answers(noState)),
		map[string]any{"name": "create: no state is not waiting", "route": "#/zones", "status": st, "click": made(),
			"page":     []string{"State not known: the server did not say"},
			"pageHas":  []string{"[data-cz=state][data-state=unknown]"},
			"pageNone": []string{"[data-state=waiting]", "button.copy"}},
	))
	uiExec(t, znSpec(with("", map[string]any{"POST /api/zones/pending": err(400, noSource)}),
		map[string]any{"name": "create: no default model source", "route": "#/zones", "status": st, "click": []any{open},
			"page":    []string{"No default is set for new machines: give one for this zone, or set it under Settings, Agents."},
			"pageHas": []string{"a[data-cz=settings-link][href='#/settings/agents']"}},
		map[string]any{"name": "create: the server's 400 with the Settings link", "route": "#/zones", "status": st, "click": made(),
			"page":    []string{noSource, "Set the default model source"},
			"pageHas": []string{"[data-cz=error] a[href='#/settings/agents']", "input[data-cz=name]"}},
	))
	uiExec(t, znSpec(with(src, map[string]any{"POST /api/zones/pending": err(409, "the name zone-9 is already taken by the zone "+znMac(9)+" (active, lost or removed): choose another name")}),
		map[string]any{"name": "create: a taken name (409)", "route": "#/zones", "status": st, "click": made(),
			"page":     []string{"the name zone-9 is already taken by the zone " + znMac(9) + " (active, lost or removed): choose another name"},
			"pageHas":  []string{"[data-cz=error]", "input[data-cz=name]"},
			"pageNone": []string{"[data-cz=error] a"}},
	))
}
