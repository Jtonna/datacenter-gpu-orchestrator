package scheduler

import (
	"fmt"
	"slices"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
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
}

// timers applies every time-based rule that is due at c.now.
func (c *schedCore) timers() {
	for _, id := range c.sortedNodeIDs() {
		ns := c.nodes[id]
		if ns == nil {
			continue
		}
		if ns.quarOpen && !c.now.Before(ns.quarUntil) {
			ns.quarOpen = false
			c.log.Info("node released from quarantine", "node", id)
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
		// Node loss.
		if lost := c.lostNode(in); lost != "" {
			c.nodeLost(in, lost)
			continue
		}
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
		if ns := c.nodes[pn.ID]; ns != nil && !ns.missingSince.IsZero() && c.now.Sub(ns.missingSince) >= nodeLossAfter {
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
	c.log.Error("node lost", "profile", in.prof.Name, "node", id,
		"missing_for", c.now.Sub(c.ns(id).missingSince), "in_flight", in.inFlight, "waiters", len(in.waiters))
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
