package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

func registerWithStartup(t *testing.T, e *env, id, startID, boot string, startup string) map[string]any {
	t.Helper()
	req := agentproto.RegisterRequest{ID: id, Name: "console-1", AgentRelease: "0.1.0", AgentStartID: startID, OS: "linux", Arch: "amd64",
		Install: "bare", Addr: "192.0.2.101", Backends: []string{"vulkan"}}
	req.FillEmpty()
	b, _ := json.Marshal(req)
	s := strings.TrimSuffix(string(b), "}") + `,"startup":` + startup + `}`
	code, out, _ := e.do("POST", agentproto.PathRegister, s)
	if code != 200 {
		t.Fatalf("register: %d %s", code, out)
	}
	var m map[string]any
	json.Unmarshal([]byte(out), &m)
	return m
}

func TestStartupRoutes(t *testing.T) {
	e, p, au := agentsEnv(t)
	const id = "02:00:00:00:00:31"
	ans := registerWithStartup(t, e, id, "s1", "b1",
		`{"boot_id":"b1","agent_start_s":20,"userspace_start_s":3,"soft_reboots":0,"marks":{"agent_start":20},"external_marks":{"network_ok":9.5}}`)
	if ans["accepted"] != true || ans["startup_ack"] != float64(2) {
		t.Fatalf("%v", ans)
	}
	// A report that cannot be read never fails the registration.
	if ans := registerWithStartup(t, e, "02:00:00:00:00:32", "x1", "b9", `"soon"`); ans["accepted"] != true || ans["startup_ack"] != nil {
		t.Fatalf("%v", ans)
	}

	// expect-restart: a comment and a kind, a paused node.
	for _, c := range []struct {
		body string
		code int
		has  string
	}{
		{`{"kind":"soft"}`, 400, "comment"},
		{`{"kind":"warm","comment":"x"}`, 400, "kind"},
		{`{"kind":"soft","comment":"x"}`, 409, "not paused"},
	} {
		code, out, _ := e.do("POST", "/api/nodes/"+id+"/expect-restart", c.body)
		if code != c.code || !strings.Contains(out, c.has) {
			t.Fatalf("%s: %d %s", c.body, code, out)
		}
	}
	if code, out, _ := e.do("POST", "/api/nodes/02:00:00:00:00:99/expect-restart", `{"kind":"soft","comment":"x"}`); code != 404 {
		t.Fatalf("unknown node: %d %s", code, out)
	}
	if _, err := p.SetPaused(id, true); err != nil {
		t.Fatal(err)
	}
	code, out, _ := e.do("POST", "/api/nodes/"+id+"/expect-restart", `{"kind":"soft","comment":"systemd update"}`)
	if code != 200 || !strings.Contains(out, `"valid_s":600`) {
		t.Fatalf("%d %s", code, out)
	}
	au.mu.Lock()
	last := au.es[len(au.es)-1]
	au.mu.Unlock()
	if last.Action != "expect_restart" || last.Target != "node:"+id || last.Comment != "systemd update" {
		t.Fatalf("audit %+v", last)
	}

	// The node's records.
	code, out, _ = e.do("GET", "/api/nodes/"+id+"/startups?limit=5", "")
	var list struct {
		Startups []struct {
			Record       map[string]any   `json:"record"`
			MarksInOrder []map[string]any `json:"marks_in_order"`
			Condition    string           `json:"condition"`
		} `json:"startups"`
		NotIncluded string `json:"not_included"`
	}
	json.Unmarshal([]byte(out), &list)
	if code != 200 || len(list.Startups) != 1 || list.NotIncluded != StartupNotIncluded {
		t.Fatalf("%d %s", code, out)
	}
	s0 := list.Startups[0]
	if s0.Record["kind"] != "full" || s0.Record["inferred"] != true || len(s0.MarksInOrder) != 2 ||
		s0.MarksInOrder[0]["name"] != "network_ok" || !strings.HasPrefix(s0.Condition, "missing registered") {
		t.Fatalf("%s", out)
	}
	if code, _, _ := e.do("GET", "/api/nodes/"+id+"/startups?limit=0", ""); code != 400 {
		t.Fatalf("limit 0: %d", code)
	}

	// The figures per node and kind.
	code, out, _ = e.do("GET", "/api/startups", "")
	if code != 200 || !strings.Contains(out, `"kind":"full"`) || !strings.Contains(out, StartupNotIncluded[:30]) {
		t.Fatalf("%d %s", code, out)
	}

	// status and the node feed carry last_startup, the same object.
	code, out, _ = e.do("GET", "/api/status", "")
	if code != 200 || !strings.Contains(out, `"last_startup":{"kind":"full"`) {
		t.Fatalf("status %s", out)
	}
}
