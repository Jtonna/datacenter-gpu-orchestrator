package scheduler

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
)

// The model's own slope. Where a model has measurements of one placement
// shape (same model file, slots, cache type, flash attention, extra
// arguments, host kind, AND device count) at two or more contexts, its real
// memory per context token on that device count is read from its own rows:
// the need at another context r is the straight line through the two rows
// nearest r, in both directions. The validation on the live store (294
// leave-one-out predictions over seven models, Gemma's sliding window among
// them) found every series exactly linear on one device count; across
// device counts the per-device part differs by model (and the first extra
// device costs more than the next ones), so a line never joins rows of two
// device counts.
//
//   - Confirmed: a third row (or more) on the same count lies on the line
//     within slopeBoundMiB. The figure counts as a measurement.
//   - Two points only: the formula's step is the other witness. Where it
//     predicts one row from the other within the bound (the same pair check
//     as derive.go's, at the line's tighter bound), the line counts as
//     confirmed (by the formula); otherwise nothing checks it and the
//     figure gets the estimate margin.
//   - A row on the count off the line by more than the bound: no line on
//     that count (the model is not straight there).
//   - A sliding-window model (the header's <arch>.attention.sliding_window)
//     is concave in the context: steeper below its window, where the window
//     layers still grow. A chord across the window under-predicts between
//     its ends. So rows below the window are not used and nothing is derived
//     below it; and, as a second guard (the window llama.cpp uses may differ
//     from the header's), between two rows its line is used only when
//     confirmed (three collinear points of a concave function make it
//     straight between them).
//   - A falling line (less memory for a longer window) is not used.
//
// Order of sources (needcount.go, withLearned): a measurement of the shape
// on the same count; the server-hosted twin for a host: node profile; a
// confirmed line, which joins the shape's own measurements as a row on its
// count (needFor decides between them as between measurements, so a real
// measurement on more devices still counts where it is higher); a
// measurement of the shape on another count; a two-point line (+ estimate
// margin); the formula-step derivation (derive.go: upward unless checked);
// the estimate.

// slopeBoundMiB is how far a third row may be off the line for the line to
// count as confirmed. The stored totals are sums of buffer sizes: on one
// program build and one file a series is exact, and any real deviation
// means something else changed.
const slopeBoundMiB = 128

// NeedOwnSlope is a need read from the model's own line (ownslope.go).
const NeedOwnSlope NeedSource = "derived, own slope"

// DerivedSlope is the line an own-slope need is read from.
type DerivedSlope struct {
	Devices     int
	MiBPer1kCtx float64
	FromCtx     []int // the two measurements the line goes through
	ConfirmedBy []int // further measurements on the line within the bound
	// ByFormula: two rows only, and the formula's step predicts one from
	// the other within the bound.
	ByFormula bool
	Confirmed bool
}

// slopeFig is the line's figure at r on one device count.
type slopeFig struct {
	total int
	line  *DerivedSlope
	from  []*learnedNeed
}

// ownSlope is the line of rows (one placement group, any count) on devices
// devices at the context of key, or ok false.
func (c *schedCore) ownSlope(rows []*learnedNeed, key needKey, devices int, fileBytes int64, window int) (slopeFig, bool) {
	swa := window > 0
	if key.ctx < window {
		return slopeFig{}, false // below a sliding window: not straight there
	}
	var pts []*learnedNeed
	for _, l := range rows {
		if l.count() != devices || l.TotalMiB <= 0 || l.key.ctx == key.ctx || l.key.ctx < max(window, 1) || !c.trusted(l) {
			continue
		}
		if l.modelBytes > 0 && fileBytes > 0 && l.modelBytes != fileBytes {
			continue
		}
		pts = append(pts, l)
	}
	if len(pts) < 2 {
		return slopeFig{}, false
	}
	r := key.ctx
	slices.SortStableFunc(pts, func(a, b *learnedNeed) int {
		return cmp.Or(cmp.Compare(abs(a.key.ctx-r), abs(b.key.ctx-r)), cmp.Compare(b.key.ctx, a.key.ctx))
	})
	a, b := pts[0], pts[1]
	if a.key.ctx > b.key.ctx {
		a, b = b, a
	}
	dx, dy := b.key.ctx-a.key.ctx, b.TotalMiB-a.TotalMiB
	if dy < 0 {
		return slopeFig{}, false
	}
	at := func(x int) int { // the line at x, rounded up
		num := dy * (x - a.key.ctx)
		v := a.TotalMiB + num/dx
		if num%dx > 0 {
			v++
		}
		return v
	}
	line := &DerivedSlope{Devices: devices, MiBPer1kCtx: float64(dy) * 1024 / float64(dx), FromCtx: []int{a.key.ctx, b.key.ctx}}
	from := []*learnedNeed{a, b}
	for _, o := range pts[2:] {
		if abs(at(o.key.ctx)-o.TotalMiB) > slopeBoundMiB {
			return slopeFig{}, false // not straight on this count
		}
		line.ConfirmedBy = append(line.ConfirmedBy, o.key.ctx)
		from = append(from, o)
	}
	slices.Sort(line.ConfirmedBy)
	line.Confirmed = len(line.ConfirmedBy) > 0
	if swa && a.key.ctx < r && r < b.key.ctx && !line.Confirmed {
		return slopeFig{}, false // an unconfirmed chord of a concave model
	}
	t := at(r)
	if t <= 0 {
		return slopeFig{}, false
	}
	return slopeFig{total: t, line: line, from: from}, true
}

// slopeFigs is p's need figures from its own lines, one per device count
// that has one; two-point lines only with twoPoint (padded by the estimate
// margin).
func (c *schedCore) slopeFigs(p *core.Profile, base NeedInfo, twoPoint bool) []learnedFig {
	if p.Kind == core.KindCommand || base.Need.Measured {
		return nil
	}
	key := needKeyOf(p)
	e := c.deriveIndex().groups[key.noCtx()]
	if e == nil {
		return nil
	}
	if _, ok := c.derive.est[key]; !ok && base.Need.MiB > 0 { // the profile's own estimate is the one at its window
		if c.derive.est == nil {
			c.derive.est = map[needKey]NeedInfo{}
		}
		c.derive.est[key] = base
	}
	counts := map[int]bool{}
	for _, l := range e.rows {
		counts[l.count()] = true
	}
	var out []learnedFig
	for _, dv := range slices.Sorted(mapKeys(counts)) {
		f, ok := c.ownSlope(e.rows, key, dv, base.FileBytes, base.window)
		if ok && !f.line.Confirmed {
			a, b := f.from[0], f.from[1]
			if st, sok := c.ctxDelta(a.key, b.key); sok && abs(st.derive(a)-b.TotalMiB) <= slopeBoundMiB {
				f.line.Confirmed, f.line.ByFormula = true, true
			}
		}
		if !ok || (!f.line.Confirmed && !twoPoint) {
			continue
		}
		t := f.total
		if !f.line.Confirmed {
			t = estimate.MarginMiB(t, permille(c.cfg.Defaults.EstimateMargin))
		}
		fig := learnedFig{devices: dv, total: t, trusted: true, slope: f.line}
		for _, l := range f.from {
			fig.restored = fig.restored || l.restored
			if l.at.After(fig.at) {
				fig.at = l.at
			}
		}
		out = append(out, fig)
	}
	return out
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// slopeWords is the basis of an own-slope need.
func (c *schedCore) slopeWords(info NeedInfo) string {
	sl := info.Slope
	s := fmt.Sprintf("own slope of this model on %s: %s MiB per 1k context, from ctx %d and %d", plural(sl.Devices, "device"),
		strconv.FormatFloat(sl.MiBPer1kCtx, 'f', -1, 64), sl.FromCtx[0], sl.FromCtx[1])
	if !info.MeasuredAt.IsZero() {
		s += fmt.Sprintf(" (latest %s)", dateOf(info.MeasuredAt))
	}
	if sl.ByFormula {
		s += fmt.Sprintf("; confirmed by the formula's step within %d MiB", slopeBoundMiB)
	} else if sl.Confirmed {
		cs := make([]string, len(sl.ConfirmedBy))
		for i, x := range sl.ConfirmedBy {
			cs[i] = strconv.Itoa(x)
		}
		s += fmt.Sprintf("; confirmed by ctx %s within %d MiB", strings.Join(cs, ", "), slopeBoundMiB)
	}
	if info.BaseMiB > 0 {
		s += fmt.Sprintf("; the estimate is %d MiB", info.BaseMiB)
	}
	s += info.countWords()
	if !sl.Confirmed {
		s += fmt.Sprintf("; no third measurement confirms it: +%s%% margin", strconv.FormatFloat(c.cfg.Defaults.EstimateMargin*100, 'f', -1, 64))
	}
	return s
}

// plural is "1 device" or "n devices".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
