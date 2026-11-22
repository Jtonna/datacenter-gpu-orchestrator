package agentproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// The start-up report is optional everywhere: bodies without it do not name
// it, older bodies decode without it, and older decoders ignore it.
func TestStartupIsOptionalOnTheWire(t *testing.T) {
	reg := RegisterRequest{ID: "02:00:00:00:00:01"}
	reg.FillEmpty()
	for name, v := range map[string]any{"registration": reg, "heartbeat": Heartbeat{Devices: []Device{}},
		"registration answer": RegisterAnswer{Accepted: true}, "heartbeat answer": HeartbeatAnswer{Release: Release}} {
		b, _ := json.Marshal(v)
		for _, f := range []string{"startup", "startup_ack"} {
			if strings.Contains(string(b), `"`+f+`"`) {
				t.Errorf("%s without a start-up report names %s: %s", name, f, b)
			}
		}
	}

	up, soft := 3.25, int64(1)
	st := &Startup{BootID: "0123456789abcdef0123456789abcdef", AgentStartS: 21.5, UserspaceStartS: &up, SoftReboots: &soft,
		Marks:         map[string]float64{StartupMarkAgentStart: 21.5, StartupMarkRegistered: 23.75},
		ExternalMarks: map[string]float64{"network_ok": 9.5, "some_new_mark": 30}}
	reg.Startup = st
	b, _ := json.Marshal(reg)
	for _, want := range []string{`"startup":{"boot_id":"0123456789abcdef0123456789abcdef","agent_start_s":21.5,"userspace_start_s":3.25,"soft_reboots":1,`,
		`"marks":{"agent_start":21.5,"registered":23.75}`, `"external_marks":{"network_ok":9.5,"some_new_mark":30}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("registration lacks %s: %s", want, b)
		}
	}
	var back RegisterRequest
	if err := json.Unmarshal(b, &back); err != nil || back.Startup == nil || back.Startup.Count() != 4 || *back.Startup.SoftReboots != 1 {
		t.Fatalf("round trip: %+v %v", back.Startup, err)
	}
	// Without systemd facts the two fields are left out, not sent as zero.
	b, _ = json.Marshal(Startup{BootID: "b", Marks: map[string]float64{}, ExternalMarks: map[string]float64{}})
	if strings.Contains(string(b), "userspace_start_s") || strings.Contains(string(b), "soft_reboots") {
		t.Fatalf("absent systemd facts are sent: %s", b)
	}
	// An older Potluck decodes the registration into a type without startup.
	var old struct {
		ID           string `json:"id"`
		AgentStartID string `json:"agent_start_id"`
	}
	if err := json.Unmarshal([]byte(`{"id":"x","agent_start_id":"s","startup":{"boot_id":"b","marks":{}}}`), &old); err != nil || old.ID != "x" {
		t.Fatalf("older decoder: %v", err)
	}
	// The answers: an older agent ignores startup_ack; a newer one reads it.
	var ha HeartbeatAnswer
	if err := json.Unmarshal([]byte(`{"release":"0.1.3","paused":false,"startup_ack":4}`), &ha); err != nil || ha.StartupAck != 4 {
		t.Fatalf("heartbeat answer %+v %v", ha, err)
	}
	if (*Startup)(nil).Count() != 0 {
		t.Fatal("nil count")
	}
}

func TestStartupMarkNames(t *testing.T) {
	for _, ok := range []string{"network_ok", "boot_progress_start", "a", "x.y-z_1", strings.Repeat("a", 64)} {
		if !ValidStartupMarkName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "two words", "näme", strings.Repeat("a", 65), "a/b", "a=b"} {
		if ValidStartupMarkName(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}
