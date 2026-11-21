package scheduler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// A forced profile that can never fit its forced nodes, not even with nothing
// of ours loaded on them, is refused before any victim is stopped (agent
// contract 11.2, CR-9). A forced profile that fits once the victims are gone
// is placed as before: evict, wait for the node, check again on the real
// figures, start.

// fitFirstAgents is agents membership with one shared-memory node "a" whose
// agent reports 13000 MiB usable while it is empty, and an automatic profile
// "qwen" (3000 MiB) loaded and ready on it, idle and past its hold. While qwen
// is loaded the agent reports 10000 MiB usable. Profile "dev" is forced onto a
// and needs devNeed MiB.
func fitFirstAgents(t *testing.T, devNeed int) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 13000))
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.llama("qwen", 3000)
	h.llama("dev", devNeed, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	w := h.request("qwen")
	h.launched(WorkerResult{LaunchIDs: map[core.NodeID]string{"a": "l-a"}})
	h.healthy("qwen")
	h.release(h.granted(w))
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 10000 })
	h.viewChanged()
	h.advance(31 * time.Second) // past min_hold
	h.wantState("qwen", "ready")
	return h
}

// nothingStopped: qwen is still ready and no stop was sent to its child or its
// worker launch.
func (h *harness) nothingStopped(profile string) {
	h.t.Helper()
	h.wantState(profile, "ready")
	if in := h.inst(profile); in.victimOf != "" {
		h.t.Fatalf("%s was made a victim of %s", profile, in.victimOf)
	}
	if st := h.child(profile).stops; len(st) != 0 {
		h.t.Fatalf("%s's child was stopped: %v", profile, st)
	}
	if len(h.wstops) != 0 {
		h.t.Fatalf("worker launches stopped: %+v", h.wstops)
	}
}

// 1 and 5: dev needs 14691 MiB; a would have 10000 + 3000 = 13000 MiB with
// nothing loaded. Refused at once (no time passes), qwen untouched, and the
// message names the node, the need and the "even with nothing loaded" figure.
func TestFitFirst_AgentsRefusedBeforeEviction(t *testing.T) {
	h := fitFirstAgents(t, 14691)
	w := h.request("dev")
	e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	h.nothingStopped("qwen")
	h.noEntry("dev")
	h.wantState("dev", "absent")
	if len(h.wlaunches) != 1 { // qwen's own launch only
		t.Fatalf("%d worker launches, want 1", len(h.wlaunches))
	}
	for _, want := range []string{"node a", "14691 MiB", "13000 MiB", "even with nothing loaded", "qwen", "would_not_fit"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	if e.NeedMiB != 14691 || e.CapacityMiB != 13000 {
		t.Errorf("need %d capacity %d, want 14691 and 13000", e.NeedMiB, e.CapacityMiB)
	}
	h.mustLog("forced profile cannot fit even with its nodes empty; refused before any eviction", "profile", "dev")
	if rep := h.c.dryPlan("dev"); rep.Action != PlanReject || !strings.Contains(rep.Reason, "even with nothing loaded") {
		t.Fatalf("plan %+v", rep)
	}
	// Later requests are refused the same way; qwen keeps serving.
	h.failed(h.request("dev"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	h.nothingStopped("qwen")
	h.granted(h.request("qwen"))
}

// 2: dev needs 12500 MiB, which fits a once qwen is gone (13000 MiB): qwen is
// evicted, a settles, the check on the real figures passes and dev starts.
func TestFitFirst_AgentsFitsOnlyAfterEvictionStillEvicts(t *testing.T) {
	h := fitFirstAgents(t, 12500)
	h.autoExit = false
	w := h.request("dev")
	h.wantState("qwen", "stopping")
	h.wantClaim("dev", "a")
	h.pending(w)
	// qwen's memory comes back when it exits.
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 13000 })
	h.exitChild(h.inst("qwen").iid, instanceExit(0))
	h.step(evTick{})
	h.wantState("qwen", "absent")
	h.advance(3 * time.Second) // settle
	h.wantState("dev", "starting")
	if l := h.lastLaunch(); l.spec.Profile != "dev" {
		t.Fatalf("last launch is for %s", l.spec.Profile)
	}
	h.pending(w)
}

// 2, the final check stays: the "nothing loaded" figure is an estimate. Here a
// gives back less than qwen was expected to hold, so dev is refused after the
// eviction, on the real figures, as before.
func TestFitFirst_AgentsCheckAfterEvictionStays(t *testing.T) {
	h := fitFirstAgents(t, 12500)
	h.autoExit = false
	w := h.request("dev")
	h.wantState("qwen", "stopping")
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 12000 })
	h.exitChild(h.inst("qwen").iid, instanceExit(0))
	h.step(evTick{})
	h.advance(3 * time.Second)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	if strings.Contains(e.Message, "even with nothing loaded") || !strings.Contains(e.Message, "12000") {
		t.Fatalf("message %q", e.Message)
	}
}

// 3: dev is forced onto a (memory figure unknown, nothing on it) and b (qwen
// loaded). The unknown figure refuses dev, and that happens before qwen is
// stopped. (As in TestAgents_ForcedNodeWithUnknownFigureIsRefused, a keeps a
// free figure above min_share, so the placer offers it.)
func TestFitFirst_AgentsUnknownFigureRefusedBeforeEviction(t *testing.T) {
	h := unknownFigureHarness(t, 13000)
	e := h.failed(h.request("dev"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	if !strings.Contains(e.Message, "node a") || !strings.Contains(e.Message, "memory figure is unknown") {
		t.Fatalf("message %q", e.Message)
	}
	h.nothingStopped("qwen")
	h.noEntry("dev")
}

// 3, as the pool reports it: an unknown figure comes with FreeMiB 0, so the
// placer does not offer a (budget under min_share) and no plan is made. dev
// queues; qwen is not stopped.
func TestFitFirst_AgentsUnknownFigureAsReportedEvictsNothing(t *testing.T) {
	h := unknownFigureHarness(t, 0)
	w := h.request("dev")
	h.pending(w)
	h.noClaim("dev")
	h.advance(time.Minute)
	h.pending(w)
	h.nothingStopped("qwen")
}

func unknownFigureHarness(t *testing.T, aFree int) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 13000), laptop("b", 7000))
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.llama("qwen", 3000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
	h.llama("dev", 6000, func(p *core.Profile) { p.ForceNodes, p.TensorSplit = []string{"a", "b"}, []int{1, 1} })
	w := h.request("qwen")
	h.launched(WorkerResult{LaunchIDs: map[core.NodeID]string{"b": "l-b"}})
	h.healthy("qwen")
	h.release(h.granted(w))
	h.view.modify("a", func(n *core.Node) { n.FreeUnknown, n.FreeMiB = true, aFree })
	h.view.modify("b", func(n *core.Node) { n.FreeMiB = 4000 })
	h.viewChanged()
	h.advance(31 * time.Second)
	h.wantState("qwen", "ready")
	return h
}

// fitFirstFleet is fleet membership with one dedicated-VRAM node "a": 13000
// MiB free while empty (fleet and pre-flight), 10000 MiB while "qwen" (3000
// MiB) is loaded. The pre-flight override of qwen's load has ended (10
// minutes), so the scheduler holds the fleet's figure. "dev" is forced onto a.
func fitFirstFleet(t *testing.T, mode string, devNeed int) *harness {
	t.Helper()
	h := newHarness(t).nodes(dedicatedPS5("a", 2, 13000))
	h.cfg.Defaults.ForcedFitCheck = mode
	h.llama("qwen", 3000, func(p *core.Profile) { p.TTL = ptrDur(time.Hour) })
	h.llama("dev", devNeed, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.release(h.ready("qwen"))
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 10000 })
	h.viewChanged()
	h.advance(11 * time.Minute) // past min_hold and the pre-flight override
	if ns := h.c.nodes["a"]; ns.ovUntil.After(h.clk.Now()) {
		t.Fatalf("pre-flight override still in force")
	}
	h.wantState("qwen", "ready")
	return h
}

// 4: forced_fit_check reject in fleet membership refuses before eviction.
func TestFitFirst_FleetRejectRefusedBeforeEviction(t *testing.T) {
	h := fitFirstFleet(t, config.ForcedFitReject, 14691)
	e := h.failed(h.request("dev"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	h.wantState("qwen", "ready")
	if st := h.child("qwen").stops; len(st) != 0 {
		t.Fatalf("qwen's child was stopped: %v", st)
	}
	h.noEntry("dev")
	for _, want := range []string{"a", "14691 MiB", "13000 MiB", "even with nothing loaded", "forced_fit_check is reject"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	if rep := h.c.dryPlan("dev"); rep.Action != PlanReject || !strings.Contains(rep.Reason, "even with nothing loaded") {
		t.Fatalf("plan %+v", rep)
	}
}

// 4: with a pre-flight override still in force on the held node, that figure
// was taken while the node was empty, so it is used as is (qwen's share is not
// added on top of it).
func TestFitFirst_FleetRejectUsesTheEmptyPreflightFigure(t *testing.T) {
	h := newHarness(t).nodes(dedicatedPS5("a", 2, 13000))
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("qwen", 3000)
	h.llama("dev", 14691, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.release(h.ready("qwen")) // pre-flight said 13000 free
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 10000 })
	h.viewChanged()
	h.advance(31 * time.Second)
	e := h.failed(h.request("dev"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	h.wantState("qwen", "ready")
	if !strings.Contains(e.Message, "13000 MiB") || strings.Contains(e.Message, "16000 MiB") {
		t.Fatalf("message %q", e.Message)
	}
}

// 4: with forced_fit_check warn (or off) the fleet path is unchanged: qwen is
// evicted and dev starts (the warning comes at the pre-flight, as before).
func TestFitFirst_FleetWarnAndOffUnchanged(t *testing.T) {
	for _, mode := range []string{config.ForcedFitWarn, config.ForcedFitOff} {
		t.Run(mode, func(t *testing.T) {
			h := fitFirstFleet(t, mode, 14691)
			w := h.request("dev")
			h.wantState("qwen", "absent") // stopped at once (idle), auto exit
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = 13000 })
			h.advance(3 * time.Second) // settle
			h.wantState("dev", "starting")
			h.pending(w)
			if n := len(h.logs.find("forced profile may not fit")); (mode == config.ForcedFitWarn) != (n > 0) {
				t.Fatalf("%s: %d warnings", mode, n)
			}
		})
	}
}

// 4: reject mode, dev fits once qwen is gone: evicted and started as before.
func TestFitFirst_FleetRejectFitsAfterEviction(t *testing.T) {
	h := fitFirstFleet(t, config.ForcedFitReject, 12500)
	w := h.request("dev")
	h.wantState("qwen", "absent")
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 13000 })
	h.advance(3 * time.Second)
	h.wantState("dev", "starting")
	h.pending(w)
}

// Automatic placement: an instance on a node that takes no new load (paused,
// or listed with an unplaced reason) is not evicted, because the node may not
// take the new load. Before, the victim was stopped and the new profile was
// then started on the paused node.
func TestFitFirst_NoEvictionForANodeThatTakesNoNewLoad(t *testing.T) {
	for _, why := range []string{UnplacedPaused, UnplacedProgramsNotReady} {
		t.Run(why, func(t *testing.T) {
			h := newHarness(t).nodes(ps5("a", 2, 13000))
			h.cfg.Defaults.Membership = config.MembershipAgents
			h.cfg.Defaults.WorkerProbeInterval = 0
			h.c.agents = true
			h.llama("qwen", 3000)
			h.llama("other", 4000)
			w := h.request("qwen")
			h.launched(WorkerResult{LaunchIDs: map[core.NodeID]string{"a": "l-a"}})
			h.healthy("qwen")
			h.release(h.granted(w))
			if why == UnplacedPaused {
				h.view.modify("a", func(n *core.Node) { n.Paused = true })
			} else {
				h.view.setUnplaced("a", why)
			}
			h.viewChanged()
			h.advance(31 * time.Second)
			wo := h.request("other")
			h.advance(5 * time.Second)
			h.pending(wo)
			h.noClaim("other")
			h.nothingStopped("qwen")
			if len(h.wlaunches) != 1 {
				t.Fatalf("%d worker launches, want qwen's only", len(h.wlaunches))
			}
		})
	}
}
