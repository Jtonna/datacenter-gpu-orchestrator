package scheduler

import (
	"fmt"
	"slices"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

var timeZero time.Time

// accumulate advances every unclaimed entry's queue_timeout clock by the
// time since the last step, unless an instance was starting during that
// time (design 8.3). State does not change between steps, so the state
// left by the previous step decides.
func (c *schedCore) accumulate() {
	if c.last.IsZero() {
		c.last = c.now
		return
	}
	d := c.now.Sub(c.last)
	if d <= 0 || c.anyStarting() {
		return
	}
	for _, e := range c.entries {
		if !e.claimed() {
			e.clock += d
		}
	}
}

// runPasses runs the queue pass of design 8.3 after every event.
//
// DESIGN: 8.3 says "one pass". A pass can unblock an entry it has already
// visited (entry B, younger than A, drops its claim in step 5 and its
// draining victim returns to ready and admits A's waiters, or frees what
// A needs); with exactly one pass that progress would wait for an
// unrelated event, possibly forever. The pass is repeated (at most
// maxPassesPerStep times) while it changes something; each repeat applies
// the same rules in the same order.
func (c *schedCore) runPasses() {
	for i := 0; i < maxPassesPerStep; i++ {
		c.changed = false
		for _, e := range c.orderedEntries() {
			if c.entries[e.prof.Name] == e {
				c.evalEntry(e)
			}
		}
		if !c.changed {
			return
		}
	}
}

func (c *schedCore) evalEntry(e *entry) {
	p := e.prof
	if c.shutdown {
		c.answerEntry(e, err503(ReasonShuttingDown, "the scheduler is shutting down", queueRetryAfter))
		return
	}
	if b, on := c.backoffActive(p.Name); on {
		c.answerEntry(e, err503(ReasonBackoff, fmt.Sprintf("profile %s is backed off after a failed load", p.Name), b.until.Sub(c.now)))
		return
	}
	if why, bad := c.forcedExcluded(p); bad && c.insts[p.Name] == nil {
		c.answerEntry(e, c.excludedForcedErr(p, why))
		return
	}
	if c.insts[p.Name] == nil {
		if err := c.failFastForced(p, len(e.waiters)); err != nil {
			c.answerEntry(e, err)
			return
		}
		if !e.claimed() && len(e.waiters) > 0 {
			if err := c.forcedNeverFits(p); err != nil {
				c.refuseNeverFits(p, err, "queue")
				c.answerEntry(e, err)
				return
			}
		}
	}
	if e.claimed() {
		switch {
		case c.claimBroken(e):
			// Step 5: a claimed node is no longer eligible.
			c.dropClaim(e)
			if len(e.waiters) == 0 {
				c.deleteEntry(e) // step 6, third case
				return
			}
		case len(e.waiters) == 0 && !e.stoppedAny:
			// Step 6, first case: nothing was unloaded for it yet.
			c.dropClaim(e)
			c.deleteEntry(e)
			return
		case c.claimSettled(e):
			// Step 4.
			if c.startOnClaim(e) {
				return
			}
			if len(e.waiters) == 0 {
				c.deleteEntry(e)
				return
			}
		default:
			return // waiting for victims and settling
		}
	}
	if len(e.waiters) == 0 {
		c.deleteEntry(e)
		return
	}
	if c.insts[p.Name] == nil {
		if c.place(e) {
			return
		}
		c.noteOccupied(e) // occupied.go: observation only
		if c.plan(e) {
			return
		}
	}
	c.checkTimeout(e)
}

// place is step 1: the placer over the free nodes at live budgets.
func (c *schedCore) place(e *entry) bool {
	req, _, err := c.request(e.prof)
	if err != nil {
		c.needFailed(e, err)
		return true
	}
	pl, gate, tried, err := c.gatedPlace(e.prof, req) // neverfits.go
	if err != nil {
		return false
	}
	if gate != "" {
		if ne := c.gateNeverFits(e.prof, tried); ne != nil {
			c.log.Warn("request cannot fit even with nothing of ours in the way; refused", "profile", e.prof.Name,
				"need_mib", ne.NeedMiB, "available_mib", ne.CapacityMiB, "nodes", placementIDs(pl), "tried", placementsText(tried), "reason", ne.Message)
			c.answerEntry(e, ne)
			return true
		}
		return false
	}
	if c.forcedFitWaits(e.prof, pl) { // neverfits.go
		return false
	}
	c.deleteEntry(e)
	c.startInstance(e.prof, pl, e.waiters, e)
	return true
}

// plan is step 2: the placer over free nodes plus evictable instances.
func (c *schedCore) plan(e *entry) bool {
	req, _, err := c.request(e.prof)
	if err != nil {
		c.needFailed(e, err)
		return true
	}
	ev := c.evictables(e.prof, e.clock)
	if len(ev) == 0 {
		return false
	}
	res, gate, err := c.evictPlan(e.prof, req, ev) // tight.go
	if err != nil || gate != "" {
		return false
	}
	// A forced profile whose node was just released waits: the refusal
	// below must run on figures that do not lag, and nothing is evicted for
	// a request it would then refuse (seventh review AB1). Only where such
	// a refusal exists (eighth review): in fleet membership with the
	// defaults nothing refuses a forced placement before its launch, and
	// it evicts as in 0.1.6.
	if len(e.prof.ForceNodes) > 0 && c.refusesBeforeEvict(e.prof) && c.anyJustReleased(res.Placement) {
		return false
	}
	if c.evictWaits(e.prof, res.Placement, res.Victims, e.evictHeld) {
		if e.evictHeld.IsZero() {
			e.evictHeld = c.now
		}
		return false
	}
	// A forced profile that could not fit its nodes even with nothing loaded
	// on them is refused now, before any victim is stopped (forcedfit.go).
	if ferr := c.forcedRefusalBeforeEvict(e.prof, res.Placement); ferr != nil {
		var nodes []string
		for _, n := range res.Placement.Nodes {
			nodes = append(nodes, string(n.ID))
		}
		c.log.Warn("forced profile cannot fit even with its nodes empty; refused before any eviction", "profile", e.prof.Name,
			"need_mib", ferr.NeedMiB, "free_mib", ferr.CapacityMiB, "nodes", nodes,
			"would_evict", slices.Clone(res.Victims), "reason", ferr.Message)
		c.answerEntry(e, ferr)
		return true
	}
	e.claim = make([]core.NodeID, 0, len(res.Placement.Nodes))
	for _, n := range res.Placement.Nodes {
		e.claim = append(e.claim, n.ID)
		c.ns(n.ID).claim = e.prof.Name
	}
	e.victims = slices.Clone(res.Victims)
	e.stoppedAny = false
	c.occEvicted(e, res.Victims) // occupied.go
	c.changed = true
	c.log.Info("claim made", "profile", e.prof.Name, "nodes", nodeIDStrings(e.claim), "victims", slices.Clone(res.Victims),
		"waiters", len(e.waiters))
	for _, v := range res.Victims {
		in := c.insts[v]
		c.recordInst(in, history.TEviction, history.Info,
			fmt.Sprintf("%s is being evicted to make room for %s, which waited %s (%d waiting).", v, e.prof.Name,
				e.clock.Round(time.Second), len(e.waiters)),
			map[string]any{"victim": v, "for_profile": e.prof.Name, "waited_s": secs(e.clock), "waiters": len(e.waiters),
				"victim_busy": in.inFlight > 0, "in_flight": in.inFlight})
		in.victimOf = e.prof.Name
		if in.inFlight == 0 {
			c.stopFor(in, stopGraceful, "evicted for "+e.prof.Name)
		} else {
			in.state = stDraining
			in.drainInFlight = in.inFlight
			c.log.Info("victim draining", "profile", v, "entry", e.prof.Name, "reason", "evicted for "+e.prof.Name,
				"in_flight", in.inFlight)
		}
	}
	return true
}

// evictables lists the instances evictable for an entry of profile p
// whose clock reads clock (design 8.3), with their eligible nodes at plan
// budgets.
func (c *schedCore) evictables(p *core.Profile, clock time.Duration) []placer.Evictable {
	var out []placer.Evictable
	for _, in := range c.sortedInsts() {
		if !c.evictable(in, clock) || c.onNoAuto(p, in) {
			continue
		}
		ev := placer.Evictable{Instance: in.prof.Name, Busy: in.inFlight > 0, LoadDuration: in.loadDuration}
		if in.inFlight == 0 {
			ev.IdleFor = c.now.Sub(in.lastUsed)
		}
		for _, pn := range in.placement.Nodes {
			n, ok := c.node(pn.ID)
			// A node that takes no new load is not offered: evicting its
			// instance would stop a model to make room the node may not use.
			if !ok || !c.eligible(pn.ID) || c.noNewLoad(pn.ID) || (localOnly(p) && !n.Local) {
				continue
			}
			ev.Nodes = append(ev.Nodes, c.candidate(p, pn.ID, n, c.planBudget(pn.ID, n), false))
		}
		if len(ev.Nodes) > 0 {
			out = append(out, ev)
		}
	}
	return out
}

// onNoAuto: an automatic profile p never evicts an instance that runs on a
// no_auto node, even when the instance also uses other nodes (CR-9 review).
func (c *schedCore) onNoAuto(p *core.Profile, in *inst) bool {
	for _, pn := range in.placement.Nodes {
		if n, ok := c.node(pn.ID); ok && c.autoOff(p, n) {
			return true
		}
	}
	return false
}

// evictable: ready, past its hold, and idle or the entry has been unclaimed
// for at least drain_after.
//
// DESIGN: "unclaimed for at least drain_after" is read as the entry's
// accumulated unclaimed time, the same clock as queue_timeout, which 7.5
// calls "its accumulated unclaimed time". So it pauses while an instance
// is starting, and a returned entry keeps what it had.
func (c *schedCore) evictable(in *inst, clock time.Duration) bool {
	if in.state != stReady || c.now.Sub(in.readyAt) < c.hold(in) {
		return false
	}
	return in.inFlight == 0 || clock >= c.cfg.Defaults.DrainAfter
}

// claimBroken is step 5: a claimed node stopped being eligible (offline,
// gone from the view, quarantined, or the view went stale), or it takes no new
// load any more (paused, held or unplaced after the claim was made): pause
// wins over a claim, and the request is planned again.
//
// DESIGN: 7.1's eligibility includes "budget >= min_share", but a claimed
// node held by a victim has almost no live budget until the victim exits,
// so applying that rule here would drop every claim at once. Step 5 lists
// "offline, quarantined, or the fleet view goes stale", and that is what
// is checked (plus allow_tiers); a node without the room is found by step
// 4's re-fit, which records the empty figure.
func (c *schedCore) claimBroken(e *entry) bool {
	for _, id := range e.claim {
		n, ok := c.node(id)
		if !ok || !c.eligible(id) || c.noNewLoad(id) {
			return true
		}
		if len(e.prof.AllowTiers) > 0 && !slices.Contains(e.prof.AllowTiers, n.Priority) {
			return true
		}
	}
	return false
}

// claimSettled: every victim has exited and every claimed node has settled.
func (c *schedCore) claimSettled(e *entry) bool {
	for _, v := range e.victims {
		if in := c.insts[v]; in != nil && in.victimOf == e.prof.Name {
			return false
		}
	}
	for _, id := range e.claim {
		ns := c.ns(id)
		if ns.holder != "" || ns.settle != setNone {
			return false
		}
	}
	return true
}

// claimNodes is a placement's node list for the claimed ids.
func claimNodes(ids []core.NodeID) []core.PlacedNode {
	out := make([]core.PlacedNode, len(ids))
	for i, id := range ids {
		out[i] = core.PlacedNode{ID: id}
	}
	return out
}

// startOnClaim is step 4: place over exactly the claimed nodes at live
// budgets. On failure the observed free figures become the nodes' empty
// figures and the claim is dropped.
func (c *schedCore) startOnClaim(e *entry) bool {
	req, _, err := c.request(e.prof)
	if err != nil {
		c.needFailed(e, err)
		return true
	}
	var cands []placer.Candidate
	for _, id := range e.claim {
		n, _ := c.node(id)
		cands = append(cands, c.candidate(e.prof, id, n, c.liveBudget(id, n), true))
	}
	pl, err := placer.Place(req, cands)
	gate := ""
	if err == nil {
		gate = c.reserveGate(e.prof, pl, nil)
	}
	// The victims' release makes the claimed nodes just released: their
	// figures may still show the victims. Keep the claim and decide on the
	// next figures; never refuse, drop the claim or learn an empty figure
	// from a lagging one (seventh review AS1).
	if c.anyJustReleased(core.Placement{Nodes: claimNodes(e.claim)}) && (err != nil || gate != "" || c.forcedFitWaits(e.prof, pl)) {
		return true
	}
	if err == nil && gate == "" {
		c.dropClaim(e) // nodes it does not use are released
		c.deleteEntry(e)
		c.startInstance(e.prof, pl, e.waiters, e)
		return true
	}
	for _, id := range e.claim {
		n, _ := c.node(id)
		ns := c.ns(id)
		ns.emptyClaim, ns.hasClaim = c.freeMiB(id, n), true
	}
	c.log.Info("claimed nodes no longer fit; claim dropped", "profile", e.prof.Name, "nodes", e.claim)
	c.dropClaim(e)
	return false
}

// dropClaim releases an entry's claim. Victims still draining return to
// ready (and admit their own profile's entry); stopping ones stay so.
func (c *schedCore) dropClaim(e *entry) {
	for _, id := range e.claim {
		if ns := c.nodes[id]; ns != nil && ns.claim == e.prof.Name {
			ns.claim = ""
		}
	}
	victims := e.victims
	e.claim, e.victims, e.stoppedAny = nil, nil, false
	c.changed = true
	for _, v := range victims {
		in := c.insts[v]
		if in == nil || in.victimOf != e.prof.Name {
			continue
		}
		in.victimOf = ""
		if in.state == stDraining && !in.drain { // drain.go: an asked drain goes on
			in.state = stReady
			c.admitEntry(in)
		}
	}
}

// checkTimeout answers an unclaimed entry whose clock passed queue_timeout.
func (c *schedCore) checkTimeout(e *entry) {
	if e.claimed() || e.clock < c.cfg.Defaults.QueueTimeout {
		return
	}
	reason, why := c.waitWhy(e.prof)
	c.occRefused(e, "queue_timeout") // occupied.go
	c.log.Warn("queue timeout", "profile", e.prof.Name, "waited", e.clock, "reason", string(reason), "detail", why, "waiters", len(e.waiters))
	c.answerEntry(e, err503(reason, fmt.Sprintf("no room for %s within queue_timeout: %s", e.prof.Name, why), queueRetryAfter))
}

// answerEntry fails every waiter of an entry, then drops its claim and
// deletes it. Waiters are answered first so that a victim returning to
// ready cannot admit them.
func (c *schedCore) answerEntry(e *entry, err *Error) {
	c.occRefused(e, "refused: "+string(err.Reason)) // occupied.go; a timeout said so first
	ws := e.waiters
	e.waiters = nil
	c.failAll(ws, err)
	c.dropClaim(e)
	c.deleteEntry(e)
}

// needFailed: the profile's need could not be determined (unreadable model
// file). That is a load failure for the profile: 502 and backoff.
func (c *schedCore) needFailed(e *entry, err error) {
	c.occRefused(e, "refused: memory need unknown") // occupied.go
	ws := e.waiters
	e.waiters = nil
	c.failAll(ws, err502(fmt.Sprintf("memory need of %s: %v", e.prof.Name, err), nil))
	c.dropClaim(e)
	c.deleteEntry(e)
	c.startBackoff(e.prof.Name)
}
