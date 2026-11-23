package scheduler

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The memory reservation rule. Most nodes are shared-memory consoles whose
// worker takes its device memory in one step, so the check before a launch
// is the only real protection. A profile's reserved figure on a node is its
// need share (measured or estimated, as the fit rules have it) plus a fill
// reserve for the growth while the conversation fills the context. A stored
// peak can only raise that figure, never lower it: reserved = max(need share
// + fill reserve, peak), because a peak is not known to have been seen with
// the context filled. Placement
// can count a node's memory at the larger of what is reserved there and what
// the node measures as used, on the smaller of its latest and its average
// free figure. Every setting defaults to the rules of the release before it
// (config.Defaults: fill_reserve_kib_per_token, placement_uses_reserved,
// margin_measured_mib, margin_estimated_percent, margin_estimated_min_mib).

// Bases of a reserved figure (the plan's memory section, the node view).
const (
	BasisMeasured  = "measured"
	BasisPeak      = "peak"
	BasisEstimated = "estimated"
)

// avgStale: what is placed on node id changed less than a window ago, so its
// average free figure still holds samples from before (an unloaded or
// evicted model's): it is not used then (review S5). With a window of 0 the
// average is the latest sample and never stale.
func (c *schedCore) avgStale(id core.NodeID) bool {
	if c.avgAlways {
		return false
	}
	w := c.cfg.Defaults.FreeAverageWindow
	ns := c.nodes[id]
	return w > 0 && ns != nil && !ns.heldChanged.IsZero() && c.now.Before(ns.heldChanged.Add(w))
}

// reserveOn: the fill reserve is on, and with it stored peaks are used.
func (c *schedCore) reserveOn() bool { return c.cfg.Defaults.FillReserveKiBPerToken > 0 }

// newMargins: a configured per-node margin replaces the split skew margin.
func (c *schedCore) newMargins() bool {
	d := c.cfg.Defaults
	return d.MarginMeasuredMiB > 0 || d.MarginEstimatedPercent > 0
}

// newRules: any part of the reservation rule that changes a fit decision is on.
func (c *schedCore) newRules() bool { return c.reserveOn() || c.newMargins() }

// ctxTokens is the context the profile's server holds: ctx x parallel.
func ctxTokens(p *core.Profile) int { return max(p.Ctx, 0) * max(p.Parallel, 1) }

// fillReserveMiB is the fill reserve of p on one node, rounded up; 0 when
// the reserve is off.
func (c *schedCore) fillReserveMiB(p *core.Profile) int {
	kib := c.cfg.Defaults.FillReserveKiBPerToken
	if kib <= 0 || p.Kind == core.KindCommand {
		return 0
	}
	return int((int64(kib)*int64(ctxTokens(p)) + 1023) / 1024)
}

// peakFor is the stored per-node peak of p for placement pl (the node set and
// split it was measured with), and its sum; ok is false when there is none
// for that placement or the reserve is off.
func (c *schedCore) peakFor(p *core.Profile, pl core.Placement) (peaks []int, ok bool) {
	if !c.reserveOn() {
		return nil, false
	}
	l := c.learnedAt(p, len(pl.Nodes))
	if l == nil || len(pl.Nodes) == 0 {
		return nil, false
	}
	fracs := instance.SplitFractions(*p, pl)
	peaks = make([]int, len(pl.Nodes))
	for i, pn := range pl.Nodes {
		if _, ok := l.shareFor(pl, fracs, pn.ID); !ok {
			return nil, false
		}
		for _, n := range l.Nodes {
			if n.ID == pn.ID {
				peaks[i] = n.PeakMiB
			}
		}
		if peaks[i] <= 0 {
			return nil, false
		}
	}
	return peaks, true
}

// peakNeed is the need a placement of k devices is held to by p's stored
// peaks while the reserve is on: for each measurement with a peak on every
// node, the peak total less the fill reserve of the placement's nodes, as
// the placer adds that back on every node. A placement on fewer devices
// than the peak was measured on keeps the whole peak (the missing nodes'
// fill reserve is not taken off: review SS4); one on more devices adds the
// estimator's per-device cost for each extra device. 0 when there is none.
func (c *schedCore) peakNeed(p *core.Profile, info NeedInfo, k int) int {
	if !c.reserveOn() || k < 1 {
		return 0
	}
	fill, out := c.fillReserveMiB(p), 0
	for _, l := range c.learnedOf(p) {
		sum := 0
		for _, n := range l.Nodes {
			if n.PeakMiB <= 0 {
				sum = 0
				break
			}
			sum += n.PeakMiB
		}
		if sum <= 0 || len(l.Nodes) == 0 {
			continue
		}
		j := l.count()
		out = max(out, sum-min(j, k)*fill+max(k-j, 0)*c.perDeviceCost(info))
	}
	return out
}

// applyReserve adds the reservation rule to a placer request for p with
// need info: the fill reserve on every node, the stored peaks when they
// are above need + fill reserve (they only raise the need, never lower it),
// and the configured per-node margins; for every device count when the
// request carries a need per count (needcount.go).
func (c *schedCore) applyReserve(req *placer.Request, p *core.Profile, info NeedInfo) {
	req.PerNodeReserveMiB = c.fillReserveMiB(p)
	k := info.Devices
	if k == 0 {
		k = max(len(p.ForceNodes), 1)
	}
	cn := c.countReserve(p, info, k)
	req.Need, req.MarginMiB, req.MarginPermille, req.MarginMinMiB = cn.Need, cn.MarginMiB, cn.MarginPermille, cn.MarginMinMiB
	for i := range req.ByCount {
		if i > 0 {
			req.ByCount[i] = c.countReserve(p, info.at(i), i)
		}
	}
	if !c.marginCounts() {
		// tight.go: no margin is part of the fit (neither a configured
		// one nor the split skew margin it replaces)
		req.MarginMiB, req.MarginPermille, req.MarginMinMiB, req.SkewPermille = 0, 0, 0, 0
		for i := range req.ByCount {
			req.ByCount[i].MarginMiB, req.ByCount[i].MarginPermille, req.ByCount[i].MarginMinMiB = 0, 0, 0
		}
	}
}

// countReserve is the need and margins of a placement of k devices whose
// need is info.
func (c *schedCore) countReserve(p *core.Profile, info NeedInfo, k int) placer.CountNeed {
	d := c.cfg.Defaults
	cn := placer.CountNeed{Need: info.Need}
	if pn := c.peakNeed(p, info, k); pn > info.Need.WithMargin() {
		cn.Need = placer.Measured(pn)
	}
	switch {
	case cn.Need.Measured && d.MarginMeasuredMiB > 0:
		cn.MarginMiB = d.MarginMeasuredMiB
	case !cn.Need.Measured && d.MarginEstimatedPercent > 0:
		cn.MarginPermille, cn.MarginMinMiB = d.MarginEstimatedPercent*10, d.MarginEstimatedMinMiB
	}
	return cn
}

// reservedShare is what one node of a placement is expected to take for a
// profile, assuming the context is filled completely, and the margin it is
// held to.
type reservedShare struct {
	ID         core.NodeID
	Weights    int // the node's share of the model file
	Context    int // the rest of its share: KV cache, compute buffers, overhead
	Host       int // host_overhead_mib when the node hosts the load (in Context too); 0 otherwise
	Fill       int // the fill reserve (0 when a peak is used, or the reserve is off)
	Total      int // Weights + Context + Fill
	Basis      string
	Margin     int
	MarginRule string
}

// reservedShares is the reserved figure of p on every node of pl, in
// placement order, with the margin each node is held to. The split of a
// share into weights and context uses the model file size (weights x the
// node's fraction); the rest is context. Nil when the need is unknown.
func (c *schedCore) reservedShares(p *core.Profile, pl core.Placement) ([]reservedShare, NeedInfo) {
	info, err := c.needAt(p, len(pl.Nodes))
	if err != nil || info.Need.MiB <= 0 || len(pl.Nodes) == 0 {
		return nil, info
	}
	fracs := []float64{1}
	if len(pl.Nodes) > 1 {
		fracs = instance.SplitFractions(*p, pl)
		if len(fracs) != len(pl.Nodes) {
			return nil, info
		}
	}
	need := info.Need.WithMargin()
	dev := c.deviceCost(info)
	fileMiB := int((info.FileBytes + (1 << 20) - 1) >> 20)
	peaks, havePeak := c.peakFor(p, pl)
	// Without per-node peaks for this node set and split, the peaks of the
	// shape still raise the need of a placement of this many devices.
	raised := false
	if pn := c.peakNeed(p, info, len(pl.Nodes)); !havePeak && pn > need {
		need, dev, raised = pn, 0, true
	}
	fill := c.fillReserveMiB(p)
	skew := c.cfg.Defaults.SplitSkewMargin
	d := c.cfg.Defaults
	// The per-node figures measured on this node set and split, scaled up
	// when they add up to less than the need (they follow a later, smaller
	// load than the need's): never below the need, the measured skew kept.
	var learnedMiB []int
	if len(pl.Nodes) > 1 {
		learnedMiB = c.learnedShares(p, pl, fracs, need)
	}
	out := make([]reservedShare, len(pl.Nodes))
	for i, pn := range pl.Nodes {
		r := reservedShare{ID: pn.ID, Basis: BasisEstimated}
		if info.Need.Measured {
			r.Basis = BasisMeasured
		}
		if raised {
			r.Basis = BasisPeak
		}
		share, learned := 0, false
		switch {
		case len(pl.Nodes) == 1:
			share = need + dev
		default:
			if learnedMiB != nil {
				share, learned, r.Basis = learnedMiB[i], true, BasisMeasured
			} else {
				share = int(math.Ceil(float64(need)*fracs[i]-1e-6)) + dev
			}
		}
		// A host on the node takes its process besides its device share
		// (hostnode.go); a stored peak counts it already.
		r.Host = c.hostOverhead(p, pl, pn.ID)
		share += r.Host
		r.Fill = fill
		if havePeak && peaks[i] > share+fill {
			// the peak is above need share + fill reserve: it counts whole
			share, r.Fill, r.Basis = peaks[i], 0, BasisPeak
		}
		r.Weights = min(int(math.Ceil(float64(fileMiB)*fracs[i]-1e-6)), share)
		r.Context = share - r.Weights
		r.Total = share + r.Fill
		n, ok := c.node(pn.ID)
		shared := ok && n.SharedMemory
		switch {
		case !shared:
			r.MarginRule = "dedicated memory: no per-node margin"
		case (r.Basis == BasisMeasured || r.Basis == BasisPeak) && d.MarginMeasuredMiB > 0:
			r.Margin, r.MarginRule = d.MarginMeasuredMiB, "margin_measured_mib"
		case r.Basis == BasisEstimated && d.MarginEstimatedPercent > 0:
			r.Margin = max(int((int64(share)*int64(d.MarginEstimatedPercent)+99)/100), d.MarginEstimatedMinMiB)
			r.MarginRule = fmt.Sprintf("margin_estimated_percent %d%%, at least %d MiB", d.MarginEstimatedPercent, d.MarginEstimatedMinMiB)
		case len(pl.Nodes) == 1:
			r.MarginRule = "single node: no split margin"
		case learned || havePeak:
			r.MarginRule = "measured for this node set and split: no margin"
		default:
			// The split skew margin as the fit rules apply it: one rounding
			// over need x fraction x (1 + skew).
			r.Margin = int(math.Ceil(float64(need)*fracs[i]*(1+skew)-1e-6)) + dev - share
			r.MarginRule = fmt.Sprintf("split_skew_margin %g", skew)
		}
		out[i] = r
	}
	return out, info
}

// ---- peaks ----

// recordBaseFree remembers, when an instance is created, each node's free
// figure as the view reported it: what the node gives up for the instance
// is measured against it (measured use, peaks).
func (c *schedCore) recordBaseFree(in *inst) {
	in.baseFree = map[core.NodeID]int{}
	for _, pn := range in.placement.Nodes {
		if n, ok := c.raw[pn.ID]; ok && !n.FreeUnknown {
			in.baseFree[pn.ID] = n.FreeMiB
		}
	}
}

// measuredUse is what node id gives up for instance in now: its free figure
// when the instance was created less its free figure now. ok is false when
// either is unknown.
func (c *schedCore) measuredUse(in *inst, id core.NodeID) (int, bool) {
	base, ok := in.baseFree[id]
	if !ok {
		return 0, false
	}
	n, ok := c.raw[id]
	if !ok || n.FreeUnknown {
		return 0, false
	}
	return max(base-n.FreeMiB, 0), true
}

// trackPeaks records, for every loaded instance, the most each node gave up
// for it in this load-to-unload cycle (in.cyclePeak). Nothing is stored
// while the cycle runs: endPeakCycle commits the cycle's peaks when it ends,
// so the reservation of a loaded instance stays the one it was placed with
// and its drift shows. Only with the reserve on (review N1).
func (c *schedCore) trackPeaks() {
	if !c.reserveOn() {
		return
	}
	for _, in := range c.insts {
		if in.state != stReady && in.state != stDraining {
			continue
		}
		for _, pn := range in.placement.Nodes {
			if used, ok := c.measuredUse(in, pn.ID); ok {
				if in.cyclePeak == nil {
					in.cyclePeak = map[core.NodeID]int{}
				}
				in.cyclePeak[pn.ID] = max(in.cyclePeak[pn.ID], used)
			}
		}
	}
}

// endPeakCycle ends instance in's load-to-unload cycle (removeInst) and
// commits its peaks beside the learned need (same node set and split). When
// the cycle ended normally (an unload, a ttl, an eviction, a shutdown) the
// stored per-node peaks become this cycle's, so one spike holds only until
// a normal cycle without it ends (review S6); a node not measured in the
// cycle keeps no peak. When it ended abnormally (a failed worker, a lost
// node) the peaks can only rise. A peak never lowers a reserved figure
// below need + fill reserve (reservedShares).
func (c *schedCore) endPeakCycle(in *inst) {
	c.releaseFloor(in)
	if !c.reserveOn() || in.readyAt.IsZero() || len(in.cyclePeak) == 0 {
		return
	}
	l := c.learnedAt(in.prof, len(in.placement.Nodes))
	if l == nil || len(l.Nodes) == 0 || !slices.Equal(l.placement, in.nodeIDs()) {
		return
	}
	normal, changed := normalStop(in), false
	for i := range l.Nodes {
		n := &l.Nodes[i]
		p := in.cyclePeak[n.ID]
		if !normal {
			p = max(p, n.PeakMiB)
		}
		if p != n.PeakMiB {
			n.PeakMiB, changed = p, true
		}
	}
	if changed {
		l.peakAt = c.now
		c.storeLearned(l)
		c.changed = true
	}
}

// releaseFloor: when a load that measured its shape on this device count
// ends normally, a restored total held above this run's figure no longer
// holds; the figure becomes the highest this run measured (review SB1).
func (c *schedCore) releaseFloor(in *inst) {
	if in.readyAt.IsZero() || !normalStop(in) {
		return
	}
	l := c.learnedAt(in.prof, len(in.placement.Nodes))
	if l == nil || l.floor == 0 || l.runHigh == 0 {
		return
	}
	l.floor = 0
	if l.TotalMiB != l.runHigh {
		c.log.Info("learned memory need lowered after a normal cycle", "profile", in.prof.Name, "from_mib", l.TotalMiB, "to_mib", l.runHigh)
		l.TotalMiB = l.runHigh
		c.storeLearned(l)
		c.changed = true
	}
}

// normalStop: the instance was stopped by the scheduler or an operator, not
// by a failure.
func normalStop(in *inst) bool {
	r := in.stopReason
	if r == "" && in.victimOf != "" {
		return true
	}
	return r == "admin" || r == "ttl" || r == "shutdown" || strings.HasPrefix(r, "evicted for ")
}

// ---- the node side ----

// reservedOn is what is reserved on node id: the instance holding it (placed,
// loading, loaded or stopping), with its reserved share there. ok is false
// when nothing holds the node or the share cannot be told.
func (c *schedCore) reservedOn(id core.NodeID) (in *inst, r reservedShare, ok bool) {
	ns := c.nodes[id]
	if ns == nil || ns.holder == "" {
		return nil, r, false
	}
	in = c.insts[ns.holder]
	if in == nil {
		return nil, r, false
	}
	shares, _ := c.reservedShares(in.prof, in.placement)
	for i, pn := range in.placement.Nodes {
		if pn.ID == id && i < len(shares) {
			return in, shares[i], true
		}
	}
	return in, r, false
}

// unmeasuredReserve is the part of what is reserved on node id that its free
// figure does not show yet: reserved - measured use, at least 0. A node
// whose use cannot be measured counts its whole reservation.
func (c *schedCore) unmeasuredReserve(id core.NodeID) int {
	in, r, ok := c.reservedOn(id)
	if !ok || in == c.fitting {
		return 0
	}
	used, known := c.measuredUse(in, id)
	if !known {
		return r.Total
	}
	return max(r.Total-used, 0)
}

// reservedBudget is the budget placement_uses_reserved places on: the
// node's free figure, at most its average, less what is reserved there but
// not measured yet, less headroom. On a shared-memory node the free figure
// is the fleet's (available memory less the watchdog floor), never the
// pre-flight's device figure alone: sharedBudgetFrom takes the smaller of
// the two. liveBudget never lets it above the old path's figure.
func (c *schedCore) reservedBudget(id core.NodeID, n core.Node) int {
	free := c.freeMiB(id, n)
	if n.SharedMemory {
		free = n.FreeMiB
	}
	if n.FreeAvgKnown && !c.avgStale(id) && n.FreeAvgMiB < free {
		free = n.FreeAvgMiB
	}
	free -= c.unmeasuredReserve(id)
	if n.SharedMemory {
		b, _ := c.sharedBudgetFrom(id, n, free)
		return min(b, free-n.HeadroomMiB)
	}
	return free - n.HeadroomMiB
}

// reservedShortfalls is sharedShortfalls under the reservation rule: every
// shared-memory node of the placement, of any size, must hold its reserved
// figure plus its margin.
func (c *schedCore) reservedShortfalls(p *core.Profile, pl core.Placement, empty bool) (out []nodeShort, ok bool) {
	shares, _ := c.reservedShares(p, pl)
	for i, pn := range pl.Nodes {
		if i >= len(shares) {
			break
		}
		n, ok := c.node(pn.ID)
		if !ok || !n.SharedMemory {
			continue
		}
		usable, how, ok := c.fitUsable(pn.ID, n, empty)
		if !ok {
			return nil, false
		}
		r := shares[i]
		if need := r.Total + c.counted(r.Margin); need > usable {
			fit := fmt.Sprintf("%s: weights %d + context %d + fill reserve %d MiB, margin %d MiB (%s)",
				r.Basis, r.Weights, r.Context, r.Fill, r.Margin, r.MarginRule)
			out = append(out, nodeShort{label: nodeLabel(pn.ID, n), share: need, usable: usable, fitText: fit, how: how, empty: empty})
		}
	}
	return out, true
}

// ---- views ----

// nodeReservation fills the reservation part of a node's status.
func (c *schedCore) nodeReservation(id core.NodeID, n core.Node, s *NodeStatus) {
	s.FreeForNewLoadMiB = c.liveBudget(id, n)
	if n.FreeAvgKnown {
		v := n.FreeAvgMiB
		s.FreeAvgMiB = &v
	}
	s.Reserved = []NodeReservation{}
	in, r, ok := c.reservedOn(id)
	if !ok {
		return
	}
	s.Reserved = append(s.Reserved, NodeReservation{Profile: in.prof.Name, State: in.state.String(), Basis: r.Basis, Ctx: ctxTokens(in.prof),
		WeightsMiB: r.Weights, ContextMiB: r.Context, FillReserveMiB: r.Fill, TotalMiB: r.Total})
	s.ReservedMiB = r.Total
	if used, known := c.measuredUse(in, id); known {
		drift := used - r.Total
		s.MeasuredUsedMiB, s.DriftMiB = &used, &drift
		if warn := c.cfg.Defaults.ReservedDriftWarnMiB; drift > warn {
			s.DriftWarning = fmt.Sprintf("the node gave up %d MiB since %s was placed, %d MiB more than is reserved for it (warn above %d MiB): something else uses memory there, or the need is too low",
				used, in.prof.Name, drift, warn)
		}
	}
}

// planNote explains the weights and context split of the memory section.
const planNote = "weights are the model file's share by the split fraction; context is the rest of the share (KV cache, compute buffers, device overhead); will_take adds the fill reserve, or is the stored peak when that is higher"

// gateOn: a reservation setting is on, so every placement is held to the
// memory rows (reserveGate); with the defaults the old rules decide alone.
func (c *schedCore) gateOn() bool { return c.newRules() || c.cfg.Defaults.PlacementUsesReserved }

// memoryRows is, for each node of placement pl, what p will take there with
// its context filled, what the node has and what remains. The nodes held by
// a profile in freed (p itself, the victims of an eviction) count at their
// figure with nothing of ours loaded. ok is false when the need is unknown.
// The plan's memory section and the reservation gate both use it, so the
// plan's fits is the decision.
func (c *schedCore) memoryRows(p *core.Profile, pl core.Placement, freed map[string]bool) (rows []PlanMemoryNode, ok bool) {
	shares, _ := c.reservedShares(p, pl)
	if shares == nil {
		return nil, false
	}
	for i, pn := range pl.Nodes {
		n, _ := c.node(pn.ID)
		r := shares[i]
		e := PlanMemoryNode{ID: pn.ID, Node: nodeLabel(pn.ID, n), Shared: n.SharedMemory, TotalMiB: n.TotalMiB,
			WeightsMiB: r.Weights, ContextMiB: r.Context, FillReserveMiB: r.Fill, WillTakeMiB: r.Total, Basis: r.Basis,
			FreeNowMiB: c.freeMiB(pn.ID, n), HeadroomMiB: n.HeadroomMiB, MarginMiB: r.Margin, MarginRule: r.MarginRule,
			ReservedBy: []PlanReservedBy{}, AvailableMiB: c.liveBudget(pn.ID, n), HostOverheadMiB: r.Host}
		e.FreeAvgMiB, e.FreeAvgKnown = e.FreeNowMiB, n.FreeAvgKnown
		if n.FreeAvgKnown {
			e.FreeAvgMiB = n.FreeAvgMiB
		}
		if in, held, ok := c.reservedOn(pn.ID); ok {
			e.ReservedBy = append(e.ReservedBy, PlanReservedBy{Profile: in.prof.Name, MiB: held.Total, State: in.state.String()})
			if free, _, ok := c.emptyFree(pn.ID, n); ok && freed[in.prof.Name] {
				if n.SharedMemory {
					e.AvailableMiB, _ = c.sharedBudgetFrom(pn.ID, n, free)
				} else {
					e.AvailableMiB = free - n.HeadroomMiB
				}
			}
		}
		e.SpareMiB = e.AvailableMiB - e.WillTakeMiB
		e.RemainsMiB = e.SpareMiB - e.MarginMiB
		c.planFigures(&e, pn.ID, n, freedHere(c, pn.ID, freed)) // planfigures.go
		rows = append(rows, e)
	}
	return rows, true
}

// reserveGate is the reservation rule's check of a placement the placer
// found for p (victims are the profiles an eviction would stop): every node
// must keep remains_mib >= 0 in memoryRows. It returns why not, or "" when
// it fits, the gate is off (the defaults) or the need is unknown (the other
// checks decide then).
func (c *schedCore) reserveGate(p *core.Profile, pl core.Placement, victims []string) (why string) {
	c.gated = gateMark{profile: p.Name, nodes: placementIDs(pl), step: c.steps}
	defer func() { c.gated.passed = why == "" }()
	if !c.gateOn() {
		return ""
	}
	freed := map[string]bool{p.Name: true}
	for _, v := range victims {
		freed[v] = true
	}
	rows, ok := c.memoryRows(p, pl, freed)
	if !ok {
		return ""
	}
	if node, by := c.rowsShort(rows); by > 0 {
		if !c.marginCounts() {
			return fmt.Sprintf("memory reservation: short by %d MiB on %s even without the margin (weights + context + fill reserve above what it has, the headroom kept)", by, node)
		}
		return fmt.Sprintf("memory reservation: node %s would be short by %d MiB (weights + context + fill reserve + margin above what it has)", node, by)
	}
	return ""
}

// planMemory is the memory section of a plan: on the placement made (or,
// with none, on the nodes the profile would be spread over), what each node
// will take assuming the context is filled, what it has, what is reserved
// there and what remains. The nodes of the profile's own instance and of the
// plan's victims count as freed (for a loaded profile: as if reloaded).
func (c *schedCore) planMemory(p *core.Profile, placed *core.Placement, victims []PlanVictim) *PlanMemory {
	info, err := c.need(p)
	if err != nil {
		return nil
	}
	m := &PlanMemory{PlacementUsesReserved: c.cfg.Defaults.PlacementUsesReserved, Decides: c.gateOn(), Note: planNote, Nodes: []PlanMemoryNode{}}
	if info.FileBytes <= 0 {
		m.Note = "the model file's size is not known, so all of the share is shown as context; " + planNote
	}
	var pl core.Placement
	if placed != nil && len(placed.Nodes) > 0 {
		pl = *placed
		if c.insts[p.Name] != nil {
			m.Note = "the profile is loaded: fits says whether it would fit again if reloaded on its nodes; " + m.Note
		}
	} else {
		m.Hypothetical = true
		pl = c.hypotheticalPlacement(p)
		if len(pl.Nodes) == 0 {
			m.Note = "no node can take a share of this profile now; " + m.Note
			return m
		}
	}
	freed := map[string]bool{p.Name: true}
	for _, v := range victims {
		freed[v.Profile] = true
	}
	rows, ok := c.memoryRows(p, pl, freed)
	if !ok {
		return nil
	}
	m.Nodes = rows
	m.ShortNode, m.ShortfallMiB = c.rowsShort(rows)
	m.Fits = !m.Hypothetical && m.ShortfallMiB == 0
	m.TightFit = len(c.markTight(m.Nodes)) > 0 && m.Fits
	if m.Hypothetical && m.ShortfallMiB == 0 {
		m.Note = "no placement now although these nodes have the memory: they are held, claimed, settling or not allowed for the profile; " + m.Note
	}
	return m
}

// hypotheticalPlacement is where a profile with no placement would go: its
// forced nodes, or the max_pool nodes it may use with the largest budgets,
// split by budget as an automatic placement is. Free nodes are taken; only
// when none is free are held ones shown.
func (c *schedCore) hypotheticalPlacement(p *core.Profile) core.Placement {
	var pl core.Placement
	if len(p.ForceNodes) > 0 {
		for _, name := range p.ForceNodes {
			id, err := c.view.Resolve(name)
			if err != nil {
				continue
			}
			n, ok := c.node(id)
			if !ok {
				continue
			}
			pl.Nodes = append(pl.Nodes, core.PlacedNode{ID: id, Addr: n.Addr, Local: n.Local, BudgetMiB: c.liveBudget(id, n)})
		}
		if len(pl.Nodes) == len(p.ForceNodes) && len(p.TensorSplit) > 0 {
			pl.TensorSplit = slices.Clone(p.TensorSplit)
		}
		return pl
	}
	var cands, held []core.PlacedNode
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok || !c.eligible(id) || (localOnly(p) && !n.Local) || c.autoOff(p, n) {
			continue
		}
		if len(p.AllowTiers) > 0 && !slices.Contains(p.AllowTiers, n.Priority) {
			continue
		}
		pn := core.PlacedNode{ID: id, Addr: n.Addr, Local: n.Local, BudgetMiB: c.liveBudget(id, n)}
		if c.isFree(id) {
			cands = append(cands, pn)
		} else {
			held = append(held, pn)
		}
	}
	if len(cands) == 0 {
		cands = held
	}
	slices.SortStableFunc(cands, func(a, b core.PlacedNode) int { return b.BudgetMiB - a.BudgetMiB })
	k := max(c.cfg.Defaults.MaxPool, 1)
	if localOnly(p) {
		k = 1
	}
	if len(cands) > k {
		cands = cands[:k]
	}
	pl.Nodes = cands
	return pl
}

// gateMark is the latest gate call: the profile, the node set, the step it
// was made in, and whether it passed (checkGated).
type gateMark struct {
	profile string
	nodes   []string
	step    uint64
	passed  bool
}

// checkGateBeforeLaunch makes every launch check that the gate passed for
// this profile and node set in the same step (the tests turn it on): a
// launch path that skips the gate, or launches after a refusal, panics
// (review SB2, third review TN1).
var checkGateBeforeLaunch = false

// checkGated is called where an instance starts to take memory: the child
// launch in fleet membership and the worker launches with agents.
func (c *schedCore) checkGated(in *inst) {
	if !checkGateBeforeLaunch {
		return
	}
	g := c.gated
	if g.profile != in.prof.Name || g.step != c.steps || !slices.Equal(g.nodes, placementIDs(in.placement)) || !g.passed {
		panic(fmt.Sprintf("scheduler: %s launched on %v without a passed gate in this step (last gate: %s on %v, step %d of %d, passed %v)",
			in.prof.Name, placementIDs(in.placement), g.profile, g.nodes, g.step, c.steps, g.passed))
	}
}

// placementIDs is pl's node ids in device order.
func placementIDs(pl core.Placement) []string {
	out := make([]string, len(pl.Nodes))
	for i, n := range pl.Nodes {
		out[i] = string(n.ID)
	}
	return out
}
