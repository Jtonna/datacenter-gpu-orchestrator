package api

import (
	"slices"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The plan row's figures reach the JSON under the names the docs and the UI
// use; on a node with memory of its own only placeable_mib is added.
func TestPlanRowFigureNames(t *testing.T) {
	a, f, l, adj := 13656, 1000, 9000, -656
	reading, why := "current", "the device reports less"
	row := jsonView(scheduler.PlanMemoryNode{PlaceableMiB: 11632, MemAvailableMiB: &a, MemAvailableAvgMiB: &a, MemAvailableReading: &reading,
		WatchdogFloorMiB: &f, PlaceableAdjustMiB: &adj, PlaceableAdjustWhy: &why, PredictedLowestAvailableMiB: &l}).(map[string]any)
	for _, k := range append([]string{"placeable_mib", "free_now_mib", "available_mib", "headroom_mib"}, planMemoryNodeOptionalKeys...) {
		if _, ok := row[k]; !ok {
			t.Errorf("no %s in %v", k, row)
		}
	}
	plain := jsonView(scheduler.PlanMemoryNode{PlaceableMiB: 1}).(map[string]any)
	for _, k := range planMemoryNodeOptionalKeys {
		if _, ok := plain[k]; ok {
			t.Errorf("%s on a node with memory of its own", k)
		}
	}
	if !slices.Contains(planMemoryNodeKeys, "placeable_mib") {
		t.Error("placeable_mib is not in the plan row keys")
	}
}

// The Placement preview ("How the memory was worked out") draws one line per shared-memory node:
// "available A − floor F − headroom H = placeable P; after this load about
// L left", from the server's figures.
func TestUIPlanFiguresLine(t *testing.T) {
	e, _, _ := cfgEnv(t)
	if st, m := e.cdo("POST", "/api/config/profiles", `{"id":"p1","model":"p1.gguf","ctx":2048,"needs_mib":100}`, nil); st >= 300 {
		t.Fatalf("%d %v", st, m)
	}
	row := map[string]any{"id": "02:00:00:00:00:a1", "node": "console-a", "shared": true, "total_mib": 16000.0, "weights_mib": 5000.0, "context_mib": 1000.0,
		"fill_reserve_mib": 0.0, "will_take_mib": 6000.0, "basis": "measured", "free_now_mib": 12656.0, "free_avg_mib": 12656.0, "free_avg_known": false,
		"reserved_by": []any{}, "headroom_mib": 1024.0, "available_mib": 11632.0, "margin_mib": 300.0, "margin_rule": "margin_measured_mib",
		"remains_mib": 5332.0, "host_overhead_mib": 0.0, "spare_mib": 5632.0, "tight": false, "margin_short_mib": 0.0, "placeable_mib": 11632.0,
		"mem_available_mib": 13656.0, "mem_available_reading": "current", "watchdog_floor_mib": 1000.0, "predicted_lowest_available_mib": 7656.0}
	plan := map[string]any{"model": "p1", "action": "start", "would_start": true, "nodes": []any{"console-a"}, "reason": "", "needs_mib": 6000.0,
		"memory": map[string]any{"decides": true, "fits": true, "hypothetical": false, "nodes": []any{row}, "note": "", "placement_uses_reserved": true,
			"short_node": "", "shortfall_mib": 0.0, "tight_fit": false}}
	uiExec(t, map[string]any{"modules": []string{"views/placement.js"}, "base": e.front.URL, "fetch": map[string]any{"/api/plan": plan}, "cases": []any{
		map[string]any{"name": "placement preview, the figures line", "route": "#/placement/p1", "status": "fetch",
			"expect": []string{"available 13,656 MiB − floor 1,000 MiB − headroom 1,024 MiB = placeable 11,632 MiB", "after this load about 7,656 MiB left"}},
	}})
}
