package scheduler

import (
	"fmt"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// When the reservation gate (reserve.go) refuses the placement the placer
// found, the request waits only if waiting can help: something of ours
// holds, loads, drains, claims or settles on a node the profile could use,
// or such a node is paused or may come back with other figures. Otherwise
// the nodes stay as they are, the request would wait until queue_timeout
// for nothing, and it is refused at once (the live finding after 0.1.6:
// with the reservation settings on, a forced profile that can never fit
// was queued where the defaults refuse it).

// gateNeverFits is the refusal of p on placement pl, which the gate has
// just refused, when waiting cannot help; nil when it can (the request
// waits, as before).
func (c *schedCore) gateNeverFits(p *core.Profile, pl core.Placement) *Error {
	if !c.gateOn() || len(pl.Nodes) == 0 || c.waitCanHelp(p) {
		return nil
	}
	rows, ok := c.memoryRows(p, pl, map[string]bool{p.Name: true})
	if !ok {
		return nil
	}
	var parts []string
	take, has := 0, 0
	for _, r := range rows {
		take, has = take+r.WillTakeMiB+r.MarginMiB, has+r.AvailableMiB
		if r.RemainsMiB >= 0 {
			continue
		}
		margin := fmt.Sprintf("margin %d MiB", r.MarginMiB)
		if r.MarginRule != "" {
			margin += " (" + r.MarginRule + ")"
		}
		parts = append(parts, fmt.Sprintf("node %s would take %d MiB (weights %d + context %d + fill reserve %d) + %s but has %d MiB available; short by %d MiB",
			r.Node, r.WillTakeMiB, r.WeightsMiB, r.ContextMiB, r.FillReserveMiB, margin, r.AvailableMiB, -r.RemainsMiB))
	}
	if len(parts) == 0 {
		return nil
	}
	where, reason := "any placement of its nodes", ReasonDoesNotFit
	if len(p.ForceNodes) > 0 {
		where, reason = "the nodes it is forced onto", ReasonForcedDoesNotFit
	}
	msg := fmt.Sprintf("profile %s does not fit %s by the memory reservation rule: %s; nothing of ours is loaded, loading or claimed there and no node can come back, so waiting cannot help",
		p.Name, where, strings.Join(parts, "; "))
	e := err503(reason, msg, 0)
	if reason == ReasonForcedDoesNotFit {
		e.RetryAfter = queueRetryAfter // as the other "forced nodes do not fit" refusals
	} else {
		msg += c.excludedNote(p)
		e.Message = msg
	}
	e.NeedMiB, e.CapacityMiB = take, has
	return e
}

// waitCanHelp reports whether something can change on the nodes p could
// use: for a forced profile its forced nodes (a node not in the view, not
// eligible, with an unknown figure or unplaced may come back), else every
// reachable node it may be placed on, as the capacity check counts them. A
// node that is not free holds, loads, drains, is claimed, settles or is
// paused. A node whose free figure is a pre-flight's for a while (an
// instance was just returned on it) waits for the view's figure again.
// Without a fresh view nothing is decided.
func (c *schedCore) waitCanHelp(p *core.Profile) bool {
	if len(p.ForceNodes) > 0 {
		for _, name := range p.ForceNodes {
			id, err := c.view.Resolve(name)
			if err != nil {
				return true
			}
			n, ok := c.node(id)
			if !ok || !c.eligible(id) || n.FreeUnknown || c.unplaced[id] != "" || !c.isFree(id) || c.overridden(id) {
				return true
			}
		}
		return false
	}
	if !c.fresh {
		return true
	}
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		// as the capacity check: an unreachable node is not counted
		if !ok || c.excluded(n) || !c.reachable(n) || (localOnly(p) && !n.Local) || c.autoOff(p, n) {
			continue
		}
		if !c.eligible(id) || n.FreeUnknown || c.unplaced[id] != "" || !c.isFree(id) || c.overridden(id) {
			return true
		}
	}
	return false
}

// overridden: node id's free figure is a pre-flight's until it expires.
func (c *schedCore) overridden(id core.NodeID) bool {
	ns := c.nodes[id]
	return ns != nil && !ns.ovUntil.IsZero() && c.now.Before(ns.ovUntil)
}
