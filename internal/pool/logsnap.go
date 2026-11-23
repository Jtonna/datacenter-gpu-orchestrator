package pool

import (
	"encoding/json"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Central logging (CR-10, design section 4.4): the log routes must never wait
// for the pool. The pool publishes what they need as an immutable snapshot
// behind an atomic pointer, each time one of these facts changes: a
// registration, a node leaving or being removed, a launch starting or ending.
// A heartbeat changes none of them and publishes nothing.

// LogNode is what the log routes need to know about a node.
type LogNode struct {
	StartID  string // agent_start_id of the node's registration
	Name     string
	Platform string
	Release  string // the agent's release
}

type logLaunch struct{ node, profile string }

// LogSnapshot is the pool's state as the log routes see it. It is never
// changed after it was published.
type LogSnapshot struct {
	nodes    map[string]LogNode
	launches map[string]logLaunch
}

// Node returns a node whose heartbeat would be accepted: registered, not
// left, not removed.
func (s *LogSnapshot) Node(id string) (LogNode, bool) {
	n, ok := s.nodes[id]
	return n, ok
}

// Profile returns the profile of a launch Potluck holds, or held until
// lately, on that node.
func (s *LogSnapshot) Profile(launchID, nodeID string) (string, bool) {
	l, ok := s.launches[launchID]
	if !ok || l.node != nodeID {
		return "", false
	}
	return l.profile, true
}

// LogSnapshot returns the current snapshot. It takes no lock and never
// returns nil.
func (p *Pool) LogSnapshot() *LogSnapshot { return p.logSnap.Load() }

// keepEndedLaunches is how many ended launches stay in the snapshot: the
// last lines of a launch (the ones that say why it ended) arrive after
// Potluck has dropped it.
const keepEndedLaunches = 256

type endedLaunch struct {
	id string
	logLaunch
}

// endLaunchLocked drops a launch from the pool and remembers its profile for
// the log routes. confirmed: the launch is known not to run any more (the
// agent reported it exited or no longer has it, was reset, or never started
// it); only then is the stop seen (core.Node.StopSeenAt). A worker still
// stopping when the wait gave up leaves it unseen: its memory may still be
// held, and the scheduler then waits to the end of the window (TS1).
func (p *Pool) endLaunchLocked(l *launch, confirmed bool) {
	if _, held := p.launches[l.id]; !held {
		return
	}
	delete(p.launches, l.id)
	if a := p.agents[string(l.node)]; a != nil {
		if confirmed {
			a.stopSeen, a.figsSinceStop = p.o.Clock(), 0
		}
		p.rebuildLocked()
	}
	p.ended = append(p.ended, endedLaunch{l.id, logLaunch{string(l.node), l.profile}})
	if n := len(p.ended) - keepEndedLaunches; n > 0 {
		p.ended = append(p.ended[:0:0], p.ended[n:]...)
	}
}

// publishLogSnapLocked builds and publishes a new snapshot.
func (p *Pool) publishLogSnapLocked() {
	s := &LogSnapshot{nodes: make(map[string]LogNode, len(p.agents)), launches: make(map[string]logLaunch, len(p.launches)+len(p.ended))}
	for id, a := range p.agents {
		if a.left || p.admin[id].Removed {
			continue
		}
		s.nodes[id] = LogNode{StartID: a.reg.AgentStartID, Name: a.reg.Name, Platform: a.platform, Release: a.reg.AgentRelease}
	}
	for _, e := range p.ended {
		s.launches[e.id] = e.logLaunch
	}
	for id, l := range p.launches {
		s.launches[id] = logLaunch{string(l.node), l.profile}
	}
	p.logSnap.Store(s)
}

// NodeLogs is the "logs" part of a node's status (CR-10): what Potluck's log
// route counted for the node, and what the node's agent reports about its
// own shipping.
type NodeLogs struct {
	AcceptedBytes    int64            `json:"accepted_bytes"`
	AcceptedRequests int64            `json:"accepted_requests"`
	Refused          map[string]int64 `json:"refused"` // by reason word
	// Agent is from the agent's last heartbeat; absent when it reports none
	// (release 0.1.0, or shipping is off).
	Agent *agentproto.LogShipping `json:"agent,omitempty"`
}

// parseShipping reads the heartbeat's logs object. Anything unreadable is
// taken as "not reported".
func parseShipping(raw json.RawMessage) *agentproto.LogShipping {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v agentproto.LogShipping
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if len(v.LastError) > 512 {
		v.LastError = v.LastError[:512]
	}
	return &v
}
