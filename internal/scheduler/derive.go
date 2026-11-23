package scheduler

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
)

// Derived needs. A llama profile with no measurement of its own shape (nor,
// for a host: node profile, of the same shape hosted on the scheduler's
// machine) but with one of the same model at another context, with
// everything else that keys a shape equal (slots, cache type, flash
// attention, extra arguments, host kind), takes its need from that
// measurement instead of the generic estimate: the measured total, which
// holds the weights and every fixed overhead as they really are, with the
// part that grows with the window recomputed by the estimate's own formula:
//
//	derived(r, d devices) = measured(m, d) + (estimate(r) - estimate(m)) + d x (compute(r) - compute(m))
//
// estimate is the weights plus the KV cache (estimate.NeedMiB), compute the
// per-device compute buffer (estimate.ComputeMiB); the weights cancel.
//
// Write E(x) = estimate(x) + d x compute(x): the generic figure without its
// margin and device overhead. A measurement holds its estimate when
// measured(m) >= E(m) - deriveToleranceMiB.
//
// The direction matters. A model whose real window cost is k times the
// formula's has truth(r) - derived(r) = (k - 1) x (formula(r) - formula(m)):
// with k <= 1 (the formula over-counts, as for sliding-window or MLA
// caches) an upward derivation (r > m) over-predicts, safe, and a downward
// one under-predicts. Three modes:
//
//   - checked: the model has pairs of its own measurements at two contexts
//     (each predicted from the other, from sources that hold their
//     estimate), all within deriveToleranceMiB. Both directions, from a
//     source that holds its estimate; the figure counts as measured
//     (measured margin).
//   - conservative: the model's pairs disagree, but no upward pair
//     under-predicts by more than the tolerance (the model uses less than
//     the formula), or the source sits below its estimate (which proves
//     k < 1: the weights are the file, compute and overhead are not
//     negative). Upward only, with the estimate margin, and only where the
//     figure is below the generic estimate path's; else the estimate.
//   - unchecked: no pair of its own and a source that holds its estimate.
//     k is not known, so upward only, with the estimate margin.
//
// An upward pair that under-predicts by more than the tolerance (a model
// that costs more than the formula) stops every derivation for the model.
//
// What is guaranteed (TestDerivedNeed_Invariant): a derived figure is never
// below both the truth and E(r) - deriveToleranceMiB. From a source that
// holds its estimate, derived(r) - E(r) = measured(m) - E(m) >= -tolerance,
// in either direction, whatever k; from a source below its estimate k < 1,
// and the derivation is upward, so it is at least the truth. With k > 1 and
// no pair to show it, nothing does better than the generic estimate itself,
// which under-counts such a model too.
//
// The first load of the shape records its own measurement, which replaces
// the derived figure.

// deriveToleranceMiB is how far a prediction of one stored measurement from
// another may be off for the derivation to count as checked, and how far a
// measurement may be below the estimate of its own window and still hold it.
const deriveToleranceMiB = 512

// deriveName prefixes the name of the profiles the estimate of another
// context is asked for: no configured profile has it (names are validated),
// so a provider's cache never mixes them up with a real one.
const deriveName = "\x00derive "

// How a derived need was derived (derive.go, top).
type deriveMode int

const (
	deriveChecked deriveMode = iota + 1
	deriveConservative
	deriveUnchecked
)

// DerivePair is one check of the derivation: the measurement at ToCtx
// predicted from the one at FromCtx, both on Devices devices. A pair whose
// FromMiB is below the estimate of its window (BelowEstimate) is listed but
// does not count towards checked.
type DerivePair struct {
	Model           string
	Shape           string // the measured shape at ToCtx
	Devices         int
	FromCtx, ToCtx  int
	FromMiB         int
	PredictedMiB    int
	MeasuredMiB     int
	ErrMiB          int // PredictedMiB - MeasuredMiB: below 0 is an underestimate
	FromAt, ToAt    time.Time
	WithinTolerance bool
	BelowEstimate   bool
}

// noCtx is k with the context left out: the shapes a derivation may use.
func (k needKey) noCtx() needKey {
	k.ctx = 0
	return k
}

// profileOfKey is a profile with exactly the need-relevant settings of k.
func profileOfKey(k needKey) *core.Profile {
	p := &core.Profile{Name: deriveName + k.shape(), Kind: k.kind, Model: k.model, Ctx: k.ctx, Parallel: k.parallel,
		KVQuant: k.kvQuant, FlashAttn: k.flashAttn, Host: k.host}
	if k.extraArgs != "" {
		p.ExtraArgs = strings.Split(k.extraArgs, "\x00")
	}
	return p
}

// ctxStep is what the estimate says about two windows of one model: the
// change of the total (KV only: the weights cancel) and of the per-device
// compute buffer, and the source window's estimate and compute buffer.
type ctxStep struct {
	total, perDevice int
	fromEst, fromDev int
}

// ctxDelta is the ctxStep from the window of from to that of to. ok is
// false when either is not an estimate (a command profile, or a provider
// that does not estimate).
func (c *schedCore) ctxDelta(from, to needKey) (ctxStep, bool) {
	if from.kind == core.KindCommand || to.kind == core.KindCommand {
		return ctxStep{}, false
	}
	a, err := c.needs.Need(profileOfKey(from))
	if err != nil || a.Need.Measured || a.Need.MiB <= 0 {
		return ctxStep{}, false
	}
	b, err := c.needs.Need(profileOfKey(to))
	if err != nil || b.Need.Measured || b.Need.MiB <= 0 || b.FileBytes != a.FileBytes {
		return ctxStep{}, false
	}
	return ctxStep{total: b.Need.MiB - a.Need.MiB, perDevice: b.DeviceMiB - a.DeviceMiB, fromEst: a.Need.MiB, fromDev: a.DeviceMiB}, true
}

// derive is the figure of l moved to the other window of s.
func (s ctxStep) derive(l *learnedNeed) int { return l.TotalMiB + s.total + l.count()*s.perDevice }

// holds: measurement l holds its window's estimate, E(m) - tolerance.
func (s ctxStep) holds(l *learnedNeed) bool {
	return l.TotalMiB+deriveToleranceMiB >= s.fromEst+l.count()*s.fromDev
}

// derivePairs checks the derivation on every pair of trusted measurements
// of one model at two contexts on the same device count, everything else
// that keys a shape equal; only those of group when group is not nil.
func (c *schedCore) derivePairs(group *needKey) []DerivePair {
	byGroup := map[needKey][]*learnedNeed{}
	for _, ls := range c.learned {
		for _, l := range ls {
			if l.TotalMiB <= 0 || !c.trusted(l) || l.key.kind == core.KindCommand {
				continue
			}
			g := l.key.noCtx()
			if group != nil && g != *group {
				continue
			}
			byGroup[g] = append(byGroup[g], l)
		}
	}
	var out []DerivePair
	for _, ls := range byGroup {
		for _, a := range ls {
			for _, b := range ls {
				if a.key.ctx == b.key.ctx || a.count() != b.count() {
					continue
				}
				if a.modelBytes > 0 && b.modelBytes > 0 && a.modelBytes != b.modelBytes {
					continue // another file under the same name
				}
				st, ok := c.ctxDelta(a.key, b.key)
				if !ok {
					continue
				}
				pred := st.derive(a)
				e := pred - b.TotalMiB
				out = append(out, DerivePair{Model: a.key.model, Shape: b.key.shape(), Devices: a.count(), FromCtx: a.key.ctx, ToCtx: b.key.ctx,
					FromMiB: a.TotalMiB, PredictedMiB: pred, MeasuredMiB: b.TotalMiB, ErrMiB: e, FromAt: a.at, ToAt: b.at,
					WithinTolerance: abs(e) <= deriveToleranceMiB, BelowEstimate: !st.holds(a)})
			}
		}
	}
	slices.SortFunc(out, func(x, y DerivePair) int {
		return cmp.Or(strings.Compare(x.Model, y.Model), cmp.Compare(x.Devices, y.Devices), cmp.Compare(x.FromCtx, y.FromCtx),
			cmp.Compare(x.ToCtx, y.ToCtx), strings.Compare(x.Shape, y.Shape))
	})
	return out
}

// deriveCheck is what a model's own pairs say. checked: there are pairs
// from sources that hold their estimate and every pair is within the
// tolerance. upSafe: no upward pair under-predicts by more than it. worst is
// the largest error by size of the counted pairs, the underestimate on a tie.
type deriveCheck struct {
	pairs, worst    int
	checked, upSafe bool
	anyPairs        bool
}

func (c *schedCore) checkDerivation(k needKey) deriveCheck {
	g := k.noCtx()
	ch := deriveCheck{upSafe: true}
	allWithin := true
	for _, p := range c.derivePairs(&g) {
		ch.anyPairs = true
		allWithin = allWithin && p.WithinTolerance
		if p.ToCtx > p.FromCtx && p.ErrMiB < -deriveToleranceMiB {
			ch.upSafe = false
		}
		if p.BelowEstimate {
			continue
		}
		ch.pairs++
		if abs(p.ErrMiB) > abs(ch.worst) || (abs(p.ErrMiB) == abs(ch.worst) && p.ErrMiB < ch.worst) {
			ch.worst = p.ErrMiB
		}
	}
	ch.checked = ch.pairs > 0 && allWithin
	return ch
}

// derivedSources lists, by context, the measurements of the same model and
// settings at other windows than p's (same file when both sizes are known).
func (c *schedCore) derivedSources(p *core.Profile, fileBytes int64) map[int][]*learnedNeed {
	key := needKeyOf(p)
	g := key.noCtx()
	out := map[int][]*learnedNeed{}
	for _, ls := range c.learned {
		for _, l := range ls {
			if l.TotalMiB <= 0 || l.key.noCtx() != g || l.key.ctx == key.ctx {
				continue
			}
			if l.modelBytes > 0 && fileBytes > 0 && l.modelBytes != fileBytes {
				continue
			}
			out[l.key.ctx] = append(out[l.key.ctx], l)
		}
	}
	for _, ls := range out {
		slices.SortFunc(ls, func(a, b *learnedNeed) int { return a.count() - b.count() })
	}
	return out
}

// nearestCtx is the context of srcs nearest r (the larger on a tie), among
// those ok allows; -1 when none.
func nearestCtx(srcs map[int][]*learnedNeed, r int, ok func(int) bool) int {
	best := -1
	for ctx := range srcs {
		if !ok(ctx) {
			continue
		}
		if best < 0 || abs(ctx-r) < abs(best-r) || (abs(ctx-r) == abs(best-r) && ctx > best) {
			best = ctx
		}
	}
	return best
}

// withDerived is withLearned for a profile without measurements of its own
// shape: the derived figures when a derivation applies (see the top of the
// file), else info unchanged.
func (c *schedCore) withDerived(p *core.Profile, info, base NeedInfo) NeedInfo {
	if p.Kind == core.KindCommand || base.Need.Measured {
		return info // a configured need is never replaced
	}
	srcs := c.derivedSources(p, base.FileBytes)
	if len(srcs) == 0 {
		return info
	}
	key := needKeyOf(p)
	ch := c.checkDerivation(key)
	if !ch.upSafe {
		return info // the model costs more than the formula: no derivation
	}
	steps := map[int]ctxStep{}
	step := func(ctx int) (ctxStep, bool) {
		if s, ok := steps[ctx]; ok {
			return s, true
		}
		s, ok := c.ctxDelta(srcs[ctx][0].key, key)
		if ok {
			steps[ctx] = s
		}
		return s, ok
	}
	holding := func(ctx int) bool { // some row at ctx holds its estimate
		s, ok := step(ctx)
		return ok && slices.ContainsFunc(srcs[ctx], s.holds)
	}
	upward := func(ctx int) bool { _, ok := step(ctx); return ok && ctx < key.ctx }

	mode, from := deriveChecked, -1
	if ch.checked {
		from = nearestCtx(srcs, key.ctx, holding)
	}
	if from < 0 {
		from = nearestCtx(srcs, key.ctx, upward)
		mode = deriveUnchecked
		if from >= 0 && (ch.anyPairs || !holding(from)) {
			mode = deriveConservative
		}
	}
	if from < 0 {
		return info
	}
	st := steps[from]
	perDevice := c.perDeviceCost(base)
	figs := make([]learnedFig, 0, len(srcs[from]))
	k, latest := 0, time.Time{}
	for _, l := range srcs[from] {
		t := st.derive(l)
		switch {
		case mode == deriveChecked && !st.holds(l):
			continue
		case mode != deriveChecked:
			t = estimate.MarginMiB(t, permille(c.cfg.Defaults.EstimateMargin))
		}
		if mode == deriveConservative && t >= info.Need.WithMargin()+l.count()*perDevice {
			continue // no better than the estimate path: keep the estimate
		}
		if t <= 0 {
			continue
		}
		figs = append(figs, learnedFig{devices: l.count(), total: t, restored: l.restored, trusted: c.trusted(l), at: l.at})
		if k == 0 || l.at.After(latest) {
			k, latest = l.count(), l.at
		}
	}
	if len(figs) == 0 {
		return info
	}
	info.figs, info.derive = figs, &deriveFrom{ctx: from, check: ch, mode: mode}
	info.baseNeed, info.baseSource, info.perDevice = info.Need, info.Source, perDevice
	if len(p.ForceNodes) > 0 {
		k = len(p.ForceNodes)
	}
	return info.at(max(k, 1))
}

// deriveFrom is what a NeedInfo keeps of the derivation its figs come from.
type deriveFrom struct {
	ctx   int
	check deriveCheck
	mode  deriveMode
}

// derivedWords is the basis of a derived need.
func (c *schedCore) derivedWords(info NeedInfo) string {
	d := info.derive
	margin := strconv.FormatFloat(c.cfg.Defaults.EstimateMargin*100, 'f', -1, 64)
	var s string
	switch d.mode {
	case deriveConservative:
		s = fmt.Sprintf("derived upward from ctx %d (conservative: this model uses less than the formula), measured %s", info.DerivedFromCtx, dateOf(info.MeasuredAt))
	case deriveUnchecked:
		s = fmt.Sprintf("derived upward from the measurement at ctx %d (%s)", info.DerivedFromCtx, dateOf(info.MeasuredAt))
	default:
		s = fmt.Sprintf("derived from the measurement at ctx %d (%s)", info.DerivedFromCtx, dateOf(info.MeasuredAt))
	}
	if info.BaseMiB > 0 {
		s += fmt.Sprintf("; the estimate is %d MiB", info.BaseMiB)
	}
	s += info.countWords()
	switch d.mode {
	case deriveConservative:
		s += fmt.Sprintf("; +%s%% margin", margin)
	case deriveUnchecked:
		s += fmt.Sprintf("; no pair of this model's measurements checks the derivation yet: +%s%% margin", margin)
	default:
		s += fmt.Sprintf("; checked on %d pairs of this model's measurements, worst error %+d MiB", d.check.pairs, d.check.worst)
	}
	return s
}
