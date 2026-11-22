package scheduler

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// arrive is design 8.2 steps 2-4 for one request whose model name the
// caller has already resolved (step 1).
func (c *schedCore) arrive(w *waiter) {
	w.id = c.next()
	w.arrived = c.now
	c.waiters[w.id] = w
	p := c.cfg.Profiles[w.profile]
	if p == nil {
		c.fail(w, errUnknown(w.profile))
		return
	}
	if c.shutdown {
		c.fail(w, err503(ReasonShuttingDown, "the scheduler is shutting down", queueRetryAfter))
		return
	}
	if b, on := c.backoffActive(p.Name); on {
		c.fail(w, err503(ReasonBackoff, fmt.Sprintf("profile %s is backed off after a failed load", p.Name), b.until.Sub(c.now)))
		return
	}
	if in := c.insts[p.Name]; in != nil {
		switch in.state {
		case stReady:
			c.admit(in, w)
			return
		case stStarting:
			in.waiters = append(in.waiters, w)
			if in.benchRun == "" {
				in.benchRun = w.benchRun
			}
			return
		}
	}
	if why, bad := c.forcedExcluded(p); bad {
		c.fail(w, c.excludedForcedErr(p, why))
		return
	}
	if c.insts[p.Name] != nil {
		// draining or stopping: it waits for that instance to go
	} else if err := c.failFastForced(p, 1); err != nil {
		c.fail(w, err)
		return
	} else if err := c.forcedNeverFits(p); err != nil {
		// Can never fit its forced nodes, not even with them empty: refused
		// now, not after min_hold, a request in flight or a claim (forcedfit.go).
		c.refuseNeverFits(p, err, "arrival")
		c.fail(w, err)
		return
	}
	// draining, stopping or no instance: the queue (7.2 capacity check first).
	if err := c.capacity(w.profile); err != nil {
		var np *placer.NoPlacement
		if errors.As(err, &np) && np.Reason == placer.ExceedsCapacity {
			// Permanent (the config or the cluster must change), so no
			// Retry-After: RetryAfter stays zero and the API omits the header.
			e := &Error{Status: http.StatusServiceUnavailable, Reason: ReasonDoesNotFit,
				Message: fmt.Sprintf("profile %s needs %d MiB (fit rule requires %d MiB) but the cluster can hold at most %d MiB",
					p.Name, np.NeedMiB, np.RequiredMiB, np.CapacityMiB) + c.excludedNote(p)}
			e.NeedMiB, e.RequiredMiB, e.CapacityMiB = np.NeedMiB, np.RequiredMiB, np.CapacityMiB
			c.log.Warn("request can never fit the cluster", "profile", p.Name, "need_mib", np.NeedMiB,
				"required_mib", np.RequiredMiB, "capacity_mib", np.CapacityMiB)
			c.fail(w, e)
			return
		}
		// The need could not be determined: a load failure for the profile.
		c.fail(w, err502(err.Error(), nil))
		c.startBackoff(p.Name)
		return
	}
	e := c.entries[p.Name]
	if e == nil {
		e = &entry{prof: p, seq: c.next(), created: c.now}
		c.entries[p.Name] = e
		e.waiters = append(e.waiters, w)
		c.log.Info("queue entry created", "profile", p.Name, "alias", w.alias, "waiters", len(e.waiters))
	} else {
		e.waiters = append(e.waiters, w)
		c.log.Debug("request joined queue entry", "profile", p.Name, "alias", w.alias, "waiters", len(e.waiters))
	}
	c.changed = true
}

// forcedExcluded finds a force_nodes entry of p that resolves to a node
// excluded by config (excluding wins, design 6). Config load catches the
// cases it can; this catches an id against a hostname glob and the like. A
// forced node that is merely absent from the fleet is not reported here: it
// may come online, so the request queues.
//
// A command profile is tied to the local node in the same way: when
// exclude_nodes names the local node, it fails the same way.
func (c *schedCore) forcedExcluded(p *core.Profile) (string, bool) {
	if localOnly(p) {
		for _, id := range c.sortedNodeIDs() {
			if n, ok := c.node(id); ok && n.Local && c.excluded(n) {
				return fmt.Sprintf("profile %s is a command profile and runs only on the local node %s, which is excluded by config (excluding wins: change exclude_nodes)",
					p.Name, nodeLabel(id, n)), true
			}
		}
	}
	for _, name := range p.ForceNodes {
		id, err := c.view.Resolve(name)
		if err != nil {
			continue
		}
		if n, ok := c.node(id); ok && c.excluded(n) {
			return fmt.Sprintf("profile %s forces node %s, but node %s is excluded by config (excluding wins: change exclude_nodes or force_nodes)",
				p.Name, name, nodeLabel(id, n)), true
		}
	}
	return "", false
}

// excludedForcedErr is the permanent failure for a forced profile that names
// an excluded node: no Retry-After, because only a config change fixes it.
func (c *schedCore) excludedForcedErr(p *core.Profile, why string) *Error {
	c.log.Warn("forced profile names an excluded node", "profile", p.Name, "detail", why)
	return &Error{Status: http.StatusServiceUnavailable, Reason: ReasonNodeExcluded, Message: why}
}

// excludedNote lists the nodes exclude_nodes kept out of a capacity check
// that the profile could otherwise have used, so a "does not fit" is not a
// mystery. Empty when none.
func (c *schedCore) excludedNote(p *core.Profile) string {
	var names []string
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok || !c.excluded(n) || !(n.Local || n.Online) || (localOnly(p) && !n.Local) {
			continue
		}
		names = append(names, nodeLabel(id, n))
	}
	out := ""
	if len(names) > 0 {
		out = "; nodes excluded by config and not counted: " + strings.Join(names, ", ")
	}
	var noAuto []string
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok || c.excluded(n) || !c.autoOff(p, n) || !(n.Local || n.Online) || (localOnly(p) && !n.Local) {
			continue
		}
		noAuto = append(noAuto, nodeLabel(id, n))
	}
	if len(noAuto) > 0 {
		out += "; nodes in no_auto_nodes (used only by profiles that force them) and not counted: " + strings.Join(noAuto, ", ")
	}
	return out
}

// capacity is the fail-fast check of design 7.2: the need against the plan
// budgets of every node the profile could ever use (best max_pool of them),
// counting quarantined and settling nodes. Skipped while the view is
// stale. A need error is returned as is.
func (c *schedCore) capacity(profile string) error {
	p := c.cfg.Profiles[profile]
	req, _, err := c.request(p)
	if err != nil {
		return err
	}
	if !c.fresh && !localOnly(p) {
		return nil
	}
	var cands []placer.Candidate
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok || !c.reachable(n) || (localOnly(p) && !n.Local) || c.autoOff(p, n) {
			continue
		}
		cands = append(cands, placer.FromNode(n, c.planBudget(id, n)))
	}
	return placer.CheckCapacity(req, cands)
}

// admit gives a waiter to a ready instance: in_flight++ and a lease. A
// synthetic waiter (POST /api/load) is simply removed.
func (c *schedCore) admit(in *inst, w *waiter) {
	if in.state != stReady {
		panic(fmt.Sprintf("scheduler: admit to %s instance of %s", in.state, in.prof.Name))
	}
	if w.synthetic {
		c.answer(w, answer{})
		return
	}
	id := c.next()
	c.leases[id] = &lease{id: id, profile: in.prof.Name, iid: in.iid}
	in.inFlight++
	in.served++
	in.lastUsed = c.now
	addr := "127.0.0.1:" + strconv.Itoa(in.child.Port())
	c.answer(w, answer{grant: &grant{ID: id, Addr: addr, Profile: in.prof.Name, Alias: w.alias}})
	c.emit(FeedAdmitted, in.prof.Name, func(e *FeedEvent) { e.RequestID = id })
}

// admitEntry deletes the profile's entry, if any, and admits its waiters
// to the instance (design 8.3: on ready, or on return from draining).
func (c *schedCore) admitEntry(in *inst) {
	e := c.entries[in.prof.Name]
	if e == nil {
		return
	}
	c.deleteEntry(e)
	for _, w := range e.waiters {
		c.admit(in, w)
	}
}

// cancel is design 8.2 step 5: the client went away while waiting.
func (c *schedCore) cancel(w *waiter) {
	if w.answered || c.waiters[w.id] != w {
		return
	}
	if e := c.entries[w.profile]; e != nil && slices.Contains(e.waiters, w) {
		e.waiters = slices.DeleteFunc(e.waiters, func(x *waiter) bool { return x == w })
		if len(e.waiters) == 0 && !e.claimed() {
			c.deleteEntry(e)
		}
	}
	if in := c.insts[w.profile]; in != nil {
		in.waiters = slices.DeleteFunc(in.waiters, func(x *waiter) bool { return x == w })
	}
	c.fail(w, &Error{Status: StatusClientClosed, Reason: ReasonClientGone, Message: "client disconnected while waiting"})
	c.changed = true
}

// release ends an admitted request.
func (c *schedCore) release(id uint64) {
	l := c.leases[id]
	if l == nil {
		return
	}
	delete(c.leases, id)
	c.emit(FeedFinished, l.profile, func(e *FeedEvent) { e.RequestID = id })
	in := c.instByIID(l.iid)
	if in == nil {
		return
	}
	in.inFlight--
	in.lastUsed = c.now
	if in.state == stDraining && in.inFlight == 0 {
		c.stopFor(in, stopGraceful, "evicted for "+in.victimOf)
	}
	c.changed = true
}

func (c *schedCore) deleteEntry(e *entry) {
	if c.entries[e.prof.Name] == e {
		delete(c.entries, e.prof.Name)
	}
	for _, id := range e.claim {
		if ns := c.nodes[id]; ns != nil && ns.claim == e.prof.Name {
			ns.claim = ""
		}
	}
	c.changed = true
}

var _ = http.StatusOK
