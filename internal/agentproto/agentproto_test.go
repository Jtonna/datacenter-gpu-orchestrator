package agentproto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLaunchStateNone(t *testing.T) {
	b, _ := json.Marshal(LaunchState{State: StateNone})
	if string(b) != `{"state":"none"}` {
		t.Fatal(string(b))
	}
	b, _ = json.Marshal(LaunchState{LaunchID: "ln-1", Role: RoleWorker, State: StateReady})
	for _, want := range []string{`"exit":null`, `"load":null`, `"command":[]`, `"lease_expires_in_s":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	if strings.Contains(string(b), "client_connected") {
		t.Error("client_connected is omitted when unknown")
	}
}

func TestStatsPresenceRules(t *testing.T) {
	absent := Stats{StatsVersion: 1, Sources: map[string]Source{SourceNode: {Status: SourceAbsent}}}
	b, _ := json.Marshal(absent)
	if string(b) != `{"stats_version":1,"sources":{"node":{"status":"absent"}},"gpus":[]}` {
		t.Fatal(string(b))
	}
	ms := int64(5)
	ok := Stats{StatsVersion: 1, Sources: map[string]Source{SourceNode: {Status: SourceOK, AgeMS: &ms, ScrapeMS: &ms}}}
	b, _ = json.Marshal(ok)
	for _, want := range []string{`"cpu":{}`, `"net":[]`, `"temps":[]`, `"fans":[]`, `"gpus":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	if strings.Contains(string(b), "null") {
		t.Fatal("null in stats:", string(b))
	}
}

func TestRegisterFillEmpty(t *testing.T) {
	var r RegisterRequest
	r.FillEmpty()
	b, _ := json.Marshal(r)
	for _, want := range []string{`"gpus":[]`, `"backends":[]`, `"programs":[]`, `"foreign_processes":[]`, `"launch_id":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestRefusalStatuses(t *testing.T) {
	want := map[string]int{"busy": 409, "paused": 409, "release_mismatch": 409, "model_source_unreachable": 503,
		"model_not_found": 404, "low_memory": 507, "worker_unreachable": 502, "port_in_use": 409}
	for k, v := range want {
		if LaunchRefusalStatus[k] != v {
			t.Errorf("%s: %d", k, LaunchRefusalStatus[k])
		}
	}
}
