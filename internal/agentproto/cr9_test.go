package agentproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// CR-9 adds optional fields only. A body built without them must not name
// them (it is then byte for byte what release 0.1.0 sends), and a release
// 0.1.0 body, which has none of them, must decode to their zero values.
func TestCR9FieldsAreOptionalOnTheWire(t *testing.T) {
	lid := "ln-1"
	reg := RegisterRequest{ID: "02:00:00:00:00:01", LaunchID: &lid}
	reg.FillEmpty()
	for name, v := range map[string]any{
		"registration":   reg,
		"heartbeat":      Heartbeat{AgentStartID: "s1", Devices: []Device{}},
		"launch request": LaunchRequest{LaunchID: lid, Role: RoleWorker, Worker: &WorkerSpec{ModelDir: true, ExtraArgs: []string{}}},
		"launch state":   LaunchState{LaunchID: lid, Role: RoleWorker, State: StateStopping},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"watchdog_floor_mib", "expect_mib", "stop_reason", "stop_message"} {
			if strings.Contains(string(b), f) {
				t.Errorf("%s without the CR-9 fields still names %s: %s", name, f, b)
			}
		}
	}

	// Release 0.1.0 bodies, as the contract shows them.
	var lr LaunchRequest
	if err := json.Unmarshal([]byte(`{"launch_id":"ln-1","role":"worker","release":"0.1.0","label":"m.gguf","lease_ttl_s":20,
		"min_mem_available_mib":9000,"worker":{"model_dir":true,"extra_args":[]}}`), &lr); err != nil || lr.Worker == nil || lr.Worker.ExpectMiB != 0 {
		t.Fatalf("0.1.0 launch request: %+v %v", lr, err)
	}
	var st LaunchState
	if err := json.Unmarshal([]byte(`{"launch_id":"ln-1","role":"worker","state":"stopping","exit":null,"lease_expires_in_s":14}`), &st); err != nil ||
		st.StopReason != "" || st.StopMessage != "" {
		t.Fatalf("0.1.0 launch state: %+v %v", st, err)
	}

	// The new fields round-trip, and a peer that does not know them (decoding
	// into a struct without them, as release 0.1.0 does) is not disturbed.
	b, _ := json.Marshal(LaunchRequest{LaunchID: lid, Role: RoleWorker, Worker: &WorkerSpec{ExtraArgs: []string{}, ExpectMiB: 6200}})
	if !strings.Contains(string(b), `"expect_mib":6200`) {
		t.Fatalf("expect_mib not sent: %s", b)
	}
	var old struct {
		LaunchID string `json:"launch_id"`
		Worker   *struct {
			ModelDir  bool     `json:"model_dir"`
			ExtraArgs []string `json:"extra_args"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.LaunchID != lid || old.Worker == nil {
		t.Fatalf("a 0.1.0-shaped decoder failed on a CR-9 body: %+v %v", old, err)
	}
	if LaunchRefusalStatus[ReasonWouldNotFit] != 409 {
		t.Fatalf("would_not_fit answers %d, want 409", LaunchRefusalStatus[ReasonWouldNotFit])
	}
}
