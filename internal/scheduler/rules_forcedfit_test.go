package scheduler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// defaults.forced_fit_check: a forced profile's memory need against the sum
// of the forced nodes' free memory at pre-flight (no tier headroom taken off).

// dedicatedPS5 is a ps5() on a dedicated-VRAM tier: free memory is used raw.
func dedicatedPS5(id string, n int, free int) core.Node {
	nd := ps5(id, n, free)
	nd.SharedMemory = false
	return nd
}

// forcedFitHarness has two dedicated-VRAM workers (a, b) whose pre-flight says 13824 MiB
// free in total (6912 each) and a forced profile "f" needing need MiB.
func forcedFitHarness(t *testing.T, mode string, need placer.Need) *harness {
	h := newHarness(t).nodes(dedicatedPS5("a", 2, 15000), dedicatedPS5("b", 4, 15000))
	h.cfg.Defaults.ForcedFitCheck = mode
	h.llama("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"a", "b"} })
	h.needs["f"] = NeedInfo{Need: need, FileBytes: 1 << 30}
	h.prober.free["10.0.0.2:50052"] = 6912
	h.prober.free["10.0.0.4:50052"] = 6912
	return h
}

func TestForcedFit_OffStartsAnyway(t *testing.T) {
	h := forcedFitHarness(t, config.ForcedFitOff, placer.Measured(14336))
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
	if n := len(h.logs.find("forced profile may not fit")); n != 0 {
		t.Fatalf("off mode logged %d warnings", n)
	}
}

func TestForcedFit_WarnLogsNumbersAndStarts(t *testing.T) {
	h := forcedFitHarness(t, config.ForcedFitWarn, placer.Measured(14336))
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
	r := h.mustLog("forced profile may not fit", "profile", "f", "need_mib", 14336, "free_mib", 13824, "measured", true)
	h.mustHaveAttrs(r, "nodes")
	if r.Level.String() != "WARN" {
		t.Fatalf("level %v", r.Level)
	}
}

func TestForcedFit_RejectFailsUpFront(t *testing.T) {
	h := forcedFitHarness(t, config.ForcedFitReject, placer.Estimated(14336))
	w := h.request("f")
	e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	for _, want := range []string{"14336 MiB", "13824 MiB", "a", "b", "estimated"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	// The 503 carries the need the check used: the estimate 14336 + 5% = 15053.
	if e.NeedMiB != 15053 || e.CapacityMiB != 13824 {
		t.Errorf("need %d free %d", e.NeedMiB, e.CapacityMiB)
	}
	if !strings.Contains(e.Message, "estimated, +5% margin") {
		t.Errorf("message %q does not say the estimate carries a margin", e.Message)
	}
	h.wantState("f", "absent")
	h.noEntry("f")
	// Not a load failure: no backoff, no quarantine, nodes still usable.
	if len(h.c.backoffs) != 0 {
		t.Fatalf("backoffs %v", h.c.backoffs)
	}
	for _, id := range []core.NodeID{"a", "b"} {
		if h.c.quarantined(id) {
			t.Fatalf("node %s quarantined", id)
		}
	}
	if len(h.launcher.all()) != 0 {
		t.Fatalf("a child was started")
	}
}

func TestForcedFit_RejectMessageSaysMeasured(t *testing.T) {
	h := forcedFitHarness(t, config.ForcedFitReject, placer.Measured(14336))
	e := h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	if !strings.Contains(e.Message, "measured") || strings.Contains(e.Message, "estimated") {
		t.Fatalf("message %q", e.Message)
	}
}

func TestForcedFit_FitDoesNotSubtractHeadroom(t *testing.T) {
	// 13824 free, headroom 1000 per node: the budgets sum to 11824, but a
	// need of 13000 is under the free figure, so the forced load goes ahead.
	h := forcedFitHarness(t, config.ForcedFitReject, placer.Measured(13000))
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
}

func TestForcedFit_UsesPreflightFigureNotFleetFigure(t *testing.T) {
	// The fleet says 15000 each; the probe says 6912 each. Pre-flight wins.
	h := forcedFitHarness(t, config.ForcedFitReject, placer.Measured(20000))
	h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
}

func TestForcedFit_RejectThenFitsAfterMemoryFreed(t *testing.T) {
	h := forcedFitHarness(t, config.ForcedFitReject, placer.Measured(14336))
	h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	h.prober.free["10.0.0.2:50052"] = 7500
	h.prober.free["10.0.0.4:50052"] = 7500
	h.request("f")
	h.wantState("f", "starting")
}

func TestForcedFit_NotForcedProfilesUnaffected(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("m", 8000)
	h.prober.free["10.0.0.2:50052"] = 9000
	w := h.request("m")
	h.wantState("m", "starting")
	h.pending(w)
}

func TestForcedFit_PlanReportsTheSameOutcome(t *testing.T) {
	h := forcedFitHarness(t, config.ForcedFitReject, placer.Estimated(14336))
	// Plan has no pre-flight: it uses the free figure the scheduler holds.
	h.view.set(dedicatedPS5("a", 2, 6912), dedicatedPS5("b", 4, 6912))
	h.viewChanged()
	r := h.c.dryPlan("f")
	if r.Action != PlanReject || r.NeedMiB != 14336 || r.Measured {
		t.Fatalf("plan %+v", r)
	}
	for _, want := range []string{"14336 MiB", "13824 MiB", "estimated"} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("reason %q lacks %q", r.Reason, want)
		}
	}

	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitWarn
	r = h.c.dryPlan("f")
	if r.Action != PlanStart || !strings.Contains(r.Warning, "13824 MiB") {
		t.Fatalf("warn plan %+v", r)
	}

	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitOff
	r = h.c.dryPlan("f")
	if r.Action != PlanStart || r.Warning != "" {
		t.Fatalf("off plan %+v", r)
	}
}

// sharedHarness: one shared-memory PS5 "ps5-9032a330" whose pre-flight says
// 14875 MiB free and whose tier headroom leaves a budget of 12380 MiB.
func sharedHarness(t *testing.T, mode string) *harness {
	n := ps5("ps5-9032a330", 2, 14875)
	n.TotalMiB, n.HeadroomMiB = 14947, 14875-12380
	h := newHarness(t).nodes(n)
	h.cfg.Defaults.ForcedFitCheck = mode
	h.llama("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"ps5-9032a330"} })
	h.needs["f"] = NeedInfo{Need: placer.Estimated(13991), FileBytes: 1 << 30}
	h.prober.free["10.0.0.2:50052"] = 14875
	return h
}

func TestForcedFit_SharedMemoryUsesBudgetReject(t *testing.T) {
	h := sharedHarness(t, config.ForcedFitReject)
	e := h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	for _, want := range []string{"need 14691 MiB (estimated, +5% margin; the estimate is 13991 MiB)", "usable 12380 MiB on ps5-9032a330 (shared memory, headroom kept, from fleet figures)"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	if e.NeedMiB != 14691 || e.CapacityMiB != 12380 {
		t.Errorf("need %d usable %d", e.NeedMiB, e.CapacityMiB)
	}
	t.Logf("reject message: %s", e.Message)
	if len(h.launcher.all()) != 0 {
		t.Fatalf("a child was started")
	}
}

func TestForcedFit_SharedMemoryOffStarts(t *testing.T) {
	h := sharedHarness(t, config.ForcedFitOff)
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
}

func TestForcedFit_SharedMemoryPlanRejects(t *testing.T) {
	h := sharedHarness(t, config.ForcedFitReject)
	h.viewChanged()
	r := h.c.dryPlan("f")
	if r.Action != PlanReject || !strings.Contains(r.Reason, "usable 12380 MiB on ps5-9032a330 (shared memory, headroom kept, from fleet figures)") {
		t.Fatalf("plan %+v", r)
	}
}

// The live case: the fleet figure (real system RAM) gives a budget of 12472,
// the worker's Vulkan pre-flight overstates free at 14880. The smaller wins,
// and the plan and the real request must agree.
func TestForcedFit_SharedMemoryUsesSmallerOfFleetAndPreflight(t *testing.T) {
	n := ps5("ps5-31bd9dc4", 2, 13496) // fleet free 13496 - headroom 1024 = 12472
	n.TotalMiB, n.HeadroomMiB = 14947, 1024
	h := newHarness(t).nodes(n)
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"ps5-31bd9dc4"} })
	h.needs["f"] = NeedInfo{Need: placer.Estimated(13000), FileBytes: 1 << 30}
	h.prober.free["10.0.0.2:50052"] = 14880 // pre-flight budget would be 13856

	e := h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	want := "usable 12472 MiB on ps5-31bd9dc4 (shared memory, headroom kept, from fleet figures)"
	if !strings.Contains(e.Message, want) || e.CapacityMiB != 12472 {
		t.Fatalf("request: message %q capacity %d, want %q / 12472", e.Message, e.CapacityMiB, want)
	}
	t.Logf("request message: %s", e.Message)

	h.viewChanged()
	r := h.c.dryPlan("f")
	if r.Action != PlanReject || !strings.Contains(r.Reason, want) {
		t.Fatalf("plan %+v, want reject with %q", r, want)
	}
	t.Logf("plan reason: %s", r.Reason)
}

// With no live pre-flight (a plan before any request), the fleet figure is used.
func TestForcedFit_SharedMemoryFleetOnlyIsUsed(t *testing.T) {
	n := ps5("ps5-31bd9dc4", 2, 13496)
	n.TotalMiB, n.HeadroomMiB = 14947, 1024
	h := newHarness(t).nodes(n)
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"ps5-31bd9dc4"} })
	h.needs["f"] = NeedInfo{Need: placer.Estimated(13000), FileBytes: 1 << 30}
	h.viewChanged()
	r := h.c.dryPlan("f")
	if want := "usable 12472 MiB on ps5-31bd9dc4 (shared memory, headroom kept, from fleet figures)"; r.Action != PlanReject || !strings.Contains(r.Reason, want) {
		t.Fatalf("plan %+v lacks %q", r, want)
	}
}

func TestForcedFit_DedicatedVRAMStillUsesRawFree(t *testing.T) {
	// Need 14000 is above the 13851 budget but under the 14875 free: a
	// dedicated-VRAM node may be forced into its headroom.
	n := dedicatedPS5("lap", 2, 14875)
	h := newHarness(t).nodes(n)
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	h.llama("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"lap"} })
	h.needs["f"] = NeedInfo{Need: placer.Measured(14000), FileBytes: 1 << 30}
	h.prober.free["10.0.0.2:50052"] = 14875
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
}

func TestForcedFit_MixedNodesSumsEachNodesOwnFigure(t *testing.T) {
	// dedicated: 6000 raw free. shared: 6000 free - 1000 headroom = 5000.
	// Usable total 11000: need 11500 is rejected (raw free alone would be 12000
	// and pass). The split follows the budgets (5000 each), so the shared node
	// also takes its skew margin: need 8600 puts 4945 on it and fits, 10500
	// would put 6037 on a node with 5000.
	build := func(need int) *harness {
		h := newHarness(t).nodes(dedicatedPS5("d", 2, 6000), ps5("s", 4, 6000))
		h.cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
		h.llama("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"d", "s"} })
		h.needs["f"] = NeedInfo{Need: placer.Measured(need), FileBytes: 1 << 30}
		h.prober.free["10.0.0.2:50052"] = 6000
		h.prober.free["10.0.0.4:50052"] = 6000
		return h
	}
	h := build(11500)
	e := h.failed(h.request("f"), http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	for _, want := range []string{"usable 6000 MiB on d (free memory, no headroom kept)", "usable 5000 MiB on s (shared memory, headroom kept, from fleet figures)", "11000 MiB usable in total"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	h = build(8600)
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
}
