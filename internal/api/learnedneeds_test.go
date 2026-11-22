package api

import (
	"encoding/json"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
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

// POST /api/forget-need drops a profile's learned need and is audited; an
// unknown profile is a 404.
func TestForgetNeedIsAudited(t *testing.T) {
	e, _, _ := cfgEnv(t)
	resp := e.post("/api/forget-need", `{"model":"alpha"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body(t, resp))
	}
	var got map[string]any
	json.Unmarshal([]byte(body(t, resp)), &got)
	if sh, _ := got["profiles_of_shape"].([]any); len(sh) == 0 || sh[0] != "alpha" {
		t.Fatalf("profiles_of_shape %v", got["profiles_of_shape"])
	}
	if got["model"] != "alpha" || got["msg"] != "nothing learned" || got["forgotten"] != false {
		t.Fatalf("answer %v", got)
	}
	eventually(t, "forget audited", func() bool {
		_, m := e.cdo("GET", "/api/audit?section=runtime&action=forget_need", "", nil)
		return m["total_matching"] == float64(1)
	})
	if resp := e.post("/api/forget-need", `{"model":"nope"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown profile: %d", resp.StatusCode)
	}
	if resp := e.post("/api/forget-need", `{}`); resp.StatusCode != 400 {
		t.Fatalf("no model: %d", resp.StatusCode)
	}
}
