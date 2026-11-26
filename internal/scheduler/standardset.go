package scheduler

import (
	"context"
	"slices"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The standard set's read-only plans (profiles plan, section 6): a profile
// that does not exist yet is planned on the fleet as it is, through the
// same path as a scoped plan (dryPlanOf), and nothing is kept.

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
