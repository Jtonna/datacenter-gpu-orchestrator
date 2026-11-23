package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// deriveHarness: profiles whose estimate follows a fixed formula by context
// (4000 MiB of weights, 1 MiB of KV per 8 tokens, the real compute buffer of
// 4 KiB per token per device), for every profile and for the estimates the
// derivation asks for at other contexts.
func deriveHarness(t fatalT) *harness {
	h := newHarness(t)
	h.c.needs = NeedFunc(func(p *core.Profile) (NeedInfo, error) {
		if p.NeedsMiB > 0 {
			return NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 4000 << 20}, nil
		}
		weights := 4000
		if strings.HasPrefix(p.Model, "big") {
			weights = 20000
		}
		return NeedInfo{Need: placer.Estimated(weights + p.Ctx*max(p.Parallel, 1)/8), FileBytes: int64(weights) << 20,
			DeviceMiB: estimate.ComputeMiB(p.Ctx * max(p.Parallel, 1)), swa: strings.HasPrefix(p.Model, "swa")}, nil
	})
	h.c.llamaBuild, h.c.servedBuild = "aaaaaaa1", "aaaaaaa1"
	return h
}

// measure stores a measurement of model at ctx on devices devices.
func (h *harness) measure(model string, ctx, devices, total int, mod ...func(*core.Profile)) {
	p := &core.Profile{Name: "m", Kind: core.KindLlama, Model: model, Ctx: ctx, Parallel: 1}
	for _, m := range mod {
		m(p)
	}
	h.c.putLearned(&learnedNeed{TotalMiB: total, key: needKeyOf(p), profile: "m", devices: devices,
		at: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)})
	h.c.needs_ = map[string]NeedInfo{}
}

// the derivation's own arithmetic, for the test's expectations
func derivedFrom(total, fromCtx, toCtx, devices int) int {
	return total + (toCtx-fromCtx)/8 + devices*(estimate.ComputeMiB(toCtx)-estimate.ComputeMiB(fromCtx))
}

// A profile without a measurement of its own shape takes its need from the
// same model measured at another context: the measured total with the
// window's part recomputed, not the generic estimate. Its basis says so,
// and a measurement of its own shape replaces it.
func TestDerivedNeed_FromTheNearestContext(t *testing.T) {
	h := deriveHarness(t)
	q := h.llama("q", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 6144 }) // between its rows, no third: the line is not used
	h.measure("swa.gguf", 2048, 1, 5000)
	at8k := derivedFrom(5000, 2048, 8192, 1) + 100 // the formula is off by 100 MiB on this pair
	h.measure("swa.gguf", 8192, 1, at8k)

	info, err := h.c.need(q)
	if err != nil {
		t.Fatal(err)
	}
	want := derivedFrom(at8k, 8192, 6144, 1) // from the nearest context
	if info.Need.MiB != want || !info.Need.Measured || info.Source != NeedDerived || info.DerivedFromCtx != 8192 {
		t.Fatalf("need %+v source %q from ctx %d, want %d derived from 8192", info.Need, info.Source, info.DerivedFromCtx, want)
	}
	r := h.c.dryPlan("q")
	if r.NeedMiB != want || r.DerivedFromCtx != 8192 ||
		!strings.Contains(r.NeedBasis, "derived from the measurement at ctx 8192 (2026-11-20)") ||
		!strings.Contains(r.NeedBasis, "checked on 2 pairs of this model's measurements, worst error -100 MiB") {
		t.Fatalf("plan need %d from %d basis %q", r.NeedMiB, r.DerivedFromCtx, r.NeedBasis)
	}

	// Its first load measures its own shape: that replaces the derivation.
	h.measure("swa.gguf", 6144, 1, want+40)
	info, _ = h.c.need(q)
	if info.Need.MiB != want+40 || info.Source != NeedLearned || info.DerivedFromCtx != 0 {
		t.Fatalf("after its own measurement: %+v %q %d", info.Need, info.Source, info.DerivedFromCtx)
	}
}

// When the model's pairs of measurements disagree with the formula by more
// than the tolerance (the upward one under-predicting), nothing is derived
// by the formula step: the estimate stays (here between its two rows, where
// a sliding-window model's own line is not used either).
func TestDerivedNeed_NotUsedWhenItsPairsAreOutOfTolerance(t *testing.T) {
	h := deriveHarness(t)
	q := h.llama("q", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 4096 })
	h.measure("swa.gguf", 2048, 1, 5000)
	h.measure("swa.gguf", 8192, 1, derivedFrom(5000, 2048, 8192, 1)+deriveToleranceMiB+1)
	if info, _ := h.c.need(q); info.Source != NeedEstimated || info.DerivedFromCtx != 0 {
		t.Fatalf("%+v %q", info.Need, info.Source)
	}
	// Another model's good pairs do not overrule this model's own.
	h.measure("big.gguf", 2048, 1, 22000)
	h.measure("big.gguf", 8192, 1, derivedFrom(22000, 2048, 8192, 1))
	if info, _ := h.c.need(q); info.Source != NeedEstimated {
		t.Fatalf("%+v %q", info.Need, info.Source)
	}
}

// A model measured at one context only has no pair of its own to check the
// derivation: the derived figure is raised by the estimate margin, whatever
// other models' pairs say (one sliding-window model must not stop or vouch
// for the others).
func TestDerivedNeed_UncheckedModelGetsTheEstimateMargin(t *testing.T) {
	h := deriveHarness(t)
	q := h.llama("q", 0, func(p *core.Profile) { p.Model, p.Ctx = "qwen.gguf", 32768 })
	h.measure("qwen.gguf", 8192, 1, 6000)
	padded := estimate.MarginMiB(derivedFrom(6000, 8192, 32768, 1), permille(h.cfg.Defaults.EstimateMargin))
	check := func(what string) {
		t.Helper()
		info, _ := h.c.need(q)
		if info.Need.MiB != padded || !info.Need.Measured || info.Source != NeedDerived {
			t.Fatalf("%s: %+v %q, want %d", what, info.Need, info.Source, padded)
		}
		if b := h.c.needBasis(info); !strings.Contains(b, "no pair of this model's measurements checks the derivation yet: +5% margin") {
			t.Fatalf("%s: %s", what, b)
		}
	}
	check("no pairs")
	h.measure("big.gguf", 2048, 1, 22000)
	h.measure("big.gguf", 8192, 1, derivedFrom(22000, 2048, 8192, 1)+4000) // out of tolerance
	check("another model's pairs are bad")
}

// A measurement far below the estimate of its own window shows a model whose
// cache is smaller than the formula's (a sliding-window model): going down
// from it the formula's delta under-counts, so nothing is derived downward;
// going up it over-counts, so the derivation is used, with the estimate
// margin and only where it is below the estimate path.
func TestDerivedNeed_NotDownwardFromAMeasurementBelowItsEstimate(t *testing.T) {
	h := deriveHarness(t)
	q := h.llama("q", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 2048 })
	up := h.llama("up", 0, func(p *core.Profile) { p.Model, p.Ctx = "swa.gguf", 65536 })
	est32k := 4000 + 32768/8 + estimate.ComputeMiB(32768)
	low := est32k - deriveToleranceMiB - 1
	h.measure("swa.gguf", 32768, 1, low)
	if info, _ := h.c.need(q); info.Source != NeedEstimated || info.DerivedFromCtx != 0 {
		t.Fatalf("derived downward from a row below its estimate: %+v %q", info.Need, info.Source)
	}
	info, _ := h.c.need(up)
	want := estimate.MarginMiB(derivedFrom(low, 32768, 65536, 1), 50)
	if info.Source != NeedDerived || info.Need.MiB != want || info.DerivedFromCtx != 32768 {
		t.Fatalf("upward: %+v %q from %d, want %d", info.Need, info.Source, info.DerivedFromCtx, want)
	}
	if b := h.c.needBasis(info); !strings.HasPrefix(b, "derived upward from ctx 32768 (conservative: this model uses less than the formula)") ||
		!strings.Contains(b, "+5% margin") {
		t.Fatal(b)
	}
	// Such a row is listed in the check but not used in it.
	h.measure("swa.gguf", 8192, 1, 4000+8192/8+estimate.ComputeMiB(8192)+300)
	ps := h.c.derivePairs(nil)
	if len(ps) != 2 {
		t.Fatalf("%+v", ps)
	}
	for _, p := range ps {
		if p.BelowEstimate != (p.FromCtx == 32768) {
			t.Fatalf("%+v", p)
		}
	}
	if ch := h.c.checkDerivation(needKeyOf(q)); ch.pairs != 1 {
		t.Fatalf("pairs used %+v", ch)
	}
}

// Only the same model with every other shape setting equal is used; a
// configured need is never replaced; and a placement on as many devices as
// a measurement at the other context takes that row.
func TestDerivedNeed_OnlyTheSameSettings(t *testing.T) {
	h := deriveHarness(t)
	q := h.llama("q", 0, func(p *core.Profile) { p.Model, p.Ctx = "qwen.gguf", 32768 })
	h.measure("qwen.gguf", 8192, 1, 6000, func(p *core.Profile) { p.KVQuant = "q8_0" })
	h.measure("qwen.gguf", 8192, 1, 6000, func(p *core.Profile) { p.Parallel = 2 })
	h.measure("qwen.gguf", 8192, 1, 6000, func(p *core.Profile) { p.Host = core.HostNode })
	h.measure("other.gguf", 8192, 1, 6000)
	if info, _ := h.c.need(q); info.Source != NeedEstimated {
		t.Fatalf("other settings used: %+v %q from %d", info.Need, info.Source, info.DerivedFromCtx)
	}
	c := h.llama("c", 7000, func(p *core.Profile) { p.Model, p.Ctx = "qwen.gguf", 32768 })
	h.measure("qwen.gguf", 8192, 1, 6000)
	if info, _ := h.c.need(c); info.Need.MiB != 7000 || info.Source != NeedConfigured {
		t.Fatalf("configured need replaced: %+v %q", info.Need, info.Source)
	}

	h = deriveHarness(t) // measured on two devices only
	two := h.llama("two", 0, func(p *core.Profile) { p.Model, p.Ctx, p.ForceNodes = "qwen.gguf", 32768, []string{"a", "b"} })
	h.measure("qwen.gguf", 8192, 2, 6600)
	info, _ := h.c.need(two)
	if want := estimate.MarginMiB(derivedFrom(6600, 8192, 32768, 2), 50); info.Need.MiB != want || info.Devices != 2 || info.FromDevices != 2 {
		t.Fatalf("two devices: %+v from %d devices, want %d", info.Need, info.FromDevices, want)
	}
}

// The validation table: every pair of one model's measurements at two
// contexts on the same device count, each predicted from the other.
func TestDerivePairs(t *testing.T) {
	h := deriveHarness(t)
	h.measure("qwen.gguf", 2048, 1, 5000)
	h.measure("qwen.gguf", 8192, 1, derivedFrom(5000, 2048, 8192, 1)-200)
	h.measure("qwen.gguf", 8192, 2, 7000) // no partner on 2 devices
	ps := h.c.derivePairs(nil)
	if len(ps) != 2 {
		t.Fatalf("%+v", ps)
	}
	if p := ps[0]; p.FromCtx != 2048 || p.ToCtx != 8192 || p.ErrMiB != 200 || p.MeasuredMiB != p.PredictedMiB-200 || !p.WithinTolerance {
		t.Fatalf("%+v", p)
	}
	if p := ps[1]; p.FromCtx != 8192 || p.ToCtx != 2048 || p.ErrMiB != -200 {
		t.Fatalf("%+v", p)
	}
}
