package pool

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// The release is 0.1.10 (the Nvidia free figure from the exporter, the
// setup script opening 50061, same-release re-fetch, the DEBUG line naming
// allowed call sources). It was 0.1.9 (a console as host, --advertise-addr,
// the GPU under WSL2), 0.1.6 before (boot_origin, the file cache eviction
// and the busy file on the agent; the reservation rule and needs by shape on
// Potluck); 0.1.7 and 0.1.8 were scheduler images only that still served
// 0.1.6. It was 0.1.4 since the node feed and start-up records, 0.1.3 since
// fit before eviction, 0.1.2 since CR-10's last batch, 0.1.1 since the CR-9
// review; 0.1.5 was a scheduler image only that still served 0.1.4. An agent
// of an older release, 0.1.0 to 0.1.9, gets the update path of contract 5.4:
// refused with agent_too_old and required_release 0.1.10, never a load. The
// agents in the field run 0.1.9, so that row is the roll-out of this
// release; compareRelease orders by number, so 0.1.9 < 0.1.10. A newer agent
// (0.1.11, 0.2.0) waits for Potluck (potluck_too_old). A 0.1.10 agent that
// sends a 0.1.0-shaped body (none of the CR-9, CR-10 or later fields, no
// capabilities) is accepted: every new field is optional, the start-up
// report (startup, startup_ack) among them.
func TestRelease0110AndOlderAgents(t *testing.T) {
	if agentproto.Release != "0.1.10" {
		t.Fatalf("agentproto.Release = %q, want 0.1.10", agentproto.Release)
	}
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: agentproto.Release, Llama: llamaBuild,
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("A")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("R")},
		}})
	rel, err := release.Load(root, agentproto.Release)
	if err != nil {
		t.Fatal(err)
	}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	cfg := &config.Config{Defaults: d}
	p, err := New(Options{Config: func() *config.Config { return cfg }, Programs: Programs{Release: rel},
		PotluckRelease: agentproto.Release, Clock: time.Now, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	p.inlineCallBack = true
	p.callBack = func(context.Context, string) error { return nil } // nothing on the network

	for i, r := range []string{"0.1.0", "0.1.1", "0.1.2", "0.1.3", "0.1.4", "0.1.5", "0.1.6", "0.1.7", "0.1.8", "0.1.9"} {
		old := regReq(fmt.Sprintf("02:00:00:00:00:7%x", i))
		old.AgentRelease = r
		ans := p.Register(old)
		if ans.Accepted || ans.Reason != agentproto.ReasonAgentTooOld || ans.RequiredRelease != "0.1.10" {
			t.Fatalf("%s agent: %+v", r, ans)
		}
	}
	for i, r := range []string{"0.1.11", "0.2.0"} {
		newer := regReq(fmt.Sprintf("02:00:00:00:00:6%x", i))
		newer.AgentRelease = r
		if ans := p.Register(newer); ans.Accepted || ans.Reason != agentproto.ReasonPotluckTooOld {
			t.Fatalf("%s agent: %+v", r, ans)
		}
	}
	cur := regReq("02:00:00:00:00:7f") // no watchdog_floor_mib: the 0.1.0 shape
	cur.AgentRelease = agentproto.Release
	if ans := p.Register(cur); !ans.Accepted || ans.Release != agentproto.Release || ans.StartupAck != 0 {
		t.Fatalf("0.1.10 agent with a 0.1.0-shaped body: %+v", ans)
	}
	// An agent that sends no start-up report gets no record and no
	// startup_ack, and its heartbeats are taken as before.
	if res := p.Heartbeat(cur.ID, hbBody("s1", "", true, "true")); res.Status != 200 || res.Answer.StartupAck != 0 {
		t.Fatalf("heartbeat without a start-up report: %+v", res)
	}
	for _, f := range p.Facts() {
		if f.LastStartup != nil {
			t.Fatalf("a start-up record for an agent that sent no report: %+v", f)
		}
	}
	if all, err := p.Startups().All(); err != nil || len(all) != 0 {
		t.Fatalf("%v %v", all, err)
	}
}
