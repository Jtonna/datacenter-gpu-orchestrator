package api

import (
	"fmt"
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
			"zone_state": z.state, "chip": z.chip, "release": "0.1.10", "platform": "linux-amd64-vulkan"}
		if z.group != "" {
			a["group"] = z.group
		}
		if z.temp > 0 {
			a["stats"] = map[string]any{"headline_temp_c": z.temp}
		}
		if z.excluded {
			n["excluded_by"] = z.id
			a["excluded"], a["excluded_by"], a["excluded_actor"], a["excluded_at"] = true, z.id, "anonymous", znIso(-time.Hour)
		}
		if z.paused {
			a["paused_actor"], a["paused_at"] = "anonymous", znIso(-2*time.Hour)
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
	for _, z := range members {
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
// hover is the health popup's sentence), Create new zone, the toolbar
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
				"Free", "In use, darker is busier", "Loading", "Down", "Switched off", "Page 1 of 1"},
			"has": []string{"button.fc.on[data-show='']", "[data-group=rack-a] .gc", "[data-group=rack-a] div.avail[role=img]", "[data-zone='" + znMac(1) + "'] div.avail[role=img]",
				"input.cb[data-sel='group:rack-a']", "button.more[aria-expanded=true][data-fold=rack-a]"},
			"titles": []string{"6 of 8 reachable · 2 down", "Share of the last 7 days the zone was reachable", "Average across the group", "2 in use"},
			"none":   []string{"h1"}},
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
		map[string]any{"name": "fleet: create new zone says what is missing", "route": "#/zones", "status": st, "click": []string{"button[data-act=create]"},
			"pageHas": []string{"div.dlg"}, "page": []string{"Create new zone", "not built yet"}},
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
