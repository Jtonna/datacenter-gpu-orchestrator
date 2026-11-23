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
// part that grows with the window recomputed by the estimate's own formula
// for the requested window:
//
//	derived(k devices) = measured(ctx m, k) + (estimate(r) - estimate(m)) + k x (compute(r) - compute(m))
//
// estimate is the weights plus the KV cache (estimate.NeedMiB) and compute
// the per-device compute buffer (estimate.ComputeMiB); the weights cancel.
//
// Two checks keep it to the models the formula describes:
//
//   - The measurement must hold the estimate's cache: measured(m) at least
//     estimate(m) - deriveToleranceMiB. A model whose real cache is smaller
//     than the formula's (a sliding-window model such as Gemma measures a
//     fraction of it) fails, and nothing is derived from that row: the
//     formula's delta would be far off, below the real figure going down.
//   - The model's own pairs: every pair of its trusted measurements at two
//     contexts on the same device count, each predicted from the other
//     (derivePairs). When it has pairs, all must be within the tolerance,
//     else nothing is derived; the derived figure then counts as measured
//     (measured margin). When it has none, the derived figure is raised by
//     the estimate margin: nothing checked the formula on this model.
//
// Nothing derived: the need falls through to the estimate. The first load of
// the shape records its own measurement, which replaces the derived figure.

// deriveToleranceMiB is how far a prediction of one stored measurement from
// another (derivePairs) may be off, either way, for the derivation to count
// as checked; and how far a measurement may be below the estimate of its own
// window and still hold the estimate's cache.
const deriveToleranceMiB = 512

// deriveName prefixes the name of the profiles the estimate of another
// context is asked for: no configured profile has it (names are validated),
// so a provider's cache never mixes them up with a real one.
const deriveName = "\x00derive "

// DerivePair is one check of the derivation: the measurement at ToCtx
// predicted from the one at FromCtx, both on Devices devices. A pair whose
// FromMiB is below the estimate of its window (BelowEstimate) is listed but
// not used: no derivation starts from such a row.
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

// ctxDelta is what the estimate adds going from the window of from to that
// of to: the total (weights cancel: KV only) and the per-device compute
// buffer, and the estimate of from itself. ok is false when either is not an
// estimate (a command profile, or a provider that does not estimate).
func (c *schedCore) ctxDelta(from, to needKey) (total, perDevice, fromEst int, ok bool) {
	if from.kind == core.KindCommand || to.kind == core.KindCommand {
		return 0, 0, 0, false
	}
	a, err := c.needs.Need(profileOfKey(from))
	if err != nil || a.Need.Measured || a.Need.MiB <= 0 {
		return 0, 0, 0, false
	}
	b, err := c.needs.Need(profileOfKey(to))
	if err != nil || b.Need.Measured || b.Need.MiB <= 0 || b.FileBytes != a.FileBytes {
		return 0, 0, 0, false
	}
	return b.Need.MiB - a.Need.MiB, b.DeviceMiB - a.DeviceMiB, a.Need.MiB, true
}

// holdsEstimate: a measurement of total at a window whose estimate is est
// holds the estimate's cache (see the top of the file).
func holdsEstimate(total, est int) bool { return total+deriveToleranceMiB >= est }

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
				dt, dd, est, ok := c.ctxDelta(a.key, b.key)
				if !ok {
					continue
				}
				pred := a.TotalMiB + dt + a.count()*dd
				e := pred - b.TotalMiB
				out = append(out, DerivePair{Model: a.key.model, Shape: b.key.shape(), Devices: a.count(), FromCtx: a.key.ctx, ToCtx: b.key.ctx,
					FromMiB: a.TotalMiB, PredictedMiB: pred, MeasuredMiB: b.TotalMiB, ErrMiB: e, FromAt: a.at, ToAt: b.at,
					WithinTolerance: abs(e) <= deriveToleranceMiB, BelowEstimate: !holdsEstimate(a.TotalMiB, est)})
			}
		}
	}
	slices.SortFunc(out, func(x, y DerivePair) int {
		return cmp.Or(strings.Compare(x.Model, y.Model), cmp.Compare(x.Devices, y.Devices), cmp.Compare(x.FromCtx, y.FromCtx),
			cmp.Compare(x.ToCtx, y.ToCtx), strings.Compare(x.Shape, y.Shape))
	})
	return out
}

// deriveCheck is the check a derivation for shape k rests on: its model's
// own pairs (those starting from a row that holds the estimate). ok: every
// one is within tolerance, or there is none (unchecked then). worst is the
// largest error by size, the underestimate on a tie.
type deriveCheck struct {
	ok, unchecked bool
	pairs, worst  int
}

func (c *schedCore) checkDerivation(k needKey) deriveCheck {
	g := k.noCtx()
	ch := deriveCheck{ok: true}
	for _, p := range c.derivePairs(&g) {
		if p.BelowEstimate {
			continue
		}
		ch.pairs++
		if abs(p.ErrMiB) > abs(ch.worst) || (abs(p.ErrMiB) == abs(ch.worst) && p.ErrMiB < ch.worst) {
			ch.worst = p.ErrMiB
		}
		ch.ok = ch.ok && p.WithinTolerance
	}
	ch.unchecked = ch.pairs == 0
	return ch
}

// derivedSource picks the measurements a derivation for p would start from:
// those of the nearest other context (the larger one on a tie) of the same
// model and settings. nil when there is none.
func (c *schedCore) derivedSource(p *core.Profile, fileBytes int64) []*learnedNeed {
	key := needKeyOf(p)
	g := key.noCtx()
	best := -1
	var out []*learnedNeed
	for _, ls := range c.learned {
		for _, l := range ls {
			if l.TotalMiB <= 0 || l.key.noCtx() != g || l.key.ctx == key.ctx {
				continue
			}
			if l.modelBytes > 0 && fileBytes > 0 && l.modelBytes != fileBytes {
				continue
			}
			ctx := l.key.ctx
			switch {
			case best < 0 || abs(ctx-key.ctx) < abs(best-key.ctx) || (abs(ctx-key.ctx) == abs(best-key.ctx) && ctx > best):
				best, out = ctx, []*learnedNeed{l}
			case ctx == best:
				out = append(out, l)
			}
		}
	}
	slices.SortFunc(out, func(a, b *learnedNeed) int { return a.count() - b.count() })
	return out
}

// withDerived is withLearned for a profile without measurements of its own
// shape: the derived figures when a derivation applies (see the top of the
// file), else info unchanged.
func (c *schedCore) withDerived(p *core.Profile, info, base NeedInfo) NeedInfo {
	if p.Kind == core.KindCommand || base.Need.Measured {
		return info // a configured need is never replaced
	}
	src := c.derivedSource(p, base.FileBytes)
	if len(src) == 0 {
		return info
	}
	dt, dd, est, ok := c.ctxDelta(src[0].key, needKeyOf(p))
	if !ok {
		return info
	}
	ch := c.checkDerivation(needKeyOf(p))
	if !ch.ok {
		return info
	}
	figs := make([]learnedFig, 0, len(src))
	k, latest := 0, time.Time{}
	for _, l := range src {
		if !holdsEstimate(l.TotalMiB, est) {
			continue
		}
		t := l.TotalMiB + dt + l.count()*dd
		if ch.unchecked {
			t = estimate.MarginMiB(t, permille(c.cfg.Defaults.EstimateMargin))
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
	info.figs, info.derive = figs, &deriveFrom{ctx: src[0].key.ctx, check: ch}
	info.baseNeed, info.baseSource, info.perDevice = info.Need, info.Source, c.perDeviceCost(base)
	if len(p.ForceNodes) > 0 {
		k = len(p.ForceNodes)
	}
	return info.at(max(k, 1))
}

// deriveFrom is what a NeedInfo keeps of the derivation its figs come from.
type deriveFrom struct {
	ctx   int
	check deriveCheck
}

// derivedWords is the basis of a derived need.
func (c *schedCore) derivedWords(info NeedInfo) string {
	s := fmt.Sprintf("derived from the measurement at ctx %d (%s)", info.DerivedFromCtx, dateOf(info.MeasuredAt))
	if info.BaseMiB > 0 {
		s += fmt.Sprintf("; the estimate is %d MiB", info.BaseMiB)
	}
	s += info.countWords()
	if ch := info.derive.check; ch.unchecked {
		s += fmt.Sprintf("; no pair of this model's measurements checks the derivation yet: +%s%% margin",
			strconv.FormatFloat(c.cfg.Defaults.EstimateMargin*100, 'f', -1, 64))
	} else {
		s += fmt.Sprintf("; checked on %d pairs of this model's measurements, worst error %+d MiB", ch.pairs, ch.worst)
	}
	return s
}
