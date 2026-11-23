package api

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// GET /api/profiles shows a host: node profile's host node (by name), the
// overhead hosting adds there, and why it cannot host now.
func TestProfilesHostEntry(t *testing.T) {
	e := map[string]any{}
	hostEntry(e, scheduler.PlanReport{Host: core.HostNode, HostNode: "id-a", HostOverheadMiB: 1024,
		HostProblem: "host_not_capable: node console-a cannot host: its agent does not advertise host_launch"},
		map[string]string{"id-a": "console-a"})
	if e["host"] != "node" || e["host_node"] != "console-a" || e["host_overhead_mib"] != 1024 || e["host_problem"] == nil {
		t.Fatalf("%v", e)
	}
	e = map[string]any{}
	hostEntry(e, scheduler.PlanReport{Host: core.HostServer}, nil)
	if e["host"] != "server" || len(e) != 1 {
		t.Fatalf("%v", e)
	}
}

// Both UIs draw where a node-hosted profile runs: the classic Profiles list
// (from GET /api/profiles) and the new shell's model panel (from GET
// /api/models, which passes the host keys through).
func TestUIHostOnNodeDrawsInBothUIs(t *testing.T) {
	host := map[string]any{"host": "node", "host_node": "console-a", "host_overhead_mib": 1024.0, "description": "Qwen for the canary"}

	// classic: the Profiles list, with one profile hosted on a node
	e, _, _ := cfgEnv(t)
	if st, m := e.cdo("POST", "/api/config/profiles", `{"id":"h1","model":"h1.gguf","ctx":2048,"needs_mib":100,"description":"Qwen for the canary"}`, nil); st >= 300 {
		t.Fatalf("%d %v", st, m)
	}
	var profiles map[string]any
	if err := json.Unmarshal([]byte(body(t, e.get("/api/profiles"))), &profiles); err != nil {
		t.Fatal(err)
	}
	list, _ := profiles["profiles"].([]any)
	if len(list) == 0 {
		t.Fatal("no profiles in the config env")
	}
	id := "h1"
	for _, p := range list {
		if m := p.(map[string]any); m["id"] == id {
			maps.Copy(m, host)
		}
	}
	uiExec(t, map[string]any{"modules": []string{"views/profiles.js"}, "base": e.front.URL, "fetch": map[string]any{"/api/profiles": profiles}, "cases": []any{
		map[string]any{"name": "profiles list, a profile hosted on a node", "route": "#/profiles?q=" + id, "status": "fetch", "expect": []string{id, "hosted on console-a", "for the host", "Qwen for the canary"}},
	}})

	// new shell: the model panel, from the catalog
	b := benchEnv(t)
	var cat map[string]any
	if err := json.Unmarshal([]byte(body(t, b.get("/api/models"))), &cat); err != nil {
		t.Fatal(err)
	}
	key := ""
	for _, m := range cat["models"].([]any) {
		mm := m.(map[string]any)
		if mm["kind"] != "llama" || key != "" {
			continue
		}
		for _, p := range mm["profiles"].([]any) {
			if pm := p.(map[string]any); pm["generated"] != true {
				maps.Copy(pm, host)
				key = mm["key"].(string)
				break
			}
		}
	}
	if key == "" {
		t.Fatal("no hand-written llama profile in the catalog")
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": b.front.URL, "fetch": map[string]any{"/api/models": cat}, "cases": []any{
		map[string]any{"name": "model panel, a profile hosted on a node", "route": "#/models?model=" + key, "status": "fetch", "page": []string{"hosted on console-a", "for the host", "Qwen for the canary"}},
	}})
}

// The description goes through /api/config (as written) and /api/profiles.
func TestProfileDescriptionInTheAPI(t *testing.T) {
	e, _, _ := cfgEnv(t)
	if st, m := e.cdo("POST", "/api/config/profiles", `{"id":"d1","model":"d1.gguf","ctx":2048,"needs_mib":100,"description":"for people"}`, nil); st >= 300 {
		t.Fatalf("%d %v", st, m)
	}
	if b := body(t, e.get("/api/config")); !strings.Contains(b, `"description":"for people"`) {
		t.Fatalf("config: %s", b)
	}
	var doc map[string]any
	json.Unmarshal([]byte(body(t, e.get("/api/profiles"))), &doc)
	for _, p := range doc["profiles"].([]any) {
		if m := p.(map[string]any); m["id"] == "d1" && m["description"] != "for people" {
			t.Fatalf("%v", m)
		}
	}
	if st, _ := e.cdo("POST", "/api/config/profiles", `{"id":"d2","model":"d2.gguf","ctx":2048,"description":"a\nb"}`, nil); st != 400 && st != 422 {
		t.Fatalf("a two-line description: %d", st)
	}
}
