package pool

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// CR-9 review, B6: the release is 0.1.1, and a release 0.1.0 agent against
// it gets the update path of contract 5.4: refused with agent_too_old and
// required_release 0.1.1, never a load. A 0.1.1 agent that sends a 0.1.0-shaped
// body (none of the CR-9 or CR-10 fields) is accepted: every new field is
// optional.
func TestRelease011AndA010Agent(t *testing.T) {
	if agentproto.Release != "0.1.1" {
		t.Fatalf("agentproto.Release = %q, want 0.1.1", agentproto.Release)
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

	old := regReq("02:00:00:00:00:71")
	old.AgentRelease = "0.1.0"
	ans := p.Register(old)
	if ans.Accepted || ans.Reason != agentproto.ReasonAgentTooOld || ans.RequiredRelease != "0.1.1" {
		t.Fatalf("0.1.0 agent: %+v", ans)
	}
	cur := regReq("02:00:00:00:00:72") // no watchdog_floor_mib: the 0.1.0 shape
	cur.AgentRelease = agentproto.Release
	if ans := p.Register(cur); !ans.Accepted || ans.Release != agentproto.Release {
		t.Fatalf("0.1.1 agent with a 0.1.0-shaped body: %+v", ans)
	}
}
