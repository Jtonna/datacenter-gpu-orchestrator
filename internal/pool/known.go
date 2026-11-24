package pool

import (
	"fmt"
	"sort"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Known machines across a restart. The pool keeps its agents in memory only,
// so a machine whose agent does not register again after a Potluck restart
// would not be listed at all. The machines listed when the previous run
// ended are read back from the event history (KnownFrom: no table of its
// own), and one
// that has not registered when KnownGrace has passed since the start is
// listed as lost, with when it was last seen, until its agent registers or
// the owner removes it. It is never a node of the scheduler's view:
// placement and plans do not see it.

// KnownGrace is how long after the start a known machine may take to
// register before it is listed as lost (and counted down). An agent whose
// Potluck went away retries its registration after 5 s, doubling up to its
// longest wait of 60 s (potluck-agent, ErrorRetry); a refused one waits 60 s
// (RegisterRetry). 60 s plus its 5 s request timeout covers the slowest
// agent that is up, so a running machine is never listed as down for the
// length of a restart.
const KnownGrace = 65 * time.Second

// KnownNode is a machine of an earlier run: its id, its name and when its
// agent was last seen by Potluck.
type KnownNode struct {
	ID       string
	Name     string
	LastSeen time.Time
}

// HistoryScanner is a history that can be read back, oldest first
// (history.Store).
type HistoryScanner interface {
	Scan(fn func(history.Event))
}

// KnownFrom folds the history, oldest first, into the known machines: the
// machines listed when the previous run of the scheduler ended (active or
// lost in that run's own list; not removed, not left), as of the last
// schedulerStarted event. The run doing the reading (after that event) is
// not looked at. It reads these events:
//
//   - schedulerStarted (written at every start): a run begins; the one
//     before ended. Its list becomes the known machines, and the new run's
//     list starts empty.
//   - schedulerStopping (written at every shutdown and by the fatal path),
//     or after a crash the run's last event of any kind: when the run ended,
//     the last seen of a machine still active in it.
//   - nodeMembership state active (joined, back): the machine is listed,
//     active, seen then.
//   - nodeMembership state lost: the machine is listed, lost; last seen is
//     the event's last_seen, or for an event without it (older images) the
//     event's time less LostAfter.
//   - knownMachines (one per start, written by SetKnown) and nodeMembership
//     with known: true (written when a carried machine is listed lost, the
//     grace over; images v39 to v41 also wrote one per carried machine at the
//     start): the machine is listed, lost, with the last seen in the event,
//     unless it registered in the run.
//   - nodeMembership state left, and nodeAdmin action remove: the machine is
//     not listed.
//
// An older image listed only the machines that registered in a run (active,
// or lost after registering) and wrote no known: true events, so a run of
// it is judged by the same rule without telling the images apart: a machine
// it did not list is not carried. Without a schedulerStarted event nothing
// is known.
func KnownFrom(scan func(fn func(history.Event))) []KnownNode {
	type rec struct {
		name   string
		active bool // registered and not lost since: last seen when the run ends
		seen   int64
	}
	list := map[string]*rec{}
	var runLast int64
	var snap []KnownNode
	started := false
	scan(func(e history.Event) {
		if e.Type == history.TSchedulerStarted {
			snap = snap[:0]
			for id, r := range list {
				seen := r.seen
				if r.active && runLast > seen {
					seen = runLast
				}
				name := r.name
				if name == "" {
					name = id
				}
				snap = append(snap, KnownNode{ID: id, Name: name, LastSeen: time.UnixMilli(seen)})
			}
			list, runLast, started = map[string]*rec{}, e.Ts, true
			return
		}
		if e.Ts > runLast {
			runLast = e.Ts
		}
		if e.Type == history.TKnownMachines {
			for _, m := range eventMachines(e.Data["machines"]) {
				id, _ := m["id"].(string)
				ms, ok := millis(m["last_seen"])
				if _, listed := list[id]; id != "" && ok && !listed {
					name, _ := m["name"].(string)
					if name == "" {
						name = id
					}
					list[id] = &rec{name: name, seen: ms}
				}
			}
			return
		}
		id, _ := e.Data["id"].(string)
		if id == "" {
			return
		}
		if e.Type == history.TNodeAdmin {
			if e.Data["action"] == "remove" {
				delete(list, id)
			}
			return
		}
		if e.Type != history.TNodeMembership {
			return
		}
		name := id
		if len(e.Nodes) > 0 && e.Nodes[0] != "" {
			name = e.Nodes[0]
		}
		ms, hasSeen := millis(e.Data["last_seen"])
		switch {
		case e.Data["known"] == true:
			if _, ok := list[id]; !ok && hasSeen {
				list[id] = &rec{name: name, seen: ms}
			}
		case e.Data["state"] == StateActive:
			list[id] = &rec{name: name, active: true, seen: e.Ts}
		case e.Data["state"] == StateLost:
			if !hasSeen {
				ms = e.Ts - LostAfter.Milliseconds()
			}
			list[id] = &rec{name: name, seen: ms}
		case e.Data["state"] == StateLeft:
			delete(list, id)
		}
	})
	if !started {
		return nil
	}
	out := append([]KnownNode(nil), snap...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// eventMachines reads the machines list of a knownMachines event, as
// recorded ([]map[string]any) or as read back from a file ([]any).
func eventMachines(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		var out []map[string]any
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// millis reads a time in unix ms from event data, as recorded (int64) or as
// read back from a file (float64).
func millis(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// SetKnown gives the pool the machines of earlier runs (KnownFrom).
func (p *Pool) SetKnown(ks []KnownNode) {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	p.known = map[string]KnownNode{}
	var carried []map[string]any
	for _, k := range ks {
		p.known[k.ID] = k
		if _, ok := p.agents[k.ID]; ok || p.admin[k.ID].Removed {
			continue // registered already (its own event lists it) or removed
		}
		carried = append(carried, map[string]any{"id": k.ID, "name": k.Name, "last_seen": k.LastSeen.UnixMilli()})
	}
	if len(carried) == 0 {
		return
	}
	sort.Slice(carried, func(i, j int) bool { return carried[i]["id"].(string) < carried[j]["id"].(string) })
	// The carry event: this run lists these machines from its start, so the
	// next start carries them again even if this run ends inside the grace.
	// One event for the fleet, naming no node: a machine's own recent events
	// get only the warning written when it is listed as lost.
	p.historyLocked(history.Event{Ts: p.o.Clock().UnixMilli(), Type: history.TKnownMachines, Severity: history.Info,
		Summary: fmt.Sprintf("Potluck knows %s from its previous run; one whose agent has not registered %s after the start is listed as lost.",
			plural(len(carried), "machine", "machines"), KnownGrace),
		Data: map[string]any{"machines": carried}})
}

// loadKnown reads the known machines from h in the background, when h can
// be read back. Once per pool.
func (p *Pool) loadKnown(h HistorySink) {
	sc, ok := h.(HistoryScanner)
	if !ok || !p.knownLoad.CompareAndSwap(false, true) {
		return
	}
	go supervise.Once("pool.known", func() {
		ks := KnownFrom(sc.Scan)
		p.SetKnown(ks)
		p.log.Info("machines known from earlier runs", "count", len(ks), "listed_after", KnownGrace)
	}, nil)
}

// absentKnownLocked is the known machines to list now: the grace is over,
// no agent of theirs is in the pool and the owner has not removed them;
// sorted by id.
func (p *Pool) absentKnownLocked(now time.Time) []KnownNode {
	if len(p.known) == 0 || now.Sub(p.started) < KnownGrace {
		return nil
	}
	var out []KnownNode
	for id, k := range p.known {
		if _, ok := p.agents[id]; ok || p.admin[id].Removed {
			continue
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// knownFacts is the entry of a known machine that has not registered.
func (p *Pool) knownFactsLocked(cfg *config.Config, now time.Time, k KnownNode) NodeFacts {
	ago := now.Sub(k.LastSeen).Seconds()
	seen := k.LastSeen
	f := NodeFacts{ID: k.ID, Name: k.Name, State: StateLost, Paused: p.admin[k.ID].Paused, Capabilities: []string{},
		LastSeenAgoS: &ago, LastSeenAt: &seen, AbsentSinceStart: true}
	if e, ok := cfg.ExcludedBy(core.NodeID(k.ID), k.Name); ok {
		f.Excluded, f.ExcludedBy = true, e
		f.Unplaced, f.UnplacedMessage = UnplacedExcluded, ExcludedMessage(e)
	}
	return f
}

// announceKnownLocked writes each listed known machine to the history once
// per start (CheckLost calls it): the event log says the machine has not
// come back, and the next start reads its last seen from this event even
// when the older events have been pruned.
func (p *Pool) announceKnownLocked(now time.Time) {
	for _, k := range p.absentKnownLocked(now) {
		if p.knownTold[k.ID] {
			continue
		}
		if p.knownTold == nil {
			p.knownTold = map[string]bool{}
		}
		p.knownTold[k.ID] = true
		p.log.Warn("a known machine has not registered since Potluck started", "id", k.ID, "name", k.Name,
			"last_seen", k.LastSeen.UTC().Format(time.RFC3339))
		p.historyLocked(history.Event{Ts: now.UnixMilli(), Type: history.TNodeMembership, Severity: history.Warn,
			Nodes: []string{k.Name},
			Summary: fmt.Sprintf("Node %s has not come back since Potluck started; last seen %s ago.", k.Name,
				now.Sub(k.LastSeen).Round(time.Minute)),
			Data: map[string]any{"id": k.ID, "state": StateLost, "known": true, "absent_since_start": true, "last_seen": k.LastSeen.UnixMilli()}})
	}
}

// plural is "1 machine" or "N machines".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
