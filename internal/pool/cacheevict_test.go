package pool

import (
	"context"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
)

func TestEvictFileCacheAsksOnlyCapableAgents(t *testing.T) {
	r := newRig(t)
	var asked []string
	capable := r.agent(1, func(a *fakeagent.Agent) {
		a.Capabilities = []string{agentproto.CapFileCacheEvict}
		a.CacheEvict = func(req agentproto.CacheEvictRequest) (int, any) {
			asked = append(asked, req.File)
			return 200, agentproto.CacheEvictAnswer{File: req.File, FileBytes: 1000, CachedBefore: 800, CachedAfter: 3, DurationS: 0.02}
		}
	})
	busy := r.agent(2, func(a *fakeagent.Agent) {
		a.Capabilities = []string{agentproto.CapFileCacheEvict}
		a.CacheEvict = func(req agentproto.CacheEvictRequest) (int, any) {
			return 409, agentproto.LaunchRefusal{Reason: agentproto.ReasonBusy, Message: "launch ln-9 exists on this node"}
		}
	})
	old := r.agent(3) // an agent of an older release: no capabilities
	r.waitNode(capable.ID)
	r.waitNode(busy.ID)
	r.waitNode(old.ID)
	ctx := context.Background()

	got := r.p.EvictFileCache(ctx, capable.ID, "sub/m.gguf")
	if got.Status != 200 || got.Answer == nil || got.Answer.CachedAfter != 3 || got.Name != "node-1" || !got.Capable || len(asked) != 1 || asked[0] != "sub/m.gguf" {
		t.Fatalf("%+v asked=%v", got, asked)
	}
	got = r.p.EvictFileCache(ctx, busy.ID, "m.gguf")
	if got.Status != 409 || got.Reason != agentproto.ReasonBusy || got.Answer != nil || got.Message == "" {
		t.Fatalf("%+v", got)
	}
	got = r.p.EvictFileCache(ctx, old.ID, "m.gguf")
	if got.Capable || got.Status != 0 || got.Reason != agentproto.ReasonUnsupported {
		t.Fatalf("%+v", got)
	}
	for _, c := range old.Calls() {
		if c == "POST /cache/evict" {
			t.Fatal("an agent without the capability was asked")
		}
	}
	if got := r.p.EvictFileCache(ctx, "02:00:00:00:00:99", "m.gguf"); got.Reason != "unknown_node" {
		t.Fatalf("%+v", got)
	}
	// a lost node is not asked
	capable.Close()
	r.clk.advance(LostAfter + time.Second)
	r.p.CheckLost()
	if got := r.p.EvictFileCache(ctx, capable.ID, "m.gguf"); got.Reason != "node_unavailable" || len(asked) != 1 {
		t.Fatalf("%+v asked=%v", got, asked)
	}
}
