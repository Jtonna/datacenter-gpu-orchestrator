package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// A node paused (or become unplaced) after it was claimed for a waiting
// request, while the victims are still stopping, breaks the claim: the request
// is planned again and the load does not go to that node. Before, step 5
// checked eligibility and tiers only, so the paused node still got the load.
func TestClaimPause_PauseAfterClaimBreaksTheClaim(t *testing.T) {
	for _, why := range []string{UnplacedPaused, UnplacedProgramsNotReady} {
		t.Run(why, func(t *testing.T) {
			h := newHarness(t).nodes(ps5("a", 2, 13000))
			h.cfg.Defaults.Membership = config.MembershipAgents
			h.cfg.Defaults.WorkerProbeInterval = 0
			h.c.agents = true
			h.autoExit = false
			h.llama("qwen", 3000)
			h.llama("other", 11000)
			w := h.request("qwen")
			h.launched(WorkerResult{LaunchIDs: map[core.NodeID]string{"a": "l-a"}})
			h.healthy("qwen")
			h.release(h.granted(w))
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = 10000 })
			h.viewChanged()
			h.advance(31 * time.Second) // past min_hold

			wo := h.request("other")
			h.wantState("qwen", "stopping")
			h.wantClaim("other", "a")
			h.pending(wo)

			// The node is paused while qwen is still stopping.
			if why == UnplacedPaused {
				h.view.modify("a", func(n *core.Node) { n.Paused = true })
			} else {
				h.view.setUnplaced("a", why)
			}
			h.viewChanged()
			h.noClaim("other")
			h.pending(wo)

			// qwen exits and the node settles: other is not started on a.
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = 13000 })
			h.exitChild(h.inst("qwen").iid, instanceExit(0))
			h.step(evTick{})
			h.advance(5 * time.Second)
			h.pending(wo)
			h.noClaim("other")
			h.wantState("other", "absent")
			if len(h.wlaunches) != 1 {
				t.Fatalf("%d worker launches, want qwen's only", len(h.wlaunches))
			}

			// Unpaused: the request is planned again and starts on a.
			if why == UnplacedPaused {
				h.view.modify("a", func(n *core.Node) { n.Paused = false })
			} else {
				h.view.setUnplaced("a", "")
			}
			h.viewChanged()
			h.advance(time.Second)
			h.wantState("other", "starting")
			h.pending(wo)
		})
	}
}
