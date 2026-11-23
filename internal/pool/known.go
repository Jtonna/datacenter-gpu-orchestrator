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
// would not be listed at all. The machines Potluck knew are read back from
// the event history (the membership events of earlier runs: no table of its
// own), and one that has not registered when KnownGrace has passed since the
// start is listed as lost, with when it was last seen. It is never a node
// of the scheduler's view: placement and plans do not see it.

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

// KnownFrom folds the history, oldest first, into the known machines. A
// machine connected when a run ended was last seen when that run ended: its
// stopping event, or its last event of any kind when it crashed. A lost one
// was last seen when its heartbeats stopped (last_seen in the event, or the
// event's time less LostAfter for events written before that field). One
// that left (deregistered by its uninstall) is not known.
func KnownFrom(scan func(fn func(history.Event))) []KnownNode {
	type rec struct {
		name   string
		active bool
		left   bool
		seen   int64
	}
	recs := map[string]*rec{}
	var runLast int64
	scan(func(e history.Event) {
		if e.Type == history.TSchedulerStarted {
			for _, r := range recs {
				if r.active {
					r.active = false
					if runLast > r.seen {
						r.seen = runLast
					}
				}
			}
			runLast = e.Ts
			return
		}
		if e.Ts > runLast {
			runLast = e.Ts
		}
		if e.Type != history.TNodeMembership {
			return
		}
		id, _ := e.Data["id"].(string)
		if id == "" {
			return
		}
		r := recs[id]
		if r == nil {
			r = &rec{}
			recs[id] = r
		}
		if len(e.Nodes) > 0 && e.Nodes[0] != "" {
			r.name = e.Nodes[0]
		}
		switch e.Data["state"] {
		case StateActive:
			r.active, r.left, r.seen = true, false, e.Ts
		case StateLost:
			r.active, r.left = false, false
			if ms, ok := millis(e.Data["last_seen"]); ok {
				r.seen = ms
			} else {
				r.seen = e.Ts - LostAfter.Milliseconds()
			}
		case StateLeft:
			r.active, r.left, r.seen = false, true, e.Ts
		}
	})
	var out []KnownNode
	for id, r := range recs {
		if r.left {
			continue
		}
		seen := r.seen
		if r.active && runLast > seen {
			seen = runLast // connected in the run being read (this one): the pool lists its agent
		}
		name := r.name
		if name == "" {
			name = id
		}
		out = append(out, KnownNode{ID: id, Name: name, LastSeen: time.UnixMilli(seen)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
	for _, k := range ks {
		p.known[k.ID] = k
	}
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
			Data: map[string]any{"id": k.ID, "state": StateLost, "known": true, "last_seen": k.LastSeen.UnixMilli()}})
	}
}
