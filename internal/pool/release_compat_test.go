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

// The release is 0.1.2 (CR-10, the last batch; it was 0.1.1 since the CR-9
// review). An agent of an older release, 0.1.0 or 0.1.1, gets the update path
// of contract 5.4: refused with agent_too_old and required_release 0.1.2,
// never a load. A 0.1.2 agent that sends a 0.1.0-shaped body (none of the
// CR-9 or CR-10 fields) is accepted: every new field is optional.
func TestRelease012AndOlderAgents(t *testing.T) {
	if agentproto.Release != "0.1.2" {
		t.Fatalf("agentproto.Release = %q, want 0.1.2", agentproto.Release)
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

	for i, r := range []string{"0.1.0", "0.1.1"} {
		old := regReq(fmt.Sprintf("02:00:00:00:00:7%d", i))
		old.AgentRelease = r
		ans := p.Register(old)
		if ans.Accepted || ans.Reason != agentproto.ReasonAgentTooOld || ans.RequiredRelease != "0.1.2" {
			t.Fatalf("%s agent: %+v", r, ans)
		}
	}
	cur := regReq("02:00:00:00:00:72") // no watchdog_floor_mib: the 0.1.0 shape
	cur.AgentRelease = agentproto.Release
	if ans := p.Register(cur); !ans.Accepted || ans.Release != agentproto.Release {
		t.Fatalf("0.1.2 agent with a 0.1.0-shaped body: %+v", ans)
	}
}
