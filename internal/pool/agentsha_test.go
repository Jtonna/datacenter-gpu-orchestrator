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

// The registration answer names the sha256 of the agent binary Potluck
// serves for the agent's os and architecture, so an agent of the right
// release but another build fetches it (agent contract 5.4).
func TestRegisterAnswerNamesTheServedAgentSHA(t *testing.T) {
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
	p.callBack = func(context.Context, string) error { return nil }
	req := regReq("02:00:00:00:00:51")
	req.AgentRelease = agentproto.Release
	ans := p.Register(req)
	if !ans.Accepted || ans.AgentSHA256 == "" || ans.AgentSHA256 != rel.Agents()["linux-amd64"].SHA256 {
		t.Fatalf("%+v", ans)
	}
	// No agent for that os and architecture: nothing named.
	req = regReq("02:00:00:00:00:52")
	req.AgentRelease, req.Arch = agentproto.Release, "arm64"
	if ans := p.Register(req); ans.Accepted && ans.AgentSHA256 != "" {
		t.Fatalf("%+v", ans)
	}
}
