package scheduler

import (
	"fmt"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// When the reservation gate (reserve.go) refuses the placement the placer
// found, the request waits only if waiting can help: something of ours
// holds, loads, drains, claims or settles on a node the profile could use,
// or such a node is paused or may come back with other figures. Otherwise
// the nodes stay as they are, the request would wait until queue_timeout
// for nothing, and it is refused at once (the live finding after 0.1.6:
// with the reservation settings on, a forced profile that can never fit
// was queued where the defaults refuse it).

// gatedPlace is step 1 under the reservation gate: the placer over the free
// nodes, held to the gate. The gate sees per-node shares, peaks and margins
// the placer does not, so it may refuse the placer's pick where another set
// of the same free nodes passes (the fifth review's FS1). For an automatic
// profile the placer is then asked again: first without the nodes the gate
// found short, at the same size; when no such set is left, for one node
// more, with every node back. At most one retry per free node. pl is the
// first placement that passes, else the placer's first choice with gate its
// reason; tried lists every placement the gate refused, in order.
func (c *schedCore) gatedPlace(p *core.Profile, req placer.Request) (pl core.Placement, gate string, tried []core.Placement, err error) {
	cands := c.freeCandidates(p)
	pl, err = placer.Place(req, cands)
	if err != nil {
		return pl, "", nil, err
	}
	if gate = c.reserveGate(p, pl, nil); gate == "" {
		return pl, "", nil, nil
	}
	tried = []core.Placement{pl}
	if len(p.ForceNodes) > 0 {
		return pl, gate, tried, nil
	}
	out := map[core.NodeID]bool{}
	cur := pl
	for range len(cands) {
		short := c.shortNodes(p, cur)
		if len(short) == 0 {
			short = placementNodeIDs(cur) // refused without a short row: leave the set out
		}
		for _, id := range short {
			out[id] = true
		}
		var rest []placer.Candidate
		for _, cd := range cands {
			if !out[cd.ID] {
				rest = append(rest, cd)
			}
		}
		r := req
		r.MinNodes = len(cur.Nodes)
		next, perr := placer.Place(r, rest)
		if perr != nil {
			// no other set of this size: one node more, every node back
			clear(out)
			r.MinNodes = len(cur.Nodes) + 1
			if next, perr = placer.Place(r, cands); perr != nil {
				break
			}
		}
		if c.reserveGate(p, next, nil) == "" {
			return next, "", tried, nil
		}
		tried, cur = append(tried, next), next
	}
	c.reserveGate(p, pl, nil) // the mark (TN1) is the first choice's again
	return pl, gate, tried, nil
}

// shortNodes are the nodes of pl the gate finds short for p.
func (c *schedCore) shortNodes(p *core.Profile, pl core.Placement) []core.NodeID {
	rows, ok := c.memoryRows(p, pl, map[string]bool{p.Name: true})
	if !ok {
		return nil
	}
	var out []core.NodeID
	for i, r := range rows {
		if r.RemainsMiB < 0 && i < len(pl.Nodes) {
			out = append(out, pl.Nodes[i].ID)
		}
	}
	return out
}

func placementNodeIDs(pl core.Placement) []core.NodeID {
	out := make([]core.NodeID, len(pl.Nodes))
	for i, n := range pl.Nodes {
		out[i] = n.ID
	}
	return out
}

// gateNeverFits is the refusal of p after the gate refused every placement
// in tried (gatedPlace), when waiting cannot help; nil when it can (the
// request waits, as before).
func (c *schedCore) gateNeverFits(p *core.Profile, tried []core.Placement) *Error {
	if !c.gateOn() || len(tried) == 0 || c.waitCanHelp(p) {
		return nil
	}
	var parts []string
	take, has := 0, 0
	for i, pl := range tried {
		if len(pl.Nodes) == 0 {
			return nil
		}
		rows, ok := c.memoryRows(p, pl, map[string]bool{p.Name: true})
		if !ok {
			return nil
		}
		var short []string
		t, h := 0, 0
		for _, r := range rows {
			t, h = t+r.WillTakeMiB+r.MarginMiB, h+r.AvailableMiB
			if r.RemainsMiB >= 0 {
				continue
			}
			margin := fmt.Sprintf("margin %d MiB", r.MarginMiB)
			if r.MarginRule != "" {
				margin += " (" + r.MarginRule + ")"
			}
			short = append(short, fmt.Sprintf("node %s would take %d MiB (weights %d + context %d + fill reserve %d) + %s but has %d MiB available; short by %d MiB",
				r.Node, r.WillTakeMiB, r.WeightsMiB, r.ContextMiB, r.FillReserveMiB, margin, r.AvailableMiB, -r.RemainsMiB))
		}
		if len(short) == 0 {
			return nil
		}
		if i == 0 {
			take, has = t, h
		}
		if len(tried) > 1 {
			parts = append(parts, fmt.Sprintf("on %s: %s", strings.Join(placementIDs(pl), ", "), strings.Join(short, "; ")))
		} else {
			parts = append(parts, short...)
		}
	}
	reason := ReasonDoesNotFit
	where := fmt.Sprintf("any placement the placer finds on its nodes (tried %d: %s)", len(tried), placementsText(tried))
	if len(p.ForceNodes) > 0 {
		where, reason = "the nodes it is forced onto", ReasonForcedDoesNotFit
	}
	msg := fmt.Sprintf("profile %s does not fit %s by the memory reservation rule: %s; nothing of ours is loaded, loading or claimed there and no node can come back, so waiting cannot help",
		p.Name, where, strings.Join(parts, "; "))
	if reason == ReasonDoesNotFit {
		msg += c.excludedNote(p)
	}
	e := err503(reason, msg, 0)
	// as the other "forced nodes do not fit" refusals, and for an automatic
	// profile too: memory taken by others may come back (FS2)
	e.RetryAfter = queueRetryAfter
	e.NeedMiB, e.CapacityMiB = take, has
	return e
}

// placementsText is "a, b and c, d" for the node sets of pls.
func placementsText(pls []core.Placement) string {
	var sets []string
	for _, pl := range pls {
		sets = append(sets, strings.Join(placementIDs(pl), ", "))
	}
	return strings.Join(sets, " | ")
}

// waitCanHelp reports whether something can change on the nodes p could
// use: for a forced profile its forced nodes (a node not in the view, not
// eligible, with an unknown figure or unplaced may come back), else every
// reachable node it may be placed on, as the capacity check counts them. A
// node that is not free holds, loads, drains, is claimed, settles or is
// paused. A node whose free figure is a pre-flight's for a while (an
// instance was just returned on it) waits for the view's figure again, and
// so does a node something of ours left recently (justReleased).
// Without a fresh view nothing is decided.
func (c *schedCore) waitCanHelp(p *core.Profile) bool {
	if len(p.ForceNodes) > 0 {
		for _, name := range p.ForceNodes {
			id, err := c.view.Resolve(name)
			if err != nil {
				return true
			}
			n, ok := c.node(id)
			if !ok || !c.eligible(id) || n.FreeUnknown || c.unplaced[id] != "" || !c.isFree(id) || c.overridden(id) || c.justReleased(id) {
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
		if !c.eligible(id) || n.FreeUnknown || c.unplaced[id] != "" || !c.isFree(id) || c.overridden(id) || c.justReleased(id) {
			return true
		}
	}
	return false
}

// releaseGrace is how long a node's figure may still show what an instance
// of ours released there held, when free_average_window is 0.
const releaseGrace = 30 * time.Second

// justReleased: something of ours left node id recently, within
// free_average_window (or releaseGrace when that is 0). Its figure may still
// show the old load (heartbeat lag, or a worker that has not returned its
// buffers yet), so it may still change: the fifth review's FB1, the
// benchmark pattern of unloading and at once loading the next model.
func (c *schedCore) justReleased(id core.NodeID) bool {
	if c.cfg.Defaults.FreeAverageWindow > 0 {
		return c.avgStale(id)
	}
	ns := c.nodes[id]
	return ns != nil && !ns.heldChanged.IsZero() && c.now.Before(ns.heldChanged.Add(releaseGrace))
}

// overridden: node id's free figure is a pre-flight's until it expires.
func (c *schedCore) overridden(id core.NodeID) bool {
	ns := c.nodes[id]
	return ns != nil && !ns.ovUntil.IsZero() && c.now.Before(ns.ovUntil)
}
