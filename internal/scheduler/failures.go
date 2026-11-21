package scheduler

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// quarantine is design 8.4: 30 s, doubling per repeat up to 10 min.
func (c *schedCore) quarantine(id core.NodeID, cause error) {
	ns := c.ns(id)
	d := quarantineBase << min(ns.quarLevel, 16)
	if d > quarantineMax || d <= 0 {
		d = quarantineMax
	}
	ns.quarLevel++
	ns.quarUntil = c.now.Add(d)
	ns.quarOpen = true
	c.log.Warn("node quarantined", "node", id, "for", d, "until", ns.quarUntil, "level", ns.quarLevel, "err", cause)
	c.record(history.TNodeQuarantined, history.Warn, "",
		fmt.Sprintf("Node %s was quarantined for %s (level %d): %v.", c.nodeName(id), d, ns.quarLevel, cause),
		[]string{c.nodeName(id)}, "", map[string]any{"for_s": secs(d), "level": ns.quarLevel, "cause": fmt.Sprint(cause)})
	c.changed = true
}

// startBackoff is design 8.4: the profile is backed off 60 s, doubling per
// repeat up to 10 min; its entry, if any, is answered 503 and deleted.
func (c *schedCore) startBackoff(profile string) {
	b := c.backoffs[profile]
	if b == nil {
		b = &backoff{}
		c.backoffs[profile] = b
	}
	d := backoffBase << min(b.level, 16)
	if d > backoffMax || d <= 0 {
		d = backoffMax
	}
	b.level++
	b.until = c.now.Add(d)
	b.open = true
	c.log.Warn("profile backed off", "profile", profile, "for", d, "until", b.until, "level", b.level)
	c.record(history.TProfileBackedOff, history.Warn, profile,
		fmt.Sprintf("Profile %s is backed off for %s after a failed load (level %d).", profile, d, b.level),
		nil, "", map[string]any{"for_s": secs(d), "level": b.level})
	if e := c.entries[profile]; e != nil {
		c.answerEntry(e, err503(ReasonBackoff, fmt.Sprintf("profile %s is backed off after a failed load", profile), d))
	}
	c.changed = true
}

// refreshView reads the node view for this step and runs the node-loss
// bookkeeping of design 8.4.
func (c *schedCore) refreshView() {
	raw := map[core.NodeID]core.Node{}
	for _, n := range c.view.Nodes() {
		raw[n.ID] = n
	}
	c.raw = raw
	c.unplaced = nil
	if g, ok := c.view.(NodeGate); ok {
		c.unplaced = g.Unplaced()
	}
	c.unplacedMsg = nil
	if g, ok := c.view.(NodeGateDetail); ok {
		c.unplacedMsg = g.UnplacedDetail()
	}
	for _, id := range c.sortedNodeIDs() {
		if n, ok := raw[id]; ok && !c.excludedSeen[id] {
			if entry, hit := c.cfg.ExcludedBy(n.ID, n.Hostname); hit {
				c.excludedSeen[id] = true
				c.log.Info("excluded node in fleet view", "node", id, "hostname", n.Hostname, "entry", entry)
			}
		}
	}
	c.age = c.view.Age()
	c.fresh = c.age <= c.cfg.Fleet.StaleAfter
	c.trackView()

	// A post-settle fetch replaces the view's figures until the view
	// reports something new for the node.
	for id, ns := range c.nodes {
		if ns.fetched == nil {
			continue
		}
		n, ok := raw[id]
		if ok != ns.fetchHad || (ok && n != ns.fetchKey) {
			ns.fetched = nil
		}
	}

	// Node loss: a remote node in use, offline in or absent from a live
	// view for 30 s continuously. The counter restarts when the feed
	// reconnects.
	//
	// DESIGN: while the view is stale it is not "a live view", so the
	// counter is cleared rather than left running; it starts again once
	// the view is fresh and still shows the node missing.
	reconnected := false
	if lr := c.view.LastReconnect(); !lr.Equal(c.seenReconnect) {
		c.seenReconnect = lr
		reconnected = true
	}
	inUse := map[core.NodeID]bool{}
	for _, in := range c.insts {
		for _, pn := range in.placement.Nodes {
			if !pn.Local {
				inUse[pn.ID] = true
				c.ns(pn.ID)
			}
		}
	}
	for id, ns := range c.nodes {
		n, ok := raw[id]
		missing := !ok || !n.Online
		switch {
		case !inUse[id] || !c.fresh || !missing:
			ns.missingSince = timeZero
		case reconnected || ns.missingSince.IsZero():
			ns.missingSince = c.now
		}
	}
	c.trackForced(reconnected)
}

// forcedState says whether the node a force_nodes name points at is gone:
// "absent" (not in the fleet view), "offline" (in it, but offline), or ""
// (usable, the local node, excluded (handled elsewhere), or not decidable,
// as for an ambiguous hostname).
func (c *schedCore) forcedState(name string) string {
	id, err := c.view.Resolve(name)
	if err != nil {
		if errors.Is(err, fleet.ErrNotFound) {
			return forcedAbsent
		}
		return ""
	}
	n, ok := c.raw[id]
	switch {
	case !ok:
		return forcedAbsent
	case n.Local || c.excluded(n):
		return ""
	case !n.Online:
		return forcedOffline
	}
	return ""
}

const (
	forcedAbsent  = "absent from the fleet"
	forcedOffline = "offline in the fleet"
)

// trackForced keeps, for every name any profile forces, since when it has
// been continuously absent or offline in a live view (defaults.
// forced_node_grace, design 8.4).
//
// DESIGN: while the view is stale the timers are cleared, not paused: stale
// time never counts, and the clock starts again once the view is fresh and
// still shows the node missing. A feed reconnect also restarts it, as for
// node loss, so a fleet console restart does not fail requests while the
// workers are still re-registering.
func (c *schedCore) trackForced(reconnected bool) {
	seen := map[string]bool{}
	for _, p := range c.cfg.Profiles {
		for _, name := range p.ForceNodes {
			if seen[name] {
				continue
			}
			seen[name] = true
			switch {
			case !c.fresh || c.forcedState(name) == "":
				delete(c.forcedGone, name)
			case reconnected || c.forcedGone[name].IsZero():
				c.forcedGone[name] = c.now
			}
		}
	}
	for name := range c.forcedGone {
		if !seen[name] {
			delete(c.forcedGone, name)
		}
	}
}

// forcedGoneName finds the first forced node of p that has been absent or
// offline in a live view for the whole grace period, with how it is gone and
// for how long. The name is empty when there is none.
func (c *schedCore) forcedGoneName(p *core.Profile) (name, state string, missing time.Duration) {
	if !c.fresh {
		return "", "", 0
	}
	for _, n := range p.ForceNodes {
		since, ok := c.forcedGone[n]
		if !ok || c.now.Sub(since) < c.cfg.Defaults.ForcedNodeGrace {
			continue
		}
		if st := c.forcedState(n); st != "" {
			return n, st, c.now.Sub(since).Round(time.Second)
		}
	}
	return "", "", 0
}

// forcedGoneErr is the fail-fast of design 8.4: a forced node of p has been
// absent or offline in a live view for the whole grace period. Nil when none.
// It is the same 503 and reason as the queue_timeout failure, with the
// cause spelled out.
func (c *schedCore) forcedGoneErr(p *core.Profile) *Error {
	name, state, missing := c.forcedGoneName(p)
	if name == "" {
		return nil
	}
	return err503(ReasonForcedUnavailable,
		fmt.Sprintf("forced node %s is %s (for %v); profile %s cannot start until it is back, so this fails now instead of waiting for queue_timeout",
			name, state, missing, p.Name), queueRetryAfter)
}

// failFastForced returns the fail-fast error for p, logging the reason, or
// nil when no forced node is gone.
func (c *schedCore) failFastForced(p *core.Profile, waiters int) *Error {
	err := c.forcedGoneErr(p)
	if err == nil {
		return nil
	}
	name, state, missing := c.forcedGoneName(p)
	c.log.Warn("forced node unavailable; failing the request", "profile", p.Name, "node", name,
		"state", state, "for", missing, "waiters", waiters)
	return err
}

// timers applies every time-based rule that is due at c.now.
func (c *schedCore) timers() {
	// Worker health runs first, so a worker that is hung in the same step in
	// which node loss kills its instance is still named.
	c.workerHealthStep()
	c.recoveryStep()
	c.memorySamples()
	for _, id := range c.sortedNodeIDs() {
		ns := c.nodes[id]
		if ns == nil {
			continue
		}
		if ns.quarOpen && !c.now.Before(ns.quarUntil) {
			ns.quarOpen = false
			c.log.Info("node released from quarantine", "node", id)
			c.record(history.TQuarantineEnded, history.Info, "",
				fmt.Sprintf("Node %s was released from quarantine.", c.nodeName(id)), []string{c.nodeName(id)}, "", nil)
		}
		if !ns.ovUntil.IsZero() && !c.now.Before(ns.ovUntil) {
			ns.ovUntil = timeZero // the 10-minute limit of the override
		}
		if ns.settle == setFailed {
			if c.dropGone(id); c.nodes[id] == nil {
				continue
			}
		}
		if (ns.settle == setWaiting || ns.settle == setFailed) && !c.now.Before(ns.settleAt) {
			ns.settle = setFetching
			ns.fetchGen++
			c.effects = append(c.effects, effFetch{id: id, gen: ns.fetchGen})
		}
	}

	for _, name := range c.sortedBackoffNames() {
		if b := c.backoffs[name]; b.open && !c.now.Before(b.until) {
			b.open = false
			c.log.Info("profile backoff ended", "profile", name)
		}
	}

	for _, in := range c.sortedInsts() {
		if c.insts[in.prof.Name] != in {
			continue
		}
		// Load timeout (7.6).
		if in.state == stStarting && in.phase == phLoading && !c.now.Before(in.loadDL) {
			c.failLoad(in, fmt.Sprintf("load timeout (%v) passed", in.loadTimeout), nil, false)
			continue
		}
		// Load stall: the upload is measured and stopped moving.
		if in.state == stStarting && in.phase == phLoading {
			if quiet, stalled := c.loadStalled(in); stalled {
				c.failStalledLoad(in, quiet)
				continue
			}
		}
		// Memory watch: a node the load runs on ran low on memory.
		if in.state == stStarting && in.phase == phLoading {
			if label, avail, hit := c.memoryFloorHit(in); hit {
				c.cancelLowMemoryLoad(in, label, avail)
				continue
			}
		}
		// Node loss.
		if lost := c.lostNode(in); lost != "" {
			c.nodeLost(in, lost)
			continue
		}
		c.loadTimers(in)
		// Idle unload, paused for remote instances while the view is stale.
		ttl := c.cfg.EffectiveTTL(in.prof)
		if in.state == stReady && in.inFlight == 0 && ttl > 0 && !c.now.Before(in.lastUsed.Add(ttl)) &&
			(c.fresh || !in.hasRemote()) {
			c.stopFor(in, stopGraceful, "ttl")
		}
	}
}

func (c *schedCore) lostNode(in *inst) core.NodeID {
	if in.state == stStopping && in.stopMode == stopKill {
		return ""
	}
	for _, pn := range in.placement.Nodes {
		if pn.Local {
			continue
		}
		if ns := c.nodes[pn.ID]; ns != nil && !ns.missingSince.IsZero() && c.now.Sub(ns.missingSince) >= c.nodeLossAfter() {
			return pn.ID
		}
	}
	return ""
}

// nodeLost: instance → stopping with SIGKILL; in-flight requests fail
// (their connections break).
//
// DESIGN: 8.4 does not say what happens to the waiters of a starting
// instance that loses a node. They are not in flight and the profile is
// not at fault, so they go back into the profile's entry, as for a
// returned instance (7.5), keeping its creation time and clock; the
// profile is not backed off.
func (c *schedCore) nodeLost(in *inst, id core.NodeID) {
	missing := c.now.Sub(c.ns(id).missingSince)
	c.log.Error("node lost", "profile", in.prof.Name, "node", id,
		"missing_for", missing, "in_flight", in.inFlight, "waiters", len(in.waiters))
	c.record(history.TNodeLost, history.Error, in.prof.Name,
		fmt.Sprintf("Node %s was lost: offline or absent for %s, so the %s instance was killed (%d requests in flight).",
			c.nodeName(id), missing.Round(time.Second), in.prof.Name, in.inFlight),
		[]string{c.nodeName(id)}, in.sessionID, map[string]any{"missing_for_s": secs(missing), "in_flight": in.inFlight,
			"waiters": len(in.waiters), "nodes_in_instance": c.placementNames(in)})
	c.keepLostNodeHung(id, in) // design 8.4a: the node stays tracked as hung until it is back
	ws := in.waiters
	in.waiters = nil
	c.stopFor(in, stopKill, "node loss")
	c.requeue(in, ws)
}

// dropGone forgets a node whose post-settle fetch failed when it is absent
// from a fresh view and nothing holds or claims it. If it reappears it is a
// new, unknown node.
func (c *schedCore) dropGone(id core.NodeID) {
	ns := c.nodes[id]
	if ns == nil || ns.settle != setFailed || ns.holder != "" || ns.claim != "" || !c.fresh {
		return
	}
	if _, present := c.raw[id]; present {
		return
	}
	delete(c.nodes, id)
	c.changed = true
}

func (c *schedCore) fetched(id core.NodeID, gen uint64, n core.Node, err error) {
	ns := c.nodes[id]
	if ns == nil || ns.settle != setFetching || ns.fetchGen != gen {
		return
	}
	c.changed = true
	if err != nil {
		// DESIGN: "If that fetch fails, the node is treated as offline."
		// It stays unusable (and ineligible, so claims on it drop) and is
		// fetched again after another settle period, so one failed
		// fetch does not lose the node for good.
		//
		// DESIGN: the retries stop once the node is absent from a fresh
		// view and nothing holds or claims it (see dropGone): its state
		// and timer are dropped. The failure is logged once per outage.
		vn, _ := c.raw[id]
		ns.settle, ns.settleAt = setFailed, c.now.Add(max(vn.Settle, fetchRetryMin))
		if !ns.failLog {
			ns.failLog = true
			c.log.Warn("post-settle fetch failed; node treated as offline", "node", id, "err", err)
		}
		c.dropGone(id)
		return
	}
	ns.failLog = false
	ns.settle = setNone
	ns.fetched = &n
	ns.fetchKey, ns.fetchHad = c.raw[id]
}

var _ = slices.Contains[[]int]
var _ = time.Second

func (c *schedCore) sortedBackoffNames() []string {
	names := make([]string, 0, len(c.backoffs))
	for n := range c.backoffs {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
