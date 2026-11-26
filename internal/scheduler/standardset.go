package scheduler

import (
	"context"
	"slices"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The standard set's read-only plans (profiles plan, section 6): a profile
// that does not exist yet is planned on the fleet as it is, through the
// same path as a scoped plan (dryPlanOf), and nothing is kept. Whether it
// is kept is decided on an IDLE fleet (idleFleetFit): what happens to be
// loaded at that moment must not decide a model's standard set.

// candidateScope ends the name a candidate is planned under. "@" is refused
// in every configured id and alias (config.validateStage) and a space in
// every scope (validScope), so the name never meets a configured profile, a
// loaded instance or a scoped load.
const candidateScope = ScopeSep + "standard set"

// CandidatePlan is the read-only plan of one candidate profile.
type CandidatePlan struct {
	// Report is the plan; its Profile is the candidate's own id.
	Report PlanReport
	// Zones are the names of the zones it would use: the placement's, else
	// (a queue or a reject) the nodes the memory rows are for.
	Zones []string
	// MayUse is how many zones an automatic placement of it may use now:
	// the reachable nodes it may be placed on, at most max_pool.
	MayUse int
	// IdleFits: it fits with nothing loaded (idleFleetFit); IdleZones are
	// the zones that placement uses.
	IdleFits  bool
	IdleZones []string
}

// PlanCandidates plans each profile read-only on the fleet as it is. The
// profiles are not configured; nothing is stored or changed (the needs read
// for them are dropped again).
func (s *Scheduler) PlanCandidates(ctx context.Context, ps []*core.Profile) ([]CandidatePlan, error) {
	return askRead(s, ctx, "standard set plans", func(c *schedCore) []CandidatePlan {
		out := make([]CandidatePlan, 0, len(ps))
		for _, p := range ps {
			cp := *p
			cp.Name = p.Name + candidateScope
			cp.Aliases = nil
			sp := c.realScoped(&cp) // realctx.go: as a scoped plan
			r := c.dryPlanOf(sp.Name, sp)
			c.forgetScopedNeed(sp.Name)
			r.Profile = p.Name
			cpl := CandidatePlan{Report: r, MayUse: c.zonesMayUse(sp)}
			if pl, ok := c.idleFleetFit(sp); ok {
				cpl.IdleFits = true
				for _, n := range pl.Nodes {
					cpl.IdleZones = append(cpl.IdleZones, c.nodeName(n.ID))
				}
			}
			for _, n := range r.Placement {
				cpl.Zones = append(cpl.Zones, c.nodeName(n.ID))
			}
			if len(cpl.Zones) == 0 && r.Memory != nil {
				for _, n := range r.Memory.Nodes {
					cpl.Zones = append(cpl.Zones, n.Node)
				}
			}
			out = append(out, cpl)
		}
		return out
	})
}

// idleFleetFit places p as if nothing were loaded: the free nodes as they
// are, and every instance offered as a victim whatever its state, hold or
// requests in flight (the same eviction plan as plan(), tight.go evictPlan,
// with the reservation gate). The nodes' own limits stay (reachable,
// eligible, no new load, no_auto, local only). Nothing is changed.
func (c *schedCore) idleFleetFit(p *core.Profile) (core.Placement, bool) {
	if c.shutdown || c.capacity(p) != nil {
		return core.Placement{}, false
	}
	req, _, err := c.request(p)
	if err != nil {
		return core.Placement{}, false
	}
	if pl, gate, _, perr := c.gatedPlace(p, req); perr == nil && gate == "" {
		return pl, true
	}
	var ev []placer.Evictable
	for _, in := range c.sortedInsts() {
		if c.onNoAuto(p, in) {
			continue
		}
		e := placer.Evictable{Instance: in.prof.Name, LoadDuration: in.loadDuration, IdleFor: time.Hour}
		for _, pn := range in.placement.Nodes {
			n, ok := c.node(pn.ID)
			if !ok || !c.eligible(pn.ID) || c.noNewLoad(pn.ID) || (localOnly(p) && !n.Local) {
				continue
			}
			e.Nodes = append(e.Nodes, c.candidate(p, pn.ID, n, c.planBudget(pn.ID, n), false))
		}
		if len(e.Nodes) > 0 {
			ev = append(ev, e)
		}
	}
	if len(ev) == 0 {
		return core.Placement{}, false
	}
	if res, gate, err := c.evictPlan(p, req, ev); err == nil && gate == "" {
		return res.Placement, true
	}
	return core.Placement{}, false
}

// zonesMayUse counts the nodes an automatic placement of p may use now, at
// most max_pool.
func (c *schedCore) zonesMayUse(p *core.Profile) int {
	n := 0
	for _, id := range c.sortedNodeIDs() {
		nd, ok := c.node(id)
		if !ok || !c.reachable(nd) || c.autoOff(p, nd) || (localOnly(p) && !nd.Local) {
			continue
		}
		if len(p.AllowTiers) > 0 && !slices.Contains(p.AllowTiers, nd.Priority) {
			continue
		}
		n++
	}
	return min(n, max(c.cfg.Defaults.MaxPool, 1))
}
