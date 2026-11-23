package scheduler

import (
	"fmt"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// Tight fit (the owner's rule): a placement whose nodes hold what the load
// will take (weights + context at the full configured window + fill
// reserve, or the stored peak) with the headroom kept starts, even when
// the per-node margin does not fit on top: it starts as a tight fit. Only a
// placement short of that is refused or waits. The margin keeps two uses:
// automatic placement prefers a placement that meets it everywhere (a
// tight one is taken only when none does, and nothing is evicted to turn a
// tight fit into a margin-met one), and a launch that meets it asks the
// agent for it as before. defaults.margin_refuses brings the old rule back.
//
// The decisions run with the margin not counted (marginCounts is false);
// the preferring tries run inside withMargin.

// tightOn: a placement short of its margin only may start (the reservation
// gate is on and margin_refuses is off). With the defaults the gate is off
// and nothing changes.
func (c *schedCore) tightOn() bool { return c.gateOn() && !c.cfg.Defaults.MarginRefuses }

// marginCounts: the per-node margin is part of what a placement must hold
// in the decision being made.
func (c *schedCore) marginCounts() bool { return !c.tightOn() || c.preferMargin }

// counted is margin when it counts, else 0.
func (c *schedCore) counted(margin int) int {
	if c.marginCounts() {
		return margin
	}
	return 0
}

// withMargin runs f with the margin counted: the try for a placement that
// meets it.
func (c *schedCore) withMargin(f func()) {
	was := c.preferMargin
	c.preferMargin = true
	defer func() { c.preferMargin = was }()
	f()
}

// rowShort is how far row r falls short of what it must hold now: the load
// and, when it counts, the margin. 0 when it holds it.
func (c *schedCore) rowShort(r PlanMemoryNode) int {
	return max(r.WillTakeMiB+c.counted(r.MarginMiB)-r.AvailableMiB, 0)
}

// rowsShort is the node that falls shortest and by how much; "" when every
// row holds what it must.
func (c *schedCore) rowsShort(rows []PlanMemoryNode) (node string, by int) {
	for _, e := range rows {
		if s := c.rowShort(e); s > by {
			node, by = e.Node, s
		}
	}
	return node, by
}

// markTight sets Tight and MarginShortMiB on the rows that hold the load
// without the margin only, and returns them. None when tight fits are off.
func (c *schedCore) markTight(rows []PlanMemoryNode) (tight []PlanMemoryNode) {
	if !c.tightOn() {
		return nil
	}
	for i := range rows {
		r := &rows[i]
		if r.SpareMiB >= 0 && r.RemainsMiB < 0 {
			r.Tight, r.MarginShortMiB = true, -r.RemainsMiB
			tight = append(tight, *r)
		}
	}
	return tight
}

// tightText is the plan's and the log's words for a tight fit.
func tightText(tight []PlanMemoryNode) string {
	parts := make([]string, len(tight))
	for i, r := range tight {
		parts[i] = fmt.Sprintf("tight fit: %d MiB spare on %s, margin %d MiB not met", r.SpareMiB, r.Node, r.MarginMiB)
	}
	return strings.Join(parts, "; ")
}

// tightRows are the tight nodes of placement pl for p, as the gate sees
// them (p's own reservation freed).
func (c *schedCore) tightRows(p *core.Profile, pl core.Placement) []PlanMemoryNode {
	rows, ok := c.memoryRows(p, pl, map[string]bool{p.Name: true})
	if !ok {
		return nil
	}
	return c.markTight(rows)
}

// noteTight records a tight fit on a new instance and says so in the log.
func (c *schedCore) noteTight(in *inst) {
	c.fitting = in // as the gate saw it: its own reservation is not counted against it
	in.tight = c.tightRows(in.prof, in.placement)
	c.fitting = nil
	if len(in.tight) == 0 {
		return
	}
	for _, r := range in.tight {
		c.log.Warn("tight fit: loading with less than the margin spare", "profile", in.prof.Name, "node", r.Node,
			"will_take_mib", r.WillTakeMiB, "available_mib", r.AvailableMiB, "spare_mib", r.SpareMiB,
			"margin_mib", r.MarginMiB, "margin_short_mib", r.MarginShortMiB, "margin_rule", r.MarginRule)
	}
}

// tightHistory is the loadStarted event's tight_fit field.
func tightHistory(tight []PlanMemoryNode) []map[string]any {
	out := make([]map[string]any, len(tight))
	for i, r := range tight {
		out[i] = map[string]any{"id": string(r.ID), "node": r.Node, "will_take_mib": r.WillTakeMiB, "available_mib": r.AvailableMiB,
			"spare_mib": r.SpareMiB, "margin_mib": r.MarginMiB, "margin_short_mib": r.MarginShortMiB}
	}
	return out
}

// preferMet is gatedPlace's first try: a placement that meets the margin
// everywhere, found with the margin counted. ok is false when there is
// none (or tight fits are off); the caller then decides without it.
func (c *schedCore) preferMet(p *core.Profile) (pl core.Placement, ok bool) {
	if !c.tightOn() || c.preferMargin {
		return pl, false
	}
	c.withMargin(func() {
		req, _, err := c.request(p)
		if err != nil {
			return
		}
		got, gate, _, err := c.gatedPlaceAs(p, req)
		pl, ok = got, err == nil && gate == ""
	})
	return pl, ok
}

// evictPlan is the placer's eviction plan for p over the free nodes and
// evictable instances ev, held to the gate: gate is why the gate refuses
// it ("" when it passes). With tight fits on, a plan that meets the margin
// is taken when it stops no more instances than the plan without it; never
// is an instance stopped only to meet the margin.
func (c *schedCore) evictPlan(p *core.Profile, req placer.Request, ev []placer.Evictable) (res placer.PlanResult, gate string, err error) {
	res, err = placer.Plan(req, c.freeCandidates(p), ev)
	if err == nil {
		gate = c.reserveGate(p, res.Placement, res.Victims)
	}
	if !c.tightOn() || c.preferMargin {
		return res, gate, err
	}
	took := false
	c.withMargin(func() {
		sreq, _, e := c.request(p)
		if e != nil {
			return
		}
		sres, e := placer.Plan(sreq, c.freeCandidates(p), ev)
		if e != nil || c.reserveGate(p, sres.Placement, sres.Victims) != "" {
			return
		}
		if err != nil || gate != "" || len(sres.Victims) <= len(res.Victims) {
			res, gate, err, took = sres, "", nil, true
		}
	})
	if took {
		c.reserveGate(p, res.Placement, res.Victims) // the mark is the plan taken
	}
	return res, gate, err
}

// launchExpect is expect_mib of each node of a new instance's launch
// (expectShares): with the margin, as before, on every node that meets it;
// without it on a node of a tight fit, so the agent is not asked for memory
// the plan never counted on (min_mem_available_mib is the floor plus this).
// The tight nodes are taken again on the figures of the launch.
func (c *schedCore) launchExpect(in *inst) []int {
	var expect []int
	c.withMargin(func() { expect = c.expectShares(in.prof, in.placement) })
	if !c.tightOn() || expect == nil {
		return expect
	}
	c.fitting = in // reserve.go: its own reservation is not counted against it
	in.tight = c.tightRows(in.prof, in.placement)
	c.fitting = nil
	for _, r := range in.tight {
		for i, pn := range in.placement.Nodes {
			if pn.ID == r.ID && i < len(expect) {
				expect[i] = max(expect[i]-r.MarginMiB, 0)
			}
		}
	}
	return expect
}
