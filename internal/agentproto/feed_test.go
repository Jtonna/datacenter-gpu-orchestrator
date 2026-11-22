package agentproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// host_uptime_s and boot_id are optional heartbeat fields: a heartbeat
// without them does not name them, an older heartbeat decodes to their zero
// values, and an older decoder is not disturbed by them.
func TestHostUptimeAndBootIDAreOptional(t *testing.T) {
	b, err := json.Marshal(Heartbeat{AgentStartID: "s1", Devices: []Device{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"host_uptime_s", "boot_id"} {
		if strings.Contains(string(b), f) {
			t.Errorf("a heartbeat without the field still names %s: %s", f, b)
		}
	}
	var hb Heartbeat
	if err := json.Unmarshal([]byte(`{"agent_start_id":"s1","seq":3,"uptime_s":2.5,"role":"idle","launch_id":null,
		"programs_ready":true,"programs_error":null,"model_source":{"kind":"share"},"devices":[],"mem_available_mib":1}`), &hb); err != nil {
		t.Fatal(err)
	}
	if hb.HostUptimeS != nil || hb.BootID != "" {
		t.Fatalf("older heartbeat: %+v", hb)
	}
	up := 4123.25
	b, _ = json.Marshal(Heartbeat{AgentStartID: "s1", Devices: []Device{}, HostUptimeS: &up, BootID: "0123456789abcdef0123456789abcdef"})
	if !strings.Contains(string(b), `"host_uptime_s":4123.25`) || !strings.Contains(string(b), `"boot_id":"0123456789abcdef0123456789abcdef"`) {
		t.Fatalf("fields not sent: %s", b)
	}
	var old struct {
		AgentStartID string `json:"agent_start_id"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.AgentStartID != "s1" {
		t.Fatalf("an older decoder failed: %v", err)
	}
}
