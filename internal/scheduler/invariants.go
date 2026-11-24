package scheduler

import (
	"errors"
	"fmt"
	"slices"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// checkInvariants verifies the invariants of design section 11 and a few
// of the core's own. It returns all violations joined, or nil.
func (c *schedCore) checkInvariants() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// At most one instance and one entry per profile, under its own name.
	for name, in := range c.insts {
		if in.prof.Name != name {
			bad("instance of %s filed under %s", in.prof.Name, name)
		}
	}
	for name, e := range c.entries {
		if e.prof.Name != name {
			bad("entry of %s filed under %s", e.prof.Name, name)
		}
	}

	// A node has at most one holder and one claim; holder of a claimed node
	// is a victim of that claim.
	holders := map[core.NodeID]string{}
	for _, in := range c.sortedInsts() {
		for _, pn := range in.placement.Nodes {
			if h, dup := holders[pn.ID]; dup {
				bad("node %s held by %s and %s", pn.ID, h, in.prof.Name)
			}
			holders[pn.ID] = in.prof.Name
			if ns := c.nodes[pn.ID]; ns == nil || ns.holder != in.prof.Name {
				bad("node %s held by %s but recorded holder is %q", pn.ID, in.prof.Name, c.ns(pn.ID).holder)
			}
		}
	}
	claims := map[core.NodeID]string{}
	for _, e := range c.orderedEntries() {
		for _, id := range e.claim {
			if o, dup := claims[id]; dup {
				bad("node %s claimed by %s and %s", id, o, e.prof.Name)
			}
			claims[id] = e.prof.Name
			if ns := c.nodes[id]; ns == nil || ns.claim != e.prof.Name {
				bad("node %s claimed by %s but recorded claim is %q", id, e.prof.Name, c.ns(id).claim)
			}
			if h := holders[id]; h != "" {
				if in := c.insts[h]; in == nil || in.victimOf != e.prof.Name || !slices.Contains(e.victims, h) {
					bad("node %s claimed by %s but held by %s, which is not its victim", id, e.prof.Name, h)
				}
			}
		}
	}
	for id, ns := range c.nodes {
		if ns.holder != "" && holders[id] != ns.holder {
			bad("node %s records holder %s which does not hold it", id, ns.holder)
		}
		// Every claim belongs to a live entry.
		if ns.claim != "" && claims[id] != ns.claim {
			bad("node %s records claim %s which no live entry has", id, ns.claim)
		}
	}

	for _, in := range c.sortedInsts() {
		if in.inFlight < 0 {
			bad("%s has negative in_flight %d", in.prof.Name, in.inFlight)
		}
		// Draining instances admit nothing.
		if in.state == stDraining && in.inFlight > in.drainInFlight {
			bad("draining %s admitted: in_flight %d > %d at drain", in.prof.Name, in.inFlight, in.drainInFlight)
		}
		if in.state == stDraining && in.inFlight == 0 {
			bad("draining %s is idle but not stopping", in.prof.Name)
		}
		// Every victim belongs to a live claimed entry.
		if in.victimOf != "" {
			e := c.entries[in.victimOf]
			if e == nil || !e.claimed() || !slices.Contains(e.victims, in.prof.Name) {
				bad("%s is a victim of %s, which is not a live claimed entry listing it", in.prof.Name, in.victimOf)
			}
		}
		if in.state == stDraining && in.victimOf == "" && !in.drain {
			bad("%s draining for no claim", in.prof.Name)
		}
		if in.state != stStarting && len(in.waiters) > 0 {
			bad("%s %s has %d waiters", in.state, in.prof.Name, len(in.waiters))
		}
		if (in.state == stReady || in.state == stDraining) && in.child == nil {
			bad("%s %s has no child", in.state, in.prof.Name)
		}
	}

	seen := map[uint64]string{}
	note := func(w *waiter, where string) {
		if w.answered {
			bad("answered waiter %d still %s", w.id, where)
		}
		if o, dup := seen[w.id]; dup {
			bad("waiter %d both %s and %s", w.id, o, where)
		}
		seen[w.id] = where
		if c.waiters[w.id] != w {
			bad("waiter %d %s but not registered", w.id, where)
		}
	}
	for _, e := range c.orderedEntries() {
		if !e.claimed() && len(e.waiters) == 0 {
			bad("unclaimed entry %s has no waiters", e.prof.Name)
		}
		if e.claimed() && len(e.claim) == 0 {
			bad("entry %s claimed with an empty claim", e.prof.Name)
		}
		if !e.claimed() && (len(e.victims) > 0 || e.stoppedAny) {
			bad("unclaimed entry %s has victims", e.prof.Name)
		}
		for _, v := range e.victims {
			// Victims may be gone (exited); a live one must point back.
			if in := c.insts[v]; in != nil && in.victimOf != e.prof.Name && in.state != stStopping {
				bad("entry %s lists victim %s which is %s for %q", e.prof.Name, v, in.state, in.victimOf)
			}
		}
		if c.shutdown {
			bad("entry %s exists during shutdown", e.prof.Name)
		}
		for _, w := range e.waiters {
			note(w, "in entry "+e.prof.Name)
		}
	}
	for _, in := range c.sortedInsts() {
		for _, w := range in.waiters {
			note(w, "on instance "+in.prof.Name)
		}
	}
	for id, w := range c.waiters {
		if _, ok := seen[id]; !ok {
			bad("waiter %d for %s registered but nowhere", id, w.profile)
		}
	}
	for id, l := range c.leases {
		if l.id != id {
			bad("lease %d filed under %d", l.id, id)
		}
	}
	return errors.Join(errs...)
}
