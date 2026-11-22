package scheduler

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// Full exclusion: the local node and one remote node in exclude_nodes. These
// tests pin today's behaviour (no change of behaviour comes with them).

// (a) Excluding the local node removes it as a place for model layers only:
// a llama profile placed on other nodes still starts its host process
// (llama-server) as a child of the scheduler, with RPC devices alone and the
// host's CUDA hidden.
func TestFullExclusion_LlamaHostStillStartsLocally(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000), ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"host-1", "a"}
	h.llama("m", 6000)
	h.ready("m")
	h.wantNodes("m", "b")
	argv := h.child("m").argv
	at := func(flag string) string {
		i := slices.Index(argv, flag)
		if i < 0 || i+1 >= len(argv) {
			return ""
		}
		return argv[i+1]
	}
	if at("--rpc") != "192.0.2.4:50052" || at("--device") != "RPC0" {
		t.Fatalf("argv %v", argv)
	}
	if env := instance.ChildEnvOverrides(*h.inst("m").prof, h.inst("m").placement); len(env) != 1 || env[0] != "CUDA_VISIBLE_DEVICES=" {
		t.Fatalf("env %v", env)
	}
	// What b alone cannot hold is refused, naming both excluded nodes.
	h.llama("big", 20000)
	e := h.failed(h.request("big"), http.StatusServiceUnavailable, ReasonDoesNotFit)
	if !strings.Contains(e.Message, "nodes excluded by config and not counted: a, host-1") {
		t.Fatalf("error = %+v", e)
	}
}

// (e) exclude_nodes applies from the next decision after a configuration
// change: a model already loaded on the node keeps running and serving (also
// a forced one); nothing new is placed there once it is gone, and a forced
// profile is then refused.
func TestFullExclusion_LoadedInstanceKeepsRunning(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.release(h.ready("f"))
	h.wantNodes("f", "a")

	nc := cloneCfg(h.cfg)
	nc.ExcludeNodes = []string{"a"}
	nc.Revision = 2
	h.step(evConfig{cfg: nc})
	h.step(evTick{})
	if s := h.nodeStatus("a"); !s.Excluded || s.Eligible || s.Holder != "f" {
		t.Fatalf("a: %+v", s)
	}
	h.wantState("f", "ready")
	if r := h.c.dryPlan("f"); r.Action != PlanServe {
		t.Fatalf("plan = %+v", r)
	}
	h.release(h.granted(h.request("f"))) // still served
	ttl := h.c.cfg.EffectiveTTL(h.inst("f").prof)
	h.advance(ttl - time.Minute)
	h.wantState("f", "ready") // not stopped by the exclusion
	if len(h.child("f").stops) != 0 {
		t.Fatalf("stopped: %v", h.child("f").stops)
	}

	// It goes as any idle model does (its TTL); then the forced profile is
	// refused for good.
	h.advance(2 * time.Minute)
	h.wantState("f", "absent")
	if r := h.c.dryPlan("f"); r.Action != PlanReject || !strings.Contains(r.Reason, "node a is excluded by config") {
		t.Fatalf("plan = %+v", r)
	}
	e := h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonNodeExcluded)
	if e.RetryAfter != 0 {
		t.Fatalf("error = %+v", e)
	}
}

// (d) A learned need is kept per profile, whatever exclude_nodes says: the
// exclusion of the node it was measured on drops neither the learned need
// nor its stored row, while running nor at a restart.
func TestFullExclusion_LearnedNeedKept(t *testing.T) {
	st := openNeedStore(t)
	h := measureF(t, st) // measured on node a
	nc := cloneCfg(h.c.cfg)
	nc.ExcludeNodes = []string{"a", "host-1"}
	h.step(evConfig{cfg: nc})
	flushNeeds(h, st)
	if _, ok := h.c.learned["f"]; !ok {
		t.Fatal("the learned need was dropped")
	}
	if _, ok := storedNeeds(t, st)["f"]; !ok {
		t.Fatal("the stored row was removed")
	}
	h2 := restart(t, st, func(h *harness) { h.cfg.ExcludeNodes = []string{"a", "host-1"} })
	if _, ok := h2.c.statusDoc().LearnedNeeds["f"]; !ok {
		t.Fatal("not restored at a restart")
	}
}
