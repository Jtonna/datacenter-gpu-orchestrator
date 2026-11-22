package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The review of the reservation rule (potluck-reserve-review.md): each
// finding's failing case as a test.

// loadAndUnload loads profile m (need needMiB on one PS5 "a" with a 12400
// budget, ctx 65536, fill reserve 8 KiB per token = 512 MiB) and lets the
// load measure 8000 MiB; grew is what the node gives up on top of that while
// it is loaded (0: no growth seen). Then it unloads m and frees the node.
func loadAndUnload(t *testing.T, needMiB, grew int) *harness {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.cfg.Defaults.FillReserveKiBPerToken = 8
	h.llama("m", needMiB, func(p *core.Profile) { p.Ctx = 65536 })
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 7000, 700, 300)) // measured 8000
	h.healthy("m")
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 8000 + grew })
	h.viewChanged()
	h.step(evUnload{profile: "m", force: true})
	h.advance(10 * time.Second)
	h.view.modify("a", func(n *core.Node) { n.FreeMiB += 8000 + grew })
	h.viewChanged()
	return h
}

// planNode is the plan's memory row of the single node.
func planNode(t *testing.T, h *harness, profile string) PlanMemoryNode {
	t.Helper()
	r := h.c.dryPlan(profile)
	if r.Memory == nil || len(r.Memory.Nodes) != 1 {
		t.Fatalf("plan memory %+v", r.Memory)
	}
	return r.Memory.Nodes[0]
}

// B1: a peak equal to the load's own figure (no growth seen) does not
// remove the fill reserve: reserved = max(need + fill, peak).
func TestReview_B1_PeakAtTheLoadFigureKeepsTheReserve(t *testing.T) {
	h := loadAndUnload(t, 8000, 0)
	req, _, err := h.c.request(h.c.cfg.Profiles["m"])
	if err != nil {
		t.Fatal(err)
	}
	if req.Need != placer.Measured(8000) || req.PerNodeReserveMiB != 512 {
		t.Fatalf("request need %+v reserve %d, want 8000 + 512", req.Need, req.PerNodeReserveMiB)
	}
	if n := planNode(t, h, "m"); n.WillTakeMiB != 8512 || n.FillReserveMiB != 512 || n.Basis != BasisMeasured {
		t.Fatalf("plan node %+v, want 8000 + 512 measured", n)
	}
}

// B1: a peak between the need and the need + fill reserve counts as need +
// fill reserve.
func TestReview_B1_PeakBelowNeedPlusReserve(t *testing.T) {
	h := loadAndUnload(t, 8000, 200) // peak 8200 < 8512
	if n := planNode(t, h, "m"); n.WillTakeMiB != 8512 || n.FillReserveMiB != 512 {
		t.Fatalf("plan node %+v, want 8512", n)
	}
	h = loadAndUnload(t, 8000, 900) // peak 8900 > 8512: the peak counts
	if n := planNode(t, h, "m"); n.WillTakeMiB != 8900 || n.Basis != BasisPeak {
		t.Fatalf("plan node %+v, want the peak 8900", n)
	}
	req, _, _ := h.c.request(h.c.cfg.Profiles["m"])
	if got := req.Need.WithMargin() + req.PerNodeReserveMiB; got != 8900 {
		t.Fatalf("request need %+v reserve %d: %d, want the peak 8900", req.Need, req.PerNodeReserveMiB, got)
	}
}

// B2: a configured needs_mib above the peak is never lowered by it.
func TestReview_B2_ConfiguredNeedAboveThePeak(t *testing.T) {
	h := loadAndUnload(t, 11000, 100) // needs_mib 11000, peak 8100
	req, _, _ := h.c.request(h.c.cfg.Profiles["m"])
	if req.Need != placer.Measured(11000) || req.PerNodeReserveMiB != 512 {
		t.Fatalf("request need %+v reserve %d, want 11000 + 512", req.Need, req.PerNodeReserveMiB)
	}
	if n := planNode(t, h, "m"); n.WillTakeMiB != 11512 {
		t.Fatalf("plan node %+v, want 11512", n)
	}
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", BudgetMiB: 12400}}}
	if got := h.c.expectShares(h.c.cfg.Profiles["m"], pl); len(got) != 1 || got[0] < 11512 {
		t.Fatalf("expect %v, want at least 11512", got)
	}
}

// B2: a learned total above the sum of the peaks (the peaks follow a later,
// smaller load) is never lowered by them.
func TestReview_B2_LearnedTotalAboveThePeakSum(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400))
	h.cfg.Defaults.FillReserveKiBPerToken = 8
	p := h.llama("m", 0, func(p *core.Profile) { p.Ctx = 4096 }) // fill 32 a node
	h.needs["m"] = NeedInfo{Need: placer.Estimated(9000), FileBytes: 1 << 30}
	h.c.learned[shapeOf(p)] = &learnedNeed{TotalMiB: 10000, key: needKeyOf(p), profile: "m", placement: []core.NodeID{"a", "b"},
		split: []float64{0.5, 0.5}, Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: 4500, PeakMiB: 4600}, {ID: "b", Fraction: 0.5, MiB: 4500, PeakMiB: 4600}}}
	h.viewChanged()
	req, _, _ := h.c.request(p)
	if req.Need != placer.Measured(10000) || req.PerNodeReserveMiB != 32 {
		t.Fatalf("request need %+v reserve %d, want 10000 + 32 a node", req.Need, req.PerNodeReserveMiB)
	}
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", BudgetMiB: 12400}, {ID: "b", BudgetMiB: 12400}}}
	shares, _ := h.c.reservedShares(p, pl)
	for _, s := range shares {
		if s.Total < 5000+32 {
			t.Fatalf("share %+v, want at least 10000 / 2 + 32", s)
		}
	}
}

// B3: with placement_uses_reserved, a node's budget is never above what the
// old path gives, in either membership mode: not during a pre-flight
// override above the fleet's figure (the Vulkan figure), and not with an
// average above the latest figure. The reviewer's case: fleet figure 9000,
// headroom 1000, pre-flight 11000: 8000 either way.
func TestReview_B3_TheSettingNeverRaisesABudget(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, ov := range []int{0, 7000, 11000} { // no override, below, above the fleet figure
			for _, avg := range []int{0, 6000, 12000} { // no average, below, above
				for _, shared := range []bool{true, false} {
					var h *harness
					n := ps5("a", 2, 9000)
					if !shared {
						n = laptop("a", 9000)
					}
					if avg > 0 {
						n.FreeAvgMiB, n.FreeAvgKnown = avg, true
					}
					if mode == "agents" {
						h = agentsHarness(t)
						h.view.set(n, laptop("b", 7000))
					} else {
						h = newHarness(t).nodes(n)
					}
					h.viewChanged()
					if ov > 0 {
						ns := h.c.ns("a")
						ns.ovFree, ns.ovUntil = ov, h.clk.Now().Add(time.Minute)
					}
					nn, _ := h.c.node("a")
					h.c.cfg.Defaults.PlacementUsesReserved = false
					off := h.c.liveBudget("a", nn)
					h.c.cfg.Defaults.PlacementUsesReserved = true
					on := h.c.liveBudget("a", nn)
					if on > off {
						t.Errorf("%s shared %v override %d average %d: budget %d with the setting, %d without", mode, shared, ov, avg, on, off)
					}
					if mode == "fleet" && shared && ov == 11000 && avg == 0 && on != 8000 {
						t.Errorf("the reviewer's case: %d, want 8000", on)
					}
				}
			}
		}
	}
}
