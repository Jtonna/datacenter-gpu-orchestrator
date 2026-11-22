package scheduler

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// forcedFit is the outcome of defaults.forced_fit_check for one forced
// profile. Two things are compared:
//
//   - the memory need against the sum of the forced nodes' usable memory. On a
//     dedicated-VRAM node that is its free memory with no headroom taken off (a
//     forced profile may eat into it). On a shared-memory node (a PS5: GPU
//     memory is system RAM) it is the smaller of the fleet-based budget (what
//     the automatic placer uses) and pre-flight free minus tier headroom,
//     because the Vulkan pre-flight reports nearly all the RAM as free and a
//     load that exhausts it takes the machine down;
//   - for a placement of two or more nodes, every shared-memory node's
//     expected share against that node's own usable memory (sharedShortfalls):
//     a split model does not land evenly, so one node can be overfilled while
//     the total fits.
//
// The need is the estimate with defaults.estimate_margin when it is an
// estimate, a measurement learned this run when there is one, else needs_mib.
type forcedFit struct {
	mode     string // config.ForcedFitWarn or config.ForcedFitReject
	need     int    // MiB, with the estimate margin where it is an estimate
	free     int    // MiB usable, summed over the forced nodes
	measured bool   // need is measured (needs_mib or learned), not an estimate
	basis    string // plain words: where the need comes from
	total    bool   // the total does not fit
	nodes    []string
	detail   []string    // per node: which figure was used, in plain words
	short    []nodeShort // shared-memory nodes whose own share does not fit
	empty    bool        // the figures are the nodes' with nothing of ours loaded on them
}

// message is the plain-words account used in the 503, the warning and the plan.
func (f *forcedFit) message(profile string) string {
	var parts []string
	if f.total {
		have := "have only"
		if f.empty {
			have = "would have only"
		}
		parts = append(parts, fmt.Sprintf("profile %s: need %d MiB (%s), but the nodes it is forced onto %s %d MiB usable in total%s (%s)",
			profile, f.need, f.basis, have, f.free, emptyWords(f.empty, "them"), strings.Join(f.detail, "; ")))
	}
	for _, s := range f.short {
		parts = append(parts, fmt.Sprintf("profile %s: need %d MiB (%s) split over the forced nodes, %s", profile, f.need, f.basis, s.text()))
	}
	return strings.Join(parts, "; ")
}

// forcedFitCheck compares p's need with the usable memory of the nodes of the
// forced placement pl as the scheduler holds it: the pre-flight figure while it
// lasts, else the fleet's, on dedicated-VRAM nodes; the smaller of the two,
// minus tier headroom, on shared-memory nodes. It returns nil when the check
// is off, the profile is not forced, the need is unknown, or everything fits.
func (c *schedCore) forcedFitCheck(p *core.Profile, pl core.Placement) *forcedFit {
	return c.forcedFitCheckOn(p, pl, false)
}

// forcedFitCheckOn is forcedFitCheck on the figures the scheduler holds now,
// or, with empty, on the figures the nodes would have with nothing of ours
// loaded on them (fitUsable). With empty it also returns nil when such a
// figure cannot be told.
func (c *schedCore) forcedFitCheckOn(p *core.Profile, pl core.Placement, empty bool) *forcedFit {
	mode := c.cfg.Defaults.ForcedFitCheck
	if (mode != config.ForcedFitWarn && mode != config.ForcedFitReject) || len(p.ForceNodes) == 0 || len(pl.Nodes) == 0 {
		return nil
	}
	info, err := c.need(p)
	if err != nil {
		return nil
	}
	f := &forcedFit{mode: mode, need: info.Need.WithMargin(), measured: info.Need.Measured, basis: c.needBasis(info), empty: empty}
	for _, pn := range pl.Nodes {
		n, ok := c.node(pn.ID)
		if !ok {
			return nil
		}
		label := nodeLabel(pn.ID, n)
		usable, how, ok := c.fitUsable(pn.ID, n, empty)
		if !ok {
			return nil
		}
		f.free += usable
		f.nodes = append(f.nodes, label)
		f.detail = append(f.detail, fmt.Sprintf("usable %d MiB on %s (%s)", usable, label, how))
	}
	f.total = f.need > f.free
	short, ok := c.sharedShortfalls(p, info, pl, empty)
	if !ok {
		return nil
	}
	f.short = short
	if !f.total && len(f.short) == 0 {
		return nil
	}
	return f
}

// fitUsable is the usable figure forced_fit_check holds node id to: on a
// dedicated-VRAM node its free memory with no headroom kept, on a
// shared-memory node sharedBudget. With empty it is the figure the node would
// have with nothing of ours loaded on it (emptyFree in place of the fleet's
// free figure; a pre-flight figure is used as is, because the pre-flight ran
// while the node was free). ok is false when that cannot be told.
func (c *schedCore) fitUsable(id core.NodeID, n core.Node, empty bool) (usable int, how string, ok bool) {
	if !empty {
		if n.SharedMemory {
			usable, how = c.sharedBudget(id, n)
			return usable, how, true
		}
		return c.freeMiB(id, n), "free memory, no headroom kept", true
	}
	free, note, ok := c.emptyFree(id, n)
	if !ok {
		return 0, "", false
	}
	if n.SharedMemory {
		usable, how = c.sharedBudgetFrom(id, n, free)
		return usable, how + note, true
	}
	if pf, on := c.preflightFree(id); on {
		return pf, "free memory from the pre-flight taken before the node's load, no headroom kept", true
	}
	return free, "free memory, no headroom kept" + note, true
}

// emptyFree is node id's free figure as the view gives it (core.Node.FreeMiB:
// the device's free memory, or on a node whose device shares system memory
// the smaller of that and available memory minus the watchdog floor) plus
// what the instance holding the node is expected to hold there (heldOn): the
// free figure the node would have with nothing of ours loaded on it. It is an
// estimate; the checks made when the instance is placed after the eviction
// stay. note says what was added, in plain words. ok is false when the held
// figure cannot be told.
func (c *schedCore) emptyFree(id core.NodeID, n core.Node) (free int, note string, ok bool) {
	held, who, ok := c.heldOn(id)
	if !ok {
		return 0, "", false
	}
	if held > 0 {
		note = fmt.Sprintf(", counting the %d MiB %s holds there as freed", held, who)
	}
	return n.FreeMiB + held, note, true
}

// heldOn is what the instance holding node id is expected to hold there: its
// share as expectShares gives it (measured by a load of this run when there
// is one, else its need with the estimate margin times its split fraction).
// 0 when nothing holds the node; ok is false when the share cannot be told.
func (c *schedCore) heldOn(id core.NodeID) (mib int, profile string, ok bool) {
	ns := c.nodes[id]
	if ns == nil || ns.holder == "" {
		return 0, "", true
	}
	in := c.insts[ns.holder]
	if in == nil {
		return 0, "", true
	}
	shares := c.expectShares(in.prof, in.placement)
	for i, pn := range in.placement.Nodes {
		if pn.ID == id && i < len(shares) {
			return shares[i], in.prof.Name, true
		}
	}
	return 0, "", false
}

// preflightFree is the pre-flight override of node id while it lasts.
func (c *schedCore) preflightFree(id core.NodeID) (int, bool) {
	if ns := c.nodes[id]; ns != nil && !ns.ovUntil.IsZero() && c.now.Before(ns.ovUntil) {
		return ns.ovFree, true
	}
	return 0, false
}

// emptyWords marks a figure taken with nothing loaded, in plain words.
func emptyWords(empty bool, what string) string {
	if !empty {
		return ""
	}
	return " even with nothing loaded on " + what
}

// forcedFitErr is the refusal for reject mode: 503, like the other
// placement refusals. Free memory can change, so it carries a Retry-After.
func (f *forcedFit) err(profile string) *Error {
	e := err503(ReasonForcedDoesNotFit, f.message(profile)+" (defaults.forced_fit_check is reject)", queueRetryAfter)
	e.NeedMiB, e.CapacityMiB = f.need, f.free
	return e
}

// expectShares is the memory each node of pl is expected to take, in
// placement order (agent contract CR-9, expect_mib): the whole need on a
// single node; else the node's share of the split, measured by a load of
// this run with the same node set and split when there is one, else
// need x fraction, with the split skew margin on a shared-memory node (the
// figure the fit rules hold such a node to; a node with memory of its own is
// held to the total only, so its share takes no margin), plus, for an
// estimate, what every device holds beyond its share (deviceCost). Nil when
// the need is unknown.
func (c *schedCore) expectShares(p *core.Profile, pl core.Placement) []int {
	info, err := c.need(p)
	if err != nil || info.Need.MiB <= 0 || len(pl.Nodes) == 0 {
		return nil
	}
	need := info.Need.WithMargin()
	dev := c.deviceCost(info)
	if len(pl.Nodes) == 1 {
		return []int{need + dev}
	}
	fracs := instance.SplitFractions(*p, pl)
	if len(fracs) != len(pl.Nodes) {
		return nil
	}
	out := make([]int, len(pl.Nodes))
	for i, pn := range pl.Nodes {
		if m, ok := c.learnedFor(p).shareFor(pl, fracs, pn.ID); ok {
			out[i] = m
			continue
		}
		share := float64(need) * fracs[i]
		if n, ok := c.node(pn.ID); ok && n.SharedMemory {
			share *= 1 + c.cfg.Defaults.SplitSkewMargin
		}
		out[i] = int(math.Ceil(share-1e-6)) + dev
	}
	return out
}

// agentForcedFit is the fit check of a forced placement in agents membership
// (agent contract CR-9). No pre-flight runs there, so the figures are the
// agents' own: a remote node whose expected share (expectShares) is more than
// its usable memory would have its launch refused by its agent
// (would_not_fit), so the placement is refused here instead of being sent.
// Usable is the node's free figure as the view gives it: available memory
// minus the watchdog floor on a node whose device shares system memory, the
// device's free memory on any other. It does not depend on
// defaults.forced_fit_check, which adds the tier headroom on top. Nil when
// the placement is not forced, not in agents membership, or fits.
func (c *schedCore) agentForcedFit(p *core.Profile, pl core.Placement) *Error {
	return c.agentForcedFitOn(p, pl, false)
}

// agentForcedFitOn is agentForcedFit on the figures the agents report now, or,
// with empty, on the figures the nodes would have with nothing of ours loaded
// on them (emptyFree). With empty a node whose held figure cannot be told is
// taken as fitting (the check after the eviction decides); a node whose own
// figure is unknown is refused either way.
func (c *schedCore) agentForcedFitOn(p *core.Profile, pl core.Placement, empty bool) *Error {
	if !c.agents || len(p.ForceNodes) == 0 {
		return nil
	}
	shares := c.expectShares(p, pl)
	if shares == nil {
		return nil
	}
	var parts []string
	need, usableSum := 0, 0
	for i, pn := range pl.Nodes {
		if pn.Local {
			continue
		}
		n, ok := c.node(pn.ID)
		if !ok || n.FreeUnknown {
			// No figure is never "start unchecked" (CR-9 review): the
			// agent would fall back to MemAvailable, but Potluck cannot
			// tell whether the share fits, so it does not send it.
			parts = append(parts, fmt.Sprintf("node %s is expected to take %d MiB but its memory figure is unknown (its agent reports no usable device)",
				nodeLabel(pn.ID, n), shares[i]))
			need += shares[i]
			continue
		}
		usable, note := c.freeMiB(pn.ID, n), ""
		if empty {
			free, nt, ok := c.emptyFree(pn.ID, n)
			if !ok {
				continue
			}
			usable, note = free, nt
		}
		need, usableSum = need+shares[i], usableSum+usable
		if shares[i] <= usable {
			continue
		}
		how := "its device's free memory"
		if n.SharedMemory {
			floor := c.cfg.Defaults.WatchdogFloorMiB
			if n.WatchdogFloorMiB > 0 {
				floor = n.WatchdogFloorMiB
			}
			how = fmt.Sprintf("available memory minus the watchdog floor of %d MiB", floor)
		}
		has := "has"
		if empty {
			has = "would have only"
		}
		parts = append(parts, fmt.Sprintf("node %s is expected to take %d MiB but %s %d MiB usable%s (%s%s)",
			nodeLabel(pn.ID, n), shares[i], has, usable, emptyWords(empty, "it"), how, note))
	}
	if len(parts) == 0 {
		return nil
	}
	msg := fmt.Sprintf("profile %s does not fit the nodes it is forced onto: %s; the node's agent would refuse the launch (would_not_fit), so it is not sent",
		p.Name, strings.Join(parts, "; "))
	if empty {
		msg += " and nothing loaded is stopped for it"
	}
	e := err503(ReasonForcedDoesNotFit, msg, queueRetryAfter)
	e.NeedMiB, e.CapacityMiB = need, usableSum
	return e
}

// forcedRefusalBeforeEvict is the fit check of a forced placement made before
// any victim is stopped for it, on the memory the forced nodes would have with
// nothing of ours loaded on them: a request that can never fit there is
// refused at once and costs no loaded model. It applies agentForcedFit in
// agents membership, and defaults.forced_fit_check when that is reject (with
// warn or off nothing changes here). The checks made after the eviction stay:
// the "nothing loaded" figure is an estimate.
func (c *schedCore) forcedRefusalBeforeEvict(p *core.Profile, pl core.Placement) *Error {
	if len(p.ForceNodes) == 0 {
		return nil
	}
	if e := c.agentForcedFitOn(p, pl, true); e != nil {
		return e
	}
	if ff := c.forcedFitCheckOn(p, pl, true); ff != nil && ff.mode == config.ForcedFitReject {
		return ff.err(p.Name)
	}
	return nil
}

// forcedNeverFits is forcedRefusalBeforeEvict run when a request for forced
// profile p arrives and each time its queue entry is looked at again while it
// waits, on the placement the forced nodes make (config order, each at the
// budget a plan would give it). So a request that can never fit is refused at
// once, whatever is in the way: a holder younger than min_hold, a holder with
// a request in flight, or another entry's claim. Same error, same wording.
//
// It returns nil (the request waits, or the start path decides) when:
//   - a forced node is not in the view, not eligible (offline, lost, stale
//     view, quarantined, its fetch failed), or unplaced for any reason but a
//     pause (re-registering, programs not ready, silent, ...): it may come
//     back with other figures;
//   - a forced node's memory figure is unknown (that stays a wait);
//   - every forced node is free: the start path checks the real figures in
//     the same step (pre-flight in fleet membership, agentForcedGate in
//     agents membership).
func (c *schedCore) forcedNeverFits(p *core.Profile) *Error {
	if len(p.ForceNodes) == 0 {
		return nil
	}
	var pl core.Placement
	allFree := true
	for _, name := range p.ForceNodes {
		id, err := c.view.Resolve(name)
		if err != nil {
			return nil
		}
		n, ok := c.node(id)
		if !ok || !c.eligible(id) || n.FreeUnknown {
			return nil
		}
		if why := c.unplaced[id]; why != "" && why != UnplacedPaused {
			return nil
		}
		budget := c.planBudget(id, n)
		if c.isFree(id) {
			budget = c.liveBudget(id, n)
		} else {
			allFree = false
		}
		pl.Nodes = append(pl.Nodes, core.PlacedNode{ID: id, Addr: n.Addr, Local: n.Local, BudgetMiB: budget})
	}
	if allFree {
		return nil
	}
	if len(p.TensorSplit) > 0 {
		pl.TensorSplit = slices.Clone(p.TensorSplit)
	}
	return c.forcedRefusalBeforeEvict(p, pl)
}

// refuseNeverFits logs a forcedNeverFits refusal; at says where it was made
// ("arrival" or "queue").
func (c *schedCore) refuseNeverFits(p *core.Profile, e *Error, at string) {
	c.log.Warn("forced profile cannot fit even with its nodes empty; refused before any eviction", "profile", p.Name,
		"need_mib", e.NeedMiB, "free_mib", e.CapacityMiB, "nodes", slices.Clone(p.ForceNodes), "at", at, "reason", e.Message)
}

// agentForcedGate applies agentForcedFit and, as the pre-flight does in fleet
// membership, defaults.forced_fit_check to a new instance before its worker
// launches are sent. It returns false when the instance was refused: not a
// load failure, so no backoff and no quarantine; the nodes are released
// unsettled.
func (c *schedCore) agentForcedGate(in *inst) bool {
	err := c.agentForcedFit(in.prof, in.placement)
	if err == nil {
		ff := c.forcedFitCheck(in.prof, in.placement)
		if ff == nil {
			return true
		}
		if ff.mode != config.ForcedFitReject {
			c.log.Warn("forced profile may not fit", "profile", in.prof.Name, "need_mib", ff.need,
				"free_mib", ff.free, "nodes", ff.nodes, "measured", ff.measured)
			return true
		}
		err = ff.err(in.prof.Name)
	}
	c.log.Warn("forced profile does not fit; rejecting", "profile", in.prof.Name, "need_mib", err.NeedMiB,
		"free_mib", err.CapacityMiB, "nodes", nodeIDStrings(in.nodeIDs()), "reason", err.Message)
	ws := in.waiters
	in.waiters = nil
	c.failAll(ws, err)
	c.removeInst(in, false)
	return false
}

var _ = http.StatusOK
