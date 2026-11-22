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
// loaded on them, is refused when the request arrives and each time its queue
// entry is looked at again (forcedNeverFits): the answer does not depend on
// min_hold, on a request in flight on the holder, or on a claim. A forced node
// that is absent, offline, unplaced (re-registering and the like) or has an
// unknown memory figure keeps the request waiting.

// neverFitsAgents is fitFirstAgents without the wait past min_hold: qwen
// (3000 MiB) is ready on shared-memory node a (10000 MiB usable while it is
// loaded, 13000 MiB empty) and dev, forced onto a, needs devNeed MiB.
func neverFitsAgents(t *testing.T, devNeed int) *harness {
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
	h.wantState("qwen", "ready")
	return h
}

// neverFitsRefused: dev is refused at once with the "even with nothing loaded"
// message, nothing is stopped, no entry stays, and the dry-run plan gives the
// same answer with the same words.
func (h *harness) neverFitsRefused(w *waiter) *Error {
	h.t.Helper()
	e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	for _, want := range []string{"node a", "14691 MiB", "13000 MiB", "even with nothing loaded", "would_not_fit"} {
		if !strings.Contains(e.Message, want) {
			h.t.Fatalf("message %q lacks %q", e.Message, want)
		}
	}
	h.noEntry("dev")
	if rep := h.c.dryPlan("dev"); rep.Action != PlanReject || rep.Reason != e.Message {
		h.t.Fatalf("plan %s %q, want reject %q", rep.Action, rep.Reason, e.Message)
	}
	return e
}

// The holder is younger than min_hold: before, plan() was never reached and
// the request waited for queue_timeout ("forced nodes in use").
func TestNeverFits_HolderYoungerThanMinHold(t *testing.T) {
	h := neverFitsAgents(t, 14691)
	if rep := h.c.dryPlan("dev"); rep.Action != PlanReject {
		t.Fatalf("plan before the request: %+v", rep)
	}
	h.neverFitsRefused(h.request("dev"))
	h.nothingStopped("qwen")
	h.mustLog("forced profile cannot fit even with its nodes empty; refused before any eviction", "profile", "dev", "at", "arrival")
}

// The holder has a request in flight (past min_hold): before, the plan made qwen
// a draining victim only after drain_after; now nothing waits and nothing drains.
func TestNeverFits_HolderWithRequestInFlight(t *testing.T) {
	h := neverFitsAgents(t, 14691)
	g := h.granted(h.request("qwen"))
	h.advance(31 * time.Second)
	h.neverFitsRefused(h.request("dev"))
	h.nothingStopped("qwen")
	if in := h.inst("qwen"); in.inFlight != 1 {
		t.Fatalf("qwen in flight %d", in.inFlight)
	}
	h.release(g)
}

// Another entry has claimed the forced node (qwen is being stopped for it):
// dev is refused at once instead of waiting behind that claim.
func TestNeverFits_NodeClaimedByAnotherEntry(t *testing.T) {
	h := neverFitsAgents(t, 14691)
	h.llama("mid", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.autoExit = false
	h.advance(31 * time.Second)
	wm := h.request("mid")
	h.wantClaim("mid", "a")
	h.wantState("qwen", "stopping")
	h.neverFitsRefused(h.request("dev"))
	h.pending(wm)
	h.wantClaim("mid", "a")
}

// A request that waited (the forced node was re-registering, so unplaced) is
// refused when its entry is looked at again once the node is back.
func TestNeverFits_RefusedWhileWaiting(t *testing.T) {
	h := neverFitsAgents(t, 14691)
	h.view.setUnplaced("a", UnplacedProgramsNotReady)
	h.viewChanged()
	w := h.request("dev")
	h.pending(w)
	if rep := h.c.dryPlan("dev"); rep.Action == PlanReject {
		t.Fatalf("plan while a is unplaced: %+v", rep)
	}
	h.view.setUnplaced("a", "")
	h.viewChanged()
	h.neverFitsRefused(w)
	h.nothingStopped("qwen")
	h.mustLog("forced profile cannot fit even with its nodes empty; refused before any eviction", "profile", "dev", "at", "queue")
}

// Waits, not refusals: a forced node absent from the view, offline, or with an
// unknown memory figure. The other forced node holds qwen (young), so the
// check would refuse if it ran.
func TestNeverFits_UnknownOrAbsentNodesWait(t *testing.T) {
	cases := map[string]func(h *harness){
		"absent": func(h *harness) {}, // c is never in the view
		"offline": func(h *harness) {
			h.view.set(ps5("a", 2, 10000), ps5("c", 3, 13000))
			h.view.modify("c", func(n *core.Node) { n.Online = false })
		},
		"unknown": func(h *harness) {
			h.view.set(ps5("a", 2, 10000), ps5("c", 3, 13000))
			h.view.modify("c", func(n *core.Node) { n.FreeUnknown, n.FreeMiB = true, 0 })
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := neverFitsAgents(t, 14691)
			h.llama("dev", 30000, func(p *core.Profile) { p.ForceNodes = []string{"a", "c"} })
			setup(h)
			h.viewChanged()
			w := h.request("dev")
			h.pending(w)
			if rep := h.c.dryPlan("dev"); rep.Action == PlanReject && strings.Contains(rep.Reason, "even with nothing loaded") {
				t.Fatalf("plan %+v", rep)
			}
			h.wantState("qwen", "ready")
		})
	}
}

// A forced profile that fits once the holder is gone still waits for min_hold
// and is then placed by eviction, as before.
func TestNeverFits_FitsAfterEvictionStillWaits(t *testing.T) {
	h := neverFitsAgents(t, 12500)
	w := h.request("dev")
	h.pending(w)
	h.wantState("qwen", "ready")
	if rep := h.c.dryPlan("dev"); rep.Action != PlanQueue {
		t.Fatalf("plan %+v", rep)
	}
	h.advance(31 * time.Second)
	h.wantClaim("dev", "a")
}

// Fleet membership with forced_fit_check reject: the same refusal at arrival
// while the holder is younger than min_hold.
func TestNeverFits_FleetRejectHolderYoung(t *testing.T) {
	h := newHarness(t).nodes(dedicatedPS5("a", 2, 13000))
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("qwen", 3000, func(p *core.Profile) { p.TTL = ptrDur(time.Hour) })
	h.llama("dev", 14691, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.release(h.ready("qwen"))
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 10000 })
	h.viewChanged()
	e := h.failed(h.request("dev"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	if !strings.Contains(e.Message, "even with nothing loaded") {
		t.Fatalf("message %q", e.Message)
	}
	h.wantState("qwen", "ready")
	if rep := h.c.dryPlan("dev"); rep.Action != PlanReject || rep.Reason != e.Message {
		t.Fatalf("plan %+v", rep)
	}
}
