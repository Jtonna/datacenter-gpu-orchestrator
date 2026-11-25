package scheduler

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

// scopedMeasured loads m@e2 (ctx 8192 over production m's 4096) on node a,
// with a measured load of 11800 MiB, and returns the harness.
func scopedMeasured(t *testing.T) *harness {
	t.Helper()
	h := estHarness(t, 9000)
	h.c.cfg.Profiles["m"].Ctx = 4096
	h.needs["m@e2"] = NeedInfo{Need: placer.Estimated(9500), FileBytes: 1 << 30}
	w := h.scopedLoad(ScopedLoad{Model: "m", Scope: "e2", Ctx: ptrInt(8192)})
	if a, ok := h.peek(w); ok && a.err != nil {
		t.Fatal(a.err)
	}
	setLoad(h, "m@e2", rpcDev(addrA, 11000, 500, 300)) // measured 11800
	h.healthy("m@e2")
	return h
}

// The status names a scoped instance's production profile and scope, so a
// page can tell it apart; a production instance has neither.
func TestScopedVisibility_StatusNamesProductionAndScope(t *testing.T) {
	h := scopedMeasured(t)
	var got *InstanceStatus
	for i, in := range h.c.statusDoc().Instances {
		if in.Profile == "m@e2" {
			got = &h.c.statusDoc().Instances[i]
		}
	}
	if got == nil || got.Production != "m" || got.Scope != "e2" {
		t.Fatalf("scoped instance status %+v", got)
	}
	h = estHarness(t, 9000)
	h.request("m")
	h.healthy("m")
	for _, in := range h.c.statusDoc().Instances {
		if in.Profile == "m" && (in.Production != "" || in.Scope != "") {
			t.Fatalf("production instance status %+v", in)
		}
	}
}

// The learnedNeed event of a scoped load says it was a run-scoped load, and
// is filed under the production id with the scope.
func TestScopedVisibility_LearnedNeedSentence(t *testing.T) {
	h := scopedMeasured(t)
	e := h.one(history.TLearnedNeed)
	if e.Profile != "m" || e.Data["scope"] != "e2" ||
		!strings.HasPrefix(e.Summary, "Measured the memory need of run-scoped load e2 of m at 11800 MiB") || strings.Contains(e.Summary, "m@e2") {
		t.Fatalf("event profile %q scope %v summary %q", e.Profile, e.Data["scope"], e.Summary)
	}
}

// A measurement of a scoped shape is stored under that shape's key, never
// production's: production's plan at its own shape is unchanged by it, and
// is not derived from it either (a measurement of another context of the
// same model would otherwise feed production's estimate, derive.go).
func TestScopedVisibility_ScopedMeasurementNeverFeedsProduction(t *testing.T) {
	plan := func(origin string) (PlanReport, string, string) {
		dir := t.TempDir()
		if err := ggufgen.Write(filepath.Join(dir, "w.gguf"), ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8, Context: 4 * 32768}); err != nil { // trained context covers 4 x ctx: these test the fit formula, not the cap
			t.Fatal(err)
		}
		h := newHarness(t).nodes(ps5("a", 2, 15000))
		h.c.cfg.ModelsDir = dir
		h.c.needs = &ConfigNeeds{Cfg: h.c.cfg}
		h.llama("w", 0, func(p *core.Profile) { p.Model, p.Ctx = "w.gguf", 32768 })
		h.viewChanged()
		sp := mustScoped(t, h, ScopedLoad{Model: "w", Scope: "e2", Ctx: ptrInt(8192)})
		est, err := h.c.need(sp)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" { // the measurement of the 8192 shape, by origin
			h.c.putLearned(&learnedNeed{TotalMiB: est.Need.MiB + 2000, key: needKeyOf(sp), profile: origin, devices: 1, at: h.c.now})
		}
		prod := h.c.cfg.Profiles["w"]
		if h.c.learnedAt(prod, 1) != nil {
			t.Fatalf("production at its own shape found a measurement (origin %q)", origin)
		}
		return h.c.dryPlan("w"), shapeOf(prod), shapeOf(sp)
	}
	none, prodShape, scopedShape := plan("")
	if prodShape == scopedShape {
		t.Fatalf("one key for both shapes: %s", prodShape)
	}
	t.Logf("production key %s; scoped key %s", prodShape, scopedShape)
	// control: a production profile's measurement at 8192 would be derived from
	if other, _, _ := plan("w-8k"); other.DerivedFromCtx != 8192 {
		t.Fatalf("control: a production measurement at 8192 is not derived from (plan %+v); the test proves nothing", other)
	}
	got, _, _ := plan("w@e2")
	if got.DerivedFromCtx != 0 || got.Measured || got.NeedMiB != none.NeedMiB || got.NeedBasis != none.NeedBasis {
		t.Fatalf("production plan fed by the scoped measurement: need %d basis %q derived %d (without it: %d %q)",
			got.NeedMiB, got.NeedBasis, got.DerivedFromCtx, none.NeedMiB, none.NeedBasis)
	}
}

// A scoped load at production's own shape is the same shape: one key, so
// its measurement is that shape's, as production's own load's would be
// (pinned, so the rule is explicit).
func TestScopedVisibility_SameShapeIsOneKey(t *testing.T) {
	h := estHarness(t, 9000)
	sp := mustScoped(t, h, ScopedLoad{Model: "m", Scope: "same"})
	if shapeOf(sp) != shapeOf(h.c.cfg.Profiles["m"]) {
		t.Fatal("same shape, two keys")
	}
	h.c.putLearned(&learnedNeed{TotalMiB: 9900, key: needKeyOf(sp), profile: sp.Name, devices: 1, at: h.c.now.Add(-time.Minute)})
	if l := h.c.learnedAt(h.c.cfg.Profiles["m"], 1); l == nil || l.TotalMiB != 9900 {
		t.Fatalf("learned %+v", l)
	}
}
