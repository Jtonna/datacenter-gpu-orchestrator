package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// A node the view lists as not placeable (programs not verified, model source
// unreachable, a launch Potluck does not hold) takes no new load: the request
// waits, nothing is sent to the agent, and the load starts once the reason is
// gone. The reason is in the node's status.
func TestAgents_UnplacedNodeTakesNoLoadAndTheRequestWaits(t *testing.T) {
	for _, why := range []string{UnplacedProgramsNotReady, UnplacedModelSourceUnreachable, UnplacedOrphanLaunch, UnplacedSilent} {
		h := agentsHarness(t)
		h.view.setUnplaced("a", why)
		h.viewChanged()
		w := h.request("m")
		h.pending(w)
		if len(h.wlaunches) != 0 {
			t.Fatalf("%s: a worker launch was sent to a node that is not placeable", why)
		}
		h.wantState("m", "absent")
		if s := h.nodeStatus("a"); s.Unplaced != why || s.Online != true {
			t.Fatalf("%s: status %+v", why, s)
		}
		if s := h.nodeStatus("b"); s.Unplaced != "" {
			t.Fatalf("%s: status of the other node %+v", why, s)
		}
		if _, on := h.c.backoffActive("m"); on {
			t.Fatalf("%s: the profile was backed off", why)
		}
		h.view.setUnplaced("a", "")
		h.viewChanged()
		if len(h.wlaunches) != 1 {
			t.Fatalf("%s: %d launches after the node became placeable, want 1", why, len(h.wlaunches))
		}
		h.launched(readyIDs())
		h.healthy("m")
		h.granted(w)
	}
}

// A node that matches no tier is listed, with its reason, and is outside the
// pool: never eligible and not counted in capacity.
func TestAgents_NoTierMatchNodeIsListedAndOutOfThePool(t *testing.T) {
	h := agentsHarness(t)
	h.llama("big", 9000, func(p *core.Profile) { p.TTL = ptrDur(0) }) // fits on a (15000) only
	h.view.setUnplaced("a", UnplacedNoTierMatch)
	h.viewChanged()
	s := h.nodeStatus("a")
	if s.Unplaced != UnplacedNoTierMatch || s.Eligible {
		t.Fatalf("status %+v", s)
	}
	w := h.request("big")
	if a, ok := h.peek(w); !ok || a.err == nil || a.err.Status != 503 {
		t.Fatalf("a profile that only fits on the node without a tier must be refused, got %+v %v", a, ok)
	}
	if len(h.wlaunches) != 0 {
		t.Fatal("a worker launch was sent to a node without a tier")
	}
}

// The launcher is told when the load is over, so it reads the launches four
// times a second only during the load (agent contract 6.3).
func TestAgents_LauncherIsToldWhenTheLoadIsOver(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m")
	h.launched(readyIDs())
	if len(h.wloaded) != 0 {
		t.Fatal("told before the instance was ready")
	}
	iid := h.inst("m").iid
	h.healthy("m")
	h.granted(w)
	if len(h.wloaded) != 1 || h.wloaded[0] != iid {
		t.Fatalf("loaded %v, want [%d]", h.wloaded, iid)
	}
}
