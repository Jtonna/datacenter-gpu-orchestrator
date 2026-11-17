package scheduler

import (
	"fmt"
	"slices"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
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
		if c.place(e) || c.plan(e) {
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
	pl, err := placer.Place(req, c.freeCandidates(e.prof))
	if err != nil {
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
	res, err := placer.Plan(req, c.freeCandidates(e.prof), ev)
	if err != nil {
		return false
	}
	e.claim = make([]core.NodeID, 0, len(res.Placement.Nodes))
	for _, n := range res.Placement.Nodes {
		e.claim = append(e.claim, n.ID)
		c.ns(n.ID).claim = e.prof.Name
	}
	e.victims = slices.Clone(res.Victims)
	e.stoppedAny = false
	c.changed = true
	for _, v := range res.Victims {
		in := c.insts[v]
		in.victimOf = e.prof.Name
		if in.inFlight == 0 {
			c.stopInst(in, stopGraceful)
		} else {
			in.state = stDraining
			in.drainInFlight = in.inFlight
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
		if !c.evictable(in, clock) {
			continue
		}
		ev := placer.Evictable{Instance: in.prof.Name, Busy: in.inFlight > 0, LoadDuration: in.loadDuration}
		if in.inFlight == 0 {
			ev.IdleFor = c.now.Sub(in.lastUsed)
		}
		for _, pn := range in.placement.Nodes {
			n, ok := c.node(pn.ID)
			if !ok || !c.eligible(pn.ID) || (localOnly(p) && !n.Local) {
				continue
			}
			ev.Nodes = append(ev.Nodes, placer.FromNode(n, c.planBudget(pn.ID, n)))
		}
		if len(ev.Nodes) > 0 {
			out = append(out, ev)
		}
	}
	return out
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
// gone from the view, quarantined, or the view went stale).
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
		if !ok || !c.eligible(id) {
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
		cands = append(cands, placer.FromNode(n, c.liveBudget(id, n)))
	}
	pl, err := placer.Place(req, cands)
	if err == nil {
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
		if in.state == stDraining {
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
	c.answerEntry(e, err503(c.timeoutReason(e.prof), fmt.Sprintf("no room for %s within queue_timeout", e.prof.Name), queueRetryAfter))
}

// timeoutReason names why an entry waited too long.
func (c *schedCore) timeoutReason(p *core.Profile) Reason {
	if len(p.ForceNodes) > 0 {
		for _, name := range p.ForceNodes {
			id, err := c.view.Resolve(name)
			if err != nil || !c.eligible(id) {
				if !c.fresh {
					return ReasonFleetStale
				}
				return ReasonForcedUnavailable
			}
		}
	}
	if !c.fresh && !localOnly(p) {
		return ReasonFleetStale
	}
	return ReasonClusterBusy
}

// answerEntry fails every waiter of an entry, then drops its claim and
// deletes it. Waiters are answered first so that a victim returning to
// ready cannot admit them.
func (c *schedCore) answerEntry(e *entry, err *Error) {
	ws := e.waiters
	e.waiters = nil
	c.failAll(ws, err)
	c.dropClaim(e)
	c.deleteEntry(e)
}

// needFailed: the profile's need could not be determined (unreadable model
// file). That is a load failure for the profile: 502 and backoff.
func (c *schedCore) needFailed(e *entry, err error) {
	ws := e.waiters
	e.waiters = nil
	c.failAll(ws, err502(fmt.Sprintf("memory need of %s: %v", e.prof.Name, err), nil))
	c.dropClaim(e)
	c.deleteEntry(e)
	c.startBackoff(e.prof.Name)
}
