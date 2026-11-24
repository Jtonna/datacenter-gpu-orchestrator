package scheduler

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// say words a message for the membership: messages are written once, in the
// fleet wording, and with agents the fleet phrases become pool phrases
// (config.PoolText). Reason codes never go through it.
func (c *schedCore) say(text string) string { return config.PoolText(c.agents, text) }

// nodeLabel is the name shown for a node in reasons: its hostname, else its id.
func nodeLabel(id core.NodeID, n core.Node) string {
	if n.Hostname != "" {
		return n.Hostname
	}
	return string(id)
}

// waitWhy says why a profile with no instance cannot be placed now. It is
// pure (it changes nothing) and feeds /api/plan, /api/status and the
// queue_timeout 503. The Reason is the 503's reason; the text is for people
// and carries the placer's typed reason through, or a more specific cause
// the placer cannot see because the scheduler filtered the node out first:
// quarantined nodes, a node without a known address, a forced node that is
// not in the fleet view.
func (c *schedCore) waitWhy(p *core.Profile) (Reason, string) {
	req, _, err := c.request(p)
	if err != nil {
		return ReasonClusterBusy, "memory need unknown: " + err.Error()
	}
	_, perr := placer.Place(req, c.freeCandidates(p))
	if perr == nil {
		return ReasonClusterBusy, "a placement is available; the next queue pass starts it"
	}
	reason, text := ReasonClusterBusy, perr.Error()
	var np *placer.NoPlacement
	if errors.As(perr, &np) {
		switch np.Reason {
		case placer.ForcedUnavailable:
			reason, text = c.forcedWhy(p, np)
		case placer.NoFit:
			if r, t, ok := c.filteredWhy(p, req); ok {
				reason, text = r, t
			}
		}
	}
	if !c.fresh && !localOnly(p) {
		return ReasonFleetStale, string(ReasonFleetStale) + "; " + text
	}
	return reason, text
}

// forcedWhy explains the missing force_nodes of a ForcedUnavailable.
func (c *schedCore) forcedWhy(p *core.Profile, np *placer.NoPlacement) (Reason, string) {
	var parts []string
	reason := ReasonForcedUnavailable
	var other, busy []string
	for _, name := range np.Missing {
		id, err := c.view.Resolve(name)
		n, ok := c.node(id)
		switch {
		case err != nil || !ok:
			parts = append(parts, c.say(fmt.Sprintf("forced node %s is not in the fleet view", name)))
		case c.excluded(n):
			parts = append(parts, fmt.Sprintf("forced node %s: node %s is excluded by config", name, nodeLabel(id, n)))
			reason = ReasonNodeExcluded
		case !n.Local && n.Addr == "":
			parts = append(parts, fmt.Sprintf("node %s has no known address", name))
		case c.quarantined(id):
			parts = append(parts, fmt.Sprintf("forced node %s is quarantined until %s", name, c.nodes[id].quarUntil.Format("15:04:05")))
		case c.pausedNode(n):
			parts = append(parts, fmt.Sprintf("forced node %s is paused", name))
		case n.Paused && n.UnpausePending:
			parts = append(parts, fmt.Sprintf("forced node %s was unpaused; its agent has not been told yet (next heartbeat)", name))
		case c.eligible(id):
			busy = append(busy, name) // held, claimed or settling: a wait, not an absence
		default:
			other = append(other, name) // offline or otherwise ineligible
		}
	}
	if len(parts) == 0 && len(other) == 0 {
		reason = ReasonClusterBusy
	}
	if len(busy) > 0 {
		parts = append(parts, "forced nodes in use: "+strings.Join(busy, ", "))
	}
	if len(other) > 0 {
		parts = append(parts, "forced nodes unavailable: "+strings.Join(other, ", "))
	}
	return reason, strings.Join(parts, "; ")
}

// filteredWhy answers NoFit: would the need fit if quarantined or
// address-less nodes were usable? If so they are the cause.
func (c *schedCore) filteredWhy(p *core.Profile, req placer.Request) (Reason, string, bool) {
	var cands []placer.Candidate
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok || (localOnly(p) && !n.Local) || !(n.Local || n.Online) {
			continue
		}
		if ns := c.nodes[id]; ns != nil && (ns.holder != "" || ns.claim != "" || ns.settle != setNone) {
			continue
		}
		cands = append(cands, c.candidate(p, id, n, c.liveBudget(id, n), true))
	}
	if _, err := placer.Place(req, cands); err != nil {
		return "", "", false
	}
	var parts []string
	var until time.Time
	var noAddr, excluded, noAuto []string
	for _, cd := range cands {
		n, _ := c.node(cd.ID)
		switch {
		case c.excluded(n):
			excluded = append(excluded, nodeLabel(cd.ID, n))
		case c.autoOff(p, n):
			noAuto = append(noAuto, nodeLabel(cd.ID, n))
		case c.quarantined(cd.ID):
			if u := c.nodes[cd.ID].quarUntil; u.After(until) {
				until = u
			}
		case !n.Local && n.Addr == "":
			noAddr = append(noAddr, nodeLabel(cd.ID, n))
		}
	}
	reason := ReasonClusterBusy
	if !until.IsZero() {
		parts = append(parts, fmt.Sprintf("nodes that would fit are quarantined until %s", until.Format("15:04:05")))
		reason = ReasonNodesQuarantined
	}
	slices.Sort(noAddr)
	for _, name := range noAddr {
		parts = append(parts, fmt.Sprintf("node %s has no known address", name))
		if reason == ReasonClusterBusy {
			reason = ReasonNodeNoAddress
		}
	}
	// An excluded node is named, but it does not set the reason: this profile
	// does not force it (forcedWhy covers that), so it is an ordinary lack of
	// room in the remaining pool, refused as such (0.1.7 review, ES3). The
	// reason "node excluded" is kept for a profile that forces an excluded
	// node, which can run nowhere else.
	slices.Sort(excluded)
	if len(excluded) > 0 {
		word := "nodes"
		if len(excluded) == 1 {
			word = "node"
		}
		parts = append(parts, fmt.Sprintf("%d %s excluded by config: %s", len(excluded), word, strings.Join(excluded, ", ")))
	}
	slices.Sort(noAuto)
	for _, name := range noAuto {
		parts = append(parts, fmt.Sprintf("node %s is in no_auto_nodes: only a profile that forces it uses it", name))
	}
	if len(parts) == 0 {
		return "", "", false
	}
	return reason, strings.Join(parts, "; "), true
}
