package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// TN1 (third review): the launch check asserts the gate PASSED for the
// profile and node set in this step, not only that it was called.
func TestReview3_TN1_ALaunchAfterARefusedGatePanics(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.llama("m", 8000)
	h.request("m")
	in := h.inst("m")
	h.c.steps++
	h.c.gated = gateMark{profile: "m", nodes: placementIDs(in.placement), step: h.c.steps, passed: false}
	defer func() {
		if recover() == nil {
			t.Fatal("a launch after a refused gate did not panic")
		}
	}()
	h.c.launch(in)
}

// TN1: the gate records whether it passed.
func TestReview3_TN1_TheGateRecordsItsAnswer(t *testing.T) {
	h := oneAndThree(t) // the tight settings: the gate is on
	p := h.c.cfg.Profiles["three"]
	h.request("three")
	in := h.inst("three")
	if in == nil {
		t.Fatal("not started")
	}
	if why := h.c.reserveGate(p, in.placement, nil); why != "" || !h.c.gated.passed {
		t.Fatalf("a fitting placement: gate %q, passed %v", why, h.c.gated.passed)
	}
	h.nodes(psNode("a", 2, 3000), psNode("b", 4, 3000), psNode("c", 6, 3000)) // the nodes shrink
	h.viewChanged()
	if why := h.c.reserveGate(p, in.placement, nil); why == "" || h.c.gated.passed {
		t.Fatalf("a placement that does not fit: gate %q, passed %v", why, h.c.gated.passed)
	}
}

// TN1: with agents, a placement made only of the local node launches
// through launch, which now checks the gate as well.
func TestReview3_TN1_AnAgentsLocalOnlyLaunchIsChecked(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000))
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.llama("m", 2000, func(p *core.Profile) { p.ForceNodes = []string{"host-1"} })
	h.viewChanged()
	h.request("m") // gated in place, launched in the same step: no panic
	in := h.inst("m")
	if in == nil {
		t.Fatal("not started")
	}
	h.c.steps++
	defer func() {
		if recover() == nil {
			t.Fatal("an agents local-only start without the gate did not panic")
		}
	}()
	h.c.startInstance(in.prof, in.placement, nil, nil)
}
