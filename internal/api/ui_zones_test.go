package api

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The Zones pages (Zones-Fleet, -Selected, -Group, -Detail, -Confirm) on the
// v43 data: zone_state and group on each zone, the one set of counts of GET
// /api/nodes, GET /api/groups, GET /api/nodes/uptime and the availability
// strips. The clock is fixed at 16:14 UTC on Tue 24 Nov 2026.
var znNow = time.Date(2026, 11, 24, 16, 14, 0, 0, time.UTC)

// zn is one zone of the test fleet.
type zn struct {
	id, name, group, state string // state: the server's zone_state
	chip                   string
	holder                 string
	role                   string
	share                  float64
	usedGB, totalGB        float64
	temp                   float64 // 0: none reported
	paused, excluded       bool
	addr                   string
	uptime                 float64 // 0..1; -1 none
}

func znMac(i int) string { return fmt.Sprintf("02:00:00:00:01:%02x", i) }

// znFleet: rack-a (node-a host of alpha 60 %, node-b its helper, node-c
// loading beta, node-d free), home (node-e set aside, node-f lost, node-g
// paused), and node-h excluded in no group.
func znFleet() []zn {
	return []zn{
		{id: znMac(1), name: "node-a", group: "rack-a", state: "in_use", chip: "Oberon", holder: "alpha", role: "host", share: 0.6, usedGB: 10.7, totalGB: 13.5, temp: 61, addr: "192.0.2.11", uptime: 0.995},
		{id: znMac(2), name: "node-b", group: "rack-a", state: "in_use", chip: "Oberon", holder: "alpha", role: "helper", share: 0.4, usedGB: 10.4, totalGB: 13.5, temp: 63, addr: "192.0.2.12", uptime: 0.996},
		{id: znMac(3), name: "node-c", group: "rack-a", state: "loading", chip: "Oberon", holder: "beta", role: "host", share: 1, usedGB: 4.2, totalGB: 13.5, temp: 58, addr: "192.0.2.13", uptime: 0.988},
		{id: znMac(4), name: "node-d", group: "rack-a", state: "free", chip: "Oberon", usedGB: 0.3, totalGB: 13.5, temp: 55, addr: "192.0.2.14", uptime: 1},
		{id: znMac(5), name: "node-e", group: "home", state: "set_aside", chip: "RTX 3070 Ti", usedGB: 0.3, totalGB: 8, temp: 45, addr: "192.0.2.15", uptime: 0.994},
		{id: znMac(6), name: "node-f", group: "home", state: "lost", chip: "RTX 3070 Ti", totalGB: 8, addr: "192.0.2.16", uptime: 0.947},
		{id: znMac(7), name: "node-g", group: "home", state: "off", chip: "RTX 3070 Ti", usedGB: 1.2, totalGB: 8, temp: 44, paused: true, addr: "192.0.2.17", uptime: -1},
		{id: znMac(8), name: "node-h", state: "off", chip: "RTX 5060", usedGB: 0.5, totalGB: 8, temp: 40, excluded: true, addr: "192.0.2.18", uptime: -1},
	}
}

// znMore adds n free zones of rack-b (for paging).
func znMore(fleet []zn, n int) []zn {
	for i := 0; i < n; i++ {
		fleet = append(fleet, zn{id: znMac(100 + i), name: fmt.Sprintf("rb-%02d", i+1), group: "rack-b", state: "free", chip: "Oberon", usedGB: 0.2, totalGB: 13.5, temp: 50, addr: fmt.Sprintf("192.0.2.%d", 100+i), uptime: 0.999})
	}
	return fleet
}

func znIso(d time.Duration) string { return znNow.Add(d).UTC().Format(time.RFC3339Nano) }

// znStatus is the live status (/ws "status") of a fleet.
func znStatus(fleet []zn) map[string]any {
	nodes, agents, insts := []any{}, []any{}, map[string]map[string]any{}
	quar := []any{}
	for _, z := range fleet {
		n := map[string]any{"id": z.id, "hostname": z.name, "online": z.state != "lost", "total_mib": z.totalGB * 1024, "free_mib": (z.totalGB - z.usedGB) * 1024,
			"holder": z.holder, "claimed_by": "", "excluded": z.excluded, "excluded_by": "", "quarantined": nil, "missing_since": nil, "free_at": nil, "settling": "", "settle_until": nil}
		a := map[string]any{"id": z.id, "name": z.name, "addr": z.addr + ":50052", "state": "active", "paused": z.paused, "placeable": z.state == "free", "last_seen_ago_s": 1,
			"zone_state": z.state, "chip": z.chip, "release": "0.1.10", "platform": "linux-amd64-vulkan", "host_uptime_s": 12*86400 + 4*3600}
		n["priority"] = 1
		if z.group != "" {
			a["group"] = z.group
		}
		if z.temp > 0 {
			a["stats"] = map[string]any{"headline_temp_c": z.temp}
			if z.name == "node-e" {
				a["stats"] = map[string]any{"headline_temp_c": z.temp, "net": []any{map[string]any{"name": "eth0", "link_mbps": 1000}}}
			}
		}
		if z.excluded {
			n["excluded_by"] = z.id
			a["excluded"], a["excluded_by"], a["excluded_actor"], a["excluded_at"] = true, z.id, "anonymous", znIso(-time.Hour)
		}
		if z.paused {
			a["paused_actor"], a["paused_at"] = "anonymous", znIso(-2*time.Hour)
		}
		if z.name == "node-a" { // its last cold load (last_load_link); the others have none yet
			a["last_load_link"] = map[string]any{"mb_s": 104.4, "bytes": 2234000000, "load_s": 21.4, "at": znIso(-14 * time.Hour), "model": "alpha", "kind": "lower_bound"}
		}
		switch z.state {
		case "lost":
			n["missing_since"] = znIso(-54 * time.Minute)
			a["state"] = "lost"
		case "set_aside":
			n["quarantined"] = znIso(-time.Minute)
			quar = append(quar, map[string]any{"node": z.id, "until": znIso(3 * time.Minute), "reason": "preflight_failed", "message": "the pre-load check failed"})
		}
		if z.holder != "" {
			in := insts[z.holder]
			if in == nil {
				st := "ready"
				if z.state == "loading" {
					st = "starting"
				}
				in = map[string]any{"profile": z.holder, "state": st, "in_flight": 2, "parallel": 4, "nodes": []any{}, "ready_since": znIso(-38 * time.Minute)}
				if st == "starting" {
					in["load_progress"] = 0.49
				}
				insts[z.holder] = in
			}
			in["nodes"] = append(in["nodes"].([]any), map[string]any{"id": z.id, "role": z.role, "share": z.share})
		}
		nodes, agents = append(nodes, n), append(agents, a)
	}
	is := []any{}
	for _, p := range []string{"alpha", "beta"} {
		if insts[p] != nil {
			is = append(is, insts[p])
		}
	}
	return map[string]any{"time": znIso(0), "membership": "agents", "instances": is, "queue": []any{}, "quarantines": quar,
		"view": map[string]any{"age": 0, "connected": true, "nodes": nodes}, "agents": agents}
}

// znCounts is the one set of counts of a fleet (GET /api/nodes counts).
func znCounts(fleet []zn) map[string]any {
	c := map[string]int{}
	for _, z := range fleet {
		c[z.state]++
	}
	total := len(fleet)
	down := c["lost"] + c["set_aside"]
	return map[string]any{"total": total, "reachable": total - down, "up": c["free"] + c["in_use"] + c["loading"], "down": down, "lost": c["lost"],
		"set_aside": c["set_aside"], "off": c["off"], "in_use": c["in_use"], "free": c["free"], "loading": c["loading"]}
}

func znSubset(fleet []zn, group string) []zn {
	out := []zn{}
	for _, z := range fleet {
		if z.group == group {
			out = append(out, z)
		}
	}
	return out
}

// znGroupDoc is one group of GET /api/groups (or its ungrouped part).
func znGroupDoc(name string, members []zn, uptime any) map[string]any {
	ids, names := []any{}, []any{}
	used, total, temps, hot, hotName, hotID := 0.0, 0.0, []float64{}, 0.0, "", ""
	attn, models, accepting := []any{}, map[string]int{}, 0
	chips, chipless := map[string]bool{}, false
	for _, z := range members {
		if z.chip != "" {
			chips[z.chip] = true
		} else {
			chipless = true
		}
		ids, names = append(ids, z.id), append(names, z.name)
		total += z.totalGB * 1024
		if z.state != "lost" {
			used += z.usedGB * 1024
		}
		if z.temp > 0 {
			temps = append(temps, z.temp)
			if z.temp > hot {
				hot, hotName, hotID = z.temp, z.name, z.id
			}
		}
		if z.state == "lost" || z.state == "set_aside" {
			attn = append(attn, map[string]any{"id": z.id, "name": z.name, "state": z.state})
		}
		if z.holder != "" {
			models[z.holder]++
		}
		if !z.paused {
			accepting++
		}
	}
	avg := 0.0
	for _, t := range temps {
		avg += t
	}
	if len(temps) > 0 {
		avg /= float64(len(temps))
	}
	ms := []any{}
	for _, p := range []string{"alpha", "beta"} {
		if models[p] > 0 {
			ms = append(ms, map[string]any{"profile": p, "zones": models[p]})
		}
	}
	acc := "all"
	if accepting == 0 {
		acc = "none"
	} else if accepting < len(members) {
		acc = "mixed"
	}
	g := map[string]any{"members": ids, "member_names": names, "count": len(members), "counts": znCounts(members),
		"memory": map[string]any{"used_mib": used, "total_mib": total}, "temp": map[string]any{"avg_c": avg, "hottest_c": hot, "hottest_id": hotID, "hottest_name": hotName},
		"accepting": acc, "needs_attention": attn, "models": ms, "uptime_7d": map[string]any{"uptime": uptime}}
	if name != "" {
		g["name"] = name
	}
	// the group's chip, as the server gives it: the one name when every
	// member reports it, "mixed" when two differ, none when a member reports none
	if len(chips) > 1 {
		g["chip"] = "mixed"
	} else if len(chips) == 1 && !chipless {
		for c := range chips {
			g["chip"] = c
		}
	}
	return g
}

func znGroups(fleet []zn, uptimes map[string]any) map[string]any {
	seen, gs := map[string]bool{}, []any{}
	for _, z := range fleet {
		if z.group != "" && !seen[z.group] {
			seen[z.group] = true
			gs = append(gs, znGroupDoc(z.group, znSubset(fleet, z.group), uptimes[z.group]))
		}
	}
	return map[string]any{"revision": 3, "groups": gs, "ungrouped": znGroupDoc("", znSubset(fleet, ""), uptimes[""]),
		"uptime_window": map[string]any{"days": 7, "complete": true}}
}

func znUptime(fleet []zn, complete bool) map[string]any {
	zones := []any{}
	for _, z := range fleet {
		var u any
		if z.uptime >= 0 {
			u = z.uptime
		}
		zones = append(zones, map[string]any{"id": z.id, "name": z.name, "group": z.group, "uptime": u})
	}
	w := map[string]any{"days": 7, "complete": complete, "recording": true, "from": znIso(-7 * 24 * time.Hour), "to": znIso(0)}
	if !complete {
		w["covered_from"] = "2026-11-23T16:36:00.000Z"
	}
	return map[string]any{"window": w, "zones": zones, "groups": []any{}, "fleet": map[string]any{"uptime": 0.99}}
}

func znAvail(fleet []zn) map[string]any {
	hour := znNow.Truncate(time.Hour)
	nodes := []any{}
	for _, z := range fleet {
		cells := []any{}
		for i := 23; i >= 0; i-- {
			st, lvl := "free", 0
			switch z.state {
			case "in_use":
				st, lvl = "in_use", 2+i%3
			case "lost":
				if i < 2 {
					st = "down"
				}
			case "off":
				st = "off"
			}
			cells = append(cells, map[string]any{"start": hour.Add(-time.Duration(i) * time.Hour).UTC().Format(time.RFC3339), "state": st, "busy_level": lvl})
		}
		nodes = append(nodes, map[string]any{"id": z.id, "name": z.name, "cells": cells})
	}
	return map[string]any{"step": "hour", "recording": true, "nodes": nodes}
}

// znFetch: the reads of the Zones page for a fleet.
func znFetch(fleet []zn, complete bool) map[string]any {
	up := map[string]any{"rack-a": 0.991, "home": 0.962, "rack-b": 0.999, "": nil}
	return map[string]any{
		"/api/nodes":              map[string]any{"nodes": []any{}, "counts": znCounts(fleet)},
		"/api/groups":             znGroups(fleet, up),
		"/api/nodes/uptime":       znUptime(fleet, complete),
		"/api/nodes/availability": znAvail(fleet),
		"/api/history":            map[string]any{"events": []any{}},
	}
}

func znSpec(fetch map[string]any, cases ...any) map[string]any {
	return map[string]any{"shell": "next", "app": true, "now": znNow.Format(time.RFC3339), "tz": "UTC", "fetch": fetch, "cases": cases}
}

// Zones-Fleet: the count pills from the one set of counts (the ALL pill's
// hover is the health popup's sentence), Create new zone (its dialog is
// in ui_zones_create_test.go), the toolbar
// (search, Chip, Model, Memory free, Grouped, Collapse all, Sort), the
// column heads, a group row per group with its totals and a "No group" row,
// the zone rows (status words, memory, temperature, 7-day uptime, the
// 24-hour strip) and the footer (colour key and pager).
func TestUINextZonesFleet(t *testing.T) {
	fleet := znFleet()
	st := znStatus(fleet)
	f := znFetch(fleet, true)
	uiExec(t, znSpec(f,
		map[string]any{"name": "fleet: pills and groups", "route": "#/zones", "status": st, "width": 1536,
			"expect": []string{"ALL 8 ZONES", "2 in use", "1 free", "1 loading", "2 down", "2 off", "Create new zone",
				"Chip: any", "Model: any", "Memory free: any", "Grouped", "Collapse all", "Sort: needs attention first",
				"Zone", "Model status", "Memory", "Temp", "Uptime", "24 h ago", "Availability, hour by hour", "now",
				"rack-a", "4 · Oberon", "home", "3 · RTX 3070 Ti", "No group",
				"node-a", "Host 60%", "alpha", "10.7 / 13.5 GB", "61 °C", "99.5%",
				"node-b", "Helper 40%", "node-c", "49% loaded", "beta", "node-d", "Free",
				"node-e", "Set aside", "load failed, back at 16:17", "node-f", "Offline", "gone for 54 min",
				"node-g", "Switched off", "node-h", "99.1%", "96.2%",
				"Free", "In use, darker is busier", "Loading", "Down", "Switched off"},
			"has": []string{"button.fc.on[data-show='']", "[data-group=rack-a] .gc", "[data-group=rack-a] div.avail[role=img]", "[data-zone='" + znMac(1) + "'] div.avail[role=img]",
				"input.cb[data-sel='group:rack-a']", "button.more[aria-expanded=true][data-fold=rack-a]"},
			"titles": []string{"6 of 8 reachable · 2 down", "Share of the last 7 days the zone was reachable", "Average across the group", "2 in use"},
			// the shell's fleet card says the same sentence from the same zone states
			"pageHas": []string{"div.fr.gh[data-group=home]"}, "page": []string{"6 of 8 reachable", "2 down · 2 off · Updated"},
			"ws":   []any{map[string]any{"type": "hello", "data": map[string]any{}}, map[string]any{"type": "status", "data": st}},
			"none": []string{"h1", "nav.pager"}},
		map[string]any{"name": "fleet: incomplete window", "route": "#/zones", "status": st, "fetch": map[string]any{"/api/nodes/uptime": znUptime(fleet, false)},
			"expect": []string{"since 23 Nov"}},
		map[string]any{"name": "fleet: a pill filters", "route": "#/zones?show=down", "status": st,
			"expect": []string{"node-e", "node-f"}, "absent": []string{"node-a", "node-d", "node-h"}, "has": []string{"button.fc.on[data-show=down]"}},
		map[string]any{"name": "fleet: search name, chip, model or address", "route": "#/zones", "status": st,
			"click":  []any{map[string]any{"type": "input[data-search=zones]", "value": "3070"}},
			"expect": []string{"node-e", "node-f", "node-g"}, "absent": []string{"node-a", "node-h"}},
		map[string]any{"name": "fleet: search a model", "route": "#/zones?q=beta", "status": st,
			"expect": []string{"node-c"}, "absent": []string{"node-a", "node-e"}},
		map[string]any{"name": "fleet: search an address", "route": "#/zones?q=192.0.2.16", "status": st,
			"expect": []string{"node-f"}, "absent": []string{"node-a", "node-e"}},
		map[string]any{"name": "fleet: the chip filter", "route": "#/zones", "status": st, "click": []string{"button[data-dd=zn-chip]", "button[data-dd-opt=Oberon]"},
			"expect": []string{"node-a", "node-d", "Chip: Oberon"}, "absent": []string{"node-e", "node-h"}, "hash": "#/zones?chip=Oberon"},
		map[string]any{"name": "fleet: the model filter", "route": "#/zones?model=alpha", "status": st,
			"expect": []string{"node-a", "node-b", "Model: alpha"}, "absent": []string{"node-c", "node-d"}},
		map[string]any{"name": "fleet: memory free", "route": "#/zones?mem=high", "status": st,
			"expect": []string{"node-d", "node-h"}, "absent": []string{"node-a", "node-b"}},
		map[string]any{"name": "fleet: flat", "route": "#/zones", "status": st, "click": []string{"button[data-act=grouped]"},
			"expect": []string{"node-a", "node-h"}, "absent": []string{"No group", "4 · Oberon"}, "hash": "#/zones?flat=1"},
		map[string]any{"name": "fleet: collapse a group", "route": "#/zones", "status": st, "click": []string{"button.more[data-fold=rack-a]"},
			"expect": []string{"rack-a", "node-e"}, "absent": []string{"node-a", "node-d"}, "has": []string{"button.more[aria-expanded=false][data-fold=rack-a]"}},
		map[string]any{"name": "fleet: collapse all", "route": "#/zones", "status": st, "click": []string{"button[data-act=collapse]"},
			"expect": []string{"rack-a", "home", "No group", "Expand all"}, "absent": []string{"node-a", "node-e", "node-h"}},
		map[string]any{"name": "fleet: a zone opens its panel", "route": "#/zones", "status": st, "width": 1536, "click": []string{"[data-zone='" + znMac(4) + "']"},
			"pageHas": []string{"aside.side"}, "hash": "#/zones/node-d"},
		map[string]any{"name": "fleet: create new zone opens its dialog", "route": "#/zones", "status": st, "click": []string{"button[data-act=create]"},
			"pageHas": []string{"div.dlg [data-create-zone]"}, "page": []string{"Create new zone", "Zone name"}, "notOnPage": []string{"not built yet"}},
		map[string]any{"name": "fleet: sort by name", "route": "#/zones?sort=name&flat=1", "status": st, "expect": []string{"Sort: name"}},
	))
}

// Paging: 16 zones a page, the page in the address, the group row again on
// the next page; no groups at all: a flat list, Grouped off with its reason.
func TestUINextZonesPagingAndNoGroups(t *testing.T) {
	big := znMore(znFleet(), 30)
	st := znStatus(big)
	f := znFetch(big, true)
	plain := znFleet()
	for i := range plain {
		plain[i].group = ""
	}
	uiExec(t, znSpec(f,
		map[string]any{"name": "paging: page 1", "route": "#/zones?flat=1&sort=name", "status": st,
			"expect": []string{"ALL 38 ZONES", "Page 1 of 3", "node-a", "rb-08"}, "absent": []string{"rb-09"}, "has": []string{"nav.pager button[data-page='2']"}},
		map[string]any{"name": "paging: the next page", "route": "#/zones?flat=1&sort=name", "status": st, "click": []string{"nav.pager button[data-page=next]"},
			"expect": []string{"Page 2 of 3", "rb-09"}, "absent": []string{"node-a"}, "hash": "#/zones?flat=1&sort=name&page=2"},
		map[string]any{"name": "paging: a group row again on its next page", "route": "#/zones?page=2&sort=name", "status": st,
			"expect": []string{"rack-b", "rb-"}},
		map[string]any{"name": "no groups: a flat list", "route": "#/zones", "status": znStatus(plain), "fetch": znFetch(plain, true),
			"expect": []string{"node-a", "node-h"}, "absent": []string{"No group", "Collapse all"}, "has": []string{"button[data-act=grouped][disabled]"},
			"titles": []string{"No groups yet"}},
	))
}

// Zones-Fleet-Selected: ticked zones turn the footer into the selection bar
// (New group, Move to group, Reboot, Stop accepting models, Force unload,
// Clear); a right click opens the menu for the selection (a right click on
// an unticked zone selects that one alone). The group writes go to the v43
// routes with the comment in the body and X-Comment; Reboot and Open SSH are
// drawn but disabled with a plain reason (no server support, and a PS5 is
// never rebooted from here).
func TestUINextZonesSelected(t *testing.T) {
	fleet := znFleet()
	st := znStatus(fleet)
	fetch := znFetch(fleet, true)
	tick := func(is ...int) []any {
		out := []any{}
		for _, i := range is {
			out = append(out, "input[data-sel='"+znMac(i)+"']")
		}
		return out
	}
	menuOn := func(i int, is ...int) []any {
		return append(tick(is...), map[string]any{"context": "div.fr[data-zone='" + znMac(i) + "']"})
	}
	then := func(a []any, more ...any) []any { return append(append([]any{}, a...), more...) }
	esc := func(s string) string { return strings.NewReplacer(":", "%3A").Replace(s) }
	ok := map[string]any{"ok": true}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range fetch {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	writes := with(map[string]any{
		"POST /api/groups/home/members":                 ok,
		"POST /api/groups":                              map[string]any{"name": "rack-z"},
		"DELETE /api/groups/rack-a/members":             ok,
		"DELETE /api/groups/home/members":               ok,
		"POST /api/nodes/" + znMac(1) + "/force-unload": ok,
		"POST /api/nodes/" + znMac(3) + "/force-unload": ok,
	})
	exists := with(map[string]any{"POST /api/groups": map[string]any{"$status": 409, "$body": map[string]any{"error": map[string]any{"message": "a group with that name exists"}}}})
	reboot := "Potluck cannot restart a machine yet"

	uiExec(t, znSpec(fetch,
		map[string]any{"name": "selected: the bar", "route": "#/zones", "status": st, "click": tick(1, 2),
			"expect": []string{"2 selected", "New group", "Move to group", "Reboot", "Stop accepting models", "Force unload", "Clear"},
			"absent": []string{"In use, darker is busier"},
			"has":    []string{"div.fr.chk[data-zone='" + znMac(1) + "']", "button[data-bulk=reboot][disabled]", "button.red[data-bulk=force]", "button[data-bulk=newgroup]", "button[data-bulk=move]"},
			"titles": []string{reboot, "a PS5 is never rebooted from here"}},
		map[string]any{"name": "selected: the right-click menu", "route": "#/zones", "status": st, "click": menuOn(1, 1, 2),
			"pageHas": []string{"div.cm[role=menu]", "button[data-cm=ssh][disabled]", "button[data-cm=reboot][disabled]", "button.red[data-cm=force]", "button[data-cm=move][aria-haspopup=true]"},
			"page":    []string{"2 zones selected", "Move to group", "New group from selection", "Remove from group", "Open SSH", "Reboot", "Stop accepting models", "Force unload"},
			"titles":  []string{"Not available yet: SSH from these pages is left out for now", reboot}},
		map[string]any{"name": "selected: a right click on an unticked zone selects it alone", "route": "#/zones", "status": st, "click": menuOn(4, 1),
			"page": []string{"1 zone selected"}, "expect": []string{"1 selected"}, "pageHas": []string{"div.fr.chk[data-zone='" + znMac(4) + "']"}, "pageNone": []string{"div.fr.chk[data-zone='" + znMac(1) + "']"}},
		map[string]any{"name": "selected: Escape closes the menu", "route": "#/zones", "status": st, "click": menuOn(1, 1), "keys": []string{"Escape"},
			"pageNone": []string{"div.cm"}, "expect": []string{"1 selected"}},
		map[string]any{"name": "selected: Move to group lists the other groups", "route": "#/zones", "status": st, "click": then(menuOn(1, 1, 2), "button[data-cm=move]"),
			"pageHas": []string{"div.cm.sub button[data-move=home]", "button[data-cm=move][aria-expanded=true]"}, "pageNone": []string{"button[data-move=rack-a]"},
			"page": []string{"New group…"}},
		map[string]any{"name": "selected: Clear", "route": "#/zones", "status": st, "click": then(tick(1, 2), "button[data-act=clear]"),
			"expect": []string{"In use, darker is busier"}, "absent": []string{"2 selected"}},
	))
	uiExec(t, znSpec(writes,
		map[string]any{"name": "selected: move two zones to home", "route": "#/zones", "status": st,
			"click": then(menuOn(1, 1, 2), "button[data-cm=move]", "button[data-move=home]",
				map[string]any{"type": "textarea[data-field=comment]", "value": "rack swap"}, "button[data-go]"),
			"fetchedQuery": []string{`^POST /api/groups/home/members \{"nodes":\["` + znMac(1) + `","` + znMac(2) + `"\],"comment":"rack swap"\}`}},
		map[string]any{"name": "selected: the move asks first", "route": "#/zones", "status": st,
			"click": then(menuOn(1, 1, 2), "button[data-cm=move]", "button[data-move=home]"),
			"page":  []string{"Move 2 zones to home?", "Comment (required)", "node-a", "node-b"}},
		map[string]any{"name": "selected: a new group from the bar", "route": "#/zones", "status": st,
			"click": then(tick(4, 8), "button[data-bulk=newgroup]", map[string]any{"type": "input[data-field=name]", "value": "rack-z"},
				map[string]any{"type": "textarea[data-field=comment]", "value": "spare zones"}, "button[data-go]"),
			"fetchedQuery": []string{`^POST /api/groups \{"name":"rack-z","members":\["` + znMac(4) + `","` + znMac(8) + `"\],"comment":"spare zones"\}`}},
		map[string]any{"name": "selected: a group name the server refuses", "route": "#/zones", "status": st, "fetch": exists,
			"click": then(menuOn(4, 4), "button[data-cm=newgroup]", map[string]any{"type": "input[data-field=name]", "value": "home"},
				map[string]any{"type": "textarea[data-field=comment]", "value": "x"}, "button[data-go]"),
			"page": []string{"a group with that name exists"}, "pageHas": []string{"div.dlg"}},
		map[string]any{"name": "selected: a name that is not a group name", "route": "#/zones", "status": st,
			"click": then(menuOn(4, 4), "button[data-cm=newgroup]", map[string]any{"type": "input[data-field=name]", "value": "my group"},
				map[string]any{"type": "textarea[data-field=comment]", "value": "x"}, "button[data-go]"),
			"page": []string{"1 to 64 letters, digits"}},
		map[string]any{"name": "selected: remove from group", "route": "#/zones", "status": st,
			"click":        then(menuOn(1, 1, 5), "button[data-cm=remove]", map[string]any{"type": "textarea[data-field=comment]", "value": "out"}, "button[data-go]"),
			"fetchedQuery": []string{`^DELETE /api/groups/rack-a/members \{"nodes":\["` + znMac(1) + `"\],"comment":"out"\}`, `^DELETE /api/groups/home/members \{"nodes":\["` + znMac(5) + `"\],"comment":"out"\}`}},
		map[string]any{"name": "selected: stop accepting models from the menu", "route": "#/zones", "status": st,
			"click": then(menuOn(1, 1, 2), "button[data-cm=pause]"), "page": []string{"Stop accepting models on 2 zones?", "Comment (required)"}},
		map[string]any{"name": "selected: force unload ends what they hold", "route": "#/zones", "status": st,
			"click": then(tick(1, 3, 4), "button[data-bulk=force]"),
			"page":  []string{"Force unload 2 zones?", "alpha", "beta", "Requests on them are cut off"}},
		map[string]any{"name": "selected: force unload sends one per zone", "route": "#/zones", "status": st,
			"click":        then(tick(1, 3, 4), "button[data-bulk=force]", map[string]any{"type": "textarea[data-field=comment]", "value": "stuck"}, "button[data-go]"),
			"fetchedQuery": []string{`^POST /api/nodes/` + esc(znMac(1)) + `/force-unload \{"comment":"stuck"\}`, `^POST /api/nodes/` + esc(znMac(3)) + `/force-unload`}},
	))
}

// Zones-Fleet-Group: a click on a group row opens its panel at
// #/zones/group/<name> (docked beside the list, which narrows to Zone, Model
// status and Memory, with Filters gathering the filters and the pills
// without loading and off): the group's name, "N zones · chip · N GB
// each", the Accepting models switch (on, off or mixed), Rename, Add zones,
// Reboot all (disabled, with the reason), the figures (memory, average and
// hottest temperature, in use, ping not measured, 7-day uptime), the zones
// that need attention (each opens its zone), the models here with Unload,
// and the link to its activity (not filterable by group yet). Each write
// asks for a comment and uses the v43 group routes or POST /api/unload.
func TestUINextZonesGroupPanel(t *testing.T) {
	fleet := znFleet()
	st := znStatus(fleet)
	fetch := znFetch(fleet, true)
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range fetch {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	ok := map[string]any{"ok": true}
	writes := with(map[string]any{
		"POST /api/groups/rack-a/pause":   ok,
		"POST /api/groups/home/unpause":   ok,
		"PATCH /api/groups/rack-a":        map[string]any{"name": "rack-z"},
		"POST /api/groups/rack-a/members": ok,
		"POST /api/unload":                map[string]any{"msg": "ok", "model": "alpha"},
	})
	typed := func(sel, v string) map[string]any { return map[string]any{"type": sel, "value": v} }
	wide := 1600

	uiExec(t, znSpec(fetch,
		map[string]any{"name": "group: a click on its row opens the panel", "route": "#/zones", "status": st, "width": wide,
			"click": []any{"div.fr.gh[data-group=rack-a]"}, "hash": "#/zones/group/rack-a",
			"pageHas": []string{"aside.side.docked", "div.fr.gh.sel[data-group=rack-a]"}},
		map[string]any{"name": "group: the panel", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"page": []string{"Selected group", "rack-a", "4 zones · Oberon · 13.5 GB each", "Accepting models", "Rename", "Add zones", "Reboot all",
				"Memory", "26", "of 54 GB", "Temperature, average", "59 °C", "Hottest", "63 °C", "node-b", "In use", "2", "of 4 zones",
				"Ping, average", "not measured", "Uptime, 7 days", "99.1%", "Needs attention", "Nothing needs a look",
				"Models here", "alpha", "2 zones", "beta", "1 zone", "Unload", "Activity for this group"},
			"pageHas": []string{"aside.side button.switch[role=switch][aria-checked=true]", "aside.side button[data-gp=reboot][disabled]", "aside.side button[data-unload=alpha]",
				"div.fr.v2n[data-zone='" + znMac(1) + "']", "div.mb[role=img]"},
			"pageNone": []string{"div.fr.v2[data-zone='" + znMac(1) + "']", "button[data-act=collapse]", "button[data-show=load]", "button[data-show=off]"},
			"expect":   []string{"Filters", "2 in use", "1 free", "2 down"},
			"titles":   []string{"Ping is not measured yet", "a PS5 is never rebooted from here", "the history filtered by this group"}},
		map[string]any{"name": "group: Filters shows the filters", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click": []any{"button[data-act=filters]"}, "expect": []string{"Chip: any", "Model: any", "Memory free: any", "Sort: needs attention first"}},
		map[string]any{"name": "group: a filter keeps the panel open", "route": "#/zones/group/rack-a?q=node-a", "status": st, "width": wide,
			"expect": []string{"node-a"}, "absent": []string{"node-d"}, "page": []string{"Selected group"}},
		map[string]any{"name": "group: needs attention and the mixed switch", "route": "#/zones/group/home", "status": st, "width": wide,
			"page":    []string{"Needs attention", "node-e", "Set aside", "node-f", "Offline", "Some zones"},
			"pageHas": []string{"aside.side button.switch[aria-checked=mixed]", "[data-attn='" + znMac(6) + "']"}},
		map[string]any{"name": "group: a zone that needs attention opens its panel", "route": "#/zones/group/home", "status": st, "width": wide,
			"click": []any{"[data-attn='" + znMac(6) + "']"}, "hash": "#/zones/node-f", "page": []string{"Selected zone"}},
		map[string]any{"name": "group: a group that is not there", "route": "#/zones/group/nope", "status": st, "width": wide,
			"page": []string{"No group named nope"}},
		map[string]any{"name": "group: close", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click": []any{"aside.side button.x"}, "hash": "#/zones", "pageNone": []string{"aside.side"}},
		map[string]any{"name": "group: the unload asks first", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click":   []any{"button[data-unload=alpha]"},
			"page":    []string{"Unload alpha?", "2 requests are in flight.", "node-a", "node-b", "Wait for the 2 requests, then unload", "Force unload now", "Both requests fail straight away.", "Comment (required)"},
			"pageHas": []string{"input[data-when=wait][checked]", "input[data-when=force]"}},
		map[string]any{"name": "group: its recent events, the history filtered by group", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"page": []string{"Recent events", "Activity for this group"}, "fetchedQuery": []string{`^GET /api/history\?.*group=rack-a`},
			"titles": []string{"the history filtered by this group"}},
	))
	uiExec(t, znSpec(writes,
		map[string]any{"name": "group: stop accepting models", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click":        []any{"aside.side button.switch", typed("textarea[data-field=comment]", "maintenance"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/groups/rack-a/pause \{"comment":"maintenance"\}`}},
		map[string]any{"name": "group: the switch asks first", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click": []any{"aside.side button.switch"}, "page": []string{"Stop accepting models on rack-a?", "4 zones", "What they hold now keeps serving"}},
		map[string]any{"name": "group: a mixed group accepts again", "route": "#/zones/group/home", "status": st, "width": wide,
			"click":        []any{"aside.side button.switch", typed("textarea[data-field=comment]", "back"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/groups/home/unpause \{"comment":"back"\}`}},
		map[string]any{"name": "group: rename", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click":        []any{"button[data-gp=rename]", typed("input[data-field=name]", "rack-z"), typed("textarea[data-field=comment]", "new name"), "button[data-go]"},
			"fetchedQuery": []string{`^PATCH /api/groups/rack-a \{"name":"rack-z","comment":"new name"\}`}, "hash": "#/zones/group/rack-z"},
		map[string]any{"name": "group: add zones", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click":        []any{"button[data-gp=add]", "input[data-add='" + znMac(8) + "']", typed("textarea[data-field=comment]", "more"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/groups/rack-a/members \{"nodes":\["` + znMac(8) + `"\],"comment":"more"\}`}},
		map[string]any{"name": "group: add zones lists the zones outside it", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click": []any{"button[data-gp=add]"}, "page": []string{"Add zones to rack-a", "node-e", "home", "node-h", "No group"},
			"pageNone": []string{"input[data-add='" + znMac(1) + "']"}},
		map[string]any{"name": "group: unload a model", "route": "#/zones/group/rack-a", "status": st, "width": wide,
			"click":        []any{"button[data-unload=alpha]", "input[data-when=force]", typed("textarea[data-field=comment]", "free it"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/unload \{"model":"alpha","force":true,"comment":"free it"\}`}},
	))
}

// Zones-Fleet-Detail: the zone panel (#/zones/<name>) as designed, as far as
// data exists: the name with its chip, the state line (state · group ·
// tier), the Accepting models switch, the SSH row drawn as not available
// yet (Copy, Open terminal disabled with the reason; the owner left SSH out
// for now) and Reboot (disabled: a console is never rebooted from here), the
// figures (memory with its bar, temperature, ping not measured, the
// machine's uptime, the link speed of the last cold load or "no cold load
// yet", 7-day uptime), what is loaded now with Force unload (the unload
// dialog), Load a model (a profile, then Load: the plan first), the 7-day
// grid by hour from /api/nodes/availability, and the link to its activity.
// Each write asks for a comment.
func TestUINextZonesDetail(t *testing.T) {
	fleet := znFleet()
	st := znStatus(fleet)
	fetch := znFetch(fleet, true)
	fetch["/api/profiles"] = map[string]any{"profiles": []any{map[string]any{"id": "alpha", "ctx": 32768}, map[string]any{"id": "gamma", "ctx": 8192}}}
	writes := map[string]any{}
	for k, v := range fetch {
		writes[k] = v
	}
	writes["POST /api/nodes/"+znMac(1)+"/pause"] = map[string]any{"ok": true}
	writes["POST /api/unload"] = map[string]any{"msg": "ok", "model": "alpha"}
	typed := func(sel, v string) map[string]any { return map[string]any{"type": sel, "value": v} }
	esc := func(s string) string { return strings.NewReplacer(":", "%3A").Replace(s) }
	wide := 1600
	// the agents report no host_uptime_s (an older agent; a console fed by the fleet console)
	noUp := znStatus(fleet)
	for _, a := range noUp["agents"].([]any) {
		delete(a.(map[string]any), "host_uptime_s")
	}

	uiExec(t, znSpec(fetch,
		map[string]any{"name": "detail: a zone in use", "route": "#/zones/node-a", "status": st, "width": wide,
			"page": []string{"Selected zone", "node-a", "| Oberon", "In use", "rack-a", "tier 1", "Accepting models",
				"SSH: not available yet", "Copy", "Open terminal", "Reboot",
				"Memory", "10.7", "of 13.5 GB", "Temperature", "61 °C", "Ping", "not measured", "Uptime", "12 d 4 h",
				"Link speed", "≥ 104 MB/s", "last cold load, 24 Nov 02:14", "Up, last 7 days", "99.5%",
				"Loaded now", "alpha", "Host 60%", "helper", "node-b", "2 requests", "loaded 38 min ago", "Force unload",
				"Load a model", "Choose a profile", "Last 7 days", "Idle", "Busy", "Down", "Activity for this zone"},
			"notOnPage": []string{"ssh 192.0.2.11", "2 runs"},
			"pageHas": []string{"aside.side button.switch[aria-checked=true]", "button[data-zp=terminal][disabled]", "button[data-zp=copy][disabled]", "button[data-zp=reboot][disabled]",
				"button[data-zp=load][disabled]", "select[data-zp=profile]", "button.red[data-zp=force]", "div.wg[role=img]", "div.cmd[data-zp=ssh]", "a[data-zp=activity]"},
			"titles": []string{"Ping is not measured yet", "a PS5 is never rebooted from here", "Not available yet: SSH from these pages is left out for now",
				"Choose a profile first", "At least this fast"},
			"fetchedQuery": []string{`^GET /api/nodes/availability\?.*node=` + esc(znMac(1)), `^GET /api/profiles`}},
		map[string]any{"name": "detail: no cold load yet", "route": "#/zones/node-e", "status": st, "width": wide,
			"page": []string{"Link speed", "no cold load yet", "Set aside", "home", "Nothing is loaded"}, "notOnPage": []string{"Mbit/s", "0 MB/s"},
			"titles": []string{"No load has sent this zone a model over the network yet"}},
		map[string]any{"name": "detail: a lost zone", "route": "#/zones/node-f", "status": st, "width": wide,
			"page": []string{"Offline", "gone for 54 min", "Nothing is loaded"}, "pageNone": []string{"b[data-zp=uptime]"}},
		// Uptime is the machine's own uptime (host_uptime_s + last_seen_ago_s):
		// drawn when the agent reports it, left out (not a dash) when not
		map[string]any{"name": "detail: the machine's uptime", "route": "#/zones/node-a", "status": st, "width": wide,
			"pageHas": []string{"b[data-zp=uptime]"}, "titles": []string{"How long the machine has been up since it last started"}},
		map[string]any{"name": "detail: no uptime reported: no tile", "route": "#/zones/node-a", "status": noUp, "width": wide,
			"page": []string{"Memory", "Temperature", "Ping", "Link speed"}, "pageNone": []string{"b[data-zp=uptime]"}, "notOnPage": []string{"Uptime"}},
		map[string]any{"name": "detail: a zone switched off", "route": "#/zones/node-g", "status": st, "width": wide,
			"page": []string{"Switched off"}, "pageHas": []string{"aside.side button.switch[aria-checked=false]"}},
		map[string]any{"name": "detail: an excluded zone", "route": "#/zones/node-h", "status": st, "width": wide,
			"pageHas": []string{"aside.side button.switch[disabled]"}, "titles": []string{"exclude_nodes"}},
		map[string]any{"name": "detail: the switch asks first", "route": "#/zones/node-a", "status": st, "width": wide,
			"click": []any{"aside.side button.switch"}, "page": []string{"Stop accepting models on node-a?", "Comment (required)"}},
		map[string]any{"name": "detail: force unload names the helper", "route": "#/zones/node-a", "status": st, "width": wide,
			"click": []any{"button[data-zp=force]"}, "page": []string{"Unload alpha from node-a?", "2 requests are in flight.", "node-a is the host, so the model also unloads from its helper node-b."}},
	))
	uiExec(t, znSpec(writes,
		map[string]any{"name": "detail: stop accepting models", "route": "#/zones/node-a", "status": st, "width": wide,
			"click":        []any{"aside.side button.switch", typed("textarea[data-field=comment]", "maintenance"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/nodes/` + esc(znMac(1)) + `/pause \{"comment":"maintenance"\}`}},
		map[string]any{"name": "detail: force unload", "route": "#/zones/node-a", "status": st, "width": wide,
			"click":        []any{"button[data-zp=force]", "input[data-when=force]", typed("textarea[data-field=comment]", "stuck"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/unload \{"model":"alpha","force":true,"comment":"stuck"\}`}, "notFetched": []string{`^POST /api/models/drain`}},
	))
}

// Zones-Confirm: the unload dialog. It says what the unload ends (the
// requests in flight, the helpers that go with a host) and offers "Wait for
// the requests, then unload" (chosen when requests are going: POST
// /api/models/drain/<model>; its sentence says new requests wait in the
// queue meanwhile and the next load may start on another free zone) or
// "Force unload now", and asks for a comment. After a drain the dialog shows
// the server's answer: its estimate ("About 40 s", or "no estimate yet") and
// its next_load sentence. With nothing running it offers "Unload now" and
// sends no force; while the model loads, only force (a drain is refused
// while loading). Force unload on a selection of one zone opens this dialog
// too. The Reboot dialog of the board is not built: Reboot stays disabled.
func TestUINextZonesConfirm(t *testing.T) {
	fleet := znFleet()
	st := znStatus(fleet)
	fetch := znFetch(fleet, true)
	writes := map[string]any{}
	for k, v := range fetch {
		writes[k] = v
	}
	writes["POST /api/unload"] = map[string]any{"msg": "ok", "model": "alpha"}
	writes["POST /api/models/drain/alpha"] = map[string]any{"msg": "draining", "model": "alpha", "eta_s": 40, "next_load": "The next load may start on another zone."}
	noEta := map[string]any{}
	for k, v := range writes {
		noEta[k] = v
	}
	noEta["POST /api/models/drain/alpha"] = map[string]any{"msg": "draining", "model": "alpha"}
	idle := znStatus(fleet)
	for _, in := range idle["instances"].([]any) {
		in.(map[string]any)["in_flight"] = 0
	}
	typed := func(sel, v string) map[string]any { return map[string]any{"type": sel, "value": v} }

	uiExec(t, znSpec(fetch,
		map[string]any{"name": "confirm: requests in flight", "route": "#/zones/node-a", "status": st, "width": 1600,
			"click": []any{"button[data-zp=force]"},
			"page": []string{"Unload alpha from node-a?", "2 requests are in flight.", "is the host, so the model also unloads from its helper",
				"Wait for the 2 requests, then unload", "New requests for this model wait in the queue meanwhile. The next load may start on another free zone.",
				"Force unload now", "Both requests fail straight away.", "Cancel", "Unload"},
			"notOnPage": []string{"Not built yet", "2 runs"},
			"pageHas":   []string{"div[role=radiogroup] input[data-when=wait][checked]", "input[data-when=force]", "div.box.warn", "button.dng[data-go]"},
			"pageNone":  []string{"input[data-when=wait][disabled]"}},
		map[string]any{"name": "confirm: a comment is required", "route": "#/zones/node-a", "status": st, "width": 1600,
			"click": []any{"button[data-zp=force]", "button[data-go]"}, "page": []string{"A comment is required."}},
		map[string]any{"name": "confirm: nothing running", "route": "#/zones/node-a", "status": idle, "width": 1600,
			"click": []any{"button[data-zp=force]"},
			"page":  []string{"No request is in flight.", "Unload now"}, "pageNone": []string{"input[data-when=wait]"}},
		map[string]any{"name": "confirm: one zone ticked opens the dialog", "route": "#/zones", "status": st,
			"click": []any{"input[data-sel='" + znMac(1) + "']", "button[data-bulk=force]"},
			"page":  []string{"Unload alpha from node-a?", "Force unload now"}},
		map[string]any{"name": "confirm: while it loads only force", "route": "#/zones/node-c", "status": st, "width": 1600,
			"click": []any{"button[data-zp=force]"},
			"page":  []string{"It is still loading: the load is cancelled.", "Force unload now"}, "pageHas": []string{"input[data-when=wait][disabled]", "input[data-when=force][checked]"}},
	))
	uiExec(t, znSpec(writes,
		map[string]any{"name": "confirm: wait for the requests, then unload", "route": "#/zones/node-a", "status": st, "width": 1600,
			"click":        []any{"button[data-zp=force]", typed("textarea[data-field=comment]", "free it up"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/models/drain/alpha$`}, "notFetched": []string{`^POST /api/unload`},
			"page":    []string{"alpha unloads when its requests end.", "About 40 s.", "New requests for it wait in the queue meanwhile.", "The next load may start on another zone."},
			"pageHas": []string{"[data-drained]", "button[data-close]"}},
		map[string]any{"name": "confirm: unload without force", "route": "#/zones/node-a", "status": idle, "width": 1600,
			"click":        []any{"button[data-zp=force]", typed("textarea[data-field=comment]", "done"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/unload \{"model":"alpha","force":false,"comment":"done"\}`}},
	))
	uiExec(t, znSpec(noEta,
		map[string]any{"name": "confirm: a drain with no estimate yet", "route": "#/zones/node-a", "status": st, "width": 1600,
			"click":        []any{"button[data-zp=force]", typed("textarea[data-field=comment]", "free it up"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/models/drain/alpha$`},
			"page":         []string{"No estimate yet", "The next load may start on another free zone."}, "notOnPage": []string{"About 40 s"}},
	))
}

// A draining instance (state "draining", drain, drain_eta_s, in_flight)
// and one loaded on chosen zones (pinned_nodes): the zone row says it
// unloads when its requests end; the zone panel gives the estimate (or "no
// estimate yet") and a Cancel unload that asks for a comment and sends
// POST /api/models/undrain/<model>; the group's Models here has Cancel
// unload instead of Unload; a pinned instance says "on chosen zones" with
// the zones on hover.
func TestUINextZonesDrainAndPinned(t *testing.T) {
	fleet := znFleet()
	draining := func(eta bool) map[string]any {
		st := znStatus(fleet)
		in := st["instances"].([]any)[0].(map[string]any) // alpha: node-a host, node-b helper
		in["state"], in["drain"] = "draining", true
		if eta {
			in["drain_eta_s"] = 40
		}
		return st
	}
	pinned := znStatus(fleet)
	pinned["instances"].([]any)[0].(map[string]any)["pinned_nodes"] = []any{"node-a", "node-b"}
	fetch := znFetch(fleet, true)
	fetch["POST /api/models/undrain/alpha"] = map[string]any{"msg": "ok", "model": "alpha"}
	typed := func(sel, v string) map[string]any { return map[string]any{"type": sel, "value": v} }
	uiExec(t, znSpec(fetch,
		map[string]any{"name": "drain: the row", "route": "#/zones", "status": draining(true), "width": 1536,
			"expect": []string{"node-a", "unloads when its requests end"}},
		map[string]any{"name": "drain: the zone panel with an estimate", "route": "#/zones/node-a", "status": draining(true), "width": 1600,
			"page":    []string{"Unloads when its requests end, about 40 s left. New requests wait in the queue.", "Cancel unload", "2 requests"},
			"pageHas": []string{"button[data-zp=undrain]", "button[data-zp=force]"}},
		map[string]any{"name": "drain: no estimate yet", "route": "#/zones/node-a", "status": draining(false), "width": 1600,
			"page": []string{"Unloads when its requests end, no estimate yet."}},
		map[string]any{"name": "drain: the group's models", "route": "#/zones/group/rack-a", "status": draining(true), "width": 1600,
			"page": []string{"unloads when its requests end, about 40 s left"}, "pageHas": []string{"button[data-undrain=alpha]", "button[data-unload=beta]"}, "pageNone": []string{"button[data-unload=alpha]"}},
		map[string]any{"name": "drain: cancel it", "route": "#/zones/node-a", "status": draining(true), "width": 1600,
			"click": []any{"button[data-zp=undrain]", typed("textarea[data-field=comment]", "still needed"), "button[data-go]"},
			"fetchedQuery": []string{`^POST /api/models/undrain/alpha$`}},
		map[string]any{"name": "drain: cancel asks first", "route": "#/zones/node-a", "status": draining(true), "width": 1600,
			"click": []any{"button[data-zp=undrain]"}, "page": []string{"Cancel the unload of alpha?", "It stops waiting to unload and takes requests again", "Comment (required)"}},
		map[string]any{"name": "drain: a dialog on a draining model offers force only", "route": "#/zones/node-a", "status": draining(true), "width": 1600,
			"click": []any{"button[data-zp=force]"}, "page": []string{"It is already waiting for its requests."},
			"pageHas": []string{"input[data-when=wait][disabled]", "input[data-when=force][checked]"}},
		map[string]any{"name": "pinned: on chosen zones", "route": "#/zones/node-a", "status": pinned, "width": 1600,
			"page": []string{"on chosen zones"}, "titles": []string{"Loaded on chosen zones: node-a, node-b"}, "pageHas": []string{"[data-pinned]"}},
		map[string]any{"name": "pinned: an ordinary instance says nothing of it", "route": "#/zones/node-c", "status": pinned, "width": 1600,
			"pageNone": []string{"[data-pinned]", "[data-draining]"}},
	))
}

// Load a model on chosen zones (Zones-Fleet-Detail's "Load a model"): the
// zone panel offers the profiles (GET /api/profiles; generated ones in their
// own group); Load opens the dialog with the zone picker (this zone ticked;
// a zone that takes a model only when named carries its "Named only"
// badge). The plan comes first (POST /api/plan {"model","nodes"}): its
// verdict and its memory line; then Load sends POST /api/load with the same
// body and the comment. A rejecting plan shows the server's sentence and
// sends no load; a refused load (409 chosen zone unusable, 409 already
// loaded or queued, 400 unknown zone or named twice) shows the server's
// sentence in the dialog.
func TestUINextZonesLoad(t *testing.T) {
	fleet := znFleet()
	st := znStatus(fleet)
	for _, n := range st["view"].(map[string]any)["nodes"].([]any) {
		if n.(map[string]any)["hostname"] == "node-e" {
			n.(map[string]any)["auto"] = false // no_auto_nodes: named only
		}
	}
	base := znFetch(fleet, true)
	base["/api/profiles"] = map[string]any{"profiles": []any{
		map[string]any{"id": "alpha", "model": "alpha-27b", "ctx": 32768},
		map[string]any{"id": "gamma", "model": "gamma-4b", "ctx": 8192},
		map[string]any{"id": "bench-gamma-2048", "model": "gamma-4b", "ctx": 2048, "generated": true},
	}}
	with := func(extra map[string]any) map[string]any {
		f := map[string]any{}
		for k, v := range base {
			f[k] = v
		}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	start := map[string]any{"model": "gamma", "action": "start", "placement": []any{map[string]any{"id": znMac(4), "budget_mib": 13100}},
		"need_mib": 3900, "fit_need_mib": 4095, "tight_fit": true, "warning": "", "reason": ""}
	reject := map[string]any{"model": "gamma", "action": "reject", "placement": []any{}, "need_mib": 3900, "required_mib": 4300,
		"reason": "gamma cannot be loaded on the chosen zones: node-g is paused"}
	err409 := func(reason, msg string) map[string]any {
		return map[string]any{"$status": 409, "$body": map[string]any{"error": map[string]any{"code": 409, "message": msg, "reason": reason, "type": "invalid_request_error"}}}
	}
	typed := func(sel, v string) map[string]any { return map[string]any{"type": sel, "value": v} }
	pick := []any{typed("select[data-zp=profile]", "gamma"), "button[data-zp=load]"}
	then := func(more ...any) []any { return append(append([]any{}, pick...), more...) }
	wide := 1600

	uiExec(t, znSpec(with(map[string]any{"POST /api/plan": start, "POST /api/load": map[string]any{"msg": "loading", "model": "gamma", "nodes": []any{"node-d"}}}),
		map[string]any{"name": "load: the profiles", "route": "#/zones/node-d", "status": st, "width": wide,
			"pageHas": []string{"select[data-zp=profile] option[value=gamma]", "select[data-zp=profile] optgroup option[value=bench-gamma-2048]"}},
		map[string]any{"name": "load: the plan first", "route": "#/zones/node-d", "status": st, "width": wide, "click": then(),
			"page":         []string{"Load gamma on chosen zones", "Zones", "node-d", "Named only", "Starts now", "Tight fit", "Needs 4.0 GB", "usable on the chosen zones 12.8 GB", "Comment (required)"},
			"pageHas":      []string{"input[data-pick=node-d][checked]", "[data-pick-row=node-e] .b", "[data-verdict=start]", "[data-memory]"},
			"fetchedQuery": []string{`^POST /api/plan \{"model":"gamma","nodes":\["node-d"\]\}$`}, "notFetched": []string{`^POST /api/load`}},
		map[string]any{"name": "load: then the load, same body", "route": "#/zones/node-d", "status": st, "width": wide,
			"click":        then(typed("textarea[data-field=comment]", "try it here"), "button[data-go]"),
			"fetchedQuery": []string{`^POST /api/plan \{"model":"gamma","nodes":\["node-d"\]\}$`, `^POST /api/load \{"model":"gamma","nodes":\["node-d"\]\}$`}},
		map[string]any{"name": "load: a second zone asks again", "route": "#/zones/node-d", "status": st, "width": wide,
			"click":        then("input[data-pick=node-e]"),
			"fetchedQuery": []string{`^POST /api/plan \{"model":"gamma","nodes":\["node-d","node-e"\]\}$`}},
		map[string]any{"name": "load: a comment is required", "route": "#/zones/node-d", "status": st, "width": wide,
			"click": then("button[data-go]"), "page": []string{"A comment is required."}, "notFetched": []string{`^POST /api/load`}},
	))
	uiExec(t, znSpec(with(map[string]any{"POST /api/plan": reject}),
		map[string]any{"name": "load: the plan refuses", "route": "#/zones/node-d", "status": st, "width": wide,
			"click":   then(typed("textarea[data-field=comment]", "x"), "button[data-go]"),
			"page":    []string{"Refused", "gamma cannot be loaded on the chosen zones: node-g is paused", "it would need 4.2 GB with the margins"},
			"pageHas": []string{"[data-verdict=reject]", "button[data-go][disabled]"}, "notFetched": []string{`^POST /api/load`}},
	))
	uiExec(t, znSpec(with(map[string]any{"POST /api/plan": start, "POST /api/load": err409("chosen zone unusable", "gamma cannot be loaded on the chosen zones: node-d is paused")}),
		map[string]any{"name": "load: 409 chosen zone unusable", "route": "#/zones/node-d", "status": st, "width": wide,
			"click": then(typed("textarea[data-field=comment]", "x"), "button[data-go]"),
			"page":  []string{"gamma cannot be loaded on the chosen zones: node-d is paused"}, "pageHas": []string{"div.dlg"}},
	))
	uiExec(t, znSpec(with(map[string]any{"POST /api/plan": start, "POST /api/load": err409("already loaded or queued", "gamma is ready: unload it (or let the queued load start) before loading it on chosen nodes")}),
		map[string]any{"name": "load: 409 already loaded or queued", "route": "#/zones/node-d", "status": st, "width": wide,
			"click": then(typed("textarea[data-field=comment]", "x"), "button[data-go]"),
			"page":  []string{"gamma is ready: unload it (or let the queued load start) before loading it on chosen nodes"}},
	))
	bad := map[string]any{"$status": 400, "$body": map[string]any{"error": map[string]any{"code": 400, "message": `"node-d" is named twice`, "type": "invalid_request_error"}}}
	uiExec(t, znSpec(with(map[string]any{"POST /api/plan": bad}),
		map[string]any{"name": "load: 400 from the plan", "route": "#/zones/node-d", "status": st, "width": wide,
			"click": then(), "page": []string{"The plan was refused:", `"node-d" is named twice`}, "pageHas": []string{"button[data-go][disabled]"}},
	))
}

// The fleet as live has it: zones and no group. They are listed flat, with
// no group row (no lone "No group"); "Grouped" stays, disabled with the
// reason; New group is on the selection bar and in the right-click menu;
// with everything on one page there is no pager (no "Page 1 of 1") and the
// footer keeps the colour key. With one group of two, the group row comes
// first, then "No group" with the rest. A group whose zones' devices differ
// shows its count only, "mixed" and the names on hover; one device:
// "N · <chip>".
func TestUINextZonesUngroupedAndOneGroup(t *testing.T) {
	bare := znFleet()
	for i := range bare {
		bare[i].group = ""
	}
	one := znFleet()
	for i := range one {
		one[i].group = map[int]string{0: "pair", 1: "pair"}[i]
	}
	mixed := znFleet()
	mixed[7].group = "home" // node-h, RTX 5060, joins the RTX 3070 Ti laptops
	uiExec(t, znSpec(znFetch(bare, true),
		map[string]any{"name": "ungrouped: flat, no group row", "route": "#/zones", "status": znStatus(bare),
			"page":     []string{"node-a", "node-h", "Grouped", "In use, darker is busier"},
			"pageNone": []string{"div.fr.gh", "nav.pager"}, "pageHas": []string{"button[data-act=grouped][disabled]"},
			"titles": []string{"No groups yet: select zones and make one from the selection"}},
		map[string]any{"name": "ungrouped: new group from the selection bar", "route": "#/zones", "status": znStatus(bare),
			"click": []any{"input[data-sel='" + znMac(1) + "']", "input[data-sel='" + znMac(2) + "']"}, "pageHas": []string{"button[data-bulk=newgroup]"}},
		map[string]any{"name": "ungrouped: new group from the menu", "route": "#/zones", "status": znStatus(bare),
			"click": []any{map[string]any{"context": "div.fr[data-zone='" + znMac(1) + "']"}}, "pageHas": []string{"button[data-cm=newgroup]"}},
	))
	uiExec(t, znSpec(znFetch(one, true),
		map[string]any{"name": "one group: its row, then No group", "route": "#/zones", "status": znStatus(one),
			"page":    []string{"pair", "2 · Oberon", "No group", "6 zones"},
			"pageHas": []string{"div.fr.gh[data-group=pair]", "div.fr.gh[data-group='-']", "button[data-act=grouped][aria-pressed=true]"}, "pageNone": []string{"nav.pager"}},
	))
	uiExec(t, znSpec(znFetch(mixed, true),
		map[string]any{"name": "mixed chips: the count, mixed on hover", "route": "#/zones", "status": znStatus(mixed),
			"page": []string{"4 · Oberon"}, "absent": []string{"4 · RTX"},
			"titles": []string{"mixed: RTX 3070 Ti, RTX 5060"}},
	))
}
