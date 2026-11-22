package scheduler

import (
	"net/http"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The memory reservation rule (reserve.go).

// reserveOnSettings turns every part of the rule on, as recommended.
func reserveOnSettings(h *harness) {
	d := &h.cfg.Defaults
	d.FillReserveKiBPerToken, d.PlacementUsesReserved, d.MarginMeasuredMiB = 8, true, 300
}

// With the defaults nothing of the rule reaches a decision: no fill reserve
// or configured margin in the placer request, the measured free figure as
// the budget even when the average is lower, and expect_mib as before. (The
// rest of the package's tests run on the defaults and keep their
// expectations unchanged.)
func TestReserve_DefaultsChangeNothing(t *testing.T) {
	n := psNode("a", 2, 12400)
	n.FreeAvgMiB, n.FreeAvgKnown = 5000, true
	h := newHarness(t).nodes(n, psNode("b", 4, 12400))
	p := h.llama("m", 20000, func(p *core.Profile) { p.Ctx = 65536 })
	h.viewChanged()
	req, _, err := h.c.request(p)
	if err != nil {
		t.Fatal(err)
	}
	if req.PerNodeReserveMiB != 0 || req.MarginMiB != 0 || req.MarginPermille != 0 || req.MarginMinMiB != 0 || req.Need != placer.Measured(20000) {
		t.Fatalf("request %+v", req)
	}
	if b := h.c.liveBudget("a", n); b != 12400 {
		t.Fatalf("budget %d, want the measured 12400", b)
	}
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", BudgetMiB: 12400}, {ID: "b", BudgetMiB: 12400}}}
	if got := h.c.expectShares(p, pl); len(got) != 2 || got[0] != 11500 || got[1] != 11500 {
		t.Fatalf("expect %v, want 20000 x 50%% x 1.15 = 11500 each", got)
	}
}

// The reserve arithmetic per node: need share + fill reserve (fill = kib x
// ctx x parallel), split into weights (the file's share) and context; the
// margin by rule.
func TestReserve_Arithmetic(t *testing.T) {
	type want struct{ weights, context, fill, total, margin int }
	cases := []struct {
		name  string
		set   func(h *harness)
		need  NeedInfo
		ctx   int
		par   int
		nodes int
		want  []want
	}{
		{"single node, measured, reserve off", func(h *harness) {}, NeedInfo{Need: placer.Measured(9000), FileBytes: 6000 << 20}, 65536, 1, 1,
			[]want{{6000, 3000, 0, 9000, 0}}},
		{"single node, measured, 8 KiB x 65536", func(h *harness) { h.cfg.Defaults.FillReserveKiBPerToken = 8 },
			NeedInfo{Need: placer.Measured(9000), FileBytes: 6000 << 20}, 65536, 1, 1, []want{{6000, 3000, 512, 9512, 0}}},
		{"parallel counts: 8 KiB x 32768 x 2", func(h *harness) { h.cfg.Defaults.FillReserveKiBPerToken = 8 },
			NeedInfo{Need: placer.Measured(9000), FileBytes: 6000 << 20}, 32768, 2, 1, []want{{6000, 3000, 512, 9512, 0}}},
		{"two nodes, measured, skew rule", func(h *harness) { h.cfg.Defaults.FillReserveKiBPerToken = 4 },
			NeedInfo{Need: placer.Measured(20000), FileBytes: 14000 << 20}, 131072, 1, 2,
			[]want{{7000, 3000, 512, 10512, 1500}, {7000, 3000, 512, 10512, 1500}}},
		{"two nodes, measured, margin_measured_mib 300", func(h *harness) { h.cfg.Defaults.MarginMeasuredMiB = 300 },
			NeedInfo{Need: placer.Measured(20000), FileBytes: 14000 << 20}, 4096, 1, 2,
			[]want{{7000, 3000, 0, 10000, 300}, {7000, 3000, 0, 10000, 300}}},
		// 20000 x 1.05 = 21000 / 2 = 10500 + 600 overhead + 16 compute.
		{"two nodes, estimate, percent 10 at least 1200", func(h *harness) {
			h.cfg.Defaults.MarginEstimatedPercent, h.cfg.Defaults.MarginEstimatedMinMiB = 10, 1200
		}, NeedInfo{Need: placer.Estimated(20000), FileBytes: 14000 << 20, DeviceMiB: 16}, 4096, 1, 2,
			[]want{{7000, 4116, 0, 11116, 1200}, {7000, 4116, 0, 11116, 1200}}},
		{"two nodes, estimate, percent 15", func(h *harness) { h.cfg.Defaults.MarginEstimatedPercent = 15 },
			NeedInfo{Need: placer.Estimated(20000), FileBytes: 14000 << 20, DeviceMiB: 16}, 4096, 1, 2,
			[]want{{7000, 4116, 0, 11116, 1668}, {7000, 4116, 0, 11116, 1668}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400))
			tc.set(h)
			p := h.llama("m", 0, func(p *core.Profile) { p.Ctx, p.Parallel = tc.ctx, tc.par })
			h.needs["m"] = tc.need
			h.viewChanged()
			pl := core.Placement{}
			for _, id := range []core.NodeID{"a", "b"}[:tc.nodes] {
				pl.Nodes = append(pl.Nodes, core.PlacedNode{ID: id, BudgetMiB: 12400})
			}
			got, _ := h.c.reservedShares(p, pl)
			if len(got) != len(tc.want) {
				t.Fatalf("shares %+v", got)
			}
			for i, w := range tc.want {
				g := got[i]
				if g.Weights != w.weights || g.Context != w.context || g.Fill != w.fill || g.Total != w.total || g.Margin != w.margin {
					t.Fatalf("node %d: %+v, want %+v", i, g, w)
				}
				if g.Weights+g.Context+g.Fill != g.Total {
					t.Fatalf("node %d does not add up: %+v", i, g)
				}
			}
		})
	}
}

// The owner's acceptance sentence: before a load the plan states per node
// what a profile that is not loaded will take with its context filled, what
// the node has and what remains, and whether and where it fits. Three equal
// nodes, another model loaded on one of them; the plan's numbers add up and
// its fits matches the placer, with the defaults and with the rule on.
func TestReserve_AcceptancePlanOnThreeNodes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		on      bool
		need    int
		fits    bool
		action  PlanAction
		perNode int // will_take on each placed node
		margin  int
		short   int // shortfall per free node when it does not fit
	}{
		{"defaults, fits", false, 20000, true, PlanStart, 10000, 1500, 0},
		{"defaults, 23000: 11500 x 1.15 = 13225 does not fit 12400", false, 23000, false, PlanQueue, 0, 0, 825},
		{"rule on, 20000 + fill 512 + margin 300", true, 20000, true, PlanStart, 10512, 300, 0},
		{"rule on, 23000: 11500 + 512 + 300 = 12312 fits", true, 23000, true, PlanStart, 12012, 300, 0},
		{"rule on, 23400: 11700 + 512 + 300 = 12512 does not", true, 23400, false, PlanQueue, 0, 0, 112},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, 12400))
			if tc.on {
				reserveOnSettings(h)
			}
			h.llama("other", 9000)
			h.request("other")
			h.healthy("other")
			h.wantNodes("other", "a")
			h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 9000 }) // what the loaded model took
			p := h.llama("m", tc.need, func(p *core.Profile) { p.Ctx = 65536 })
			h.viewChanged()

			r := h.c.dryPlan("m")
			req, _, _ := h.c.request(p)
			_, perr := placer.Place(req, h.c.freeCandidates(p))
			m := r.Memory
			if m == nil {
				t.Fatalf("no memory section: %+v", r)
			}
			if r.Action != tc.action || m.Fits != tc.fits || m.Fits != (perr == nil) {
				t.Fatalf("action %s fits %v placer err %v; want %s %v (reason %q)", r.Action, m.Fits, perr, tc.action, tc.fits, r.Reason)
			}
			if m.PlacementUsesReserved != tc.on {
				t.Fatalf("setting %v", m.PlacementUsesReserved)
			}
			sum := 0
			for _, n := range m.Nodes {
				if n.WeightsMiB+n.ContextMiB+n.FillReserveMiB != n.WillTakeMiB || n.AvailableMiB-n.WillTakeMiB-n.MarginMiB != n.RemainsMiB {
					t.Fatalf("node %s does not add up: %+v", n.Node, n)
				}
				if n.Basis != BasisMeasured || (n.Node != "a" && n.FreeNowMiB != 13400) || n.TotalMiB != 13400 || n.HeadroomMiB != 1000 {
					t.Fatalf("node %s: %+v", n.Node, n)
				}
				sum += n.WillTakeMiB - n.FillReserveMiB
			}
			if tc.fits {
				if len(m.Nodes) != 2 || m.Nodes[0].Node != "b" || m.Nodes[1].Node != "c" || m.Hypothetical || m.ShortfallMiB != 0 {
					t.Fatalf("placed %+v", m)
				}
				for _, n := range m.Nodes {
					if n.WillTakeMiB != tc.perNode || n.MarginMiB != tc.margin || n.AvailableMiB != 12400 || len(n.ReservedBy) != 0 {
						t.Fatalf("node %s: %+v", n.Node, n)
					}
				}
				if sum != tc.need {
					t.Fatalf("the shares add up to %d, want %d", sum, tc.need)
				}
				return
			}
			// Where it would go: the two free nodes, each short by the same amount.
			if !m.Hypothetical || m.ShortNode != "b" || m.ShortfallMiB != tc.short || len(m.Nodes) != 2 || m.Nodes[1].Node != "c" {
				t.Fatalf("not fitting: %+v", m)
			}
			// The node holding the other model lists it.
			var s NodeStatus
			for _, n := range h.c.statusDoc().View.Nodes {
				if n.ID == "a" {
					s = n
				}
			}
			if len(s.Reserved) != 1 || s.Reserved[0].Profile != "other" || s.ReservedMiB != 9000+map[bool]int{true: 32}[tc.on] || s.Reserved[0].State != "ready" {
				t.Fatalf("node a: %+v", s)
			}
		})
	}
}

// A node that gives up more for its instance than is reserved there is
// flagged (never an error), and the most it gave up is kept as the peak
// when the load ends.
func TestReserve_NodeAboveItsReservation(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.cfg.Defaults.FillReserveKiBPerToken = 8 // peaks are kept only with the reserve on
	h.llama("m", 8000)
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 7000, 700, 300)) // measured 8000
	h.healthy("m")
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 9000 }) // the node gave up 9000
	h.viewChanged()
	var s NodeStatus
	for _, n := range h.c.statusDoc().View.Nodes {
		if n.ID == "a" {
			s = n
		}
	}
	if len(s.Reserved) != 1 || s.Reserved[0].Profile != "m" || s.ReservedMiB != 8032 || s.MeasuredUsedMiB == nil || *s.MeasuredUsedMiB != 9000 ||
		s.DriftMiB == nil || *s.DriftMiB != 968 || s.DriftWarning == "" {
		t.Fatalf("node %+v", s)
	}
	// Under the warn figure: no warning.
	h.cfg.Defaults.ReservedDriftWarnMiB = 1000
	for _, n := range h.c.statusDoc().View.Nodes {
		if n.ID == "a" && n.DriftWarning != "" {
			t.Fatalf("warned at the limit: %q", n.DriftWarning)
		}
	}
	// The stored peak is above need + reserve, so it replaces them.
	h.step(evUnload{profile: "m", force: true})
	h.advance(10 * time.Second)
	h.view.modify("a", func(n *core.Node) { n.FreeMiB += 9000 })
	h.viewChanged()
	if got := h.c.learnedNeeds()["m"]; got.PeakMiB != 9000 || got.MeasuredMiB != 8000 || got.PeakBasis == "" {
		t.Fatalf("peak after the cycle %+v", got)
	}
	r := h.c.dryPlan("m")
	if r.Memory == nil || len(r.Memory.Nodes) != 1 || r.Memory.Nodes[0].Basis != BasisPeak || r.Memory.Nodes[0].WillTakeMiB != 9000 ||
		r.Memory.Nodes[0].FillReserveMiB != 0 {
		t.Fatalf("plan memory %+v", r.Memory)
	}
}

// The peak is stored beside the learned need and comes back after a restart.
func TestReserve_PeakIsStored(t *testing.T) {
	st := openNeedStore(t)
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.cfg.Configured = true
	h.cfg.Defaults.FillReserveKiBPerToken = 8
	h.c.restoreNeeds(st)
	h.llama("m", 8000)
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 7000, 700, 300))
	h.healthy("m")
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 8400 })
	h.viewChanged()
	h.step(evUnload{profile: "m", force: true}) // the cycle's peak is stored when it ends
	flushNeeds(h, st)
	rec := storedNeeds(t, st)["m"]
	if len(rec.Nodes) != 1 || rec.Nodes[0].MiB != 8000 || rec.Nodes[0].PeakMiB != 8400 || rec.PeakAt.IsZero() {
		t.Fatalf("stored %+v", rec)
	}
	h2 := newHarness(t).nodes(psNode("a", 2, 12400))
	h2.cfg.Configured = true
	h2.llama("m", 8000)
	h2.c.restoreNeeds(st)
	if got := h2.c.learnedNeeds()["m"]; got.PeakMiB != 8400 || !got.Restored {
		t.Fatalf("restored %+v", got)
	}
}

// placement_uses_reserved places on min(latest, average): one lucky sample
// cannot admit a load.
func TestReserve_AverageWindow(t *testing.T) {
	for _, on := range []bool{false, true} {
		n := psNode("a", 2, 12400)                // latest: 12400 usable
		n.FreeAvgMiB, n.FreeAvgKnown = 9400, true // average: 8400 usable
		h := newHarness(t).nodes(n)
		h.cfg.Defaults.PlacementUsesReserved = on
		h.llama("m", 10000)
		h.viewChanged()
		r := h.c.dryPlan("m")
		want := PlanStart
		if on {
			want = PlanQueue
		}
		if r.Action != want || r.Memory == nil || r.Memory.Nodes[0].FreeAvgMiB != 9400 || !r.Memory.Nodes[0].FreeAvgKnown {
			t.Fatalf("on %v: plan %s %q memory %+v", on, r.Action, r.Reason, r.Memory)
		}
	}
}

// With margin_measured_mib 300 and a stored measurement, a 3-node placement
// the 15% skew rule refused is accepted; an estimated need on the same nodes
// is still held to the percent margin.
func TestReserve_MeasuredMarginAdmitsWhatTheSkewRefused(t *testing.T) {
	setup := func(t *testing.T, margin bool) *harness {
		st := openNeedStore(t)
		if err := st.Put(needstore.Record{Schema: needstore.RecordSchema, Profile: "m", Kind: "llama", Model: "m.gguf", Ctx: 4096, Parallel: 1,
			KVQuant: "f16", TotalMiB: 27000, Cause: needstore.CauseLoad, MeasuredAt: t0}); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t).nodes(psNode("a", 2, 10000), psNode("b", 4, 10000), psNode("c", 6, 10000))
		h.cfg.Configured = true
		if margin {
			h.cfg.Defaults.MarginMeasuredMiB = 300
			h.cfg.Defaults.MarginEstimatedPercent = 15
		}
		h.llama("m", 0)
		h.needs["m"] = NeedInfo{Need: placer.Estimated(20000), FileBytes: 1 << 30}
		// 24000 x 1.05 = 25200 / 3 = 8400 + 600 overhead = 9000 a node.
		h.llama("e", 0)
		h.needs["e"] = NeedInfo{Need: placer.Estimated(24000), FileBytes: 1 << 30}
		h.c.restoreNeeds(st)
		return h
	}
	// The skew rule: 27000 / 3 x 1.15 = 10350 > 10000.
	h := setup(t, false)
	h.failed(h.request("m"), http.StatusServiceUnavailable, ReasonDoesNotFit)
	// 9000 + 300 <= 10000.
	h = setup(t, true)
	w := h.request("m")
	h.wantState("m", "starting")
	h.pending(w)
	if got := len(h.nodesOf("m")); got != 3 {
		t.Fatalf("placed on %d nodes", got)
	}
	// The estimate: 9000 + 15% = 10350 > 10000, although 9000 + 300 would fit.
	h = setup(t, true)
	h.failed(h.request("e"), http.StatusServiceUnavailable, ReasonDoesNotFit)
	h = setup(t, true)
	h.cfg.Defaults.MarginEstimatedPercent = 10 // 9000 + 900 fits
	w = h.request("e")
	h.wantState("e", "starting")
	h.pending(w)
}
