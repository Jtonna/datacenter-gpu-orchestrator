package scheduler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// Memory safety on shared-memory nodes (memfit.go): the per-node fit with the
// split skew margin, the margin on an estimate, learned needs, the memory
// watch during a load, and concurrent placements.

// psNode is a PS5 whose live and plan budgets (free and total minus the 1000
// MiB headroom) are both budget.
func psNode(id string, n, budget int) core.Node {
	nd := ps5(id, n, budget+1000)
	nd.TotalMiB = budget + 1000
	return nd
}

// ---- A: per-node fit, forced placements ----

// forcedThree has three PS5s with budgets 12400, 12400 and cBudget, and a
// forced profile "f" split 33/33/34 whose need is need MiB (measured).
func forcedThree(t *testing.T, mode string, need, cBudget int) *harness {
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, cBudget))
	h.cfg.Defaults.ForcedFitCheck = mode
	h.llama("f", 0, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b", "c"}, []int{33, 33, 34}, time.Hour
	})
	h.needs["f"] = NeedInfo{Need: placer.Measured(need), FileBytes: 1 << 30}
	return h
}

func TestPerNodeFit_ForcedRejectNamesTheNodeItsShareAndItsUsableFigure(t *testing.T) {
	// 27793 MiB x 34% x 1.15 = 10868 on node c, which has only 10000 usable.
	// The total (12400 + 12400 + 10000 = 34800) holds the need, so only the
	// per-node check sees it.
	h := forcedThree(t, config.ForcedFitReject, 27793, 10000)
	e := h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	want := "node c: expected share 10868 MiB (need 27793 MiB x 34% of the split x 1.15 skew margin) but usable 10000 MiB (shared memory, headroom kept, from fleet figures)"
	if !strings.Contains(e.Message, want) {
		t.Fatalf("message %q lacks %q", e.Message, want)
	}
	if strings.Contains(e.Message, "in total") {
		t.Fatalf("the total fits; message %q must not complain about it", e.Message)
	}
	t.Logf("reject message: %s", e.Message)
	h.wantState("f", "absent")
	if len(h.launcher.all()) != 0 {
		t.Fatal("a child was started")
	}
	if len(h.c.backoffs) != 0 || h.c.quarantined("c") {
		t.Fatal("a refusal is not a load failure")
	}
}

func TestPerNodeFit_ForcedWarnLogsAndStarts(t *testing.T) {
	h := forcedThree(t, config.ForcedFitWarn, 27793, 10000)
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
	r := h.mustLog("forced profile may not fit", "profile", "f")
	if r.Level.String() != "WARN" {
		t.Fatalf("level %v", r.Level)
	}
}

func TestPerNodeFit_ForcedOffIsUnchanged(t *testing.T) {
	h := forcedThree(t, config.ForcedFitOff, 27793, 10000)
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
	if n := len(h.logs.find("forced profile may not fit")); n != 0 {
		t.Fatalf("off mode logged %d warnings", n)
	}
}

func TestPerNodeFit_ForcedFitsWhenEveryNodeHoldsItsShare(t *testing.T) {
	// Real three-way split, 27793 MiB over budgets of about 12400: 10547,
	// 10547 and 10868 MiB with the margin.
	h := forcedThree(t, config.ForcedFitReject, 27793, 12400)
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
}

func TestPerNodeFit_ForcedTwoWayRealCases(t *testing.T) {
	for _, tc := range []struct {
		need int
		ok   bool
	}{{16442, true}, {14248, true}, {21600, false}} { // 21600 x 0.5 x 1.15 = 12420 > 12400
		h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400))
		h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
		h.llama("f", 0, func(p *core.Profile) {
			p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{50, 50}, time.Hour
		})
		h.needs["f"] = NeedInfo{Need: placer.Measured(tc.need), FileBytes: 1 << 30}
		w := h.request("f")
		if tc.ok {
			h.wantState("f", "starting")
			h.pending(w)
		} else {
			e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
			if !strings.Contains(e.Message, "expected share 12420 MiB") {
				t.Fatalf("message %q", e.Message)
			}
		}
	}
}

func TestPerNodeFit_SingleNodeTakesNoMargin(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.needs["f"] = NeedInfo{Need: placer.Measured(12400), FileBytes: 1 << 30}
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
}

func TestPerNodeFit_PlanReportsTheSameRefusal(t *testing.T) {
	h := forcedThree(t, config.ForcedFitReject, 27793, 10000)
	h.viewChanged()
	r := h.c.dryPlan("f")
	if r.Action != PlanReject || !strings.Contains(r.Reason, "node c: expected share 10868 MiB") {
		t.Fatalf("plan %+v", r)
	}
	if r.NeedBasis != "measured, needs_mib" || r.FitNeedMiB != 27793 {
		t.Fatalf("basis %q fit need %d", r.NeedBasis, r.FitNeedMiB)
	}
}

func TestPerNodeFit_SplitSkewMarginIsConfigurable(t *testing.T) {
	h := forcedThree(t, config.ForcedFitReject, 27793, 10000)
	h.cfg.Defaults.SplitSkewMargin = 0 // 27793 x 34% = 9450 <= 10000
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
}

// ---- A: per-node fit, automatic placements ----

func TestPerNodeFit_AutoPlacementAddsANodeInsteadOfOverfillingOne(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, 12400), psNode("d", 8, 12400))
	h.llama("m", 33000, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	// Three nodes hold 37200 in total, but 33000 x 1.15 = 37950 would overfill
	// them with the skew; four nodes do.
	w := h.request("m")
	h.wantState("m", "starting")
	h.pending(w)
	if got := len(h.nodesOf("m")); got != 4 {
		t.Fatalf("placed on %d nodes, want 4", got)
	}
}

func TestPerNodeFit_AutoNeverPlacesAShareOverABudget(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, 12400))
	h.llama("m", 33000)
	// Only three nodes: it cannot be placed without overfilling one, so it is
	// refused at once (the capacity check) rather than loaded.
	e := h.failed(h.request("m"), http.StatusServiceUnavailable, ReasonDoesNotFit)
	if e.RequiredMiB != 37950 || e.CapacityMiB != 37200 {
		t.Fatalf("required %d capacity %d", e.RequiredMiB, e.CapacityMiB)
	}
	if len(h.launcher.all()) != 0 {
		t.Fatal("a child was started")
	}
	t.Logf("refusal: %s", e.Message)
}

func TestPerNodeFit_AutoFourNodePlacementFitsEachNodesOwnBudget(t *testing.T) {
	// One small node: the budgets are the weights of the split, so the small
	// node's share is small too.
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, 3000))
	h.llama("m", 20000)
	w := h.request("m")
	h.wantState("m", "starting")
	h.pending(w)
	in := h.inst("m")
	var sum float64
	for _, pn := range in.placement.Nodes {
		sum += float64(pn.BudgetMiB)
	}
	for _, pn := range in.placement.Nodes {
		if share := 20000 * float64(pn.BudgetMiB) / sum * 1.15; share > float64(pn.BudgetMiB) {
			t.Fatalf("node %s share %.0f over budget %d", pn.ID, share, pn.BudgetMiB)
		}
	}
}

// ---- B: margin on an estimate ----

// estHarness has one PS5 with a budget of 12000 MiB and a profile whose need
// is the estimate est (no needs_mib).
func estHarness(t *testing.T, est int) *harness {
	h := newHarness(t).nodes(psNode("a", 2, 12000))
	h.cfg.Defaults.DeviceOverheadMiB = 0
	h.llama("m", 0, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	h.needs["m"] = NeedInfo{Need: placer.Estimated(est), FileBytes: 1 << 30}
	return h
}

func TestEstimateMargin_IsAddedToAnEstimate(t *testing.T) {
	h := estHarness(t, 11400) // x 1.05 = 11970 <= 12000
	w := h.request("m")
	h.wantState("m", "starting")
	h.pending(w)

	h = estHarness(t, 11500) // x 1.05 = 12075 > 12000
	e := h.failed(h.request("m"), http.StatusServiceUnavailable, ReasonDoesNotFit)
	if e.NeedMiB != 11500 || e.RequiredMiB != 12075 {
		t.Fatalf("need %d required %d", e.NeedMiB, e.RequiredMiB)
	}

	h = estHarness(t, 11500)
	h.cfg.Defaults.EstimateMargin = 0 // configured away: 11500 <= 12000
	w = h.request("m")
	h.wantState("m", "starting")
	h.pending(w)
}

func TestEstimateMargin_NeedsMiBIsUsedAsIs(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12000))
	h.llama("m", 12000) // needs_mib: no margin
	w := h.request("m")
	h.wantState("m", "starting")
	h.pending(w)
}

func TestEstimateMargin_PlanAndForcedMessagesSayEstimated(t *testing.T) {
	h := estHarness(t, 11500)
	h.viewChanged()
	r := h.c.dryPlan("m")
	if r.NeedBasis != "estimated, +5% margin; the estimate is 11500 MiB" || r.NeedMiB != 11500 || r.FitNeedMiB != 12075 || r.Measured {
		t.Fatalf("plan %+v", r)
	}

	h = newHarness(t).nodes(psNode("a", 2, 12000))
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.needs["f"] = NeedInfo{Need: placer.Estimated(11500), FileBytes: 1 << 30}
	e := h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	if want := "need 12075 MiB (estimated, +5% margin; the estimate is 11500 MiB)"; !strings.Contains(e.Message, want) {
		t.Fatalf("message %q lacks %q", e.Message, want)
	}
}

// ---- B: learning measured sizes ----

// setLoad gives the profile's child the memory the load log would report.
func setLoad(h *harness, profile string, devs ...instance.DeviceMem) {
	h.child(profile).load = instance.LoadStats{Devices: devs}
}

func rpcDev(addr string, model, kv, compute float64) instance.DeviceMem {
	return instance.DeviceMem{Name: "RPC0[" + addr + "]", ModelMiB: model, KVMiB: kv, ComputeMiB: compute}
}

func TestLearned_MeasurementReplacesTheEstimate(t *testing.T) {
	h := estHarness(t, 9000) // 9000 x 1.05 = 9450 as an estimate
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 11000, 500, 300)) // measured 11800
	h.healthy("m")
	got := h.c.learnedNeeds()["m"]
	if got.MeasuredMiB != 11800 || got.NeedMiB != 11800 || got.Source != "measured this run" || got.EstimatedMiB != 9000 || got.ConfiguredMiB != 0 {
		t.Fatalf("learned %+v", got)
	}
	// The next fit decision uses 11800, with no margin of its own.
	h.release(h.granted(h.request("m")))
	h.step(evUnload{profile: "m", force: true})
	h.advance(10 * time.Second)
	r := h.c.dryPlan("m")
	if r.NeedMiB != 11800 || r.FitNeedMiB != 11800 || !r.Measured || !strings.HasPrefix(r.NeedBasis, "measured this run") {
		t.Fatalf("plan %+v", r)
	}
	if h.c.statusDoc().LearnedNeeds["m"].MeasuredMiB != 11800 {
		t.Fatalf("status %+v", h.c.statusDoc().LearnedNeeds)
	}
}

func TestLearned_HigherThanConfiguredWarnsOnceAndWins(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.llama("m", 10000)
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 10500, 400, 300)) // 11200 > 10000
	h.healthy("m")
	r := h.mustLog("configured needs_mib is too low: the load measured more memory; using the measured value",
		"profile", "m", "configured_mib", 10000, "measured_mib", 11200)
	if r.Level.String() != "WARN" {
		t.Fatalf("level %v", r.Level)
	}
	got := h.c.learnedNeeds()["m"]
	if got.NeedMiB != 11200 || got.ConfiguredMiB != 10000 || got.Source != "measured this run" {
		t.Fatalf("learned %+v", got)
	}
	// A second, higher measurement does not warn again.
	h.step(evUnload{profile: "m", force: true})
	h.advance(10 * time.Second)
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 10700, 400, 300))
	h.healthy("m")
	if n := len(h.logs.find("configured needs_mib is too low: the load measured more memory; using the measured value")); n != 1 {
		t.Fatalf("warned %d times", n)
	}
	if h.c.learnedNeeds()["m"].NeedMiB != 11400 {
		t.Fatalf("learned %+v", h.c.learnedNeeds()["m"])
	}
}

func TestLearned_NeverLowersAConfiguredNeed(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.llama("m", 12000)
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 8000, 400, 300)) // 8700 < 12000
	h.healthy("m")
	if n := len(h.logs.find("configured needs_mib is too low: the load measured more memory; using the measured value")); n != 0 {
		t.Fatalf("warned %d times", n)
	}
	info, _ := h.c.need(h.cfg.Profiles["m"])
	if info.Need.MiB != 12000 || info.Source != NeedConfigured {
		t.Fatalf("need %+v", info)
	}
	if st := h.c.learnedNeeds()["m"]; st.NeedMiB != 12000 || st.MeasuredMiB != 8700 || st.Source != "measured, needs_mib" {
		t.Fatalf("learned %+v", st)
	}
}

func TestLearned_PerNodeFiguresReplaceTheSkewForTheSameNodesAndSplit(t *testing.T) {
	// Measured: a 5000, b 6000 (a 45/55 split). The same set and split holds
	// exactly those figures; a different split falls back to need x fraction x skew.
	h := newHarness(t).nodes(psNode("a", 2, 6200), psNode("b", 4, 6200))
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("f", 0, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit = []string{"a", "b"}, []int{45, 55}
	})
	h.needs["f"] = NeedInfo{Need: placer.Measured(11000), FileBytes: 1 << 30}
	// Without a measurement: b share = 11000 x 55% x 1.15 = 6958 > 6200: refused.
	h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)

	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitOff
	h.request("f")
	setLoad(h, "f", rpcDev(addrA, 4800, 100, 100), rpcDev(addrB, 5800, 100, 100)) // a 5000, b 6000
	h.healthy("f")
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.step(evUnload{profile: "f", force: true})
	h.advance(10 * time.Second)
	h.viewChanged()
	if r := h.c.dryPlan("f"); r.Action != PlanStart || r.Warning != "" {
		t.Fatalf("same nodes and split: plan %+v", r)
	}
	h.cfg.Profiles["f"].TensorSplit = []int{30, 70} // another split: 11000 x 70% x 1.15 = 8855 > 6200
	if r := h.c.dryPlan("f"); r.Action != PlanReject || !strings.Contains(r.Reason, "node b: expected share") {
		t.Fatalf("other split: plan %+v", r)
	}
}

// ---- C: memory watch during a load ----

// watchHarness loads "m" onto two PS5s (budgets 12400 each) with a one-hour
// load timeout, so only the watch can end it.
func watchHarness(t *testing.T) (*harness, *waiter) {
	t.Helper()
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{1, 1}, time.Hour
	})
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: 1 << 30}
	w := h.request("m")
	h.wantState("m", "starting")
	return h, w
}

func freeOf(h *harness, id core.NodeID, free int) {
	h.view.modify(id, func(n *core.Node) { n.FreeMiB = free })
	h.viewChanged()
}

const lowMem = "load cancelled: b ran low on memory during the load (available 700 MiB, floor 768 MiB)"

func TestMemoryWatch_CancelsTheLoadWhenANodeRunsLow(t *testing.T) {
	h, w := watchHarness(t)
	h.autoExit = false
	freeOf(h, "a", 3000) // the model already sits in memory: still above the floor
	h.pending(w)
	freeOf(h, "b", 700)
	e := h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
	if !strings.Contains(e.Message, lowMem) {
		t.Fatalf("message %q lacks %q", e.Message, lowMem)
	}
	t.Logf("cancel message: %s", e.Message)
	// The child is killed, not asked to stop.
	stops := h.child("m").stops
	if len(stops) == 0 || stops[len(stops)-1] != stopKill {
		t.Fatalf("stops %v, want the last to be a kill", stops)
	}
	h.wantState("m", "stopping")
	if _, on := h.c.backoffActive("m"); !on {
		t.Fatal("no profile backoff")
	}
	if h.c.quarantined("a") || h.c.quarantined("b") {
		t.Fatal("the node must not be quarantined")
	}
	got := h.recent()
	if len(got) != 1 || got[0].Outcome != LoadFailed || got[0].Reason != lowMem {
		t.Fatalf("recent %+v", got)
	}
	var finished *LoadOutcome
	for _, ev := range h.feedLog {
		if ev.Kind == FeedLoadFinished {
			finished = ev.Outcome
		}
	}
	if finished == nil || finished.Outcome != LoadFailed || finished.Reason != lowMem {
		t.Fatalf("loadFinished event %+v", finished)
	}
	recs := h.logs.find("load cancelled: a node ran low on memory", "profile", "m", "node", "b", "available_mib", 700, "floor_mib", 768)
	if len(recs) != 1 || recs[0].Level.String() != "ERROR" || recs[0].Attrs["node_memory_mib"] != "a=3000 b=700" {
		t.Fatalf("log records %+v", recs)
	}
}

func TestMemoryWatch_NoCancelAtOrAboveTheFloor(t *testing.T) {
	h, w := watchHarness(t)
	freeOf(h, "a", 768)
	freeOf(h, "b", 900)
	h.advance(time.Minute)
	h.pending(w)
	h.wantState("m", "starting")
}

func TestMemoryWatch_StaleViewDisablesIt(t *testing.T) {
	h, w := watchHarness(t)
	h.view.frozen = true
	h.advance(2 * time.Minute) // older than stale_after (60 s)
	freeOf(h, "b", 100)
	h.pending(w)
	h.wantState("m", "starting")
	// The view is live again: the same reading now cancels.
	h.view.frozen = false
	h.viewChanged()
	h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
}

func TestMemoryWatch_ZeroDisables(t *testing.T) {
	h, w := watchHarness(t)
	h.cfg.Defaults.LoadMemoryFloorMiB = 0
	freeOf(h, "b", 10)
	h.pending(w)
	h.wantState("m", "starting")
}

func TestMemoryWatch_UnknownFigureIsNotAReading(t *testing.T) {
	h, w := watchHarness(t)
	h.view.modify("b", func(n *core.Node) { n.FreeMiB, n.FreeUnknown = 0, true })
	h.viewChanged()
	h.pending(w)
	h.wantState("m", "starting")
}

func TestMemoryWatch_OnlyWhileLoadingAndOnlySharedNodes(t *testing.T) {
	// A dedicated-VRAM worker is not watched.
	h := newHarness(t).nodes(dedicatedPS5("a", 2, 15000), dedicatedPS5("b", 4, 15000))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{1, 1}, time.Hour
	})
	w := h.request("m")
	freeOf(h, "b", 10)
	h.pending(w)
	h.wantState("m", "starting")

	// A ready instance is not watched: free memory dropping later is not a load failure.
	h, w = watchHarness(t)
	h.healthy("m")
	h.granted(w)
	freeOf(h, "b", 10)
	h.wantState("m", "ready")
}

func TestMemoryWatch_FleetMemAvailableFieldIsRead(t *testing.T) {
	// mem_available_mib reaches the scheduler as the node's FreeMiB (see the
	// fleet package's conversion); the watch compares that figure.
	h, w := watchHarness(t)
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 767 })
	h.viewChanged()
	e := h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
	if !strings.Contains(e.Message, "a ran low on memory during the load (available 767 MiB, floor 768 MiB)") {
		t.Fatalf("message %q", e.Message)
	}
}

// ---- D: concurrent placements ----

// Two loads planned at the same time cannot both count the same free memory:
// a node an instance holds (from its placement through its load and for as
// long as it runs) is not offered to the next placement, and after the
// instance goes the node is unusable until its settle time has passed and the
// fleet figures have been fetched fresh.
func TestConcurrent_InFlightLoadReservesItsNodes(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.llama("first", 10000, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	h.llama("second", 10000, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	w1 := h.request("first")
	w2 := h.request("second")
	h.wantState("first", "starting")
	h.wantState("second", "absent") // the only node is reserved: the second waits
	h.pending(w1)
	h.pending(w2)
	// The fleet has not caught up with the first load (it still says 12400
	// free): the second request must still not be placed on that figure.
	h.advance(30 * time.Second)
	h.viewChanged()
	h.wantState("second", "absent")
	if h.c.isFree("a") {
		t.Fatal("a node held by a loading instance counts as free")
	}
	for _, c := range h.c.freeCandidates(h.cfg.Profiles["second"]) {
		t.Fatalf("candidate %v offered while the node is reserved", c)
	}
}

func TestConcurrent_AfterTheFirstLoadGoesTheNodeSettlesAndIsReReadBeforeReuse(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.llama("first", 10000, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	h.llama("second", 10000, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	h.autoFetch = false
	h.request("first")
	w2 := h.request("second")
	h.healthy("first")
	// first stays loaded and holds the node. Unload it: the node then settles
	// (3 s) and is fetched fresh before anything else is placed on it.
	h.step(evUnload{profile: "first", force: true})
	h.wantState("second", "absent")
	h.advance(2 * time.Second)
	h.wantState("second", "absent") // still settling
	h.advance(2 * time.Second)
	h.wantState("second", "absent") // settled, the fetch is outstanding
	if len(h.fetches) == 0 {
		t.Fatal("no post-settle fetch requested")
	}
	// The fetch returns the figure after the first load: the memory is
	// mostly free again, so the second one is placed on that figure.
	h.completeFetches()
	h.wantState("second", "starting")
	h.pending(w2)
}

func TestConcurrent_TwoEntriesNeverShareANodeAcrossSeveralNodes(t *testing.T) {
	// Three nodes, two profiles each needing two: only one can be placed; the
	// other must not be given a node the first holds.
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, 12400))
	h.llama("one", 20000, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	h.llama("two", 20000, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	h.request("one")
	h.request("two")
	h.wantState("one", "starting")
	h.wantState("two", "absent")
	held := map[core.NodeID]bool{}
	for _, id := range h.nodesOf("one") {
		held[id] = true
	}
	if len(held) != 2 {
		t.Fatalf("first placed on %v", h.nodesOf("one"))
	}
	if e := h.c.entries["two"]; e == nil || e.claimed() {
		t.Fatalf("entry %+v", e)
	}
}
