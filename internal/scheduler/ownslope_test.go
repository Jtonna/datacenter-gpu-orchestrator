package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
)

// lineAt is the total of a line through (a, ta) and (b, tb) at r, rounded up.
func lineAt(a, ta, b, tb, r int) int {
	num := (tb - ta) * (r - a)
	v := ta + num/(b-a)
	if num%(b-a) > 0 {
		v++
	}
	return v
}

// A model measured at three contexts on one device count, on a line whose
// step is a fraction of the formula's (a sliding-window or MLA cache):
// every other context on that count takes the line, in both directions,
// as a measurement (the third point confirms it); the basis names the rows.
func TestOwnSlope_ConfirmedLineBothDirections(t *testing.T) {
	h := deriveHarness(t)
	// 40 MiB per 1k context, against the formula's 128 + 4 per 1k
	h.measure("m.gguf", 2048, 6, 8000)
	h.measure("m.gguf", 8192, 6, 8240)
	h.measure("m.gguf", 32768, 6, 9200)
	for _, c := range []struct{ ctx, want int }{{16384, 8560}, {65536, 10480}, {1024, 7960}} {
		p := h.llama("p", 0, func(p *core.Profile) { p.Model, p.Ctx, p.ForceNodes = "m.gguf", c.ctx, []string{"1", "2", "3", "4", "5", "6"} })
		info, _ := h.c.need(p)
		if info.Source != NeedOwnSlope || info.Need.MiB != c.want || !info.Need.Measured || info.Devices != 6 {
			t.Fatalf("ctx %d: %+v %q on %d, want %d by the own slope", c.ctx, info.Need, info.Source, info.Devices, c.want)
		}
		if sl := info.Slope; sl == nil || sl.MiBPer1kCtx != 40 || len(sl.FromCtx) != 2 || len(sl.ConfirmedBy) != 1 || sl.Devices != 6 {
			t.Fatalf("ctx %d: slope %+v", c.ctx, info.Slope)
		}
		b := h.c.needBasis(info)
		if !strings.HasPrefix(b, "own slope of this model on 6 devices: 40 MiB per 1k context, from ctx ") || !strings.Contains(b, "confirmed by ctx ") {
			t.Fatalf("ctx %d: %s", c.ctx, b)
		}
		r := h.c.dryPlan("p")
		if r.DerivedSlope == nil || r.DerivedSlope.MiBPer1kCtx != 40 || r.NeedMiB != c.want {
			t.Fatalf("plan: %+v", r)
		}
		delete(h.cfg.Profiles, "p")
		h.c.needs_ = map[string]NeedInfo{}
	}
}

// Two points and no third: the line holds nothing to check it against, so
// it is used with the estimate margin, in both directions.
func TestOwnSlope_TwoPointsGetTheEstimateMargin(t *testing.T) {
	h := deriveHarness(t)
	h.measure("m.gguf", 2048, 1, 5000)
	h.measure("m.gguf", 8192, 1, 5300)
	for _, ctx := range []int{4096, 32768, 1024} {
		p := h.llama("p", 0, func(p *core.Profile) { p.Model, p.Ctx = "m.gguf", ctx })
		info, _ := h.c.need(p)
		want := estimate.MarginMiB(lineAt(2048, 5000, 8192, 5300, ctx), 50)
		if info.Source != NeedOwnSlope || info.Need.MiB != want {
			t.Fatalf("ctx %d: %+v %q, want %d", ctx, info.Need, info.Source, want)
		}
		if b := h.c.needBasis(info); !strings.Contains(b, "no third measurement confirms it: +5% margin") {
			t.Fatal(b)
		}
		delete(h.cfg.Profiles, "p")
		h.c.needs_ = map[string]NeedInfo{}
	}
}

// A third point off the line by more than the bound: the model is not on a
// line on that count, so no own slope; what is left is the formula step.
func TestOwnSlope_NotWhenAPointIsOffTheLine(t *testing.T) {
	h := deriveHarness(t)
	h.measure("m.gguf", 2048, 1, 5000)
	h.measure("m.gguf", 8192, 1, 5300)
	h.measure("m.gguf", 32768, 1, lineAt(2048, 5000, 8192, 5300, 32768)+slopeBoundMiB+1)
	p := h.llama("p", 0, func(p *core.Profile) { p.Model, p.Ctx = "m.gguf", 16384 })
	if info, _ := h.c.need(p); info.Source == NeedOwnSlope {
		t.Fatalf("own slope through a point off the line: %+v", info.Need)
	}

	// A falling line (less memory for a longer window) is not a slope.
	h = deriveHarness(t)
	h.measure("m.gguf", 2048, 1, 5300)
	h.measure("m.gguf", 8192, 1, 5000)
	p = h.llama("p", 0, func(p *core.Profile) { p.Model, p.Ctx = "m.gguf", 16384 })
	if info, _ := h.c.need(p); info.Source == NeedOwnSlope {
		t.Fatalf("a falling line: %+v", info.Need)
	}
}

// Lines are drawn within one device count only: rows on different counts
// never make one, and never make a pair of the formula-step check either.
func TestOwnSlope_NeverAcrossDeviceCounts(t *testing.T) {
	h := deriveHarness(t)
	h.measure("m.gguf", 2048, 1, 10391)
	h.measure("m.gguf", 8192, 2, 12190)
	p := h.llama("p", 0, func(p *core.Profile) { p.Model, p.Ctx, p.ForceNodes = "m.gguf", 32768, []string{"a"} })
	if info, _ := h.c.need(p); info.Source == NeedOwnSlope {
		t.Fatalf("a line across device counts: %+v", info.Need)
	}
	if ps := h.c.derivePairs(nil); len(ps) != 0 {
		t.Fatalf("a formula-step pair across device counts: %+v", ps)
	}
}

// A sliding-window model is concave in the context: a line through two
// points under-predicts between them unless a third point shows it is
// straight there; outside them it over-predicts (safe).
func TestOwnSlope_SlidingWindowInterpolatesOnlyWhenConfirmed(t *testing.T) {
	h := deriveHarness(t)
	h.measure("swa.gguf", 2048, 1, 5000)
	h.measure("swa.gguf", 32768, 1, 6200)
	mid := h.llama("mid", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 8192 })
	up := h.llama("up", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 65536 })
	if info, _ := h.c.need(mid); info.Source == NeedOwnSlope {
		t.Fatalf("an unconfirmed chord of a sliding-window model: %+v", info.Need)
	}
	if info, _ := h.c.need(up); info.Source != NeedOwnSlope {
		t.Fatalf("extrapolation of a sliding-window model: %+v %q", info.Need, info.Source)
	}
	h.measure("swa.gguf", 16384, 1, lineAt(2048, 5000, 32768, 6200, 16384))
	if info, _ := h.c.need(mid); info.Source != NeedOwnSlope || !info.Need.Measured {
		t.Fatalf("a confirmed line of a sliding-window model: %+v %q", info.Need, info.Source)
	}
}

// The order of sources: a measurement of the shape on the same count wins.
// A confirmed line joins the shape's measurements as a row on its count, so
// needFor decides as between measurements: it replaces "kept for fewer",
// and a measurement on more devices, moved down by the per-device cost,
// still counts where it is higher (a real measurement is never undercut by
// a line). A two-point line does not take part at all.
func TestOwnSlope_OrderOfSources(t *testing.T) {
	h := deriveHarness(t)
	h.measure("m.gguf", 2048, 1, 5000)
	h.measure("m.gguf", 8192, 1, 5300)
	h.measure("m.gguf", 32768, 1, lineAt(2048, 5000, 8192, 5300, 32768))
	h.measure("m.gguf", 16384, 2, 6200) // the shape itself, on two devices: 6200 - 664 is below the line
	one := h.llama("one", 0, func(p *core.Profile) { p.Model, p.Ctx, p.ForceNodes = "m.gguf", 16384, []string{"a"} })
	info, _ := h.c.need(one)
	if want := lineAt(2048, 5000, 8192, 5300, 16384); info.Source != NeedOwnSlope || info.Need.MiB != want {
		t.Fatalf("confirmed line against the shape on another count: %+v %q, want %d", info.Need, info.Source, want)
	}
	h.measure("m.gguf", 16384, 2, 6600) // 6600 - 664 is above the line: the measurement counts
	if info, _ := h.c.need(one); info.Source == NeedOwnSlope || info.Need.MiB != 6600-664 {
		t.Fatalf("a measurement on more devices above the line: %+v %q", info.Need, info.Source)
	}
	h.measure("m.gguf", 16384, 2, 6200)
	h.measure("m.gguf", 16384, 1, 5800) // the shape itself on this count
	if info, _ := h.c.need(one); info.Source == NeedOwnSlope || info.Need.MiB != 5800 {
		t.Fatalf("its own measurement: %+v %q", info.Need, info.Source)
	}

	h = deriveHarness(t)
	h.measure("m.gguf", 2048, 1, 5000)
	h.measure("m.gguf", 8192, 1, 5300)
	h.measure("m.gguf", 16384, 2, 6500)
	one = h.llama("one", 0, func(p *core.Profile) { p.Model, p.Ctx, p.ForceNodes = "m.gguf", 16384, []string{"a"} })
	if info, _ := h.c.need(one); info.Source == NeedOwnSlope || info.FromDevices != 2 {
		t.Fatalf("a two-point line over a measurement on another count: %+v %q", info.Need, info.Source)
	}
}

// Two rows whose line the formula's step agrees with: the formula is the
// other witness, so the line counts as confirmed (no margin), and ranks
// as a confirmed line.
func TestOwnSlope_TwoPointsConfirmedByTheFormula(t *testing.T) {
	h := deriveHarness(t)
	h.measure("m.gguf", 2048, 1, 5000)
	h.measure("m.gguf", 8192, 1, derivedFrom(5000, 2048, 8192, 1))
	p := h.llama("p", 0, func(p *core.Profile) { p.Model, p.Ctx = "m.gguf", 32768 })
	info, _ := h.c.need(p)
	if want := derivedFrom(5000, 2048, 32768, 1); info.Source != NeedOwnSlope || info.Need.MiB != want || !info.Slope.ByFormula {
		t.Fatalf("%+v %q %+v, want %d", info.Need, info.Source, info.Slope, want)
	}
	if b := h.c.needBasis(info); !strings.Contains(b, "confirmed by the formula's step within 128 MiB") || strings.Contains(b, "margin") {
		t.Fatal(b)
	}
}

// The property for sliding-window models: a synthetic model whose window
// layers grow only up to its window W (concave in the context: steeper
// below W, flatter above) is measured at assorted contexts, some below W.
// A line through a row below W and one above (a chord across the window)
// would under-predict between them; with the rule (rows below the window
// are not used, nothing is derived below it, and a chord is used only
// when confirmed) no own-slope figure is below the truth beyond the
// rounding a line carries.
func TestOwnSlope_SlidingWindowNeverUnderPredicts(t *testing.T) {
	const w = 4096
	truth := func(ctx int) int { return 6000 + ctx/64 + min(ctx, w)/4 } // full layers + window layers
	ctxs := []int{1024, 2048, 3072, 4096, 6144, 8192, 16384, 32768, 65536}
	lines := 0
	for i, a := range ctxs {
		for _, b := range ctxs[i+1:] {
			for _, extra := range []int{0, 32768} {
				for _, r := range ctxs {
					if r == a || r == b || r == extra {
						continue
					}
					h := deriveHarness(t)
					h.window["swa.gguf"] = w
					h.measure("swa.gguf", a, 1, truth(a))
					h.measure("swa.gguf", b, 1, truth(b))
					if extra != 0 && extra != a && extra != b {
						h.measure("swa.gguf", extra, 1, truth(extra))
					}
					p := h.llama("p", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", r })
					info, _ := h.c.need(p)
					if info.Source != NeedOwnSlope {
						continue
					}
					lines++
					if info.Need.MiB < truth(r)-2 {
						t.Fatalf("under the truth: rows %d %d %d, r %d: need %d truth %d (%+v)", a, b, extra, r, info.Need.MiB, truth(r), info.Slope)
					}
					if r < w {
						t.Fatalf("a line below the window: rows %d %d %d, r %d: %+v", a, b, extra, r, info.Slope)
					}
					for _, c := range info.Slope.FromCtx {
						if c < w {
							t.Fatalf("a line through a row below the window: %+v", info.Slope)
						}
					}
				}
			}
		}
	}
	if lines < 50 {
		t.Fatalf("only %d lines exercised", lines)
	}
}

// A line decides only on its own device count. Live (rel/v28 gate): a line
// on 2 devices, moved up by four per-device costs, rose above the shape's
// own measurement on 6 devices at the same context; a line on 6 devices was
// kept for 2- and 3-console profiles. Neither may happen: on another count a
// profile takes what it took without own slope (its shape's measurements
// under the count rule, else the formula step), and a line never decides
// over a measurement of the shape on the count it is placed on.
func TestOwnSlope_OnlyOnItsOwnCount(t *testing.T) {
	h := deriveHarness(t)
	h.measure("m.gguf", 2048, 6, 15321)
	h.measure("m.gguf", 8192, 2, 15257)
	h.measure("m.gguf", 16384, 2, lineAt(8192, 15257, 32768, 19289, 16384))
	h.measure("m.gguf", 32768, 2, 19289)
	auto := h.llama("auto", 0, func(p *core.Profile) { p.Model, p.Ctx = "m.gguf", 2048 })
	if info, _ := h.c.need(auto); info.Source == NeedOwnSlope || info.Need.MiB != 15321 || info.Devices != 6 {
		t.Fatalf("a line on 2 devices over the shape's measurement on 6: %+v %q on %d", info.Need, info.Source, info.Devices)
	}
	line := lineAt(8192, 15257, 32768, 19289, 2048)
	if info, _ := h.c.needAt(auto, 2); info.Source != NeedOwnSlope || info.Need.MiB != line {
		t.Fatalf("the line on its own count: %+v %q, want %d", info.Need, info.Source, line)
	}
	for _, k := range []int{1, 3, 4, 5} {
		info, _ := h.c.needAt(auto, k)
		if info.Source == NeedOwnSlope || info.Need.MiB != 15321 {
			t.Fatalf("on %d devices: %+v %q, want the measurement on 6 kept (15321)", k, info.Need, info.Source)
		}
		if b := h.c.needBasis(info); strings.Contains(b, "own slope") {
			t.Fatalf("on %d devices: %s", k, b)
		}
	}

	// Rows on 6 devices only: a profile forced onto 2 takes the formula
	// step (as before own slope), never the 6-device line kept for 2; one
	// not forced shows the line on 6.
	h = deriveHarness(t)
	h.measure("m.gguf", 8192, 6, 30000)
	h.measure("m.gguf", 16384, 6, derivedFrom(30000, 8192, 16384, 6))
	h.measure("m.gguf", 32768, 6, derivedFrom(30000, 8192, 32768, 6))
	two := h.llama("two", 0, func(p *core.Profile) { p.Model, p.Ctx, p.ForceNodes = "m.gguf", 2048, []string{"a", "b"} })
	info, _ := h.c.need(two)
	if info.Source != NeedDerived || info.Slope != nil {
		t.Fatalf("a line on 6 devices for a profile forced onto 2: %+v %q %+v", info.Need, info.Source, info.Slope)
	}
	if b := h.c.needBasis(info); strings.Contains(b, "own slope") {
		t.Fatal(b)
	}
	free := h.llama("free", 0, func(p *core.Profile) { p.Model, p.Ctx = "m.gguf", 2048 })
	if info, _ := h.c.need(free); info.Source != NeedOwnSlope || info.Devices != 6 || info.FromDevices != 6 || strings.Contains(h.c.needBasis(info), "measured on") {
		t.Fatalf("the line on its own count, not forced: %+v %q on %d: %s", info.Need, info.Source, info.Devices, h.c.needBasis(info))
	}
	if info, _ := h.c.needAt(free, 3); info.Source != NeedDerived {
		t.Fatalf("the same profile placed on 3: %+v %q", info.Need, info.Source)
	}
}
