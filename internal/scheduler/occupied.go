package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The owner's counter for sharing a zone (potluck-forced-sharing-note):
// how often a request's ONLY option was a zone another instance holds.
// A queue entry (the requests for one profile waiting together) becomes a
// case the first time it cannot start on free nodes and:
//   - a forced profile: one of its forced nodes is held by another
//     instance ("forced nodes in use");
//   - an automatic profile: no placement fits the free nodes, but one fits
//     once the nodes other instances hold count as empty, and every such
//     placement uses a held node.
//
// One occupiedOnly history event is recorded when the case ends, with how
// it ended: waited (started after the occupant left by itself), evicted
// (started after an eviction of the occupant), timed_out (refused or
// timed out in the queue) or cancelled (every client went away). The API
// keeps the totals from these events (/api/system occupied_only).
//
// Observation only: nothing here changes a decision. The check runs the
// placer on its own copy of the candidates and touches no gate mark, claim
// or entry state other than the case itself.

// Outcomes of an occupied-only case.
const (
	OccWaited    = "waited"
	OccEvicted   = "evicted"
	OccTimedOut  = "timed_out"
	OccCancelled = "cancelled"
)

// occupiedCounterOn: the counter runs. Only tests turn it off, to prove
// that every decision is the same without it (TestReview_GeneralRule).
var occupiedCounterOn = true

// occCase is an entry's occupied-only case.
type occCase struct {
	began     time.Time
	nodes     []core.NodeID
	occupants []string
	evicted   []map[string]any
	// outcome and how are set when the entry is refused (timed_out);
	// otherwise deleteEntry decides: started or cancelled.
	outcome, how string
	done         bool
}

// noteOccupied opens entry e's case when its only option is an occupied
// zone. Called each time e could not start on free nodes; a case is
// opened once.
func (c *schedCore) noteOccupied(e *entry) {
	if !occupiedCounterOn || e.occ != nil || len(e.waiters) == 0 {
		return
	}
	nodes, occupants := c.occupiedOnly(e.prof)
	if len(nodes) == 0 {
		return
	}
	e.occ = &occCase{began: c.now, nodes: nodes, occupants: occupants}
	c.log.Info("only an occupied zone can take the request", "profile", e.prof.Name,
		"nodes", nodeIDStrings(nodes), "occupants", occupants, "waiters", len(e.waiters))
}

// occupiedOnly is the held nodes p's only option needs and the profiles
// holding them; none when p is not in that case.
func (c *schedCore) occupiedOnly(p *core.Profile) (nodes []core.NodeID, occupants []string) {
	holderOf := func(id core.NodeID) string {
		if ns := c.nodes[id]; ns != nil && ns.holder != "" && ns.holder != p.Name {
			return ns.holder
		}
		return ""
	}
	add := func(id core.NodeID, who string) {
		nodes = append(nodes, id)
		if !slices.Contains(occupants, who) {
			occupants = append(occupants, who)
		}
	}
	if len(p.ForceNodes) > 0 {
		for _, name := range p.ForceNodes {
			id, err := c.view.Resolve(name)
			if err != nil {
				continue
			}
			if who := holderOf(id); who != "" {
				add(id, who)
			}
		}
		return nodes, occupants
	}
	req, _, err := c.request(p)
	if err != nil {
		return nil, nil
	}
	cands := slices.Clone(c.freeCandidates(p))
	if _, err := placer.Place(req, cands); err == nil {
		return nil, nil // free nodes would do: it waits for something else
	}
	held := map[core.NodeID]bool{}
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok || holderOf(id) == "" || !c.eligible(id) || c.autoOff(p, n) || (localOnly(p) && !n.Local) {
			continue
		}
		held[id] = true
		cands = append(cands, c.candidate(p, id, n, c.planBudget(id, n), false))
	}
	if len(held) == 0 {
		return nil, nil
	}
	pl, err := placer.Place(req, cands)
	if err != nil {
		return nil, nil
	}
	for _, pn := range pl.Nodes {
		if held[pn.ID] {
			add(pn.ID, holderOf(pn.ID))
		}
	}
	return nodes, occupants
}

// occEvicted notes, on e's case, the instances evicted for it.
func (c *schedCore) occEvicted(e *entry, victims []string) {
	if e.occ == nil {
		return
	}
	for _, v := range victims {
		in := c.insts[v]
		if in == nil {
			continue
		}
		x := map[string]any{"profile": v, "busy": in.inFlight > 0, "in_flight": in.inFlight}
		if in.inFlight == 0 {
			x["idle_s"] = secs(c.now.Sub(in.lastUsed))
		}
		e.occ.evicted = append(e.occ.evicted, x)
	}
}

// occRefused marks e's case as refused or timed out (how says which).
func (c *schedCore) occRefused(e *entry, how string) {
	if e.occ != nil && e.occ.outcome == "" {
		e.occ.outcome, e.occ.how = OccTimedOut, how
	}
}

// occEnd records e's case, once, as its entry is deleted: refused when
// occRefused said so, else started when it still has waiters (they go to
// the instance), else cancelled.
func (c *schedCore) occEnd(e *entry) {
	o := e.occ
	if o == nil || o.done {
		return
	}
	o.done = true
	outcome := o.outcome
	switch {
	case outcome != "":
	case len(e.waiters) > 0 && len(o.evicted) > 0:
		outcome = OccEvicted
	case len(e.waiters) > 0:
		outcome = OccWaited
	default:
		outcome = OccCancelled
	}
	wait := c.now.Sub(o.began)
	data := map[string]any{"outcome": outcome, "occupied_nodes": nodeIDStrings(o.nodes), "occupants": slices.Clone(o.occupants),
		"began": o.began.UnixMilli(), "wait_s": secs(wait), "forced": len(e.prof.ForceNodes) > 0, "waiters": len(e.waiters)}
	if o.how != "" {
		data["how"] = o.how
	}
	if len(o.evicted) > 0 {
		data["evicted"] = o.evicted
	}
	names := make([]string, len(o.nodes))
	for i, id := range o.nodes {
		names[i] = c.nodeName(id)
	}
	c.record(history.TOccupiedOnly, history.Info, e.prof.Name,
		fmt.Sprintf("%s's only option was %s, held by %s; after %s it %s.", e.prof.Name, strings.Join(names, ", "),
			strings.Join(o.occupants, ", "), wait.Round(time.Second), occWords(outcome)),
		nodeIDStrings(o.nodes), "", data)
}

func occWords(outcome string) string {
	switch outcome {
	case OccWaited:
		return "started once the occupant had left"
	case OccEvicted:
		return "started after evicting the occupant"
	case OccTimedOut:
		return "was refused or timed out"
	}
	return "was cancelled by its client"
}
