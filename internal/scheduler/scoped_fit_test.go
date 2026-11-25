package scheduler

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

// A scoped load ALWAYS gets the fit check on the zones it names, whatever
// defaults.forced_fit_check says (reviewer's ruling); a production forced
// load keeps the configured rule.

// scopedFitHarness: one console "a" with free MiB, forced_fit_check off, the
// real need provider over a generated GGUF, production "w" at ctx 32768 with
// a written need of 700 MiB, and production "wbig", the same model forced
// onto "a" at 4 x ctx with no written need.
func scopedFitHarness(t *testing.T, free int) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := ggufgen.Write(filepath.Join(dir, "w.gguf"), ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8, Context: 4 * 32768}); err != nil { // trained context covers 4 x ctx: these test the fit formula, not the cap
		t.Fatal(err)
	}
	n := ps5("a", 2, free)
	n.TotalMiB = max(n.TotalMiB, free+1000)
	h := newHarness(t).nodes(n)
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitOff
	h.c.cfg.ModelsDir = dir
	h.c.needs = &ConfigNeeds{Cfg: h.c.cfg}
	h.llama("w", 700, func(p *core.Profile) { p.Model, p.Ctx = "w.gguf", 32768 })
	h.llama("wbig", 0, func(p *core.Profile) { p.Model, p.Ctx, p.ForceNodes = "w.gguf", 4*32768, []string{"a"} })
	h.viewChanged()
	return h
}

var scoped4x = ScopedLoad{Model: "w", Scope: "x4", Nodes: []string{"a"}, Ctx: ptrInt(4 * 32768)}

// The reviewer's named test: a scoped load at 4 x ctx on one named console
// is refused where the formula says it does not fit, with forced_fit_check off.
func TestScoped_FitCheckedOnNamedZonesWithForcedFitOff(t *testing.T) {
	h := scopedFitHarness(t, 15000)
	w := h.scopedLoad(scoped4x)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedDoesNotFit)
	for _, want := range []string{"w@x4", "estimated", "a scoped load is always fit-checked on the zones it names"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	if e.NeedMiB <= e.CapacityMiB || e.CapacityMiB == 0 {
		t.Errorf("figures: need %d capacity %d", e.NeedMiB, e.CapacityMiB)
	}
	h.step(evTick{})
	if h.c.insts["w@x4"] != nil || h.c.scoped["w@x4"] != nil || len(h.c.backoffs) != 0 {
		t.Fatalf("a refused scoped load left something: inst %v scoped %v backoffs %v", h.c.insts["w@x4"], h.c.scoped["w@x4"], h.c.backoffs)
	}
	h.noEntry("w@x4")
}

// The same load fits and starts when the need fits.
func TestScoped_FitCheckedLoadStartsWhenItFits(t *testing.T) {
	h := scopedFitHarness(t, 60000)
	w := h.scopedLoad(scoped4x)
	if a, ok := h.peek(w); ok && a.err != nil {
		t.Fatalf("refused: %v", a.err)
	}
	h.wantState("w@x4", "starting")
}

// A scoped plan (scope and nodes) answers the same refusal with the figures
// and holds nothing.
func TestScoped_PlanOnNamedZonesAnswersTheRefusal(t *testing.T) {
	h := scopedFitHarness(t, 15000)
	r := h.c.scopedPlan(mustScoped(t, h, scoped4x)) // what Scheduler.PlanScoped reads
	if r.Action != PlanReject || !strings.Contains(r.Reason, "a scoped load is always fit-checked on the zones it names") || !strings.Contains(r.Reason, "MiB usable") {
		t.Fatalf("plan: action %s reason %q", r.Action, r.Reason)
	}
	if r.NeedMiB <= 15000 {
		t.Fatalf("plan need %d", r.NeedMiB)
	}
	h.step(evTick{})
	if len(h.c.scoped) != 0 || len(h.c.insts) != 0 {
		t.Fatalf("a plan held something: scoped %v insts %v", h.c.scoped, h.c.insts)
	}
}

// Pinned: a production forced load in the same state behaves exactly as
// before: with forced_fit_check off it plans start, no warning, and starts.
func TestScoped_ProductionForcedLoadKeepsTheConfiguredRule(t *testing.T) {
	h := scopedFitHarness(t, 15000)
	if r := h.c.dryPlan("wbig"); r.Action != PlanStart || r.Warning != "" || r.NeedMiB <= 15000 {
		t.Fatalf("production plan: %+v", r)
	}
	w := h.request("wbig")
	h.wantState("wbig", "starting")
	h.pending(w)
	if n := len(h.logs.find("forced profile may not fit")) + len(h.logs.find("forced profile does not fit; rejecting")); n != 0 {
		t.Fatalf("off mode logged %d fit lines", n)
	}
}
