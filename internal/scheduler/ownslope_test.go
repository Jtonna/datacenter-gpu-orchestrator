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
