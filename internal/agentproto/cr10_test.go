package agentproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// CR-10 adds optional fields only. Bodies built without them must not name
// them (they are then what release 0.1.0 sends and gets), release 0.1.0
// bodies decode to their zero values, and a release 0.1.0 decoder is not
// disturbed by the new fields.
func TestCR10FieldsAreOptionalOnTheWire(t *testing.T) {
	for name, v := range map[string]any{
		"registration answer": RegisterAnswer{Accepted: true, Release: "0.1.0", Platform: "linux-amd64-vulkan", HeartbeatEveryS: 2},
		"heartbeat":           Heartbeat{AgentStartID: "s1", Devices: []Device{}},
		"heartbeat answer":    HeartbeatAnswer{Release: "0.1.0"},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "logs") {
			t.Errorf("%s without the CR-10 fields still names logs: %s", name, b)
		}
	}
	if b, _ := json.Marshal(HeartbeatAnswer{Release: "0.1.0"}); string(b) != `{"release":"0.1.0","paused":false}` {
		t.Fatalf("heartbeat answer changed for release 0.1.0: %s", b)
	}

	// Release 0.1.0 bodies, as the contract shows them.
	var hb Heartbeat
	if err := json.Unmarshal([]byte(`{"agent_start_id":"k3f9a2","seq":1841,"role":"idle","launch_id":null,"devices":[]}`), &hb); err != nil || hb.Logs != nil {
		t.Fatalf("0.1.0 heartbeat: %+v %v", hb, err)
	}
	var ra RegisterAnswer
	if err := json.Unmarshal([]byte(`{"accepted":true,"release":"0.1.0","paused":false,"stop_launch":null}`), &ra); err != nil || ra.Logs != nil {
		t.Fatalf("0.1.0 registration answer: %+v %v", ra, err)
	}
	var ha HeartbeatAnswer
	if err := json.Unmarshal([]byte(`{"release":"0.1.0","paused":false}`), &ha); err != nil || ha.LogsRevision != 0 {
		t.Fatalf("0.1.0 heartbeat answer: %+v %v", ha, err)
	}

	// The new fields on the wire, exactly as the design shows the object.
	age := 3.5
	obj := &Logs{Revision: 7, URL: "/api/pool/nodes/02:00:00:00:00:01/logs", Format: LogsFormatJSONLine,
		Limits:    LogsLimits{MaxBatchBytes: LogsMaxBatchBytes, MaxLineBytes: LogsMaxLineBytes, MaxBytesPerS: LogsMaxBytesPerS, EveryS: LogsEveryS},
		DenyUnits: []string{}}
	b, _ := json.Marshal(RegisterAnswer{Accepted: true, Release: "0.1.0", Logs: obj})
	want := `"logs":{"revision":7,"url":"/api/pool/nodes/02:00:00:00:00:01/logs","format":"jsonline","auth":null,` +
		`"limits":{"max_batch_bytes":1048576,"max_line_bytes":8192,"max_bytes_per_s":262144,"every_s":2},"deny_units":[]}`
	if !strings.Contains(string(b), want) {
		t.Fatalf("logs object on the wire:\n got %s\nwant %s", b, want)
	}
	hbNew, _ := json.Marshal(Heartbeat{AgentStartID: "s1", Devices: []Device{},
		Logs: &LogShipping{LinesSent: 10, LinesDropped: 2, LastError: "429", LastErrorAgeS: &age}})
	if !strings.Contains(string(hbNew), `"logs":{"lines_sent":10,"lines_dropped":2,"last_error":"429","last_error_age_s":3.5}`) {
		t.Fatalf("shipping figures on the wire: %s", hbNew)
	}
	haNew, _ := json.Marshal(HeartbeatAnswer{Release: "0.1.0", LogsRevision: 7})
	if string(haNew) != `{"release":"0.1.0","paused":false,"logs_revision":7}` {
		t.Fatalf("heartbeat answer: %s", haNew)
	}

	// A release 0.1.0 agent decodes the answers into structs without the new
	// fields and must get everything it knows.
	var oldReg struct {
		Accepted        bool    `json:"accepted"`
		Release         string  `json:"release"`
		HeartbeatEveryS float64 `json:"heartbeat_every_s"`
		StopLaunch      *string `json:"stop_launch"`
	}
	if err := json.Unmarshal(b, &oldReg); err != nil || !oldReg.Accepted || oldReg.Release != "0.1.0" {
		t.Fatalf("a 0.1.0-shaped decoder failed on a CR-10 registration answer: %+v %v", oldReg, err)
	}
	var oldHA struct {
		Release string `json:"release"`
		Paused  bool   `json:"paused"`
	}
	if err := json.Unmarshal(haNew, &oldHA); err != nil || oldHA.Release != "0.1.0" {
		t.Fatalf("a 0.1.0-shaped decoder failed on a CR-10 heartbeat answer: %+v %v", oldHA, err)
	}
}

// CR-10, agent batch: server_time_unix_ms is optional in both answers. A
// release 0.1.0 decoder is not disturbed by it, and a body without it
// decodes to 0 (no server time).
func TestServerTimeIsOptional(t *testing.T) {
	b, _ := json.Marshal(HeartbeatAnswer{Release: "0.1.0", LogsRevision: 7, ServerTimeUnixMs: 1700000000123})
	if string(b) != `{"release":"0.1.0","paused":false,"logs_revision":7,"server_time_unix_ms":1700000000123}` {
		t.Fatalf("heartbeat answer: %s", b)
	}
	var old struct {
		Release string `json:"release"`
		Paused  bool   `json:"paused"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.Release != "0.1.0" {
		t.Fatalf("0.1.0 decoder: %+v %v", old, err)
	}
	rb, _ := json.Marshal(RegisterAnswer{Accepted: true, Release: "0.1.0", ServerTimeUnixMs: 5})
	var oldReg struct {
		Accepted bool   `json:"accepted"`
		Release  string `json:"release"`
	}
	if err := json.Unmarshal(rb, &oldReg); err != nil || !oldReg.Accepted || !strings.Contains(string(rb), `"server_time_unix_ms":5`) {
		t.Fatalf("registration answer: %s %v", rb, err)
	}
	var ha HeartbeatAnswer
	var ra RegisterAnswer
	if json.Unmarshal([]byte(`{"release":"0.1.0","paused":false}`), &ha); ha.ServerTimeUnixMs != 0 {
		t.Fatal(ha)
	}
	if json.Unmarshal([]byte(`{"accepted":true,"release":"0.1.0","paused":false,"stop_launch":null}`), &ra); ra.ServerTimeUnixMs != 0 {
		t.Fatal(ra)
	}
}
