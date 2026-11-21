package scheduler

import (
	"fmt"
	"math"
	"net/http"
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
}

// message is the plain-words account used in the 503, the warning and the plan.
func (f *forcedFit) message(profile string) string {
	var parts []string
	if f.total {
		parts = append(parts, fmt.Sprintf("profile %s: need %d MiB (%s), but the nodes it is forced onto have only %d MiB usable in total (%s)",
			profile, f.need, f.basis, f.free, strings.Join(f.detail, "; ")))
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
	mode := c.cfg.Defaults.ForcedFitCheck
	if (mode != config.ForcedFitWarn && mode != config.ForcedFitReject) || len(p.ForceNodes) == 0 || len(pl.Nodes) == 0 {
		return nil
	}
	info, err := c.need(p)
	if err != nil {
		return nil
	}
	f := &forcedFit{mode: mode, need: info.Need.WithMargin(), measured: info.Need.Measured, basis: c.needBasis(info)}
	for _, pn := range pl.Nodes {
		n, ok := c.node(pn.ID)
		if !ok {
			return nil
		}
		label := nodeLabel(pn.ID, n)
		usable, how := c.freeMiB(pn.ID, n), "free memory, no headroom kept"
		if n.SharedMemory {
			usable, how = c.sharedBudget(pn.ID, n)
		}
		f.free += usable
		f.nodes = append(f.nodes, label)
		f.detail = append(f.detail, fmt.Sprintf("usable %d MiB on %s (%s)", usable, label, how))
	}
	f.total = f.need > f.free
	f.short = c.sharedShortfalls(p, info, pl)
	if !f.total && len(f.short) == 0 {
		return nil
	}
	return f
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
// held to the total only, so its share takes no margin). Nil when the need is
// unknown.
func (c *schedCore) expectShares(p *core.Profile, pl core.Placement) []int {
	info, err := c.need(p)
	if err != nil || info.Need.MiB <= 0 || len(pl.Nodes) == 0 {
		return nil
	}
	need := info.Need.WithMargin()
	if len(pl.Nodes) == 1 {
		return []int{need}
	}
	fracs := instance.SplitFractions(*p, pl)
	if len(fracs) != len(pl.Nodes) {
		return nil
	}
	out := make([]int, len(pl.Nodes))
	for i, pn := range pl.Nodes {
		if m, ok := c.learned[p.Name].shareFor(pl, fracs, pn.ID); ok {
			out[i] = m
			continue
		}
		share := float64(need) * fracs[i]
		if n, ok := c.node(pn.ID); ok && n.SharedMemory {
			share *= 1 + c.cfg.Defaults.SplitSkewMargin
		}
		out[i] = int(math.Ceil(share - 1e-6))
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
		n, ok := c.node(pn.ID)
		if pn.Local || !ok || n.FreeUnknown {
			continue
		}
		usable := c.freeMiB(pn.ID, n)
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
		parts = append(parts, fmt.Sprintf("node %s is expected to take %d MiB but has %d MiB usable (%s)", nodeLabel(pn.ID, n), shares[i], usable, how))
	}
	if len(parts) == 0 {
		return nil
	}
	e := err503(ReasonForcedDoesNotFit, fmt.Sprintf("profile %s does not fit the nodes it is forced onto: %s; the node's agent would refuse the launch (would_not_fit), so it is not sent",
		p.Name, strings.Join(parts, "; ")), queueRetryAfter)
	e.NeedMiB, e.CapacityMiB = need, usableSum
	return e
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
