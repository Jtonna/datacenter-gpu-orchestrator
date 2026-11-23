package scheduler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
)

// A model whose own pairs agree derives in both directions, as measured.
func TestDerivedNeed_CheckedDerivesDownward(t *testing.T) {
	h := deriveHarness(t)
	q := h.llama("q", 0, func(p *core.Profile) { p.Model, p.Ctx = "qwen.gguf", 6144 })
	h.measure("qwen.gguf", 2048, 1, 5000)
	at8k := derivedFrom(5000, 2048, 8192, 1) + 100
	h.measure("qwen.gguf", 8192, 1, at8k)
	info, _ := h.c.need(q)
	if want := derivedFrom(at8k, 8192, 6144, 1); info.Source != NeedDerived || info.Need.MiB != want || !info.Need.Measured || info.DerivedFromCtx != 8192 {
		t.Fatalf("%+v %q from %d, want %d", info.Need, info.Source, info.DerivedFromCtx, want)
	}
}

// One measurement and no pair: upward only, +5%; downward keeps the estimate.
func TestDerivedNeed_UncheckedIsUpwardOnly(t *testing.T) {
	h := deriveHarness(t)
	down := h.llama("down", 0, func(p *core.Profile) { p.Model, p.Ctx = "qwen.gguf", 2048 })
	up := h.llama("up", 0, func(p *core.Profile) { p.Model, p.Ctx = "qwen.gguf", 16384 })
	h.measure("qwen.gguf", 8192, 1, 6000)
	if info, _ := h.c.need(down); info.Source != NeedEstimated {
		t.Fatalf("downward: %+v %q", info.Need, info.Source)
	}
	info, _ := h.c.need(up)
	if want := estimate.MarginMiB(derivedFrom(6000, 8192, 16384, 1), 50); info.Source != NeedDerived || info.Need.MiB != want {
		t.Fatalf("upward: %+v %q, want %d", info.Need, info.Source, want)
	}
}

// Pairs out of tolerance because the model uses less than the formula (the
// sliding-window pattern: upward pairs over-predict, downward ones
// under-predict): upward only, estimate margin, and only where the figure is
// below the estimate path's; a model whose upward pairs under-predict (it
// uses more than the formula) gets nothing derived.
func TestDerivedNeed_ConservativeWhenTheModelUsesLess(t *testing.T) {
	h := deriveHarness(t)
	mid := h.llama("mid", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 8192 })
	far := h.llama("far", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 65536 })
	low := h.llama("low", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 1024 })
	h.measure("swa.gguf", 2048, 1, 5000)
	at32k := derivedFrom(5000, 2048, 32768, 1) - 2000
	h.measure("swa.gguf", 32768, 1, at32k)
	ch := h.c.checkDerivation(needKeyOf(far))
	if ch.checked || !ch.upSafe {
		t.Fatalf("%+v", ch)
	}
	info, _ := h.c.need(far)
	if want := estimate.MarginMiB(derivedFrom(at32k, 32768, 65536, 1), 50); info.Source != NeedDerived || info.Need.MiB != want || info.DerivedFromCtx != 32768 {
		t.Fatalf("far: %+v %q from %d, want %d", info.Need, info.Source, info.DerivedFromCtx, want)
	}
	if b := h.c.needBasis(info); !strings.HasPrefix(b, "derived upward from ctx 32768 (conservative: this model uses less than the formula)") {
		t.Fatal(b)
	}
	// From 2048, whose row sits above its estimate, the figure is not below
	// the estimate path: the estimate stays.
	est, _ := h.c.needs.Need(mid)
	if d := estimate.MarginMiB(derivedFrom(5000, 2048, 8192, 1), 50); d < est.Need.WithMargin()+h.c.perDeviceCost(est) {
		t.Fatalf("the test's premise: derived %d is below the estimate path", d)
	}
	if info, _ := h.c.need(mid); info.Source != NeedEstimated {
		t.Fatalf("mid: %+v %q", info.Need, info.Source)
	}
	if info, _ := h.c.need(low); info.Source != NeedEstimated {
		t.Fatalf("downward: %+v %q", info.Need, info.Source)
	}

	// A model above the formula, its 2048 row right at its estimate (so a
	// figure derived from it would be below the estimate path).
	h = deriveHarness(t)
	between := h.llama("between", 0, func(p *core.Profile) { p.Model, p.Ctx = "more.gguf", 16384 })
	at2k := 4000 + 2048/8 + estimate.ComputeMiB(2048)
	h.measure("more.gguf", 2048, 1, at2k)
	h.measure("more.gguf", 32768, 1, derivedFrom(at2k, 2048, 32768, 1)+2000)
	if info, _ := h.c.need(between); info.Source != NeedEstimated {
		t.Fatalf("a model above the formula: %+v %q from %d", info.Need, info.Source, info.DerivedFromCtx)
	}
}

// The property: a synthetic model whose real window cost is kf times the
// formula's, with fixed overhead o above the weights, measured at some
// contexts. For kf <= 1 a derived figure is never below the truth. For any
// kf the invariant holds: a derived figure is never below both the truth
// and E(r) - deriveToleranceMiB, E being the estimate without margin and
// device overhead (derive.go, top).
func TestDerivedNeed_Invariant(t *testing.T) {
	ctxs := []int{1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072}
	formula := func(ctx, d int) int { return ctx/8 + d*estimate.ComputeMiB(ctx) }
	derived, checked := 0, 0
	for _, kf := range []float64{0.05, 0.25, 0.5, 0.9, 1, 1.02, 1.1, 1.5, 3} {
		for _, o := range []int{0, 300, 1500} {
			for _, d := range []int{1, 2} {
				for _, at := range [][]int{{2048}, {8192}, {2048, 8192}, {1024, 32768}, {4096, 16384, 65536}} {
					truth := func(ctx int) int { return 4000 + o + int(kf*float64(formula(ctx, d))) }
					for _, r := range ctxs {
						h := deriveHarness(t)
						for _, m := range at {
							if m != r {
								h.measure("syn.gguf", m, d, truth(m))
							}
						}
						var force []string
						if d == 2 {
							force = []string{"a", "b"}
						}
						p := h.llama("p", 0, func(p *core.Profile) { p.Model, p.Ctx, p.ForceNodes = "syn.gguf", r, force })
						info, err := h.c.need(p)
						if err != nil {
							t.Fatal(err)
						}
						if info.Source != NeedDerived {
							continue
						}
						derived++
						if info.derive.mode == deriveChecked {
							checked++
						}
						what := fmt.Sprintf("kf %v o %d d %d at %v r %d: need %d truth %d (from %d, mode %d)", kf, o, d, at, r, info.Need.MiB, truth(r), info.DerivedFromCtx, info.derive.mode)
						if info.Devices != d {
							t.Fatalf("%s: devices %d", what, info.Devices)
						}
						if kf <= 1 && info.DerivedFromCtx < r && info.Need.MiB < truth(r) {
							t.Fatalf("upward below the truth: %s", what)
						}
						if e := 4000 + formula(r, d); info.Need.MiB < truth(r) && info.Need.MiB < e-deriveToleranceMiB {
							t.Fatalf("invariant: %s, E(r) %d", what, e)
						}
					}
				}
			}
		}
	}
	if derived < 100 || checked < 10 {
		t.Fatalf("too few derivations exercised: %d (%d checked)", derived, checked)
	}
}
