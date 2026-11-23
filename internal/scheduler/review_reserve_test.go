package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
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
	h.c.putLearned(&learnedNeed{TotalMiB: 10000, key: needKeyOf(p), profile: "m", placement: []core.NodeID{"a", "b"},
		split: []float64{0.5, 0.5}, Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: 4500, PeakMiB: 4600}, {ID: "b", Fraction: 0.5, MiB: 4500, PeakMiB: 4600}}})
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
			h.c.putLearned(&learnedNeed{TotalMiB: a + b, key: needKeyOf(p), profile: "m", placement: []core.NodeID{"a", "b"},
				split: []float64{0.5, 0.5}, Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: a}, {ID: "b", Fraction: 0.5, MiB: b}}})
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
//     shared node, default share + tight margin <= default available;
//   - what a request then does agrees with the gate: with agents it starts
//     exactly when the plan does; in fleet membership a launched instance's
//     rows fit on the figures its pre-flight left (review SB2), including
//     when the pre-flight reports less than the view.
//
// The table covers 1, 2 and 3 nodes (forced over the first nodes, or
// max_pool for an automatic profile) against a measurement stored on 2
// (review SB1, SS4), in fleet membership, fleet membership with a lower
// pre-flight, and agents membership, each with nothing before, right after
// an unload on a whose figure the view still shows (FB1), the same with
// another profile idle on b (sixth review SS1, SS2), and with another
// profile loaded and idle on a (the owner's case): see loadBefore and
// beforeRule. A third of the situations (34560 of 120960) store measurements on two or three device counts with an
// untrusted row among them (third review, TN5): "counts-untrusted" has a
// trusted 1-device row and an untrusted 3-device row below it (TB1),
// "counts-between" trusted rows on 1 and 3 devices and an untrusted one on
// 2 (TS1). For those, the placer's need on the placement's device count is
// checked against bounds computed here from the rows (the need rule's I2
// to I4), not from needAt.
func TestReview_GeneralRule(t *testing.T) {
	type tight struct {
		fill, margin int
		uses         bool
	}
	// Under the race detector the table runs one situation in 11 (the
	// harness drives the core on one goroutine, so the detector finds
	// nothing in it; the sample only keeps the race run short). 11 shares no
	// factor with any dimension's size; that alone does not guarantee every
	// value is reached, so the test checks that every value of every
	// dimension ran. The plain pass runs them all.
	every := 1
	if raceOn {
		every = 11
	}
	evictRefused = map[string]int{}
	evictChecked, evictSeen = map[string]int{}, map[string]int{}
	seen := map[string]bool{}
	n := 0
	for _, need := range []int{8000, 16000, 20000, 23000, 26000, 30000, 40000} { // 40000: never fits three nodes
		for _, cBudget := range []int{12400, 9000} {
			for _, avgDrop := range []int{0, 2500} {
				for _, tg := range []tight{{8, 300, true}, {64, 100, true}, {8, 1000, false}, {0, 300, true}, {8, 0, true}, {0, 0, false}} {
					for _, ctx := range []int{4096, 65536} {
						for _, shape := range []string{"auto", "forced", "skewed", "skewed-peak", "counts-untrusted", "counts-between"} {
							for _, ov := range []int{0, 15500} { // a pre-flight override above the fleet's figure on a
								for _, nodes := range []int{1, 2, 3} {
									for _, mode := range []string{"fleet", "fleet, lower pre-flight", "agents"} {
										for _, before := range []string{"", "unload", "unload-holder", "owner"} {
											n++
											if n%every != 0 {
												continue
											}
											for _, v := range []string{fmt.Sprint("need ", need), fmt.Sprint("c ", cBudget), fmt.Sprint("avg ", avgDrop),
												fmt.Sprint("tight ", tg), "ctx " + fmt.Sprint(ctx), "shape " + shape, fmt.Sprint("ov ", ov),
												fmt.Sprint("nodes ", nodes), "mode " + mode, "before " + before} {
												seen[v] = true
											}
											checkGeneralRule(t, need, cBudget, avgDrop, tg.fill, tg.margin, tg.uses, ctx, shape, ov, nodes, mode, before)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if n != 145152 {
		t.Fatalf("%d situations, the table says 145152", n)
	}
	// 7 needs + 2 + 2 + 6 settings + 2 + 6 shapes + 2 + 3 + 3 modes + 4 befores
	if len(seen) != 37 {
		t.Fatalf("%d dimension values run, want all 37", len(seen))
	}
	// the invariant of the seventh review: no eviction for a request that
	// is then refused, in either membership mode, with either settings
	total := 0
	for k, v := range evictRefused {
		total += v
		t.Errorf("%d situations (%s) evicted for m and then refused it", v, k)
	}
	if total == 0 {
		t.Logf("situations followed on: %v; of them with an eviction for m: %v", evictChecked, evictSeen)
	}
}

func checkGeneralRule(t *testing.T, need, cBudget, avgDrop, fill, margin int, uses bool, ctx int, shape string, ov, nodes int, mode, before string) {
	t.Helper()
	name := func() string {
		return fmt.Sprintf("need %d, c %d, average -%d, fill %d, margin %d, uses %v, ctx %d, %s, override %d, %d nodes, %s, before %q",
			need, cBudget, avgDrop, fill, margin, uses, ctx, shape, ov, nodes, mode, before)
	}
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, cBudget))
	key := mode + ", gate " + fmt.Sprint(fill > 0 || margin > 0 || uses)
	if mode == "agents" {
		h.cfg.Defaults.Membership, h.cfg.Defaults.WorkerProbeInterval = config.MembershipAgents, 0
		h.c.agents = true
	}
	if mode == "fleet, lower pre-flight" { // every pre-flight reports 1500 MiB less than the view
		for _, nd := range h.view.Nodes() {
			h.prober.free[nd.Addr] = nd.FreeMiB - 1500
		}
	}
	if avgDrop > 0 {
		for _, id := range []core.NodeID{"a", "b", "c"} {
			h.view.modify(id, func(n *core.Node) { n.FreeAvgMiB, n.FreeAvgKnown = n.FreeMiB-avgDrop, true })
		}
	}
	p := h.llama("m", need, func(p *core.Profile) {
		p.Ctx = ctx
		if shape != "auto" {
			p.ForceNodes, p.LoadTimeout = []string{"a", "b", "c"}[:nodes], time.Hour
			if nodes > 1 {
				p.TensorSplit = []int{1, 1, 1}[:nodes]
			}
		}
	})
	if shape == "auto" {
		h.cfg.Defaults.MaxPool = nodes
	}
	type row struct {
		devices, total int
		trusted        bool
	}
	var rows []row
	switch shape {
	case "counts-untrusted":
		rows = []row{{1, need + 500, true}, {3, need - 1000, false}}
	case "counts-between":
		rows = []row{{1, need + 200, true}, {3, need + 3000, true}, {2, need + 2500, false}}
	}
	h.c.llamaBuild, h.c.servedBuild = "aaaaaaa1", "aaaaaaa1"
	for _, r := range rows {
		l := &learnedNeed{TotalMiB: r.total, key: needKeyOf(p), profile: "m", devices: r.devices, at: h.clk.Now()}
		if !r.trusted { // restored, of another llama build
			l.restored, l.build, l.release = true, "ccccccc3", "0.1.4"
		}
		h.c.putLearned(l)
	}
	if strings.HasPrefix(shape, "skewed") { // a stored measurement 60/40 over a and b
		a := need * 6 / 10
		l := &learnedNeed{TotalMiB: need, key: needKeyOf(p), profile: "m", placement: []core.NodeID{"a", "b"},
			split: []float64{0.5, 0.5}, Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: a}, {ID: "b", Fraction: 0.5, MiB: need - a}}}
		if shape == "skewed-peak" { // peaks at the load's own figures (B1)
			l.Nodes[0].PeakMiB, l.Nodes[1].PeakMiB = l.Nodes[0].MiB, l.Nodes[1].MiB
		}
		h.c.putLearned(l)
	}
	h.viewChanged()
	orig := map[core.NodeID]int{}
	for _, nd := range h.view.Nodes() {
		orig[nd.ID] = nd.FreeMiB
	}
	if before != "" {
		loadBefore(t, h, mode, before, fill, margin, uses)
	}
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
	// the need on each device count, against bounds from the rows
	if len(rows) > 0 {
		info, _ := h.c.need(p)
		pd := h.c.cfg.Defaults.DeviceOverheadMiB + info.DeviceMiB
		for k := 1; k < len(req.ByCount); k++ {
			bound := need // a configured needs_mib is a floor
			for _, r := range rows {
				switch {
				case r.devices <= k:
					bound = max(bound, r.total+(k-r.devices)*pd)
				case r.trusted:
					bound = max(bound, r.total-(r.devices-k)*pd)
				}
			}
			if got := req.ByCount[k].Need.WithMargin(); got < bound {
				t.Errorf("%s: the need on %d devices is %d, below %d from the rows %v", name(), k, got, bound, rows)
			}
		}
	}
	pl, perr := placer.Place(req, h.c.freeCandidates(p))
	if perr != nil {
		if r.Action == PlanStart {
			t.Errorf("%s: the plan starts but the placer finds nothing", name())
		}
		beforeRule(t, h, p, r, before, name)
		afterwards(t, h, agrees(t, h, r, mode, name), orig, key, name)
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
		if r.Action == PlanStart && h.c.gateOn() && tr.Shared && dr.WillTakeMiB+tr.MarginMiB > dr.AvailableMiB {
			t.Errorf("%s: admitted, but node %s's default check fails beyond the margin difference: %d + %d > %d",
				name(), tr.Node, dr.WillTakeMiB, tr.MarginMiB, dr.AvailableMiB)
		}
	}
	beforeRule(t, h, p, r, before, name)
	// what a request does (every launch also checks the gate: checkGated)
	w := agrees(t, h, r, mode, name)
	defer afterwards(t, h, w, orig, key, name)
	in := h.c.insts["m"]
	switch {
	case mode == "agents":
		if started := in != nil; started != (r.Action == PlanStart) {
			t.Errorf("%s: plan %s (%q) but the request started %v", name(), r.Action, r.Reason, started)
		}
	case in != nil && in.phase != phPreflight && h.c.gateOn():
		h.c.fitting = in
		rows, ok := h.c.memoryRows(p, in.placement, freed)
		h.c.fitting = nil
		if node, by := rowsShort(rows); ok && by > 0 {
			t.Errorf("%s: launched, but node %s is short by %d MiB on the figures its pre-flight left", name(), node, by)
		}
	}
}

// agrees sends a request for "m" and checks it against the plan: a plan that
// rejects is a request refused at once, and a request refused at once had a
// plan that rejects (in fleet membership a plan that starts may still be
// refused by the pre-flight in the same step); one that queues is not
// answered (the live finding after 0.1.6: never fits must be refused, and
// what may fit later must wait).
func agrees(t *testing.T, h *harness, r PlanReport, mode string, name func() string) *waiter {
	t.Helper()
	// nothing of ours is loaded in these situations: a plan held back by
	// the reservation rule would wait for nothing, unless a pre-flight's
	// figure stands in for the view's for a while (the override dimension)
	overridden := h.c.overridden("a") || h.c.overridden("b") || h.c.overridden("c") || h.c.justReleased("a")
	if len(h.c.insts) == 0 && !overridden && r.Action == PlanQueue && strings.HasPrefix(r.Reason, "memory reservation") {
		t.Errorf("%s: plan queue on the reservation rule with nothing loaded (%s): it can never fit and must be refused", name(), r.Reason)
	}
	w := h.request("m")
	a, answered := h.peek(w)
	refused := answered && a.err != nil
	switch {
	case r.Action == PlanReject && !refused:
		t.Errorf("%s: plan reject (%s) but the request was not refused at once", name(), r.Reason)
	case refused && r.Action != PlanReject && !(mode != "agents" && r.Action == PlanStart):
		t.Errorf("%s: plan %s (%s) but the request was refused: %v", name(), r.Action, r.Reason, a.err)
	case r.Action == PlanQueue && answered:
		t.Errorf("%s: plan queue but the request was answered", name())
	}
	return w
}

// evictRefused counts, by mode and settings, the situations where something
// was evicted for m and m was then refused; evictChecked the situations
// followed through. TestReview_GeneralRule requires no such situation.
var evictRefused, evictChecked, evictSeen map[string]int

// afterwards follows m's request on: the stops and settles run, then the
// view catches up (every node nothing holds shows its figure from before
// the situation) and the windows end. No eviction may have been started for
// a request that is then refused (seventh review AB1, AS1).
func afterwards(t *testing.T, h *harness, w *waiter, orig map[core.NodeID]int, key string, name func() string) {
	t.Helper()
	for range 4 { // each round: stops and settles run, then the view catches up
		h.advance(10 * time.Second)
		for id, free := range orig {
			if ns := h.c.nodes[id]; ns == nil || ns.holder == "" {
				h.view.modify(id, func(n *core.Node) { n.FreeMiB = free })
			}
		}
		h.viewChanged()
	}
	h.advance(40 * time.Second)
	h.viewChanged()
	evicted := 0
	for _, e := range h.of(history.TEviction) {
		if fmt.Sprint(e.Data["for_profile"]) == "m" {
			evicted++
		}
	}
	a, ok := h.peek(w)
	if evictChecked != nil {
		evictChecked[key]++
		if evicted > 0 {
			evictSeen[key]++
		}
	}
	if evicted > 0 && ok && a.err != nil {
		if evictRefused != nil {
			evictRefused[key]++
		}
		t.Errorf("%s: evicted %d for m, then refused it: %v", name(), evicted, a.err)
	}
}

// loadBefore runs what happened on node a before m is asked for (fifth
// review): "unload" loads "x" (3000 MiB) on a, shows it in the view as 3500
// MiB used, unloads it and lets the settle end with the view still showing
// it (FB1, the benchmark pattern); "owner" leaves x loaded and idle there,
// past min_hold (the owner's case). The tight settings are on while x
// loads, as they would be live.
func loadBefore(t *testing.T, h *harness, mode, before string, fill, margin int, uses bool) {
	t.Helper()
	d := &h.c.cfg.Defaults
	d.FillReserveKiBPerToken, d.MarginMeasuredMiB, d.PlacementUsesReserved = fill, margin, uses
	h.llama("x", 3000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.viewChanged()
	if mode == "agents" {
		h.release(h.agentReady("x"))
	} else {
		h.release(h.ready("x"))
	}
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 3500 })
	h.viewChanged()
	if before == "unload-holder" { // y idle on b, past min_hold (SS1)
		h.llama("y", 2000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
		h.viewChanged()
		if mode == "agents" {
			h.release(h.agentReady("y"))
		} else {
			h.release(h.ready("y"))
		}
		h.view.modify("b", func(n *core.Node) { n.FreeMiB -= 2500 })
		h.advance(time.Minute)
	}
	switch before {
	case "unload", "unload-holder":
		h.step(evUnload{profile: "x", force: true})
		h.advance(10 * time.Second)
		if h.c.insts["x"] != nil {
			t.Fatalf("x is still there after its unload")
		}
	case "owner":
		h.advance(time.Minute)
		if in := h.c.insts["x"]; in == nil || in.state != stReady {
			t.Fatalf("x is not loaded and idle")
		}
	}
}

// beforeRule checks the plan of m against what happened before: right
// after an unload the reservation rule never refuses with "waiting cannot
// help" (the view may still show the unloaded model); with x loaded on a
// forced node, a forced m that is short even with x stopped (counted as
// the refusal counts it) is refused, never queued.
func beforeRule(t *testing.T, h *harness, p *core.Profile, r PlanReport, before string, name func() string) {
	t.Helper()
	switch before {
	case "unload", "unload-holder":
		// no refusal on the lagging figure: not by the gate, not by the
		// owner's case, not before an eviction (SS1)
		for _, s := range []string{"waiting cannot help", "even with", "nothing loaded is stopped", "would refuse the launch"} {
			if r.Action == PlanReject && strings.Contains(r.Reason, s) {
				t.Errorf("%s: refused right after the unload while the view still shows it: %s", name(), r.Reason)
			}
		}
		// an automatic profile is not moved or widened while a node of the
		// placer's first choice was just released (SS2)
		if len(p.ForceNodes) == 0 && r.Action == PlanStart {
			req, _, _ := h.c.request(p)
			first, err := placer.Place(req, h.c.freeCandidates(p))
			var got []core.NodeID
			for _, n := range r.Placement {
				got = append(got, n.ID)
			}
			if err == nil && h.c.anyJustReleased(first) && !slices.Equal(got, placementNodeIDs(first)) {
				t.Errorf("%s: started on %v right after the unload, not on the placer's first choice %v", name(), got, placementNodeIDs(first))
			}
		}
	case "owner":
		if len(p.ForceNodes) == 0 || r.Action == PlanReject || !h.c.gateOn() {
			return
		}
		var pl core.Placement
		for _, f := range p.ForceNodes {
			nd, _ := h.c.node(core.NodeID(f))
			pl.Nodes = append(pl.Nodes, core.PlacedNode{ID: nd.ID, Addr: nd.Addr})
		}
		pl.TensorSplit = p.TensorSplit
		rows, ok := h.c.memoryRows(p, pl, map[string]bool{"m": true, "x": true})
		if node, by := rowsShort(rows); ok && by > 0 && freeOrHeldBy(h, pl, "x") {
			t.Errorf("%s: plan %s (%s) but node %s is short by %d MiB even with x stopped: it must be refused", name(), r.Action, r.Reason, node, by)
		}
	}
}

// freeOrHeldBy: every node of pl is free, or held by holder alone (no claim,
// no settle, no pause): nothing else there can change.
func freeOrHeldBy(h *harness, pl core.Placement, holder string) bool {
	for _, pn := range pl.Nodes {
		if h.c.isFree(pn.ID) {
			continue
		}
		ns := h.c.nodes[pn.ID]
		if ns == nil || ns.holder != holder || ns.claim != "" || ns.settle != setNone || h.c.noNewLoad(pn.ID) {
			return false
		}
	}
	return true
}
