package api

import (
	"encoding/json"
	"testing"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/scheduler"
)

// GET /api/status carries the needs learned from loads this run under
// learned_needs, keyed by profile; the existing fields keep their names.
func TestStatusLearnedNeedsShape(t *testing.T) {
	st := scheduler.Status{LearnedNeeds: map[string]scheduler.LearnedNeed{
		"m": {MeasuredMiB: 11800, NeedMiB: 11800, Source: "measured this run", EstimatedMiB: 9000,
			Nodes: []scheduler.LearnedNodeNeed{{Node: "a", Fraction: 0.5, MiB: 5900}}},
	}}
	b, err := json.Marshal(jsonView(st))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(b, &doc)
	ln, ok := doc["learned_needs"].(map[string]any)
	if !ok {
		t.Fatalf("no learned_needs in %s", b)
	}
	m := ln["m"].(map[string]any)
	if m["measured_mib"].(float64) != 11800 || m["need_mib"].(float64) != 11800 || m["source"] != "measured this run" || m["estimated_mib"].(float64) != 9000 {
		t.Fatalf("learned_needs.m = %v", m)
	}
	if nodes := m["nodes"].([]any); len(nodes) != 1 || nodes[0].(map[string]any)["mib"].(float64) != 5900 {
		t.Fatalf("nodes %v", m["nodes"])
	}
	for _, k := range []string{"instances", "queue", "recent_loads", "view", "backoffs", "quarantines"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("existing field %q missing", k)
		}
	}
}
