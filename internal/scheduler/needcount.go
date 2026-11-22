package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
)

// Measurements by device count (review SB1). A measured total includes one
// compute buffer and one device overhead per device, so a load on one node
// does not say what the same shape takes on three. Within a shape the
// learned needs are kept per device count (c.learned[shape][devices]), and
// the need of a placement of k devices is:
//
//   - a measurement on k devices: its total, never below a total measured
//     on fewer devices;
//   - else, with measurements on fewer devices: max(estimate, each such
//     total raised by the estimator's per-device cost for every extra
//     device);
//   - else (only more devices measured): max(estimate, the total of the
//     closest count above).
//
// A learned figure replaces a configured needs_mib only when it is higher,
// as before. A restored measurement of another llama build (trusted) is
// used only where it is above the estimate, until a load of this run
// measures that count again (review SS2). Per-node figures (and peaks) are used only for the same node
// set and split (shareFor), inside the entry of their device count.

// count is the number of devices the measurement was taken on.
func (l *learnedNeed) count() int {
	switch {
	case l.devices > 0:
		return l.devices
	case len(l.placement) > 0:
		return len(l.placement)
	}
	return 1
}

// putLearned keeps l under its shape and device count.
func (c *schedCore) putLearned(l *learnedNeed) {
	s := l.key.shape()
	if c.learned[s] == nil {
		c.learned[s] = map[int]*learnedNeed{}
	}
	c.learned[s][l.count()] = l
}

// learnedAt is the measurement of p's shape on k devices, else nil.
func (c *schedCore) learnedAt(p *core.Profile, k int) *learnedNeed {
	l := c.learned[shapeOf(p)][k]
	if l == nil || l.key != needKeyOf(p) {
		return nil
	}
	return l
}

// learnedOf is every measurement of p's shape, by device count.
func (c *schedCore) learnedOf(p *core.Profile) []*learnedNeed {
	key := needKeyOf(p)
	var out []*learnedNeed
	for _, l := range c.learned[key.shape()] {
		if l.key == key {
			out = append(out, l)
		}
	}
	slices.SortFunc(out, func(a, b *learnedNeed) int { return a.count() - b.count() })
	return out
}

// displayCount is the device count p's need is shown for when no placement
// is given: its forced node count, else the count of its latest measurement
// (0 when there is none).
func (c *schedCore) displayCount(p *core.Profile) int {
	if len(p.ForceNodes) > 0 {
		return len(p.ForceNodes)
	}
	k := 0
	var at time.Time
	for _, l := range c.learnedOf(p) {
		if k == 0 || l.at.After(at) {
			k, at = l.count(), l.at
		}
	}
	return k
}

// displayLearned is the measurement shown for p (status, plans): the one on
// its display count, else nil.
func (c *schedCore) displayLearned(p *core.Profile) *learnedNeed {
	return c.learnedAt(p, c.displayCount(p))
}

// learnedFig is what a NeedInfo keeps of one measurement of its shape.
type learnedFig struct {
	devices  int
	total    int
	restored bool
	trusted  bool
	at       time.Time
}

// trusted: l was measured this run, or on this llama build (on this release
// when either build is not known).
func (c *schedCore) trusted(l *learnedNeed) bool {
	if !l.restored {
		return true
	}
	if l.build != "" && c.llamaBuild != "" {
		return release.SameCommit(l.build, c.llamaBuild)
	}
	return l.release != "" && l.release == c.buildRelease
}

// perDeviceCost is the estimator's cost of one more device: the fixed
// device overhead plus the compute buffer (NeedInfo.DeviceMiB).
func (c *schedCore) perDeviceCost(base NeedInfo) int {
	return max(c.cfg.Defaults.DeviceOverheadMiB, 0) + max(base.DeviceMiB, 0)
}

// at is the need for a placement of k devices (see the top of this file).
// Without a measurement of the shape, or with k < 1, it is info itself.
func (info NeedInfo) at(k int) NeedInfo {
	if len(info.figs) == 0 || k < 1 {
		return info
	}
	out := info
	out.Devices, out.FromDevices = k, 0
	out.Need, out.Source, out.MeasuredAt = info.baseNeed, info.baseSource, time.Time{}
	est := info.baseNeed.MiB
	if !info.baseNeed.Measured {
		est = info.baseNeed.WithMargin() + k*info.perDevice
	}
	var src *learnedFig
	fig, exact := 0, false
	for i := range info.figs {
		if f := &info.figs[i]; f.devices == k {
			src, exact = f, true
		}
	}
	switch {
	case exact:
		for _, f := range info.figs {
			if f.devices <= k {
				fig = max(fig, f.total)
			}
		}
	default:
		for i := range info.figs {
			f := &info.figs[i]
			if f.devices < k {
				fig = max(fig, f.total+(k-f.devices)*info.perDevice)
				src = f // the closest below, as the figs are sorted
			}
		}
		if src == nil {
			src = &info.figs[0] // the closest above
			fig = src.total
		}
	}
	if fig <= 0 || (fig <= est && (info.baseNeed.Measured || !exact || !src.trusted)) {
		return out
	}
	out.Need, out.Source, out.MeasuredAt, out.FromDevices = placer.Measured(fig), NeedLearned, src.at, src.devices
	if src.restored {
		out.Source = NeedRestored
	}
	return out
}

// needAt is p's need for a placement of k devices.
func (c *schedCore) needAt(p *core.Profile, k int) (NeedInfo, error) {
	info, err := c.need(p)
	if err != nil {
		return info, err
	}
	return info.at(k), nil
}

// withLearned adds p's measurements to info (the provider's figure with the
// estimate margin applied) and returns the need for its display count.
func (c *schedCore) withLearned(p *core.Profile, info, base NeedInfo) NeedInfo {
	ls := c.learnedOf(p)
	if len(ls) == 0 {
		return info
	}
	info.baseNeed, info.baseSource, info.perDevice = info.Need, info.Source, c.perDeviceCost(base)
	info.figs = make([]learnedFig, 0, len(ls))
	for _, l := range ls {
		if l.TotalMiB > 0 {
			info.figs = append(info.figs, learnedFig{devices: l.count(), total: l.TotalMiB, restored: l.restored, trusted: c.trusted(l), at: l.at})
		}
	}
	if len(info.figs) == 0 {
		return info
	}
	return info.at(max(c.displayCount(p), 1))
}

// countWords is how a figure measured on another device count was carried
// over, for the need's basis; "" for one measured on the same count.
func (info NeedInfo) countWords() string {
	if info.FromDevices == 0 || info.FromDevices == info.Devices {
		return ""
	}
	if info.FromDevices < info.Devices {
		return fmt.Sprintf("; measured on %d devices, raised by %d MiB for each of %d more", info.FromDevices, info.perDevice, info.Devices-info.FromDevices)
	}
	return fmt.Sprintf("; measured on %d devices, kept for %d", info.FromDevices, info.Devices)
}

// LearnedRow is one stored measurement, for the admin listing (GET
// /api/needs): its shape, the device count it was measured on, the
// figure, where it came from, and the profiles configured now that use it.
type LearnedRow struct {
	Shape      string
	Kind       core.Kind
	Model      string
	Ctx        int
	Parallel   int
	KVQuant    string
	FlashAttn  bool
	ExtraArgs  []string
	Devices    int
	TotalMiB   int
	Placement  []core.NodeID
	MeasuredBy string
	MeasuredAt time.Time
	Release    string
	LlamaBuild string
	Restored   bool
	// Trusted is false for a restored measurement of another llama build:
	// it counts only where it is above the estimate.
	Trusted  bool
	Profiles []string
}

// learnedRows lists every measurement, by shape and device count.
func (c *schedCore) learnedRows() []LearnedRow {
	users := map[string][]string{}
	for name, p := range c.cfg.Profiles {
		users[shapeOf(p)] = append(users[shapeOf(p)], name)
	}
	var out []LearnedRow
	for shape, ls := range c.learned {
		for _, l := range ls {
			r := LearnedRow{Shape: shape, Kind: l.key.kind, Model: l.key.model, Ctx: l.key.ctx, Parallel: l.key.parallel,
				KVQuant: l.key.kvQuant, FlashAttn: l.key.flashAttn, Devices: l.count(), TotalMiB: l.TotalMiB,
				Placement: slices.Clone(l.placement), MeasuredBy: l.profile, MeasuredAt: l.at, Release: l.release,
				LlamaBuild: l.build, Restored: l.restored, Trusted: c.trusted(l), Profiles: slices.Sorted(slices.Values(users[shape]))}
			if l.key.extraArgs != "" {
				r.ExtraArgs = strings.Split(l.key.extraArgs, "\x00")
			}
			if r.Profiles == nil {
				r.Profiles = []string{}
			}
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b LearnedRow) int {
		if a.Shape != b.Shape {
			return strings.Compare(a.Shape, b.Shape)
		}
		return a.Devices - b.Devices
	})
	return out
}

// forgetShape drops every measurement of a shape, in memory and in the
// store; it reports whether there was one.
func (c *schedCore) forgetShape(shape string) bool {
	had := len(c.learned[shape]) > 0
	if had {
		c.dropLearned(shape, "", "forgotten by an operator")
	}
	return had
}
