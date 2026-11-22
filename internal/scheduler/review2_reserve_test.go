package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The second review of the reservation rule (potluck-reserve-review.md,
// "Second pass"): each finding's failing case as a test.

// skewedFleet is the SB2 probe: two PS5s with a 9600 budget each, profile m
// forced over a and b at 1:1 with a stored split of a/b MiB, a margin of
// 300 on a measured need and no fill reserve; each pre-flight reports
// preflightFree.
func skewedFleet(t *testing.T, a, b, preflightFree int) *harness {
	h := newHarness(t).nodes(psNode("a", 2, 9600), psNode("b", 4, 9600))
	d := &h.cfg.Defaults
	d.PlacementUsesReserved, d.MarginMeasuredMiB = true, 300
	p := h.llama("m", 0, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{1, 1}, time.Hour
	})
	h.needs["m"] = NeedInfo{Need: placer.Estimated(a + b), FileBytes: 1 << 30}
	h.c.learned[shapeOf(p)] = &learnedNeed{TotalMiB: a + b, key: needKeyOf(p), profile: "m", placement: []core.NodeID{"a", "b"},
		split: []float64{0.5, 0.5}, Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: a}, {ID: "b", Fraction: 0.5, MiB: b}}}
	if preflightFree > 0 {
		h.prober.free["192.0.2.2:50052"] = preflightFree
		h.prober.free["192.0.2.4:50052"] = preflightFree
	}
	h.viewChanged()
	return h
}

// SB2: in fleet membership the exact check after the pre-flight runs the
// gate too. The reviewer's probe: a 9000/7000 split, 9600 budgets, margin
// 300; both pre-flights report 9400 (budget 8400). The gate passes on the
// view's figures (9000 + 300 <= 9600) but on the pre-flight's figures node
// a is 900 MiB short: the instance is returned, not launched.
func TestReview2_SB2_FleetPreflightRunsTheGate(t *testing.T) {
	h := skewedFleet(t, 9000, 7000, 9400)
	if r := h.c.dryPlan("m"); r.Action != PlanStart {
		t.Fatalf("on the view's figures the plan starts: %s %q", r.Action, r.Reason)
	}
	h.request("m")
	if in := h.c.insts["m"]; in != nil {
		t.Fatalf("launched in phase %v; node a is short by 900 MiB on the pre-flight figures", in.phase)
	}
	if h.entry("m") == nil {
		t.Fatal("the request should wait in its entry")
	}
	// The same split with room on the pre-flight figures launches.
	h = skewedFleet(t, 9000, 7000, 10600)
	h.request("m")
	h.wantState("m", "starting")
	if in := h.inst("m"); in.phase == phPreflight {
		t.Fatalf("still in the pre-flight")
	}
}

// SB2: every launch is checked against the gate (checkGated, on in every
// test of this package): a launch on a node set the gate was not asked
// about in the same step panics. This is what fails when a new launch path
// skips the gate.
func TestReview2_SB2_ALaunchWithoutTheGatePanics(t *testing.T) {
	if !checkGateBeforeLaunch {
		t.Fatal("the launch check is off in tests")
	}
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.llama("m", 8000)
	h.request("m")
	in := h.inst("m")
	h.c.steps++ // a later step: the gate call of the placement no longer counts
	defer func() {
		if recover() == nil {
			t.Fatal("a launch without the gate did not panic")
		}
	}()
	h.c.launch(in)
}

// SB2: in fleet membership what launches agrees with the gate on the
// pre-flight's figures: a launched instance's rows fit on the figures it
// launched with, and a returned one's plan no longer starts.
func TestReview2_SB2_FleetLaunchAgreesWithTheGate(t *testing.T) {
	for _, split := range [][2]int{{8000, 8000}, {9000, 7000}, {8900, 7100}, {9300, 6700}} {
		for _, free := range []int{0, 9400, 9900, 10600} {
			h := skewedFleet(t, split[0], split[1], free)
			h.request("m")
			in := h.c.insts["m"]
			if in == nil {
				if r := h.c.dryPlan("m"); r.Action == PlanStart {
					t.Errorf("split %v pre-flight %d: returned, but the plan now starts", split, free)
				}
				continue
			}
			h.c.fitting = in
			rows, ok := h.c.memoryRows(in.prof, in.placement, map[string]bool{"m": true})
			h.c.fitting = nil
			if node, by := rowsShort(rows); !ok || by > 0 {
				t.Errorf("split %v pre-flight %d: launched, but node %s is short by %d on its figures", split, free, node, by)
			}
		}
	}
}

// SS3: what an eviction frees on a node is what the holder's own load
// reported there, bounded by its reserved figure; never memory another
// process took on the node since the holder was placed. The case: the
// holder's log reports 8000 on a, another process takes 3000 more; 2400 is
// free: 10400 with nothing of ours loaded, not 13400.
func TestReview2_SS3_FreedIsTheHoldersOwnFigure(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	reserveOnSettings(h)
	h.llama("m", 8000, func(p *core.Profile) { p.Ctx = 65536 })
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 7000, 700, 300)) // 8000 by the log
	h.healthy("m")
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 8000 + 3000 })
	h.viewChanged()
	n, _ := h.c.node("a")
	if free, _, ok := h.c.emptyFree("a", n); !ok || free != 2400+8000 {
		t.Fatalf("empty free %d (ok %v), want 2400 + the holder's 8000", free, ok)
	}
	// While its log has not reported, its reserved figure bounds what the
	// node gave up: need 8000 + fill 512.
	setLoad(h, "m")
	if free, _, ok := h.c.emptyFree("a", n); !ok || free != 2400+8512 {
		t.Fatalf("no report: empty free %d (ok %v), want 2400 + its reserved 8512", free, ok)
	}
}
