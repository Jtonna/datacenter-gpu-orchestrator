package scheduler

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

// The reviewer's named test: a profile whose written need is far below the
// formula at a larger context. The scoped plan at 4 x ctx must NOT start
// where the formula rejects; it plans from the formula (the real need
// provider reading the GGUF header), and says the written need was not
// used. At the production shape the written need still applies (start).
// Unpinned loads: a pinned (forced) placement follows forced_fit_check,
// which is off by default, so the fit is the placer's here.
func TestScoped_WrittenNeedFarBelowFormulaAt4xCtxDoesNotStart(t *testing.T) {
	dir := t.TempDir()
	if err := ggufgen.Write(filepath.Join(dir, "w.gguf"), ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8, Context: 4 * 32768}); err != nil { // trained context covers 4 x ctx: these test the fit formula, not the cap
		t.Fatal(err)
	}
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.c.cfg.ModelsDir = dir
	h.c.needs = &ConfigNeeds{Cfg: h.c.cfg}
	h.llama("w", 700, func(p *core.Profile) { p.Model, p.Ctx = "w.gguf", 32768 })
	h.viewChanged()

	if r := h.c.dryPlan("w"); r.Action != PlanStart || r.NeedMiB != 700 {
		t.Fatalf("production (written 700 MiB): %+v", r)
	}
	same := mustScoped(t, h, ScopedLoad{Model: "w", Scope: "x0"})
	if r := h.c.scopedPlan(same); r.Action != PlanStart || r.NeedMiB != 700 || strings.Contains(r.NeedBasis, "written need not used") {
		t.Fatalf("scoped at the production shape keeps the written need: %+v", r)
	}
	big := mustScoped(t, h, ScopedLoad{Model: "w", Scope: "x4", Ctx: ptrInt(4 * 32768)})
	r := h.c.scopedPlan(big)
	if r.Action == PlanStart {
		t.Fatalf("scoped at 4 x ctx starts on the written need: %+v", r)
	}
	if r.NeedMiB <= 15000 || r.Measured || !strings.Contains(r.NeedBasis, "written need not used: shape differs from w") {
		t.Fatalf("scoped at 4 x ctx: need %d measured %v basis %q (want the formula, above the console's 15000 MiB)", r.NeedMiB, r.Measured, r.NeedBasis)
	}
	// the load is refused too, and nothing is held
	w := h.scopedLoad(ScopedLoad{Model: "w", Scope: "x4", Ctx: ptrInt(4 * 32768)})
	if a, ok := h.peek(w); !ok || a.err == nil {
		t.Fatalf("the scoped load at 4 x ctx was not refused: %+v %v", a, ok)
	}
	h.step(evTick{})
	if h.c.insts["w@x4"] != nil || h.c.scoped["w@x4"] != nil {
		t.Fatal("a refused scoped load is held")
	}
}
