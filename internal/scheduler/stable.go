package scheduler

import (
	"slices"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The stable placement order (placement_tolerance_mib; placer/stable.go).
//
// The figure a node is ordered by is its live budget held still: it is set
// to the live budget when first seen and moves to it again only when the live
// budget has moved by more than twice the tolerance, so readings that jitter
// by up to the tolerance either way never move it. It orders nodes and
// weights a split; it never decides a fit.

// trackSteady updates the held figures after the view is read.
func (c *schedCore) trackSteady() {
	tol := c.cfg.Defaults.PlacementToleranceMiB
	if tol <= 0 {
		clear(c.steady)
		return
	}
	for id := range c.steady {
		if _, ok := c.raw[id]; !ok {
			delete(c.steady, id)
		}
	}
	for id, n := range c.raw {
		live := c.liveBudget(id, n)
		if s, ok := c.steady[id]; !ok || live-s > 2*tol || s-live > 2*tol {
			c.steady[id] = live
		}
	}
}

// steadyOf is the node's held figure, or live when there is none.
func (c *schedCore) steadyOf(id core.NodeID, live int) int {
	if s, ok := c.steady[id]; ok && c.cfg.Defaults.PlacementToleranceMiB > 0 {
		return s
	}
	return live
}

// warmFor: the last load that became ready on the node used p's model file.
func (c *schedCore) warmFor(id core.NodeID, p *core.Profile) bool {
	ns := c.nodes[id]
	return ns != nil && ns.lastFile != "" && ns.lastFile == lastFileOf(p)
}

// measuredFor: a stored measurement of p's shape (any device count; for a
// host: node profile without its own, the server twin's, as planning reads
// them) has a per-node figure for the node. Such a node's peaks and learned
// shares apply when the set and split come back (memfit.go shareFor), so
// among equal consoles it goes first.
func (c *schedCore) measuredFor(id core.NodeID, p *core.Profile) bool {
	if p == nil || p.Kind == core.KindCommand {
		return false
	}
	ls, _ := c.planLearnedOf(p)
	for _, l := range ls {
		if !c.trusted(l) {
			continue
		}
		for _, n := range l.Nodes {
			if n.ID == id && (n.MiB > 0 || n.PeakMiB > 0) {
				return true
			}
		}
	}
	return false
}

// hostReadingFor: p is hosted on a node and the node has a measured host
// overhead of p's shape (any device count) that planning would use, so the
// host's row is measured, not the setting (hostoverhead.go hostOverheadOf).
func (c *schedCore) hostReadingFor(id core.NodeID, p *core.Profile) bool {
	if p == nil || p.Host != core.HostNode || c.cfg.Defaults.HostOverheadSettingOnly {
		return false
	}
	shape := shapeOf(p)
	setting := max(c.cfg.Defaults.HostOverheadMiB, 0)
	for k, e := range c.hostOHs {
		// as hostOverheadOf: a restored reading of another llama build is
		// used only when it raises the setting
		if k.shape == shape && k.node == id && !c.otherModelFile(p, e) && (!e.restored || c.buildsAgree(e.LlamaBuild) || e.MiB > setting) {
			return true
		}
	}
	return false
}

// candidate is placer.FromNode with what the stable order needs. steady
// false: budget is already a steady figure (a plan budget).
func (c *schedCore) candidate(p *core.Profile, id core.NodeID, n core.Node, budget int, steady bool) placer.Candidate {
	cd := placer.FromNode(n, budget)
	cd.Name = n.Hostname
	cd.Warm = c.warmFor(id, p)
	if !c.noPreferStored {
		cd.HostReading = c.hostReadingFor(id, p)
		cd.Measured = c.measuredFor(id, p)
	}
	if steady {
		cd.SteadyMiB = c.steadyOf(id, budget)
	}
	return cd
}

// stableHypothetical orders a hypothetical placement's candidates the way the
// placer orders a placement under the stable order, and weights the split
// by class figure. Without the tolerance it is the order by live budget.
func (c *schedCore) stableHypothetical(p *core.Profile, cands []core.PlacedNode, k int) []core.PlacedNode {
	tol := c.cfg.Defaults.PlacementToleranceMiB
	if tol <= 0 {
		slices.SortStableFunc(cands, func(a, b core.PlacedNode) int { return b.BudgetMiB - a.BudgetMiB })
		return cands[:min(k, len(cands))]
	}
	type entry struct {
		pn  core.PlacedNode
		cd  placer.Candidate
		rep int
	}
	pcs := make([]placer.Candidate, len(cands))
	for i, pn := range cands {
		n, _ := c.node(pn.ID)
		pcs[i] = c.candidate(p, pn.ID, n, pn.BudgetMiB, true)
	}
	reps := placer.ClassFigures(pcs, tol)
	es := make([]entry, len(cands))
	for i := range cands {
		es[i] = entry{cands[i], pcs[i], reps[i]}
	}
	slices.SortStableFunc(es, func(a, b entry) int {
		if a.rep != b.rep {
			return b.rep - a.rep
		}
		if placer.StableLess(a.cd, b.cd) {
			return -1
		}
		if placer.StableLess(b.cd, a.cd) {
			return 1
		}
		return 0
	})
	es = es[:min(k, len(es))]
	out := make([]core.PlacedNode, len(es))
	for i, e := range es {
		out[i] = e.pn
		if len(es) > 1 {
			out[i].SplitMiB = e.rep
		}
	}
	return out
}

// keepSteadySplit drops a placement's steady split weights when the exact
// pre-flight figures are more than noise away from what the plan was made
// with (a node's exact budget moved by more than twice the tolerance from
// its held figure: the pre-flight found something real), or no longer hold
// every node's share that way. The launch then splits by the exact figures,
// as without the tolerance.
func (c *schedCore) keepSteadySplit(req placer.Request, pl *core.Placement, cands []placer.Candidate) {
	tol := c.cfg.Defaults.PlacementToleranceMiB
	w := make([]int, len(pl.Nodes))
	for i, pn := range pl.Nodes {
		held := c.steadyOf(pn.ID, cands[i].BudgetMiB)
		if pn.SplitMiB <= 0 || tol <= 0 || cands[i].BudgetMiB-held > 2*tol || held-cands[i].BudgetMiB > 2*tol {
			w = nil
			break
		}
		w[i] = pn.SplitMiB
	}
	if w != nil && len(w) > 1 && placer.SplitFits(req, cands, w) {
		return
	}
	for i := range pl.Nodes {
		pl.Nodes[i].SplitMiB = 0
	}
}
