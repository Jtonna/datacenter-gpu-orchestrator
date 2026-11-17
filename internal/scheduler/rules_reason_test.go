package scheduler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// Why a request cannot be placed must be said precisely in /api/plan,
// /api/status and the eventual queue_timeout 503 (not just "does not fit").

func (h *harness) waitReasonOf(profile string) string {
	h.t.Helper()
	for _, e := range h.c.statusDoc().Queue {
		if e.Profile == profile {
			return e.WaitReason
		}
	}
	h.t.Fatalf("no queue entry for %s", profile)
	return ""
}

func TestReason_QuarantinedNodesThatWouldFit(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Defaults.QueueTimeout = 5 * time.Minute
	h.llama("m", 8000)
	h.step(evTick{})
	for i := 0; i < 8; i++ {
		h.c.quarantine("a", errBoom) // 10 minutes
	}
	r := h.c.dryPlan("m")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "quarantined until") || strings.Contains(r.Reason, "does not fit") {
		t.Fatalf("plan = %+v", r)
	}
	w := h.request("m")
	if got := h.waitReasonOf("m"); !strings.Contains(got, "quarantined until") {
		t.Fatalf("status wait reason = %q", got)
	}
	h.advance(5 * time.Minute)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonNodesQuarantined)
	if !strings.Contains(e.Message, "quarantined until") {
		t.Fatalf("message = %q", e.Message)
	}
}

func TestReason_NodeWithoutAddress(t *testing.T) {
	a := ps5("a", 2, 15000)
	a.Addr = ""
	// b has an address but too little free memory: the only room is on a.
	h := newHarness(t).nodes(a, ps5("b", 4, 3000))
	h.cfg.Defaults.QueueTimeout = 5 * time.Minute
	h.llama("m", 8000)
	h.step(evTick{})
	r := h.c.dryPlan("m")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "node a has no known address") {
		t.Fatalf("plan = %+v", r)
	}
	w := h.request("m")
	h.advance(5 * time.Minute)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonNodeNoAddress)
	if !strings.Contains(e.Message, "node a has no known address") {
		t.Fatalf("message = %q", e.Message)
	}
}

// One PS5 quarantined and the other without an address (the hardware test).
func TestReason_MixedQuarantinedAndNoAddress(t *testing.T) {
	b := ps5("b", 4, 15000)
	b.Addr = ""
	h := newHarness(t).nodes(ps5("a", 2, 15000), b, ps5("c", 6, 3000))
	h.llama("m", 8000, func(p *core.Profile) { p.AllowTiers = []int{1} })
	h.step(evTick{})
	h.c.quarantine("a", errBoom)
	r := h.c.dryPlan("m")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "quarantined until") || !strings.Contains(r.Reason, "node b has no known address") {
		t.Fatalf("plan = %+v", r)
	}
}

// A forced profile whose node is absent still queues (it may come online);
// only the reason text is specific.
func TestReason_ForcedNodeNotInFleetStillQueues(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a", "ghost"} })
	h.step(evTick{})
	r := h.c.dryPlan("f")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "forced node ghost is not in the fleet view") {
		t.Fatalf("plan = %+v", r)
	}
	w := h.request("f")
	h.pending(w)
	if got := h.waitReasonOf("f"); !strings.Contains(got, "forced node ghost is not in the fleet view") {
		t.Fatalf("status wait reason = %q", got)
	}
	h.advance(600 * time.Second)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedUnavailable)
	if !strings.Contains(e.Message, "forced node ghost is not in the fleet view") {
		t.Fatalf("message = %q", e.Message)
	}
}

// A plain shortage keeps the placer's typed reason.
func TestReason_PlainNoFitKeepsPlacerReason(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 12000)
	h.llama("other", 12000)
	h.ready("big")
	h.step(evTick{})
	r := h.c.dryPlan("other")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "does not fit") {
		t.Fatalf("plan = %+v", r)
	}
}
