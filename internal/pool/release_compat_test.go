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

// The release is 0.1.3 (fit before eviction, benchmark step facts, cached
// bytes; it was 0.1.2 since CR-10's last batch, 0.1.1 since the CR-9 review).
// An agent of an older release, 0.1.0, 0.1.1 or 0.1.2, gets the update path
// of contract 5.4: refused with agent_too_old and required_release 0.1.3,
// never a load. A 0.1.3 agent that sends a 0.1.0-shaped body (none of the
// CR-9, CR-10 or later fields) is accepted: every new field is optional.
// The start-up report (startup, startup_ack) is one more: additive, no new
// release.
func TestRelease013AndOlderAgents(t *testing.T) {
	if agentproto.Release != "0.1.3" {
		t.Fatalf("agentproto.Release = %q, want 0.1.3", agentproto.Release)
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

	for i, r := range []string{"0.1.0", "0.1.1", "0.1.2"} {
		old := regReq(fmt.Sprintf("02:00:00:00:00:7%d", i))
		old.AgentRelease = r
		ans := p.Register(old)
		if ans.Accepted || ans.Reason != agentproto.ReasonAgentTooOld || ans.RequiredRelease != "0.1.3" {
			t.Fatalf("%s agent: %+v", r, ans)
		}
	}
	cur := regReq("02:00:00:00:00:7f") // no watchdog_floor_mib: the 0.1.0 shape
	cur.AgentRelease = agentproto.Release
	if ans := p.Register(cur); !ans.Accepted || ans.Release != agentproto.Release || ans.StartupAck != 0 {
		t.Fatalf("0.1.3 agent with a 0.1.0-shaped body: %+v", ans)
	}
	// A 0.1.3 agent as it is today sends no start-up report: it gets no
	// record and no startup_ack, and its heartbeats are taken as before.
	if res := p.Heartbeat(cur.ID, hbBody("s1", "", true, "true")); res.Status != 200 || res.Answer.StartupAck != 0 {
		t.Fatalf("0.1.3 heartbeat: %+v", res)
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
