package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
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

// S1: the figure "with nothing of ours loaded" counts what the holder really
// gave up (or its reserved figure while that is unknown), never its margin
// or unused fill reserve, and is never above the node's total. The
// reviewer's case: 13400 empty, the holder uses 8000, fill 512, margin 300:
// 13400, not 14212.
func TestReview_S1_EmptyFreeNeverAboveTheTotal(t *testing.T) {
	for _, on := range []bool{false, true} {
		h := newHarness(t).nodes(psNode("a", 2, 12400))
		if on {
			reserveOnSettings(h)
		}
		h.llama("m", 8000, func(p *core.Profile) { p.Ctx = 65536 })
		h.request("m")
		setLoad(h, "m", rpcDev(addrA, 7000, 700, 300))
		h.healthy("m")
		h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 8000 })
		h.viewChanged()
		n, _ := h.c.node("a")
		free, _, ok := h.c.emptyFree("a", n)
		if !ok || free > n.TotalMiB || free != 13400 {
			t.Fatalf("rule on %v: empty free %d (ok %v), want 13400, at most the total %d", on, free, ok, n.TotalMiB)
		}
	}
}

// agreementCase is one situation of the plan/decision agreement table: the
// nodes, the profile asked for, and what else is loaded.
type agreementCase struct {
	name  string
	setup func(t *testing.T, h *harness) // loads neighbours, sets averages
	prof  func(h *harness)               // adds profile "m"
}

// S2, S3, S4: the plan's memory fits and the decision are one: whenever the
// plan says start or evict, its memory section fits, and whenever the memory
// section does not fit, the plan does not start; a real request then does
// what the plan said. Forced, automatic, a loading neighbour, a tight
// margin, a low average and a skewed measured split.
func TestReview_S4_PlanFitsAgreesWithTheDecision(t *testing.T) {
	low := func(t *testing.T, h *harness) { // average 8400 usable on every node
		for _, id := range []string{"a", "b", "c"} {
			h.view.modify(core.NodeID(id), func(n *core.Node) { n.FreeAvgMiB, n.FreeAvgKnown = 9400, true })
		}
	}
	none := func(t *testing.T, h *harness) {}
	loading := func(t *testing.T, h *harness) { // another profile loading on a
		h.llama("other", 9000, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
		h.request("other")
		h.wantState("other", "starting")
	}
	skewed := func(need, a, b int) func(h *harness) { // a stored split of a/b over a and b
		return func(h *harness) {
			p := h.llama("m", 0, func(p *core.Profile) { p.ForceNodes, p.TensorSplit = []string{"a", "b"}, []int{1, 1} })
			h.needs["m"] = NeedInfo{Need: placer.Estimated(need), FileBytes: 1 << 30}
			h.c.learned[shapeOf(p)] = &learnedNeed{TotalMiB: a + b, key: needKeyOf(p), profile: "m", placement: []core.NodeID{"a", "b"},
				split: []float64{0.5, 0.5}, Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: a}, {ID: "b", Fraction: 0.5, MiB: b}}}
		}
	}
	auto := func(need int) func(h *harness) {
		return func(h *harness) { h.llama("m", need, func(p *core.Profile) { p.Ctx = 65536 }) }
	}
	forced := func(need int, nodes ...string) func(h *harness) {
		return func(h *harness) {
			h.llama("m", need, func(p *core.Profile) { p.Ctx, p.ForceNodes, p.LoadTimeout = 65536, nodes, time.Hour })
		}
	}
	cases := []agreementCase{
		{"automatic fits", none, auto(20000)},
		{"automatic, tight margin fits", none, auto(23000)},
		{"automatic, tight margin does not fit", none, auto(23400)},
		{"automatic, low average", low, auto(20000)},
		{"forced fits", none, forced(10000, "a")},
		{"forced, low average (S2)", low, forced(10000, "a")},
		{"forced, too big", none, forced(12500, "a")},
		{"automatic, loading neighbour", loading, auto(20000)},
		{"forced, skewed measured split fits", none, skewed(16000, 8500, 7500)},
		{"forced, skewed measured split over a node (S3)", none, skewed(16000, 12300, 3700)},
	}
	for _, tc := range cases {
		for _, on := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: ", rule on", false: ", defaults"}[on], func(t *testing.T) {
				h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, 12400))
				if on {
					reserveOnSettings(h)
				}
				tc.setup(t, h)
				tc.prof(h)
				h.viewChanged()
				r := h.c.dryPlan("m")
				if r.Memory == nil {
					t.Fatalf("no memory section: %+v", r)
				}
				starts := r.Action == PlanStart || r.Action == PlanEvict
				if on && starts != r.Memory.Fits {
					t.Fatalf("plan %s (%q) but memory fits %v: %+v", r.Action, r.Reason, r.Memory.Fits, r.Memory)
				}
				if r.Memory.Decides != on {
					t.Fatalf("decides %v with the rule %v", r.Memory.Decides, on)
				}
				h.request("m")
				if started := h.c.insts["m"] != nil; started != (r.Action == PlanStart) {
					t.Fatalf("plan %s (%q) but the request started %v", r.Action, r.Reason, started)
				}
			})
		}
	}
}

// S5: right after what is placed on a node changes (an unload, an
// eviction), the average still holds samples from before; it is not used
// until a whole window has passed, so the re-fit is not refused on the old
// figures. After the window it counts again.
func TestReview_S5_AverageIsResetWhenThePlacementChanges(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.cfg.Defaults.PlacementUsesReserved = true
	h.llama("v", 8000)
	h.llama("m", 10000)
	h.request("v")
	h.healthy("v")
	h.step(evUnload{profile: "v", force: true})
	h.advance(10 * time.Second)                                                          // settled; well inside the 30 s window
	h.view.modify("a", func(n *core.Node) { n.FreeAvgMiB, n.FreeAvgKnown = 9400, true }) // the samples with v loaded
	h.viewChanged()
	if r := h.c.dryPlan("m"); r.Action != PlanStart {
		t.Fatalf("right after the unload: plan %s %q, want start", r.Action, r.Reason)
	}
	h.advance(h.cfg.Defaults.FreeAverageWindow)
	h.viewChanged()
	if r := h.c.dryPlan("m"); r.Action == PlanStart {
		t.Fatalf("a window later the average counts again: plan %s", r.Action)
	}
}

// cycle loads m again on node a, lets the node give up gave MiB, and ends
// the load normally (an unload) or abnormally (the worker exits).
func cycle(h *harness, gave int, normal bool) {
	h.t.Helper()
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 7000, 700, 300)) // measured 8000
	h.healthy("m")
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= gave })
	h.viewChanged()
	if normal {
		h.step(evUnload{profile: "m", force: true})
	} else {
		in := h.inst("m")
		h.exitChild(in.iid, instance.ExitStatus{Code: 1})
		h.step(evViewChanged{})
	}
	h.advance(time.Minute)
	h.view.modify("a", func(n *core.Node) { n.FreeMiB += gave })
	h.viewChanged()
}

// S6: a peak is the peak of the latest complete load-to-unload cycle that
// ended normally: one spike holds only until a normal cycle without it
// ends. A cycle that ends abnormally keeps the peak before it.
func TestReview_S6_APeakFollowsTheLatestNormalCycle(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.cfg.Defaults.FillReserveKiBPerToken = 8
	h.llama("m", 8000, func(p *core.Profile) { p.Ctx = 65536 }) // need 8000 + fill 512
	cycle(h, 9500, true)                                        // a spike
	if n := planNode(t, h, "m"); n.WillTakeMiB != 9500 || n.Basis != BasisPeak {
		t.Fatalf("after the spike: %+v, want the peak 9500", n)
	}
	cycle(h, 8900, false) // ends abnormally: the spike stays
	if n := planNode(t, h, "m"); n.WillTakeMiB != 9500 {
		t.Fatalf("after an abnormal cycle: %+v, want the peak 9500 kept", n)
	}
	cycle(h, 8700, true) // a normal cycle without the spike replaces it
	if n := planNode(t, h, "m"); n.WillTakeMiB != 8700 || n.Basis != BasisPeak {
		t.Fatalf("after a normal cycle: %+v, want its peak 8700", n)
	}
	cycle(h, 8100, true) // below need + fill: need + fill counts
	if n := planNode(t, h, "m"); n.WillTakeMiB != 8512 || n.Basis != BasisMeasured {
		t.Fatalf("after a low cycle: %+v, want 8512", n)
	}
}

// N1: with the defaults nothing of the rule writes to the store: a node
// giving up more while loaded raises no peak and queues no write.
func TestReview_N1_DefaultsWriteNoPeaks(t *testing.T) {
	st := openNeedStore(t)
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.cfg.Configured = true
	h.c.restoreNeeds(st)
	h.llama("m", 8000)
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 7000, 700, 300))
	h.healthy("m")
	h.c.takeNeedOps() // the learned need itself is stored, as before
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 9000 })
	h.viewChanged()
	if ops := h.c.takeNeedOps(); len(ops) != 0 {
		t.Fatalf("defaults queued %d store writes for a peak", len(ops))
	}
	if got := h.c.learnedNeeds()["m"]; got.PeakMiB != 0 {
		t.Fatalf("defaults kept a peak: %+v", got)
	}
}

// The general rule of the review, as a property over a table of situations:
// with tight settings,
//   - every node's reserved figure (will_take) is at least the default
//     rules' figure for the same placement plus the fill reserve (an upper
//     bound on use; a stored peak never lowers it), and
//     every node's available figure at most the default one (a lower bound
//     on free); a node's budget is never above the default one;
//   - nothing is admitted that the default check of the measured need would
//     refuse once only the margin difference is taken off: for every
//     shared node, default share + tight margin <= default available.
func TestReview_GeneralRule(t *testing.T) {
	type tight struct {
		fill, margin int
		uses         bool
	}
	n := 0
	for _, need := range []int{8000, 16000, 20000, 23000, 26000, 30000} {
		for _, cBudget := range []int{12400, 9000} {
			for _, avgDrop := range []int{0, 2500} {
				for _, tg := range []tight{{8, 300, true}, {64, 100, true}, {8, 1000, false}, {0, 300, true}, {8, 0, true}} {
					for _, ctx := range []int{4096, 65536} {
						for _, shape := range []string{"auto", "forced", "skewed", "skewed-peak"} {
							for _, ov := range []int{0, 15500} { // a pre-flight override above the fleet's figure on a
								n++
								checkGeneralRule(t, need, cBudget, avgDrop, tg.fill, tg.margin, tg.uses, ctx, shape, ov)
							}
						}
					}
				}
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d situations", n)
	}
}

func checkGeneralRule(t *testing.T, need, cBudget, avgDrop, fill, margin int, uses bool, ctx int, shape string, ov int) {
	t.Helper()
	name := func() string {
		return fmt.Sprintf("need %d, c %d, average -%d, fill %d, margin %d, uses %v, ctx %d, %s, override %d", need, cBudget, avgDrop, fill, margin, uses, ctx, shape, ov)
	}
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, cBudget))
	if avgDrop > 0 {
		for _, id := range []core.NodeID{"a", "b", "c"} {
			h.view.modify(id, func(n *core.Node) { n.FreeAvgMiB, n.FreeAvgKnown = n.FreeMiB-avgDrop, true })
		}
	}
	p := h.llama("m", need, func(p *core.Profile) {
		p.Ctx = ctx
		if shape != "auto" {
			p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{1, 1}, time.Hour
		}
	})
	if shape != "auto" && shape != "forced" { // a stored measurement 60/40 over a and b
		a := need * 6 / 10
		l := &learnedNeed{TotalMiB: need, key: needKeyOf(p), profile: "m", placement: []core.NodeID{"a", "b"},
			split: []float64{0.5, 0.5}, Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: a}, {ID: "b", Fraction: 0.5, MiB: need - a}}}
		if shape == "skewed-peak" { // peaks at the load's own figures (B1)
			l.Nodes[0].PeakMiB, l.Nodes[1].PeakMiB = l.Nodes[0].MiB, l.Nodes[1].MiB
		}
		h.c.learned[shapeOf(p)] = l
	}
	h.viewChanged()
	if ov > 0 {
		ns := h.c.ns("a")
		ns.ovFree, ns.ovUntil = ov, h.clk.Now().Add(time.Minute)
	}
	set := func(on bool) {
		d := &h.c.cfg.Defaults
		if on {
			d.FillReserveKiBPerToken, d.MarginMeasuredMiB, d.PlacementUsesReserved = fill, margin, uses
		} else {
			d.FillReserveKiBPerToken, d.MarginMeasuredMiB, d.PlacementUsesReserved = 0, 0, false
		}
	}
	// budgets
	for _, id := range []core.NodeID{"a", "b", "c"} {
		nd, _ := h.c.node(id)
		set(false)
		def := h.c.liveBudget(id, nd)
		set(true)
		if tb := h.c.liveBudget(id, nd); tb > def {
			t.Errorf("%s: node %s budget %d with tight settings, %d by default", name(), id, tb, def)
		}
	}
	// the placement the tight settings make (or the one the placer finds)
	set(true)
	r := h.c.dryPlan("m")
	req, _, err := h.c.request(p)
	if err != nil {
		t.Fatalf("%s: %v", name(), err)
	}
	pl, perr := placer.Place(req, h.c.freeCandidates(p))
	if perr != nil {
		if r.Action == PlanStart {
			t.Errorf("%s: the plan starts but the placer finds nothing", name())
		}
		return
	}
	freed := map[string]bool{"m": true}
	tightRows, _ := h.c.memoryRows(p, pl, freed)
	set(false)
	defRows, _ := h.c.memoryRows(p, pl, freed)
	set(true)
	if len(tightRows) != len(defRows) {
		t.Fatalf("%s: rows %d and %d", name(), len(tightRows), len(defRows))
	}
	for i := range tightRows {
		tr, dr := tightRows[i], defRows[i]
		if f := h.c.fillReserveMiB(p); tr.WillTakeMiB < dr.WillTakeMiB+f {
			t.Errorf("%s: node %s will take %d with tight settings, below the default %d + fill reserve %d", name(), tr.Node, tr.WillTakeMiB, dr.WillTakeMiB, f)
		}
		if tr.AvailableMiB > dr.AvailableMiB {
			t.Errorf("%s: node %s available %d with tight settings, %d by default", name(), tr.Node, tr.AvailableMiB, dr.AvailableMiB)
		}
		if r.Action == PlanStart && tr.Shared && dr.WillTakeMiB+tr.MarginMiB > dr.AvailableMiB {
			t.Errorf("%s: admitted, but node %s's default check fails beyond the margin difference: %d + %d > %d",
				name(), tr.Node, dr.WillTakeMiB, tr.MarginMiB, dr.AvailableMiB)
		}
	}
}
