package scheduler

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
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
// more, with every node back. At most one retry per free node, and none
// once a refused set holds a just-released node (it may pass on the next
// view; the request waits for it, sixth review SS2). pl is the
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
	if len(p.ForceNodes) > 0 || c.anyJustReleased(pl) {
		// a just-released node may pass on the next view: wait for the
		// preferred set rather than widen (sixth review SS2)
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
		if c.anyJustReleased(next) {
			break // as above: this set may pass on the next view
		}
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
			short = append(short, shortRowText(r, "has"))
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

// shortRowText says how row r falls short; has is "has" or "would have".
func shortRowText(r PlanMemoryNode, has string) string {
	margin := fmt.Sprintf("margin %d MiB", r.MarginMiB)
	if r.MarginRule != "" {
		margin += " (" + r.MarginRule + ")"
	}
	return fmt.Sprintf("node %s would take %d MiB (weights %d + context %d + fill reserve %d) + %s but %s %d MiB available; short by %d MiB",
		r.Node, r.WillTakeMiB, r.WeightsMiB, r.ContextMiB, r.FillReserveMiB, margin, has, r.AvailableMiB, -r.RemainsMiB)
}

// shortEvenFreed is the owner's case (fifth review): forced profile p is
// refused at once when its forced placement pl is short by the reservation
// rule even with every instance of ours on those nodes stopped. What a
// holder frees counts conservatively, at the smaller of what it reported
// and what is reserved for it (emptyFree), so a refusal is never made on
// memory a stop might not return. It waits (nil) while anything else can
// change there: a claim, a settle, an agent's hold, a pause, a holder whose
// figure cannot be told. Nil with the gate off (the defaults).
func (c *schedCore) shortEvenFreed(p *core.Profile, pl core.Placement) *Error {
	if !c.gateOn() || len(pl.Nodes) == 0 || c.anyJustReleased(pl) {
		return nil
	}
	freed := map[string]bool{p.Name: true}
	var holders []string
	for _, pn := range pl.Nodes {
		if c.isFree(pn.ID) {
			continue
		}
		ns := c.nodes[pn.ID]
		n, _ := c.node(pn.ID)
		if ns == nil || ns.holder == "" || ns.claim != "" || ns.settle != setNone || c.noNewLoad(pn.ID) {
			return nil
		}
		if _, _, ok := c.emptyFree(pn.ID, n); !ok {
			return nil
		}
		if !freed[ns.holder] {
			freed[ns.holder] = true
			holders = append(holders, ns.holder)
		}
	}
	if len(holders) == 0 {
		return nil
	}
	rows, ok := c.memoryRows(p, pl, freed)
	if !ok {
		return nil
	}
	var parts []string
	take, has := 0, 0
	for _, r := range rows {
		take, has = take+r.WillTakeMiB+r.MarginMiB, has+r.AvailableMiB
		if r.RemainsMiB < 0 {
			parts = append(parts, shortRowText(r, "would have only"))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	msg := fmt.Sprintf("profile %s does not fit the nodes it is forced onto by the memory reservation rule even with %s stopped (counted at the smaller of what %s reported and what is reserved): %s after freeing; nothing loaded is stopped for it",
		p.Name, strings.Join(holders, ", "), pronounFor(len(holders)), strings.Join(parts, "; "))
	e := err503(ReasonForcedDoesNotFit, msg, queueRetryAfter)
	e.NeedMiB, e.CapacityMiB = take, has
	return e
}

func pronounFor(n int) string {
	if n == 1 {
		return "it"
	}
	return "each"
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
//
// The window is the upper bound: a memory reading of the node taken after
// the release and its settle time (freshAfter) ends it at once (eighth
// review ES2), so a load after an unload costs about one heartbeat.
func (c *schedCore) justReleased(id core.NodeID) bool {
	end, ok := c.releaseWindowEnd(id)
	if !ok || !c.now.Before(end) {
		return false
	}
	n, _ := c.node(id)
	return !n.FigureAt.After(c.freshAfter(id))
}

// releaseWindowEnd is when node id's just-released window ends; ok is false
// when the node is held or nothing that ran has left it.
func (c *schedCore) releaseWindowEnd(id core.NodeID) (time.Time, bool) {
	ns := c.nodes[id]
	if ns == nil || ns.holder != "" || ns.releasedAt.IsZero() {
		return time.Time{}, false
	}
	w := c.cfg.Defaults.FreeAverageWindow
	if w <= 0 {
		w = releaseGrace
	}
	return ns.releasedAt.Add(w), true
}

// freshAfter is the time after which a reading of node id counts as fresh:
// the release plus the node's settle time (what the settle waits for the
// memory to come back).
func (c *schedCore) freshAfter(id core.NodeID) time.Time {
	n, _ := c.node(id)
	return c.nodes[id].releasedAt.Add(n.Settle)
}

// watchFigures tells a FigureWatch view which nodes the queue waits on for
// a fresh reading, when that changes.
func (c *schedCore) watchFigures() {
	fw, ok := c.view.(FigureWatch)
	if !ok {
		return
	}
	want := map[core.NodeID]time.Time{}
	if len(c.entries) > 0 {
		for _, id := range c.sortedNodeIDs() {
			if c.justReleased(id) {
				want[id] = c.freshAfter(id)
			}
		}
	}
	if maps.Equal(want, c.wantFig) {
		return
	}
	c.wantFig = want
	fw.WantFigures(maps.Clone(want))
}

// forcedFitWaits: in agents membership, forced profile p's check on its
// nodes' figures (agentForcedFit, forced_fit_check reject) would refuse
// placement pl, but a node of pl was just released and its figure may still
// show what left it. The request waits for the next view instead of being
// refused (sixth review SS1, the all-free case of the same check; it acts
// with the defaults too).
func (c *schedCore) forcedFitWaits(p *core.Profile, pl core.Placement) bool {
	if !c.agents || len(p.ForceNodes) == 0 || !c.anyJustReleased(pl) {
		return false
	}
	if c.agentForcedFit(p, pl) != nil {
		return true
	}
	ff := c.forcedFitCheck(p, pl)
	return ff != nil && ff.mode == config.ForcedFitReject
}

// forcedFitWaitText is the plan's reason while forcedFitWaits holds.
func forcedFitWaitText(pl core.Placement, c *schedCore) string {
	var ids []string
	for _, pn := range pl.Nodes {
		if c.justReleased(pn.ID) {
			ids = append(ids, string(pn.ID))
		}
	}
	return "waits for the next figures of " + strings.Join(ids, ", ") + ": something of ours just left there and the figure may still show it"
}

// refusesBeforeEvict: a check refuses a forced placement before an eviction
// (forcedRefusalBeforeEvict, shortEvenFreed): agents membership,
// forced_fit_check reject, or the reservation gate on.
func (c *schedCore) refusesBeforeEvict() bool {
	return c.agents || c.cfg.Defaults.ForcedFitCheck == config.ForcedFitReject || c.gateOn()
}

// anyJustReleased: a node of pl was just released (justReleased).
func (c *schedCore) anyJustReleased(pl core.Placement) bool {
	for _, pn := range pl.Nodes {
		if c.justReleased(pn.ID) {
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
