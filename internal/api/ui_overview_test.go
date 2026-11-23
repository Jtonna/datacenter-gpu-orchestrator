package api

import (
	"strings"
	"testing"
	"time"
)

// The Overview (UI slice 1c), drawn on fixture answers: the request log,
// its summary, the availability series and a status of six machines.
// Generic names only; none of the design's sample strings is used.

const (
	ovA, ovB, ovC, ovD, ovE, ovF = "02:00:00:00:00:21", "02:00:00:00:00:22", "02:00:00:00:00:23", "02:00:00:00:00:24", "02:00:00:00:00:25", "02:00:00:00:00:26"
)

func ovStatus(now time.Time) map[string]any {
	iso := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }
	node := func(id, name string, extra map[string]any) map[string]any {
		n := map[string]any{"id": id, "hostname": name, "online": true, "total_mib": 8192, "free_mib": 2048, "holder": "", "claimed_by": "",
			"excluded": false, "excluded_by": "", "quarantined": nil, "missing_since": nil, "free_at": nil, "settling": "", "settle_until": nil}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	agent := func(id, name string, extra map[string]any) map[string]any {
		a := map[string]any{"id": id, "name": name, "state": "active", "paused": false, "placeable": true, "last_seen_ago_s": 1, "stats": map[string]any{"headline_temp_c": 58.0}}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	return map[string]any{
		"time": iso(0), "membership": "agents",
		"instances": []any{
			map[string]any{"profile": "alpha", "state": "ready", "in_flight": 1, "parallel": 2,
				"nodes": []any{map[string]any{"id": ovA, "role": "host", "share": 0.6}, map[string]any{"id": ovB, "role": "helper", "share": 0.4}}},
			map[string]any{"profile": "beta", "state": "starting", "load_progress": 0.3, "nodes": []any{map[string]any{"id": ovC, "role": "host", "share": 1.0}}},
		},
		"queue":       []any{map[string]any{"profile": "beta", "waiters": 1, "claim": []any{ovC}}},
		"quarantines": []any{map[string]any{"node": ovE, "until": iso(5 * time.Minute), "reason": "preflight_failed", "message": "the pre-load check failed"}},
		"view": map[string]any{"age": 0, "connected": true, "nodes": []any{
			node(ovA, "node-a", map[string]any{"holder": "alpha"}),
			node(ovB, "node-b", map[string]any{"holder": "alpha"}),
			node(ovC, "node-c", map[string]any{"holder": "beta"}),
			node(ovD, "node-d", map[string]any{"claimed_by": "gamma", "free_at": iso(12 * time.Second), "free_basis": "settle"}),
			node(ovE, "node-e", map[string]any{"quarantined": iso(-time.Minute)}),
			node(ovF, "node-f", map[string]any{"excluded": true, "excluded_by": ovF}),
		}},
		"agents": []any{
			agent(ovA, "node-a", nil), agent(ovB, "node-b", map[string]any{"chip": "Chip X"}), agent(ovC, "node-c", nil), agent(ovD, "node-d", nil),
			agent(ovE, "node-e", map[string]any{"stats": map[string]any{"headline_temp_c": 80.0}}),
			agent(ovF, "node-f", map[string]any{"excluded": true, "excluded_by": ovF, "excluded_actor": "anonymous", "excluded_at": iso(-time.Hour)}),
		},
	}
}

func ovRequests(now time.Time) []any {
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	served := []any{map[string]any{"id": ovA, "name": "node-a", "role": "host"}, map[string]any{"id": ovB, "name": "node-b", "role": "helper"}}
	row := func(id int, extra map[string]any) map[string]any {
		r := map[string]any{"id": id, "kind": "v1", "profile": "alpha", "nodes": []any{}, "tokens_in": nil, "tokens_out": nil, "prefill_tps": nil, "decode_tps": nil}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	return []any{
		row(16, map[string]any{"status": "queued", "profile": "beta", "queued_at": ms(-30 * time.Second), "live": true}),
		row(15, map[string]any{"status": "running", "queued_at": ms(-10 * time.Second), "started_at": ms(-9 * time.Second), "tokens_in": 2460, "prefill_tps": 412.0, "tokens_out": 318, "decode_tps": 14.1, "live": true, "nodes": served}),
		row(14, map[string]any{"status": "done", "queued_at": ms(-time.Minute), "started_at": ms(-time.Minute), "duration_ms": 42500, "tokens_in": 1840, "prefill_tps": 418.0, "tokens_out": 612, "decode_tps": 14.4, "nodes": served}),
		row(13, map[string]any{"status": "failed", "reason": "cut", "message": "the zone dropped the connection", "queued_at": ms(-2 * time.Minute), "started_at": ms(-2 * time.Minute), "duration_ms": 8200, "tokens_in": 1120, "tokens_out": 37, "nodes": served}),
		row(12, map[string]any{"status": "refused", "reason": "queue_timeout", "message": "it waited longer than the queue time limit", "queued_at": ms(-20 * time.Minute), "wait_ms": 600000}),
		row(11, map[string]any{"status": "cancelled", "reason": "client_left", "message": "cancelled by the app that sent it", "queued_at": ms(-3 * time.Minute), "started_at": ms(-3 * time.Minute), "duration_ms": 4000, "tokens_in": 640, "tokens_out": 21, "nodes": served}),
	}
}

func ovFetch(now time.Time, rows []any, on bool) map[string]any {
	hour := now.Truncate(time.Hour)
	cells := func(state string, busy int) []any {
		out := []any{}
		for i := 23; i >= 0; i-- {
			out = append(out, map[string]any{"start": hour.Add(-time.Duration(i) * time.Hour).UTC().Format(time.RFC3339), "state": state, "busy_level": busy})
		}
		return out
	}
	series := []any{}
	for _, n := range []struct{ id, name, state string }{{ovA, "node-a", "in_use"}, {ovB, "node-b", "in_use"}, {ovC, "node-c", "loading"}, {ovD, "node-d", "free"}, {ovE, "node-e", "down"}, {ovF, "node-f", "off"}} {
		series = append(series, map[string]any{"id": n.id, "name": n.name, "cells": cells(n.state, 3)})
	}
	begins := now.Add(-48 * time.Hour).UnixMilli()
	if !on {
		begins = 0
	}
	total := 0
	if on {
		total = len(rows)
	}
	return map[string]any{
		"/api/requests": map[string]any{"requests": rows, "on": on, "history_begins_at": begins},
		"/api/requests/summary": map[string]any{"totals": map[string]any{"requests": total, "tokens_in": 291300 * total / 6, "tokens_out": 84900 * total / 6},
			"series": []any{}, "live": map[string]any{"queued": 0, "running": 0}, "on": on, "history_begins_at": begins, "oldest_ts": begins},
		"/api/nodes/availability": map[string]any{"recording": on, "nodes": series},
		"/api/models":             map[string]any{"models": []any{}},
	}
}

// Every row state, the zones in the design's words, the strip, the pin on
// the first running request and its highlight, the pills and dropdowns.
func TestUINextOverview(t *testing.T) {
	now := time.Now()
	st := ovStatus(now)
	fetch := ovFetch(now, ovRequests(now), true)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "overview: every row state", "route": "#/overview", "status": st,
			"expect": []string{"Day", "Week", "Month", "All time", "· today", "Customize", "View 24-hour report",
				"Tokens", "tokens in", "tokens out", "per hour", "Requests", "Queued", "Running", "Done", "Failed", "Cancelled", "All 6",
				"Status and length", "Started at", "In tok/s", "Out tok/s", "Not started", "queued ", "42.5 s", "8.2 s", "4.0 s", "10 min", "2,460", "412", "14.1"},
			"absent": []string{"Runs"},
			"has":    []string{"tr.fail", "tr.canc", "[data-run-start]", "span[title^='Failed:']", "span[title^='Refused:']", "span[title^='Cancelled']", "button.zb.on[data-zones='15']"}},
		map[string]any{"name": "overview: zones in the design's words", "route": "#/overview", "status": st,
			"expect": []string{"ALL ZONES", "3 in use", "1 free", "1 down", "1 off", "Sort: needs attention first", "Chip: any", "Memory: any", "Temperature: any",
				"Status", "Zone", "Doing", "Memory", "Temp", "24 h ago", "Availability, hour by hour", "now",
				"In use", "Host 60%", "Helper 40%", "Loading", "30% loaded", "Free in 1", "Next: ", "gamma", "Set aside", "Load failed, back at",
				"Off", "Switched off by you", " | Chip X", "6.0 / 8.0 GB", "58 °C", "Colour key", "Showing 6 of 6"},
			// the pinned running request outlines node-a and node-b as one box; the rest are dimmed
			"has": []string{".zsc.dim", "div.zr.hi.g-top[data-zone='" + ovA + "']", "div.zr.hi.g-bot[data-zone='" + ovB + "']", ".avail i.use3", ".avail i.load", ".avail i.down", ".avail i.off"}},
		// clicking the pinned toggle drops the pin; another one pins its own zones
		map[string]any{"name": "overview: unpin", "route": "#/overview", "status": st, "click": []string{"button[data-zones='15']"},
			"none": []string{".zr.hi", ".zsc.dim", "button.zb.on"}},
		map[string]any{"name": "overview: a queued request shows the zone it waits for", "route": "#/overview", "status": st, "click": []string{"button[data-zones='16']"},
			"has": []string{"div.zr.hi.g-solo[data-zone='" + ovC + "']", "button.zb.on[data-zones='16']"}, "none": []string{"div.zr.hi[data-zone='" + ovA + "']"}},
		map[string]any{"name": "overview: a refused request has no zone", "route": "#/overview", "status": st, "click": []string{"button[data-zones='12']"},
			"none": []string{".zr.hi", ".zsc.dim"}},
		// the pills and dropdowns filter and sort
		map[string]any{"name": "overview: the down pill", "route": "#/overview?show=down", "status": st, "expect": []string{"Showing 1 of 1", "node-e"}, "absent": []string{"node-a"}},
		map[string]any{"name": "overview: the off pill", "route": "#/overview?show=off", "status": st, "expect": []string{"Switched off by you"}, "absent": []string{"node-e"}},
		map[string]any{"name": "overview: chip filter", "route": "#/overview?chip=Chip+X", "status": st, "expect": []string{"Chip: Chip X", "node-b", "Showing 1 of 1"}, "absent": []string{"node-c"}},
		map[string]any{"name": "overview: hot zones", "route": "#/overview?temp=hot", "status": st, "expect": []string{"Temperature: over 75 °C", "node-e"}, "absent": []string{"node-a"}},
		map[string]any{"name": "overview: sort by name", "route": "#/overview?sort=name", "status": st, "expect": []string{"Sort: name"}},
		map[string]any{"name": "overview: a pick in the sort menu", "route": "#/overview", "status": st, "click": []string{"button[data-dd='ov-sort']", "button[data-dd-opt='temp']"},
			"expect": []string{"Sort: hottest first"}, "none": []string{".dd-menu"}},
		// periods
		map[string]any{"name": "overview: a past day", "route": "#/overview?date=2026-01-05", "status": st,
			"expect": []string{"Mon 5 Jan 2026", "00:00", "24:00"}, "absent": []string{"· today", "24 h ago"}},
		map[string]any{"name": "overview: a week", "route": "#/overview?period=week", "status": st, "expect": []string{"· this week", "per day", "Mon", "Sun"}},
		map[string]any{"name": "overview: a month", "route": "#/overview?period=month", "status": st, "expect": []string{"· this month", "per day"}},
		map[string]any{"name": "overview: all time", "route": "#/overview?period=all", "status": st, "expect": []string{"per day"}, "none": []string{"div.stepper"}},
		map[string]any{"name": "overview: history begins", "route": "#/overview?date=2020-01-06", "status": st, "expect": []string{"History begins at"}},
		// the requests list and the report it links to
		map[string]any{"name": "requests list", "route": "#/requests", "status": st, "expect": []string{"Requests", "Refused", "Sent by", "node-a, node-b", "Older", "Newer"}},
		map[string]any{"name": "24-hour report", "route": "#/report", "status": st, "expect": []string{"24-hour report", "Failed and refused requests", "Zones, hour by hour", "Print"}},
	}})

	// no request in the period, and the request log off
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": ovFetch(now, []any{}, true), "cases": []any{
		map[string]any{"name": "overview: no requests", "route": "#/overview", "status": st, "expect": []string{"No requests on this day", "No requests in this period.", "All 0"}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": ovFetch(now, ovRequests(now)[:2], false), "cases": []any{
		map[string]any{"name": "overview: the request log is off", "route": "#/overview", "status": st,
			"expect": []string{"The request log is off on this scheduler", "The request log is off.", "Not started"}},
	}})
	// nothing answers yet: the panels say they are reading
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "overview: no answers", "route": "#/overview", "expect": []string{"The totals could not be read", "The requests could not be read"}},
	}})
}

// The home page is the Overview: an empty hash opens it, the mark links to
// it, Monitor's first page is it, and it opens with the section menu closed.
func TestUINextOverviewIsHome(t *testing.T) {
	app := uiText(t, uiNextDir+"app.js")
	if !strings.Contains(app, "'#/overview'") {
		t.Fatalf("next/app.js does not open #/overview on an empty hash")
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": map[string]any{}, "cases": []any{
		map[string]any{"name": "home", "route": "#/overview",
			"links": []map[string]string{{"text": "Overview", "path": "/ui/"}, {"text": "Models", "path": "/ui/"}, {"text": "Queue", "path": "/ui/"}, {"text": "Activity", "path": "/ui/"}}},
	}})
}

// The request feed takes the live channel's "requests" topic when the
// server offers it: the snapshot replaces the poll's rows, queued / started
// / ended frames and ticks change them, and without the topic in hello the
// frames are ignored and the poll stays.
func TestUINextOverviewRequestsTopic(t *testing.T) {
	now := time.Now()
	st := ovStatus(now)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	pushed := func(id int, status string, extra map[string]any) map[string]any {
		r := map[string]any{"id": id, "status": status, "profile": "pushed-model", "queued_at": ms(-5 * time.Second), "nodes": []any{},
			"tokens_in": nil, "tokens_out": nil, "prefill_tps": nil, "decode_tps": nil}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	snapshot := map[string]any{"type": "requests", "data": map[string]any{"kind": "snapshot", "on": true, "next": 0, "history_begins_at": nil,
		"requests": []any{pushed(40, "queued", map[string]any{"live": true})}}}
	polled := ovFetch(now, ovRequests(now)[2:3], true) // one done row, profile alpha

	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": polled, "cases": []any{
		map[string]any{"name": "topic: not offered, the poll stays", "route": "#/overview", "status": st,
			"ws":     []any{map[string]any{"type": "hello", "data": map[string]any{"topics": []any{"status", "event"}}}, snapshot},
			"expect": []string{"42.5 s", "All 1"}, "absent": []string{"pushed-model"}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": polled, "cases": []any{
		map[string]any{"name": "topic: the snapshot replaces the poll", "route": "#/overview", "status": st,
			"ws":     []any{map[string]any{"type": "hello", "data": map[string]any{"topics": []any{"status", "requests"}}}, snapshot},
			"expect": []string{"pushed-model", "Not started"}, "absent": []string{"42.5 s"}},
		map[string]any{"name": "topic: started, then a tick", "route": "#/overview", "status": st,
			"ws": []any{
				map[string]any{"type": "requests", "data": map[string]any{"kind": "started", "request": pushed(40, "running", map[string]any{"live": true, "started_at": ms(-4 * time.Second)})}},
				map[string]any{"type": "requests", "data": map[string]any{"kind": "tick", "requests": []any{pushed(40, "running", map[string]any{"live": true, "started_at": ms(-4 * time.Second), "tokens_out": 777, "decode_tps": 12.5})}}},
				map[string]any{"type": "requests", "data": map[string]any{"kind": "queued", "request": pushed(41, "queued", map[string]any{"live": true})}},
			},
			"expect": []string{"777", "12.5", "Not started"}, "has": []string{"[data-run-start]", "button[data-zones='41']"}},
		map[string]any{"name": "topic: ended replaces the live row", "route": "#/overview", "status": st,
			"ws": []any{map[string]any{"type": "requests", "data": map[string]any{"kind": "ended", "request": pushed(40, "failed", map[string]any{
				"reason": "cut", "message": "pushed failure", "started_at": ms(-4 * time.Second), "ended_at": ms(-time.Second), "duration_ms": 3000, "tokens_out": 800})}}},
			"expect": []string{"3.0 s", "800"}, "has": []string{"tr.fail", "span[title^='Failed:']"}, "none": []string{"[data-run-start]"}},
	}})

	// a filtered page is read once and the topic is laid over it: under
	// ?status=running a row that ends leaves, one that starts arrives
	running := ovRequests(now)[1] // id 15, running
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": ovFetch(now, []any{running}, true), "cases": []any{
		map[string]any{"name": "topic: a filtered page follows the frames", "route": "#/requests?status=running", "status": st,
			"ws": []any{
				map[string]any{"type": "hello", "data": map[string]any{"topics": []any{"status", "requests"}}},
				map[string]any{"type": "requests", "data": map[string]any{"kind": "snapshot", "on": true, "requests": []any{running}}},
				map[string]any{"type": "requests", "data": map[string]any{"kind": "ended", "request": pushed(15, "done", map[string]any{"profile": "alpha", "started_at": ms(-9 * time.Second), "ended_at": ms(-time.Second), "duration_ms": 8000})}},
				map[string]any{"type": "requests", "data": map[string]any{"kind": "started", "request": pushed(42, "running", map[string]any{"live": true, "started_at": ms(-time.Second)})}},
			},
			"expect": []string{"pushed-model"}, "none": []string{"tr[data-req='15']"}, "has": []string{"tr[data-req='42']"}},
	}})
}
