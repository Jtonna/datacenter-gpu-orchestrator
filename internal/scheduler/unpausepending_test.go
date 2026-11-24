package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Finding A: a plan that queues only because the node it needs was just
// unpaused (its agent is told with the next heartbeat) says so, and that it
// starts within about 2 s. The action stays queue; placement is unchanged.
func TestPlan_UnpausePendingSaysStartsSoon(t *testing.T) {
	h := newHarness(t)
	a, b := ps5("ps5-a", 1, 9000), ps5("ps5-b", 2, 13000)
	a.Paused = true                         // paused: takes no load
	b.Paused, b.UnpausePending = true, true // unpaused, the agent not told yet
	h.nodes(a, b)
	h.llama("m", 10000) // fits ps5-b only
	h.llama("f", 6000, func(p *core.Profile) { p.ForceNodes = []string{"ps5-b"} })
	h.viewChanged()

	r := h.c.dryPlan("m")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "unpause pending") || !strings.Contains(r.Reason, "ps5-b") ||
		!strings.Contains(r.Reason, "starts within about 2 s") {
		t.Fatalf("auto: %s %q", r.Action, r.Reason)
	}
	r = h.c.dryPlan("f")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "unpause pending") || !strings.Contains(r.Reason, "starts within about 2 s") {
		t.Fatalf("forced: %s %q", r.Action, r.Reason)
	}

	// Only a plain pause in the way: no such promise.
	b.UnpausePending = false
	h.nodes(a, b)
	h.viewChanged()
	if r := h.c.dryPlan("m"); strings.Contains(r.Reason, "unpause pending") || strings.Contains(r.Reason, "2 s") {
		t.Fatalf("paused: %s %q", r.Action, r.Reason)
	}

	// Once the agent is told the plan starts, on the same node.
	b.Paused = false
	h.nodes(a, b)
	h.viewChanged()
	if r := h.c.dryPlan("m"); r.Action != PlanStart || len(r.Placement) != 1 || r.Placement[0].ID != "ps5-b" {
		t.Fatalf("after: %s %+v", r.Action, r.Placement)
	}
}
