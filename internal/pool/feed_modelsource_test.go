package pool

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// The feed's model_source keeps the registration's target after the first
// heartbeat (whose model_source has no target). A dir:<path> node registers
// as kind share with the path as target (agent contract 6.10), so the target
// is what tells a model folder (an absolute path) from a share (host:export).
func TestFeedModelSourceKeepsTarget(t *testing.T) {
	for _, target := range []string{"/models", "192.0.2.1:/"} {
		r := newRig(t)
		req := regReq("02:00:00:00:00:72")
		req.ModelSource = agentproto.ModelSource{Kind: agentproto.ModelSourceShare, Target: target}
		r.p.Register(req)
		hb := `{"agent_start_id":"s1","seq":1,"uptime_s":3,"role":"idle","launch_id":null,"programs_ready":true,"programs_error":null,
			"model_source":{"kind":"share","reachable":true,"checked_age_s":1.5},"mem_available_mib":9000,
			"devices":[{"name":"Vulkan0","backend":"vulkan","total_mib":14336,"free_mib":14000,"shares_system_memory":true,"read_age_s":1}]}`
		if res := r.p.Heartbeat("02:00:00:00:00:72", []byte(hb)); res.Status != 200 {
			t.Fatalf("heartbeat: %+v", res)
		}
		b, err := json.Marshal(r.p.Facts()[0].ModelSource)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got["kind"] != "share" || got["target"] != target || got["reachable"] != true || got["checked_age_s"] != 1.5 {
			t.Errorf("model_source %s, want kind share, target %q, reachable and its age", b, target)
		}
	}
	// A push node has no target, and the feed says none.
	r := newRig(t)
	req := regReq("02:00:00:00:00:73")
	req.ModelSource = agentproto.ModelSource{Kind: agentproto.ModelSourcePush}
	r.p.Register(req)
	hb := `{"agent_start_id":"s1","seq":1,"uptime_s":3,"role":"idle","launch_id":null,"programs_ready":true,"programs_error":null,
		"model_source":{"kind":"push"},"mem_available_mib":9000,"devices":[]}`
	if res := r.p.Heartbeat("02:00:00:00:00:73", []byte(hb)); res.Status != 200 {
		t.Fatalf("heartbeat: %+v", res)
	}
	b, _ := json.Marshal(r.p.Facts()[0].ModelSource)
	if string(b) != `{"kind":"push"}` || strings.Contains(string(b), "target") {
		t.Errorf("push model_source %s", b)
	}
}
