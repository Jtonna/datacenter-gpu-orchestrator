package api

import (
	"testing"
	"time"
)

// ovWith adds view nodes and agents to the Overview's fixture status.
func ovWith(now time.Time, nodes, agents []any) map[string]any {
	st := ovStatus(now)
	v := st["view"].(map[string]any)
	v["nodes"] = append(v["nodes"].([]any), nodes...)
	st["agents"] = append(st["agents"].([]any), agents...)
	return st
}

// (f) A machine an operator removed from the pool stays in the answers (by
// its MAC, with no stats) but is not a zone: the table, the pills' counts
// and "Showing N of M" leave it out.
func TestUINextOverviewLeavesRemovedOut(t *testing.T) {
	now := time.Now()
	const gone = "02:00:00:00:00:27"
	st := ovWith(now,
		[]any{map[string]any{"id": gone, "hostname": "", "online": false, "total_mib": 0, "free_mib": 0, "excluded": false, "state": "removed"}},
		[]any{map[string]any{"id": gone, "name": "", "state": "removed", "paused": false, "placeable": false}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": ovFetch(now, ovRequests(now), true), "cases": []any{
		map[string]any{"name": "removed: not drawn", "route": "#/overview", "status": st,
			"expect": []string{"Showing 6 of 6", "3 in use", "1 free", "1 down", "1 off"},
			"absent": []string{gone, "Removed", "Showing 7"}, "none": []string{"div.zr[data-zone='" + gone + "']"}},
		map[string]any{"name": "removed: not under the down pill", "route": "#/overview?show=down", "status": st,
			"expect": []string{"Showing 1 of 1", "node-e"}, "absent": []string{gone}},
	}})
}

// (b) The Temp column draws stats.headline_temp_c in whole degrees, as the
// design does ("52 °C"); a machine without it shows a dash.
func TestUINextOverviewTemperature(t *testing.T) {
	now := time.Now()
	const warm, bare = "02:00:00:00:00:28", "02:00:00:00:00:29"
	node := func(id, name string) map[string]any {
		return map[string]any{"id": id, "hostname": name, "online": true, "total_mib": 8192, "free_mib": 4096, "holder": "", "excluded": false}
	}
	st := ovWith(now, []any{node(warm, "node-g"), node(bare, "node-h")}, []any{
		map[string]any{"id": warm, "name": "node-g", "state": "active", "placeable": true, "stats": map[string]any{"headline_temp_c": 51.6}},
		map[string]any{"id": bare, "name": "node-h", "state": "active", "placeable": true},
	})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": ovFetch(now, ovRequests(now), true), "cases": []any{
		map[string]any{"name": "temp: whole degrees", "route": "#/overview?sort=name", "status": st,
			"expect": []string{"52 °C", "58 °C", "Showing 6 of 8 · scroll for more"}, "absent": []string{"51.6", "51,6"}},
	}})
}

// (c) The availability strip: an hour with any use is never Free. The
// server colours an hour in_use only from a quarter of its known time in
// use; an hour with less use comes as free with seconds.in_use > 0, and the
// page draws it in use, shaded by its busy level (busy_level, from busy:
// under 25 % level 1, under 50 % level 2, under 75 % level 3, else 4).
func TestUINextOverviewStripAnyUse(t *testing.T) {
	now := time.Now()
	fetch := ovFetch(now, ovRequests(now), true)
	hour := now.Truncate(time.Hour)
	cell := func(i int, state string, inUse float64, busy any, level int) map[string]any {
		return map[string]any{"start": hour.Add(-time.Duration(i) * time.Hour).UTC().Format(time.RFC3339), "state": state, "span_s": 3600,
			"seconds": map[string]any{"free": 3600 - inUse, "in_use": inUse}, "busy": busy, "busy_level": level}
	}
	cells := []any{}
	for i := 23; i >= 3; i-- {
		cells = append(cells, cell(i, "free", 0, nil, 0))
	}
	cells = append(cells, cell(2, "free", 120, 0.1, 1), cell(1, "free", 600, 0.9, 4), cell(0, "free", 60, 0.4, 2))
	fetch["/api/nodes/availability"] = map[string]any{"recording": true, "nodes": []any{map[string]any{"id": ovD, "name": "node-d", "cells": cells}}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "strip: a little use is in use", "route": "#/overview", "status": ovStatus(now),
			"has": []string{".avail i.use1", ".avail i.use4", ".avail i.use2", ".avail i.free"}},
	}})
}

// (d) "In tok/s": when fewer than 16 prompt tokens were read (tokens_in
// minus tokens_reused), the measured rate says nothing about the machine;
// the cell is a dash whose hover gives the reuse and the measured rate. The
// same rule in the Overview's runs and the full run list.
func TestUINextRunsInRateAfterReuse(t *testing.T) {
	now := time.Now()
	rows := ovRequests(now)
	reused := rows[2].(map[string]any) // request 14, done
	reused["tokens_in"], reused["tokens_reused"], reused["prefill_tps"] = 2460, 2450, 9123.0
	read := rows[5].(map[string]any) // request 11: 640 in, 600 reused, 40 read: the rate stands
	read["tokens_reused"], read["prefill_tps"] = 600, 377.0
	fetch := ovFetch(now, rows, true)
	cell := "tr[data-req='14'] td.mut[title^='Almost']"
	for _, route := range []string{"#/overview", "#/requests"} {
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
			map[string]any{"name": "in rate: " + route, "route": route, "status": ovStatus(now),
				"has": []string{cell}, "expect": []string{"377"}, "absent": []string{"9,123"}},
		}})
	}
}

// (d) The same rule in a run's detail (the side panel of one run).
func TestUINextRunDetailInRateAfterReuse(t *testing.T) {
	now := time.Now()
	fetch := fixtureRuns(now)
	fetch["/api/requests/101"].(map[string]any)["tokens_reused"] = 1190
	fetch["/api/requests/101"].(map[string]any)["prefill_tps"] = 8800.0
	fetch["/api/requests/102"].(map[string]any)["tokens_in"] = 300
	fetch["/api/requests/102"].(map[string]any)["prefill_tps"] = 377.0
	uiExec(t, map[string]any{"shell": "next", "app": true, "modules": []string{"_fixture/components.js"}, "fetch": fetch, "cases": []any{
		map[string]any{"name": "detail: reused prompt", "route": "#/components", "width": 1440, "click": []string{"button[data-run=101]"},
			"pageHas": []string{"aside.side span[title^='Almost']"}, "page": []string{"In tok/s"}, "notOnPage": []string{"8,800"}},
		map[string]any{"name": "detail: a read prompt", "route": "#/components", "width": 1440, "click": []string{"button[data-run=102]"},
			"page": []string{"In tok/s", "377"}},
	}})
}
