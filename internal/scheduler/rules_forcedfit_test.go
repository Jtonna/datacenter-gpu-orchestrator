package scheduler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
)

// defaults.forced_fit_check: a forced profile's memory need against the sum
// of the forced nodes' free memory at pre-flight (no tier headroom taken off).

// forcedFitHarness has two PS5 workers (a, b) whose pre-flight says 13824 MiB
// free in total (6912 each) and a forced profile "f" needing need MiB.
func forcedFitHarness(t *testing.T, mode string, need placer.Need) *harness {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
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
	if e.NeedMiB != 14336 || e.CapacityMiB != 13824 {
		t.Errorf("need %d free %d", e.NeedMiB, e.CapacityMiB)
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
	h.view.set(ps5("a", 2, 6912), ps5("b", 4, 6912))
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
