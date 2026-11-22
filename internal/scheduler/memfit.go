package scheduler

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
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
}

// learnedNeed is a profile's measurement from a load this run.
type learnedNeed struct {
	TotalMiB int
	Nodes    []learnedNode // empty when the log did not account for every node
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

// effectiveNeed applies the estimate margin and the learned measurement to a
// provider's need.
//
//   - needs_mib (measured by the operator) is used as is;
//   - an estimate is raised by defaults.estimate_margin;
//   - a measurement learned this run replaces an estimate, and replaces a
//     configured needs_mib only when it is higher (a learned figure never
//     lowers a configured one).
func (c *schedCore) effectiveNeed(p *core.Profile, base NeedInfo) NeedInfo {
	info := base
	info.BaseMiB = base.Need.MiB
	if base.Need.Measured {
		info.Source = NeedConfigured
	} else {
		info.Need = placer.EstimatedMargin(base.Need.MiB, permille(c.cfg.Defaults.EstimateMargin))
		info.Source = NeedEstimated
	}
	if l := c.learned[p.Name]; l != nil && l.TotalMiB > 0 && (!base.Need.Measured || l.TotalMiB > base.Need.MiB) {
		info.Need = placer.Measured(l.TotalMiB)
		info.Source = NeedLearned
	}
	return info
}

// needBasis is the plain-words basis of a need for messages and plans.
func (c *schedCore) needBasis(info NeedInfo) string {
	switch info.Source {
	case NeedEstimated:
		return fmt.Sprintf("estimated, +%s%% margin; the estimate is %d MiB",
			strconv.FormatFloat(c.cfg.Defaults.EstimateMargin*100, 'f', -1, 64), info.BaseMiB)
	case NeedLearned:
		if info.BaseMiB > 0 {
			return fmt.Sprintf("%s; %d MiB before", NeedLearned, info.BaseMiB)
		}
	}
	return string(info.Source)
}

// learnNeed records what a load that just became ready measured: the device
// total (the figure the log's memory_mib reports) and each node's own figure.
// A measurement above a configured needs_mib is warned about, once per profile.
func (c *schedCore) learnNeed(in *inst) {
	if in.prof.Kind != core.KindLlama || in.child == nil {
		return
	}
	load := in.child.Load()
	total := int(math.Ceil(load.TotalMiB() - 1e-6))
	if total <= 0 {
		return
	}
	l := c.learned[in.prof.Name]
	if l == nil {
		l = &learnedNeed{}
		c.learned[in.prof.Name] = l
	}
	l.TotalMiB = max(l.TotalMiB, total) // the highest seen: the safe side
	l.Nodes = nil
	byKey := map[string]int{}
	for i, pn := range in.placement.Nodes {
		if pn.Local {
			byKey[c.build.LocalDevice] = i
		} else {
			byKey[pn.Addr] = i
		}
	}
	fracs := instance.SplitFractions(*in.prof, in.placement)
	mib := make([]float64, len(in.placement.Nodes))
	seen := make([]bool, len(mib))
	for _, d := range load.Devices {
		if d.Host {
			continue
		}
		key := endpointOf(d.Name)
		if key == "" {
			key = d.Name
		}
		if i, ok := byKey[key]; ok {
			mib[i] += d.TotalMiB()
			seen[i] = true
		}
	}
	complete := len(mib) > 0 && len(fracs) == len(mib)
	for _, s := range seen {
		complete = complete && s
	}
	if complete {
		for i, pn := range in.placement.Nodes {
			l.Nodes = append(l.Nodes, learnedNode{ID: pn.ID, Fraction: fracs[i], MiB: int(math.Ceil(mib[i] - 1e-6))})
		}
	}
	base, ok := c.needs_[in.prof.Name]
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
	c.changed = true
}

// LearnedNeed is one profile's entry of status' learned_needs: the
// measurement of a load this run and the need in use now.
type LearnedNeed struct {
	MeasuredMiB   int // highest device total a load of this profile reported this run
	NeedMiB       int // the need fit decisions use now (with the estimate margin, if still an estimate)
	Source        string
	ConfiguredMiB int // needs_mib, or 0 for a profile that relies on the estimate
	EstimatedMiB  int // the estimate before the margin; 0 for a profile with needs_mib
	Nodes         []LearnedNodeNeed
}

// LearnedNodeNeed is what one node held in the measured load, with its share
// of the split.
type LearnedNodeNeed struct {
	Node     core.NodeID
	Fraction float64
	MiB      int
}

func (c *schedCore) learnedNeeds() map[string]LearnedNeed {
	out := map[string]LearnedNeed{}
	for name, l := range c.learned {
		p := c.cfg.Profiles[name]
		base, ok := c.needs_[name]
		if p == nil || !ok {
			continue
		}
		info := c.effectiveNeed(p, base)
		e := LearnedNeed{MeasuredMiB: l.TotalMiB, NeedMiB: info.Need.WithMargin(), Source: string(info.Source)}
		if base.Need.Measured {
			e.ConfiguredMiB = base.Need.MiB
		} else {
			e.EstimatedMiB = base.Need.MiB
		}
		for _, n := range l.Nodes {
			e.Nodes = append(e.Nodes, LearnedNodeNeed{Node: n.ID, Fraction: n.Fraction, MiB: n.MiB})
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
//	share = need x fraction x (1 + split_skew_margin)
//
// where fraction is the node's share of the split the child gets
// (instance.SplitFractions). A node whose share was measured by a load this
// run with the same node set and split uses that figure and no margin. A
// single-node placement takes no margin and is not checked here.
//
// With empty the usable figure is the one with nothing of ours loaded on the
// node (fitUsable); ok is false when that cannot be told.
func (c *schedCore) sharedShortfalls(p *core.Profile, info NeedInfo, pl core.Placement, empty bool) (out []nodeShort, ok bool) {
	if len(pl.Nodes) < 2 {
		return nil, true
	}
	fracs := instance.SplitFractions(*p, pl)
	if len(fracs) != len(pl.Nodes) {
		return nil, true
	}
	skew := c.cfg.Defaults.SplitSkewMargin
	need := info.Need.WithMargin()
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
		if m, ok := c.learned[p.Name].shareFor(pl, fracs, pn.ID); ok {
			share, fit = m, "measured this run for this node set and split"
		} else {
			share = int(math.Ceil(float64(need)*fracs[i]*(1+skew) - 1e-6))
			fit = fmt.Sprintf("need %d MiB x %s%% of the split x %s skew margin",
				need, strconv.FormatFloat(math.Round(fracs[i]*1000)/10, 'f', -1, 64), strconv.FormatFloat(1+skew, 'f', -1, 64))
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
