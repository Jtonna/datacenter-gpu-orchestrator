package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// A worker launch the agent refuses as "paused" is not a failed load: the node
// takes no new load for a moment (pausedRefusalHold), the request stays
// queued and is planned again, and the profile is not backed off.
func TestPausedLaunchRefusalPlansAgain(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m")
	h.launched(WorkerResult{Node: "a", Reason: agentproto.ReasonPaused, Message: "the node is paused"})
	h.pending(w)
	h.wantState("m", "absent")
	if _, on := h.c.backoffActive("m"); on {
		t.Fatal("the profile was backed off")
	}
	if len(h.wstops) != 1 {
		t.Fatalf("the other launches are stopped: %+v", h.wstops)
	}
	h.mustLog("worker launch refused: the node's agent still believes it is paused; planning again", "node", core.NodeID("a"))
	n := len(h.wlaunches)
	h.advance(pausedRefusalHold - time.Second)
	if len(h.wlaunches) != n {
		t.Fatal("tried again before the hold ended")
	}
	h.advance(2 * time.Second)
	if len(h.wlaunches) != n+1 {
		t.Fatalf("not tried again after the hold: %d launches", len(h.wlaunches))
	}
	h.launched(readyIDs())
	h.healthy("m")
	h.granted(w)
}

// While an unpause has not reached the agent the pool lists the node as
// paused: the request waits, and is placed once the node is listed unpaused.
func TestUnpausePendingNodeIsNotPlaced(t *testing.T) {
	h := agentsHarness(t)
	h.view.modify("a", func(n *core.Node) { n.Paused = true })
	h.viewChanged()
	w := h.request("m")
	h.pending(w)
	if len(h.wlaunches) != 0 {
		t.Fatal("placed on a paused node")
	}
	h.view.modify("a", func(n *core.Node) { n.Paused = false })
	h.viewChanged()
	if len(h.wlaunches) != 1 {
		t.Fatalf("%d launches after the unpause reached the agent", len(h.wlaunches))
	}
	h.launched(readyIDs())
	h.healthy("m")
	h.granted(w)
}
