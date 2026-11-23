package scheduler

import (
	"cmp"
	"fmt"
	"hash/maphash"
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
	fileBytes        int64
}

// ctxDelta is the ctxStep from the window of from to that of to. ok is
// false when either is not an estimate (a command profile, or a provider
// that does not estimate).
func (c *schedCore) ctxDelta(from, to needKey) (ctxStep, bool) {
	if from.kind == core.KindCommand || to.kind == core.KindCommand {
		return ctxStep{}, false
	}
	a, ok := c.deriveEstimate(from)
	if !ok {
		return ctxStep{}, false
	}
	b, ok := c.deriveEstimate(to)
	if !ok || b.FileBytes != a.FileBytes {
		return ctxStep{}, false
	}
	return ctxStep{total: b.Need.MiB - a.Need.MiB, perDevice: b.DeviceMiB - a.DeviceMiB, fromEst: a.Need.MiB, fromDev: a.DeviceMiB,
		fileBytes: a.FileBytes}, true
}

// deriveEstimate is the provider's estimate for the settings of k, asked
// once: it depends on the model file and k only, so it is kept across
// configuration writes (the provider's own cache is not). A change of the
// models folder forgets them all (applyConfig); withDerived forgets a
// model's when its file size no longer matches.
func (c *schedCore) deriveEstimate(k needKey) (NeedInfo, bool) {
	if e, ok := c.derive.est[k]; ok {
		return e, true
	}
	c.derive.estCalls++
	e, err := c.needs.Need(profileOfKey(k))
	if err != nil || e.Need.Measured || e.Need.MiB <= 0 {
		return NeedInfo{}, false // not kept: a missing file may appear
	}
	if c.derive.est == nil {
		c.derive.est = map[needKey]NeedInfo{}
	}
	c.derive.est[k] = e
	return e, true
}

// forgetEstimates drops the kept estimates of one model file name, and the
// pair checks built on them.
func (c *schedCore) forgetEstimates(model string) {
	for k := range c.derive.est {
		if k.model == model {
			delete(c.derive.est, k)
		}
	}
	c.derive.groups = nil
}

// derive is the figure of l moved to the other window of s.
func (s ctxStep) derive(l *learnedNeed) int { return l.TotalMiB + s.total + l.count()*s.perDevice }

// holds: measurement l holds its window's estimate, E(m) - tolerance.
func (s ctxStep) holds(l *learnedNeed) bool {
	return l.TotalMiB+deriveToleranceMiB >= s.fromEst+l.count()*s.fromDev
}

// deriveState is what the derivation keeps between calls, all of it in the
// core goroutine: the estimates of other contexts, and, for one state of
// the store (sum), the measurements by group and each group's pairs and
// check, rebuilt for a group only when its own rows change. A configuration
// write changes none of it: the work is done once per state of the store,
// not per profile, listing or configuration revision.
type deriveState struct {
	est    map[needKey]NeedInfo
	seed   maphash.Seed
	seeded bool
	sum    uint64
	groups map[needKey]*deriveGroup
	all    []DerivePair // every group's pairs, sorted; nil until asked
	// work done (tests)
	estCalls, pairBuilds int
}

type deriveGroup struct {
	rows     []*learnedNeed // every row with a total, any trust, by context then devices
	sum      uint64
	built    bool
	builtSum uint64
	pairs    []DerivePair
	check    deriveCheck
}

// deriveRowSig is what of a row the derivation depends on; the row itself
// by its address (its shape never changes), the rest by value (a row may be
// updated in place).
type deriveRowSig struct {
	row          *learnedNeed
	devices, mib int
	modelBytes   int64
	restored     bool
	build        string
	at           int64
}

// deriveIndex brings the kept state up to the store: one pass hashing the
// rows (no estimate, no pair) when nothing changed.
func (c *schedCore) deriveIndex() *deriveState {
	d := &c.derive
	if !d.seeded {
		d.seed, d.seeded = maphash.MakeSeed(), true
	}
	builds := maphash.Comparable(d.seed, [2]string{c.llamaBuild, c.servedBuild})
	rowSum := func(k int, l *learnedNeed) uint64 {
		return mix(maphash.Comparable(d.seed, deriveRowSig{l, k, l.TotalMiB, l.modelBytes, l.restored, l.build, l.at.UnixNano()}))
	}
	sum, n := builds, uint64(0)
	for _, ls := range c.learned {
		for k, l := range ls {
			sum += rowSum(k, l)
			n++
		}
	}
	sum += mix(n)
	if d.groups != nil && sum == d.sum {
		return d
	}
	old := d.groups
	d.sum, d.all, d.groups = sum, nil, map[needKey]*deriveGroup{}
	for _, ls := range c.learned {
		for k, l := range ls {
			if l.TotalMiB <= 0 || l.key.kind == core.KindCommand {
				continue
			}
			g := l.key.noCtx()
			e := d.groups[g]
			if e == nil {
				e = &deriveGroup{sum: builds}
				d.groups[g] = e
			}
			e.rows = append(e.rows, l)
			e.sum += rowSum(k, l)
		}
	}
	for g, e := range d.groups {
		slices.SortFunc(e.rows, func(a, b *learnedNeed) int { return cmp.Or(a.key.ctx-b.key.ctx, a.count()-b.count()) })
		if o := old[g]; o != nil && o.built && o.builtSum == e.sum {
			e.built, e.builtSum, e.pairs, e.check = true, e.sum, o.pairs, o.check
		}
	}
	return d
}

// mix spreads a hash so that sums of them do not cancel.
func mix(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return x
}

// group is the kept group g with its pairs and check built.
func (c *schedCore) group(g needKey) *deriveGroup { return c.groupIn(c.deriveIndex(), g) }

func (c *schedCore) groupIn(d *deriveState, g needKey) *deriveGroup {
	e := d.groups[g]
	if e == nil {
		return &deriveGroup{check: deriveCheck{upSafe: true}}
	}
	if !e.built {
		d.pairBuilds++
		e.pairs = c.pairsOf(e.rows)
		e.check = checkOf(e.pairs)
		e.built, e.builtSum = true, e.sum
	}
	return e
}

// derivePairs checks the derivation on every pair of trusted measurements
// of one model at two contexts on the same device count, everything else
// that keys a shape equal; only those of group when group is not nil.
func (c *schedCore) derivePairs(group *needKey) []DerivePair {
	if group != nil {
		return slices.Clone(c.group(*group).pairs)
	}
	d := c.deriveIndex()
	if d.all == nil {
		all := []DerivePair{}
		for g := range d.groups {
			all = append(all, c.groupIn(d, g).pairs...)
		}
		sortPairs(all)
		d.all = all
	}
	return slices.Clone(d.all)
}

func (c *schedCore) pairsOf(rows []*learnedNeed) []DerivePair {
	var out []DerivePair
	for _, a := range rows {
		if !c.trusted(a) {
			continue
		}
		for _, b := range rows {
			if a.key.ctx == b.key.ctx || a.count() != b.count() || !c.trusted(b) {
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
	sortPairs(out)
	return out
}

func sortPairs(out []DerivePair) {
	slices.SortFunc(out, func(x, y DerivePair) int {
		return cmp.Or(strings.Compare(x.Model, y.Model), cmp.Compare(x.Devices, y.Devices), cmp.Compare(x.FromCtx, y.FromCtx),
			cmp.Compare(x.ToCtx, y.ToCtx), strings.Compare(x.Shape, y.Shape))
	})
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

func (c *schedCore) checkDerivation(k needKey) deriveCheck { return c.group(k.noCtx()).check }

func checkOf(ps []DerivePair) deriveCheck {
	ch := deriveCheck{upSafe: true}
	allWithin := true
	for _, p := range ps {
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
// settings at other windows than p's (same file when both sizes are known),
// fewest devices first.
func (c *schedCore) derivedSources(d *deriveState, p *core.Profile, fileBytes int64) map[int][]*learnedNeed {
	key := needKeyOf(p)
	e := d.groups[key.noCtx()]
	if e == nil {
		return nil
	}
	out := map[int][]*learnedNeed{}
	for _, l := range e.rows {
		if l.key.ctx == key.ctx || (l.modelBytes > 0 && fileBytes > 0 && l.modelBytes != fileBytes) {
			continue
		}
		out[l.key.ctx] = append(out[l.key.ctx], l)
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
	d := c.deriveIndex()
	srcs := c.derivedSources(d, p, base.FileBytes)
	if len(srcs) == 0 {
		return info
	}
	key := needKeyOf(p)
	ch := c.groupIn(d, key.noCtx()).check
	if !ch.upSafe {
		return info // the model costs more than the formula: no derivation
	}
	if _, ok := c.derive.est[key]; !ok && base.Need.MiB > 0 { // the profile's own estimate is the one at its window
		if c.derive.est == nil {
			c.derive.est = map[needKey]NeedInfo{}
		}
		c.derive.est[key] = base
	}
	steps := map[int]ctxStep{}
	step := func(ctx int) (ctxStep, bool) {
		if s, ok := steps[ctx]; ok {
			return s, true
		}
		s, ok := c.ctxDelta(srcs[ctx][0].key, key)
		if ok && base.FileBytes > 0 && s.fileBytes != base.FileBytes { // the file changed since its estimates were kept
			c.forgetEstimates(key.model)
			s, ok = c.ctxDelta(srcs[ctx][0].key, key)
			ok = ok && s.fileBytes == base.FileBytes
		}
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
