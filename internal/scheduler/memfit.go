package scheduler

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// Memory safety on shared-memory nodes (a PS5: GPU memory is system RAM and
// the workers have no container limit, so a load that exhausts the machine
// makes it unreachable for many minutes). Three things live here:
//
//   - the margins on a need: the estimate margin (an estimate runs low) and
//     the split skew margin (a split model does not land evenly);
//   - what a successful load measured, kept for the life of the process and
//     used instead of the estimate (learned needs);
//   - the memory watch: a load is cancelled when a node it loads onto runs
//     low on memory.
//
// Nothing here blocks: it reads the fleet view the step already holds.

// permille turns a margin such as 0.15 into thousandths for the integer
// arithmetic of the fit rules.
func permille(m float64) int {
	if m <= 0 || math.IsNaN(m) {
		return 0
	}
	return int(math.Round(m * 1000))
}

// ---- learned needs ----

// learnedNode is what one node of a placement held after a load, with the
// share of the split it was given, so the figure is only reused for the same
// node set and split.
type learnedNode struct {
	ID       core.NodeID
	Fraction float64
	MiB      int
	// PeakMiB is the highest memory the node was seen to give up for the
	// profile while it stayed loaded, never below MiB once tracked; 0 when
	// none was seen (reserve.go).
	PeakMiB int
}

// learnedNeed is a profile's measurement from a load, of this run or restored
// from the store at start (needs.go).
type learnedNeed struct {
	TotalMiB int
	Nodes    []learnedNode // empty when the log did not account for every node

	key        needKey       // the need-relevant settings it was measured with (its shape)
	profile    string        // the profile that measured it last (for display)
	at         time.Time     // when it was measured (or raised)
	restored   bool          // read from the store at start, not measured this run
	release    string        // the release that measured it
	build      string        // the llama build that measured it; "" when not known
	cause      string        // needstore.CauseLoad or the exit reason that raised it
	placement  []core.NodeID // the node set it was measured on, in device order
	split      []float64     // each node's share of the split
	modelBytes int64         // the model file's size when measured; 0 when not known
	peakAt     time.Time     // when a node's peak was last raised; zero when none
	devices    int           // the device count it was measured on (count)
	// runHigh is the highest total a load of this run measured on this
	// count; floor is a restored total that still holds: the first load of
	// a run does not lower it until that load ends normally (releaseFloor).
	runHigh, floor int
}

// fractionTolerance is how far a node's split fraction may differ from the
// learned one for the per-node figure to count as the same split.
const fractionTolerance = 0.01

// shareFor is the learned MiB for node id when pl (with split fractions
// fracs) is the node set and split the measurement was taken with.
func (l *learnedNeed) shareFor(pl core.Placement, fracs []float64, id core.NodeID) (int, bool) {
	if l == nil || len(l.Nodes) == 0 || len(l.Nodes) != len(pl.Nodes) || len(fracs) != len(pl.Nodes) {
		return 0, false
	}
	got := 0
	found := false
	for i, pn := range pl.Nodes {
		var m *learnedNode
		for j := range l.Nodes {
			if l.Nodes[j].ID == pn.ID {
				m = &l.Nodes[j]
			}
		}
		if m == nil || math.Abs(m.Fraction-fracs[i]) > fractionTolerance {
			return 0, false
		}
		if pn.ID == id {
			got, found = m.MiB, true
		}
	}
	return got, found
}

// learnedShares is the per-node figures measured on pl's node set and split
// (fracs) for p's shape, for a need of need MiB on that many devices. It is
// nil when there are none, or when the measurement is not trusted (a
// restored figure of another llama build says nothing per node). When they
// add up to less than need (the need comes from another row: a smaller
// count scaled up, or a higher total) they are scaled up to it, the
// measured skew kept. It is the one source of stored per-node figures:
// expect_mib and the cache prediction (expectShares), the forced check
// (sharedShortfalls) and the reserved figures (reservedShares); fourth
// review, FS1.
func (c *schedCore) learnedShares(p *core.Profile, pl core.Placement, fracs []float64, need int) []int {
	l := c.planLearnedAt(p, len(pl.Nodes))
	if l == nil || !c.trusted(l) || len(pl.Nodes) == 0 {
		return nil
	}
	out := make([]int, len(pl.Nodes))
	sum := 0
	for i, pn := range pl.Nodes {
		m, ok := l.shareFor(pl, fracs, pn.ID)
		if !ok {
			return nil
		}
		out[i], sum = m, sum+m
	}
	if sum > 0 && sum < need {
		for i, m := range out {
			out[i] = int(math.Ceil(float64(m)*float64(need)/float64(sum) - 1e-6))
		}
	}
	return out
}

// effectiveNeed applies the estimate margin and the learned measurement to a
// provider's need.
//
//   - needs_mib (measured by the operator) is used as is;
//   - an estimate is raised by defaults.estimate_margin;
//   - a measurement learned this run, or restored at start, replaces an
//     estimate, and replaces a configured needs_mib only when it is higher (a
//     learned figure never lowers a configured one).
func (c *schedCore) effectiveNeed(p *core.Profile, base NeedInfo) NeedInfo {
	info := base
	info.BaseMiB = base.Need.MiB
	if base.Need.Measured {
		info.Source = NeedConfigured
	} else {
		info.Need = placer.EstimatedMargin(base.Need.MiB, permille(c.cfg.Defaults.EstimateMargin))
		info.Source = NeedEstimated
	}
	return c.withLearned(p, info, base) // needcount.go: by device count
}

// needBasis is the plain-words basis of a need for messages and plans.
func (c *schedCore) needBasis(info NeedInfo) string {
	if info.FromServer && (info.Source == NeedLearned || info.Source == NeedRestored) {
		info.FromServer = false
		return c.needBasis(info) + fmt.Sprintf("; hosted on the scheduler's machine (no measurement hosted on a node yet), plus %d MiB host overhead on the host node",
			max(c.cfg.Defaults.HostOverheadMiB, 0))
	}
	switch info.Source {
	case NeedDerived:
		if info.derive != nil {
			return c.derivedWords(info)
		}
	case NeedOwnSlope:
		if info.Slope != nil {
			return c.slopeWords(info)
		}
	case NeedEstimated:
		return fmt.Sprintf("estimated, +%s%% margin; the estimate is %d MiB",
			strconv.FormatFloat(c.cfg.Defaults.EstimateMargin*100, 'f', -1, 64), info.BaseMiB)
	case NeedLearned:
		if info.BaseMiB > 0 {
			return fmt.Sprintf("%s; %d MiB before", NeedLearned, info.BaseMiB) + info.countWords()
		}
		return string(info.Source) + info.countWords()
	case NeedRestored:
		s := fmt.Sprintf("measured on %s, restored at start", dateOf(info.MeasuredAt))
		if info.BaseMiB > 0 {
			s += fmt.Sprintf("; %d MiB before", info.BaseMiB)
		}
		return s + info.countWords()
	}
	return string(info.Source)
}

// dateOf is the day of t in UTC, or "an unknown date".
func dateOf(t time.Time) string {
	if t.IsZero() {
		return "an unknown date"
	}
	return t.UTC().Format("2006-01-02")
}

// loadByNode is what load reports on each node of in's placement (device
// order), and whether the node had a device in the report.
func (c *schedCore) loadByNode(in *inst, load instance.LoadStats) ([]float64, []bool) {
	byKey := map[string]int{}
	for i, pn := range in.placement.Nodes {
		switch {
		case pn.Local:
			byKey[c.build.LocalDevice] = i
		case pn.ID == in.placement.Host:
			byKey[string(pn.ID)] = i
		default:
			byKey[pn.Addr] = i
		}
	}
	mib := make([]float64, len(in.placement.Nodes))
	seen := make([]bool, len(mib))
	for _, d := range load.Devices {
		if d.Host {
			continue
		}
		key := endpointOf(d.Name)
		if key == "" {
			key = d.Name
			if h := in.placement.Host; h != "" {
				// A host on a node (phase 6): its own devices (Vulkan0, ...)
				// are the host node's; workers are named by their endpoint.
				key = string(h)
			}
		}
		if i, ok := byKey[key]; ok {
			mib[i] += d.TotalMiB()
			seen[i] = true
		}
	}
	return mib, seen
}

// reportedOn is what in's own load log reported on node id (MiB, rounded
// up); false before the log reports or when it names no device there.
func (c *schedCore) reportedOn(in *inst, id core.NodeID) (int, bool) {
	if in.child == nil {
		return 0, false
	}
	mib, seen := c.loadByNode(in, in.child.Load())
	for i, pn := range in.placement.Nodes {
		if pn.ID == id && seen[i] {
			return int(math.Ceil(mib[i] - 1e-6)), true
		}
	}
	return 0, false
}

// learnNeed records what a load that just became ready measured: the device
// total (the figure the log's memory_mib reports) and each node's own figure.
// A measurement above a configured needs_mib is warned about, once per profile.
// The figure is kept by shape and device count (needcount.go). The highest
// figure of this run is kept; a figure restored at start holds until the
// first load of this run on that count ends normally, so a lower first
// load does not lower it at once (releaseFloor). The result is queued for
// the store (needs.go).
func (c *schedCore) learnNeed(in *inst) {
	if in.prof.Kind != core.KindLlama || in.child == nil {
		return
	}
	load := in.child.Load()
	total := int(math.Ceil(load.TotalMiB() - 1e-6))
	if total <= 0 {
		return
	}
	key, k := needKeyOf(in.prof), len(in.placement.Nodes)
	l := c.learnedAt(in.prof, k)
	if l == nil {
		l = &learnedNeed{key: key, devices: k}
		c.putLearned(l)
	}
	l.profile = in.prof.Name
	if l.restored {
		l.floor = l.TotalMiB
	}
	l.runHigh = max(l.runHigh, total) // the highest seen this run: the safe side
	l.TotalMiB = max(l.runHigh, l.floor)
	l.restored, l.at, l.release, l.build, l.cause = false, c.now, c.buildRelease, c.llamaBuild, needstore.CauseLoad
	oldNodes, oldPlacement, oldPeakAt := l.Nodes, l.placement, l.peakAt
	l.placement = in.nodeIDs()
	l.Nodes, l.peakAt = nil, time.Time{}
	fracs := instance.SplitFractions(*in.prof, in.placement)
	l.split = slices.Clone(fracs)
	mib, seen := c.loadByNode(in, load)
	complete := len(mib) > 0 && len(fracs) == len(mib)
	for _, s := range seen {
		complete = complete && s
	}
	if complete {
		for i, pn := range in.placement.Nodes {
			l.Nodes = append(l.Nodes, learnedNode{ID: pn.ID, Fraction: fracs[i], MiB: int(math.Ceil(mib[i] - 1e-6))})
		}
		// A peak seen on the same node set and split is kept (reserve.go).
		if slices.Equal(oldPlacement, l.placement) && len(oldNodes) == len(l.Nodes) {
			for i := range l.Nodes {
				if o := oldNodes[i]; o.ID == l.Nodes[i].ID && math.Abs(o.Fraction-l.Nodes[i].Fraction) <= fractionTolerance && o.PeakMiB > 0 {
					l.Nodes[i].PeakMiB = o.PeakMiB
					l.peakAt = oldPeakAt
				}
			}
		}
	}
	base, ok := c.needs_[in.prof.Name]
	if ok {
		l.modelBytes = base.FileBytes
	}
	if ok && base.Need.Measured && total > base.Need.MiB && !c.warnedLow[in.prof.Name] {
		c.warnedLow[in.prof.Name] = true
		c.log.Warn("configured needs_mib is too low: the load measured more memory; using the measured value",
			"profile", in.prof.Name, "configured_mib", base.Need.MiB, "measured_mib", total)
		c.recordInst(in, history.TNeedsMiBTooLow, history.Warn,
			fmt.Sprintf("The configured needs_mib of %s (%d MiB) is too low: the load measured %d MiB, which is used from now on.",
				in.prof.Name, base.Need.MiB, total),
			map[string]any{"configured_mib": base.Need.MiB, "measured_mib": total})
	}
	c.log.Info("learned memory need", "profile", in.prof.Name, "measured_mib", total, "per_node", len(l.Nodes) > 0)
	perNode := make([]map[string]any, 0, len(l.Nodes))
	for _, n := range l.Nodes {
		perNode = append(perNode, map[string]any{"node": c.nodeName(n.ID), "fraction": n.Fraction, "mib": n.MiB})
	}
	c.recordInst(in, history.TLearnedNeed, history.Info,
		fmt.Sprintf("Measured the memory need of %s at %d MiB; fit decisions use it for the rest of this run.", in.prof.Name, total),
		map[string]any{"measured_mib": total, "highest_mib": l.TotalMiB, "per_node": perNode})
	c.storeLearned(l)
	c.changed = true
}

// LearnedNeed is one profile's entry of status' learned_needs: the
// measurement of a load (this run, or restored from the store at start) and
// the need in use now.
type LearnedNeed struct {
	MeasuredMiB   int // highest device total a load of this profile reported this run, or the restored figure
	NeedMiB       int // the need fit decisions use now (with the estimate margin, if still an estimate)
	Source        string
	ConfiguredMiB int // needs_mib, or 0 for a profile that relies on the estimate
	EstimatedMiB  int // the estimate before the margin; 0 for a profile with needs_mib or not computed yet
	Nodes         []LearnedNodeNeed

	Restored        bool          // read from the store at start, not measured this run
	MeasuredAt      time.Time     // when it was measured (or raised after a memory kill)
	Age             time.Duration // how long ago that was
	MeasuredRelease string        // the release that measured it

	// PeakMiB is the sum of the per-node peaks (reserve.go): the highest
	// memory the nodes gave up for the profile while it stayed loaded; 0
	// when none is stored. PeakBasis says what it was measured on.
	PeakMiB   int
	PeakBasis string
}

// LearnedNodeNeed is what one node held in the measured load, with its share
// of the split.
type LearnedNodeNeed struct {
	Node     core.NodeID
	Fraction float64
	MiB      int
	PeakMiB  int
}

func (c *schedCore) learnedNeeds() map[string]LearnedNeed {
	out := map[string]LearnedNeed{}
	for name, p := range c.cfg.Profiles {
		l := c.displayLearned(p)
		if l == nil {
			continue
		}
		base, ok := c.needs_[name]
		if !ok {
			// Not computed yet (a restored figure before any plan): no file
			// is read here, so the estimate is not known.
			base = NeedInfo{Need: placer.Estimated(0)}
			if p.NeedsMiB > 0 {
				base = NeedInfo{Need: placer.Measured(p.NeedsMiB)}
			}
		}
		info := c.effectiveNeed(p, base)
		e := LearnedNeed{MeasuredMiB: l.TotalMiB, NeedMiB: info.Need.WithMargin(), Source: string(info.Source),
			Restored: l.restored, MeasuredAt: l.at, Age: l.age(c.now), MeasuredRelease: l.release}
		if base.Need.Measured {
			e.ConfiguredMiB = base.Need.MiB
		} else {
			e.EstimatedMiB = base.Need.MiB
		}
		peak := 0
		for _, n := range l.Nodes {
			e.Nodes = append(e.Nodes, LearnedNodeNeed{Node: n.ID, Fraction: n.Fraction, MiB: n.MiB, PeakMiB: n.PeakMiB})
			if n.PeakMiB <= 0 {
				peak = -1
			} else if peak >= 0 {
				peak += n.PeakMiB
			}
		}
		if peak > 0 {
			e.PeakMiB = peak
			e.PeakBasis = fmt.Sprintf("highest memory %d nodes gave up while it stayed loaded, last raised on %s; used in place of need + fill reserve while fill_reserve_kib_per_token is on",
				len(l.Nodes), dateOf(l.peakAt))
		}
		out[name] = e
	}
	return out
}

// ---- per-node fit ----

// sharedBudget is the usable memory of a shared-memory node for a fit
// decision: the smaller of the budget from the fleet-reported free figure
// (minus tier headroom) and the live pre-flight free figure minus headroom.
// The pre-flight reads the Vulkan device, which overstates what a
// shared-memory machine can give. If only one figure is known, that one is used.
func (c *schedCore) sharedBudget(id core.NodeID, n core.Node) (int, string) {
	return c.sharedBudgetFrom(id, n, n.FreeMiB)
}

// sharedBudgetFrom is sharedBudget with fleetFree in place of the fleet's
// free figure (emptyFree: the figure with nothing of ours on the node).
func (c *schedCore) sharedBudgetFrom(id core.NodeID, n core.Node, fleetFree int) (int, string) {
	const fleet, preflight = "shared memory, headroom kept, from fleet figures", "shared memory, headroom kept, from pre-flight"
	pfFree, pfKnown := c.preflightFree(id)
	if pfKnown && (fleetFree <= 0 || pfFree < fleetFree) {
		return pfFree - n.HeadroomMiB, preflight
	}
	return fleetFree - n.HeadroomMiB, c.say(fleet)
}

// nodeShort is a shared-memory node whose expected share of a placement is
// more than it can give.
type nodeShort struct {
	label   string
	share   int // MiB the node is expected to take
	usable  int // MiB it has (its budget)
	fitText string
	how     string // where the usable figure comes from
	empty   bool   // usable is the figure with nothing of ours loaded on the node
}

func (s nodeShort) text() string {
	if s.empty {
		return fmt.Sprintf("node %s: expected share %d MiB (%s) but it would have only %d MiB usable even with nothing loaded on it (%s)",
			s.label, s.share, s.fitText, s.usable, s.how)
	}
	return fmt.Sprintf("node %s: expected share %d MiB (%s) but usable %d MiB (%s)", s.label, s.share, s.fitText, s.usable, s.how)
}

// sharedShortfalls checks, for a placement of two or more nodes, every
// shared-memory node's expected share against its own usable memory:
//
//	share = need x fraction x (1 + split_skew_margin) + device cost
//
// the device cost (deviceCostOn) being what every device holds beyond its share
// of an estimated need, as in the placer's rule; 0 for a measured need.
// where fraction is the node's share of the split the child gets
// (instance.SplitFractions). A node whose share was measured by a load this
// run with the same node set and split uses that figure and no margin. A
// single-node placement takes no margin and is not checked here.
//
// With empty the usable figure is the one with nothing of ours loaded on the
// node (fitUsable); ok is false when that cannot be told.
func (c *schedCore) sharedShortfalls(p *core.Profile, info NeedInfo, pl core.Placement, empty bool) (out []nodeShort, ok bool) {
	if c.newRules() {
		return c.reservedShortfalls(p, pl, empty)
	}
	if len(pl.Nodes) < 2 {
		return nil, true
	}
	fracs := instance.SplitFractions(*p, pl)
	if len(fracs) != len(pl.Nodes) {
		return nil, true
	}
	skew := c.cfg.Defaults.SplitSkewMargin
	need := info.Need.WithMargin()
	devs := make([]int, len(pl.Nodes)) // deviceoverhead.go: each node's own per-device cost
	for i, pn := range pl.Nodes {
		devs[i] = c.deviceCostOn(p, info, pn.ID, len(pl.Nodes))
	}
	learned := c.learnedShares(p, pl, fracs, need)
	for i, pn := range pl.Nodes {
		n, ok := c.node(pn.ID)
		if !ok || !n.SharedMemory {
			continue
		}
		usable, how, ok := c.fitUsable(pn.ID, n, empty)
		if !ok {
			return nil, false
		}
		var share int
		var fit string
		if learned != nil {
			l := c.planLearnedAt(p, len(pl.Nodes))
			share, fit = learned[i], "measured this run for this node set and split"
			if l.restored {
				fit = fmt.Sprintf("measured on %s for this node set and split", dateOf(l.at))
			}
			if m, _ := l.shareFor(pl, fracs, pn.ID); share > m {
				fit += fmt.Sprintf(", %d MiB raised to the need on %d devices", m, len(pl.Nodes))
			}
		}
		if fit == "" {
			share = int(math.Ceil(float64(need)*fracs[i]*(1+skew)-1e-6)) + devs[i]
			fit = fmt.Sprintf("need %d MiB x %s%% of the split x %s skew margin",
				need, strconv.FormatFloat(math.Round(fracs[i]*1000)/10, 'f', -1, 64), strconv.FormatFloat(1+skew, 'f', -1, 64))
			if devs[i] > 0 {
				fit += fmt.Sprintf(" + %d MiB per device", devs[i])
			}
		}
		if share > usable {
			out = append(out, nodeShort{label: nodeLabel(pn.ID, n), share: share, usable: usable, fitText: fit, how: how, empty: empty})
		}
	}
	return out, true
}

// ---- memory watch during a load ----

// memoryFloorHit is the first shared-memory node of a loading instance whose
// fleet-reported available memory is below defaults.load_memory_floor_mib.
// The worker allocates its whole buffer in the first seconds of a load, so the
// available figure already includes the model: the rule is simply "available
// now < floor". It never acts on stale data (a view older than stale_after),
// on a node that is offline or absent, or on a figure the fleet did not report.
// The figure is the view's, not the pre-flight override: the live reading.
func (c *schedCore) memoryFloorHit(in *inst) (label string, avail int, hit bool) {
	floor := c.cfg.Defaults.LoadMemoryFloorMiB
	if floor <= 0 || !c.fresh {
		return "", 0, false
	}
	for _, pn := range in.placement.Nodes {
		n, ok := c.raw[pn.ID]
		if !ok || !n.SharedMemory || !n.Online || n.FreeUnknown {
			continue
		}
		if n.FreeMiB < floor {
			return nodeLabel(pn.ID, n), n.FreeMiB, true
		}
	}
	return "", 0, false
}

// memoryFigures is "node=available" for every shared-memory node of the
// instance, for the log.
func (c *schedCore) memoryFigures(in *inst) string {
	var parts []string
	for _, pn := range in.placement.Nodes {
		if n, ok := c.raw[pn.ID]; ok && n.SharedMemory {
			v := strconv.Itoa(n.FreeMiB)
			if n.FreeUnknown {
				v = "unknown"
			}
			parts = append(parts, nodeLabel(pn.ID, n)+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// cancelLowMemoryLoad fails a load whose node ran low on memory: the child is
// killed (not asked to stop), waiters get the same 502 as any load failure,
// the outcome is failed and the profile is backed off. The node is not
// quarantined: it did nothing wrong.
func (c *schedCore) cancelLowMemoryLoad(in *inst, label string, avail int) {
	floor := c.cfg.Defaults.LoadMemoryFloorMiB
	c.log.Error("load cancelled: a node ran low on memory", "profile", in.prof.Name, "node", label,
		"available_mib", avail, "floor_mib", floor, "node_memory_mib", c.memoryFigures(in),
		"elapsed", c.now.Sub(in.created).Round(1e6))
	c.recordInst(in, history.TLoadLowMemory, history.Error,
		fmt.Sprintf("Loading %s was cancelled: %s ran low on memory (%d MiB available, floor %d MiB).", in.prof.Name, label, avail, floor),
		map[string]any{"node": label, "available_mib": avail, "floor_mib": floor, "elapsed_s": secs(c.now.Sub(in.created))})
	c.hist[len(c.hist)-1].Nodes = []string{label}
	msg := fmt.Sprintf("load cancelled: %s ran low on memory during the load (available %d MiB, floor %d MiB)", label, avail, floor)
	c.failLoadMode(in, msg, nil, false, stopKill)
}
