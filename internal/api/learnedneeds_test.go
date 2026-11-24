package api

import (
	"encoding/json"
	"testing"
	"time"

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

// GET /api/needs lists every learned need by shape and device count (none
// here); POST /api/needs/forget drops a shape's and is audited; a request
// without a shape is a 400 (review SS2).
func TestNeedsListAndForgetByShape(t *testing.T) {
	e, _, _ := cfgEnv(t)
	code, got := e.cdo("GET", "/api/needs", "", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, got)
	}
	if rows, ok := got["needs"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("needs %v", got["needs"])
	}
	for _, k := range []string{"host_build", "release", "served_build"} { // the builds trust depends on (FS2)
		if _, ok := got[k]; !ok {
			t.Fatalf("no %q in %v", k, got)
		}
	}
	// the check of derived needs: the tolerance and every pair (none here)
	if dc, ok := got["derivation_check"].(map[string]any); !ok || dc["tolerance_mib"] != float64(512) || dc["pairs"] == nil {
		t.Fatalf("derivation_check %v", got["derivation_check"])
	}
	code, got = e.cdo("POST", "/api/needs/forget", `{"shape":"[\"llama\",\"m.gguf\",8192,1,\"f16\",false,[]]"}`, nil)
	if code != 200 || got["forgotten"] != false || got["msg"] != "nothing learned" {
		t.Fatalf("forget: %d %v", code, got)
	}
	eventually(t, "forget by shape audited", func() bool {
		_, m := e.cdo("GET", "/api/audit?section=runtime&action=forget_need", "", nil)
		return m["total_matching"] == float64(1)
	})
	if code, _ := e.cdo("POST", "/api/needs/forget", `{}`, nil); code != 400 {
		t.Fatalf("no shape: %d", code)
	}
}

// The listing's field names.
func TestNeedRowJSON(t *testing.T) {
	b, _ := json.Marshal(needRowJSON(scheduler.LearnedRow{Shape: "s", Model: "m.gguf", Devices: 3, TotalMiB: 13000, MeasuredBy: "three",
		LlamaBuild: "a01665d3", Trusted: true, Profiles: []string{"three"}}))
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"shape", "kind", "model", "ctx", "parallel", "kv_quant", "flash_attn", "extra_args", "devices", "total_mib",
		"placement", "measured_by", "measured_at", "release", "llama_build", "restored", "trusted", "untrusted_reason", "profiles"} {
		if _, ok := m[k]; !ok {
			t.Errorf("no %q in %s", k, b)
		}
	}
}

// GET /api/needs lists the measured host overheads (none here) under
// host_overheads; a row's field names.
func TestNeedsListsHostOverheads(t *testing.T) {
	e, _, _ := cfgEnv(t)
	code, got := e.cdo("GET", "/api/needs", "", nil)
	if rows, ok := got["host_overheads"].([]any); code != 200 || !ok || len(rows) != 0 {
		t.Fatalf("%d host_overheads %v", code, got["host_overheads"])
	}
	if got["host_overhead_setting_mib"] != float64(1024) || got["host_overhead_setting_only"] != false {
		t.Fatalf("settings %v %v", got["host_overhead_setting_mib"], got["host_overhead_setting_only"])
	}
	b, _ := json.Marshal(hostOverheadJSON(scheduler.HostOverheadRow{Shape: "s", Node: "a", NodeName: "ps5-a", MiB: 600, LogHostMiB: 150,
		MeasuredBy: "q", Cause: "load", Profiles: []string{"q"}, Used: true}))
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"shape", "node", "node_name", "mib", "log_host_mib", "measured_by", "measured_at", "release", "llama_build",
		"cause", "restored", "used", "profiles"} {
		if _, ok := m[k]; !ok {
			t.Errorf("no %q in %s", k, b)
		}
	}
}

// A plan of a profile hosted on a node names which host overhead it used.
func TestHostEntryNamesTheOverheadSource(t *testing.T) {
	e := map[string]any{}
	hostEntry(e, scheduler.PlanReport{Host: "node", HostNode: "a", HostOverheadMiB: 600, HostOverheadSource: scheduler.HostOverheadMeasured,
		HostOverheadMeasuredAt: time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)}, map[string]string{"a": "ps5-a"})
	if e["host_overhead_mib"] != 600 || e["host_overhead_source"] != "measured" || e["host_overhead_measured_at"] != "2026-11-23T06:00:00.000Z" {
		t.Fatalf("%v", e)
	}
	e = map[string]any{}
	hostEntry(e, scheduler.PlanReport{Host: "node", HostNode: "a", HostOverheadMiB: 1024, HostOverheadSource: scheduler.HostOverheadFromSetting}, nil)
	if e["host_overhead_source"] != "setting" {
		t.Fatalf("%v", e)
	}
	if _, ok := e["host_overhead_measured_at"]; ok {
		t.Fatalf("no time for the setting: %v", e)
	}
}
