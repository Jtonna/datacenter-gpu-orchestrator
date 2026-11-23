package scheduler

import "time"

// nextDeadline is the earliest future time at which a time-based rule may
// change something. The runner (or a test) sends evTick then. It reports
// false when nothing is pending, so a quiet scheduler holds no timer.
func (c *schedCore) nextDeadline() (time.Time, bool) {
	var best time.Time
	add := func(t time.Time) {
		if t.After(c.now) && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	d := c.cfg.Defaults
	waiting := len(c.entries) > 0

	// Queue clocks (they run only while no instance is starting).
	if !c.anyStarting() {
		for _, e := range c.entries {
			if e.claimed() {
				continue
			}
			add(c.now.Add(d.QueueTimeout - e.clock))
			if e.clock < d.DrainAfter {
				add(c.now.Add(d.DrainAfter - e.clock))
			}
		}
	}

	// A just-released node's window ends (neverfits.go): what waits on it
	// is decided then, not at the next event (eighth review ES1).
	if waiting {
		for id, ns := range c.nodes {
			// and when a node's average counts again (evictWaits)
			if w := d.FreeAverageWindow; w > 0 && c.avgStale(id) {
				add(ns.heldChanged.Add(w))
			}
			if c.justReleased(id) {
				if end, ok := c.releaseWindowEnd(id); ok {
					add(end)
				}
			}
		}
	}

	// Forced nodes gone: the fail-fast of design 8.4 is due when the grace ends.
	for _, e := range c.entries {
		for _, name := range e.prof.ForceNodes {
			if since, ok := c.forcedGone[name]; ok {
				add(since.Add(d.ForcedNodeGrace))
			}
		}
	}

	for _, in := range c.insts {
		switch in.state {
		case stStarting:
			if in.phase == phLoading {
				add(in.loadDL)
				add(in.nextLoadLog)
				if limit := c.cfg.EffectiveLoadStallTimeout(in.prof); limit > 0 && in.measured && !in.lastGrowth.IsZero() {
					add(in.lastGrowth.Add(limit))
				}
				if c.wantsSample(in) && !in.sampling {
					add(in.nextSample)
				}
			}
		case stReady:
			if waiting {
				add(in.readyAt.Add(c.hold(in))) // becomes evictable
			}
			if ttl := c.cfg.EffectiveTTL(in.prof); ttl > 0 && in.inFlight == 0 {
				add(in.lastUsed.Add(ttl))
			}
		}
	}

	for _, ns := range c.nodes {
		if ns.settle == setWaiting || ns.settle == setFailed {
			add(ns.settleAt)
		}
		if !ns.missingSince.IsZero() {
			add(ns.missingSince.Add(c.nodeLossAfter()))
		}
		if ns.quarOpen {
			add(ns.quarUntil) // the release is logged then
		}
		if waiting {
			if !ns.ovUntil.IsZero() {
				add(ns.ovUntil)
			}
			add(ns.quarUntil)
			if !ns.agentHold.IsZero() {
				add(ns.agentHold) // agents.go: the node may be used again
			}
		}
	}
	for _, b := range c.backoffs {
		if waiting || b.open {
			add(b.until)
		}
	}

	// View freshness matters while anything could depend on it.
	if waiting || len(c.insts) > 0 {
		if c.fresh {
			add(c.now.Add(c.cfg.Fleet.StaleAfter - c.age + time.Nanosecond))
		} else {
			add(c.now.Add(staleRecheck))
		}
	}

	c.healthDeadlines(add)
	c.memoryDeadlines(add)
	c.recoveryDeadlines(add)

	if c.shutdown && !c.stopAll {
		add(c.shutdownDL)
	}
	return best, !best.IsZero()
}
