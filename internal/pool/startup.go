package pool

import (
	"fmt"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/startups"
)

// StartupStore keeps the start-up records (startups.Store in the scheduler
// database; startups.MemStore without one).
type StartupStore interface {
	Insert(r *startups.Record) error
	Update(r *startups.Record) error
	List(node string, limit int) ([]startups.Record, error)
	All() ([]startups.Record, error)
	Latest(node string) (*startups.Record, error)
	ByStart(node, agentStartID string) (*startups.Record, error)
}

// ExpectRestartFor is how long an expect-restart call stays pending.
const ExpectRestartFor = 10 * time.Minute

// Kinds an expect-restart call may name.
const (
	ExpectSoft = "soft"
	ExpectFull = "full"
)

// expectation is a pending expect-restart call.
type expectation struct {
	kind, comment string
	at            time.Time
	// startID is the agent process running at the call: a registration of
	// another one closes the expectation.
	startID string
}

// ConflictError is an expect-restart call the node's state does not allow
// (answered 409).
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// Startups returns the store of start-up records.
func (p *Pool) Startups() StartupStore { return p.o.Startups }

// ExpectRestart is POST /api/nodes/{id}/expect-restart: the operator is about
// to restart the node (kind soft or full). It is taken only while the node is
// paused and holds no launch. For ExpectRestartFor the node's absence is
// expected (logged at INFO, not as an error), and the next registration of
// another agent process closes it: that start's record is marked requested
// with the seconds it was away.
func (p *Pool) ExpectRestart(id, kind, comment string) error {
	p.mu.Lock()
	defer p.unlock()
	a := p.agents[id]
	if a == nil || p.admin[id].Removed {
		if p.knownLocked(id) {
			return &ConflictError{Msg: fmt.Sprintf("node %s is removed", id)}
		}
		return ErrUnknownNode
	}
	if a.left {
		return &ConflictError{Msg: fmt.Sprintf("node %s left the pool", id)}
	}
	if !p.admin[id].Paused {
		return &ConflictError{Msg: fmt.Sprintf("node %s is not paused; pause it first so that it takes no load while it restarts", id)}
	}
	for _, l := range p.launches {
		if l.node == core.NodeID(id) {
			return &ConflictError{Msg: fmt.Sprintf("node %s holds launch %s; unload it first", id, l.id)}
		}
	}
	if a.hb != nil && a.hb.LaunchID != nil && *a.hb.LaunchID != "" {
		return &ConflictError{Msg: fmt.Sprintf("node %s reports launch %s; wait until it has stopped", id, *a.hb.LaunchID)}
	}
	if p.expect == nil {
		p.expect = map[string]*expectation{}
	}
	p.expect[id] = &expectation{kind: kind, comment: comment, at: p.o.Clock(), startID: a.reg.AgentStartID}
	p.log.Info("restart expected: the node's absence is not an error for the next 10 minutes", "id", id, "name", a.reg.Name,
		"kind", kind, "comment", comment)
	return nil
}

// ExpectedRestart reports whether a node has a pending expect-restart call.
func (p *Pool) ExpectedRestart(id string) (kind string, at time.Time, ok bool) {
	p.mu.Lock()
	defer p.unlock()
	if e := p.expect[id]; e != nil {
		return e.kind, e.at, true
	}
	return "", time.Time{}, false
}

// startupRegisterLocked handles the start-up part of an accepted
// registration: a new agent process gets a new record (classified against
// the node's previous one), the same process (registered again) adds to its
// record. A registration without a report (an agent of 0.1.3) gets no record.
func (p *Pool) startupRegisterLocked(a, old *agent, req agentproto.RegisterRequest, now time.Time, ans *agentproto.RegisterAnswer) {
	var exp *expectation
	if e := p.expect[req.ID]; e != nil && e.startID != req.AgentStartID {
		exp = e
		delete(p.expect, req.ID)
		p.log.Info("expected restart: the node is back", "id", req.ID, "name", req.Name, "kind", e.kind,
			"away_s", round3(now.Sub(e.at).Seconds()))
	}
	if old != nil && old.rec != nil && old.reg.AgentStartID == req.AgentStartID {
		a.rec = old.rec
	}
	if req.Startup == nil {
		return
	}
	ans.StartupAck = req.Startup.Count()
	if a.rec == nil {
		r, err := p.o.Startups.ByStart(req.ID, req.AgentStartID)
		if err != nil {
			p.log.Warn("start-up records unreadable", "id", req.ID, "err", err)
		}
		a.rec = r // Potluck restarted and the same agent process registers again
	}
	if a.rec != nil {
		if a.rec.Merge(req.Startup) {
			p.saveStartupLocked(a.rec)
		}
		return
	}
	// The node's previous record: the one this run holds for the previous
	// agent process (its write may still be queued), else the store's.
	var prev *startups.Record
	if old != nil && old.rec != nil {
		prev = old.rec
	} else {
		var err error
		if prev, err = p.o.Startups.Latest(req.ID); err != nil {
			p.log.Warn("start-up records unreadable", "id", req.ID, "err", err)
		}
	}
	if prev != nil && prev.Close() {
		p.saveStartupLocked(prev)
	}
	r := startups.New(req.ID, req.Name, req.AgentRelease, req.Install, req.AgentStartID,
		req.ModelSource.Kind == agentproto.ModelSourceShare, now, req.Startup, prev)
	if exp != nil {
		r.MarkRequested(exp.kind, exp.comment, exp.at, now)
	}
	p.saveStartupLocked(r)
	a.rec = r
	p.log.Info("node start", "id", req.ID, "name", req.Name, "kind", r.Kind, "inferred", r.Inferred, "why", r.KindNote)
}

// startupHeartbeatLocked adds a heartbeat's report to the agent's record and
// returns the acknowledgement for the answer.
func (p *Pool) startupHeartbeatLocked(a *agent, rep *agentproto.Startup) int {
	if a.rec == nil {
		// The registration carried no report (it should have): take this
		// one as if it had, so the start is not lost.
		req := a.reg
		req.Startup = rep
		var ans agentproto.RegisterAnswer
		p.startupRegisterLocked(a, nil, req, a.registeredAt, &ans)
		return rep.Count()
	}
	if a.rec.Merge(rep) {
		p.saveStartupLocked(a.rec)
	}
	return rep.Count()
}

// evaluateStartupsLocked gives open records their figure when the node is
// ready for model loads (rebuildLocked calls it, after the unplaced reasons).
func (p *Pool) evaluateStartupsLocked() {
	now := p.o.Clock()
	for _, id := range p.sortedIDsLocked() {
		a := p.agents[id]
		if a.rec == nil || !a.rec.Open || a.lost || a.left || p.admin[id].Removed {
			continue
		}
		why := p.unplaced[core.NodeID(id)]
		if p.isHostAgentLocked(a) {
			why = scheduler.UnplacedHostMachine
		}
		if a.rec.Evaluate(now, why) {
			p.saveStartupLocked(a.rec)
			if a.rec.HeadlineS != nil {
				p.log.Info("node ready for model loads", "id", id, "name", a.reg.Name, "kind", a.rec.Kind,
					"seconds_after_boot_start", *a.rec.HeadlineS, "was_blocked_by", a.rec.WasBlockedBy)
			}
		}
	}
}

// checkStartupsLocked ends what has run out (CheckLost calls it): records
// past their lifetime and expect-restart calls past ExpectRestartFor.
func (p *Pool) checkStartupsLocked(now time.Time) {
	for _, id := range p.sortedIDsLocked() {
		a := p.agents[id]
		if a.rec != nil && a.rec.Expire(now) {
			p.saveStartupLocked(a.rec)
			p.log.Info("node start without a figure", "id", id, "name", a.reg.Name, "kind", a.rec.Kind, "condition", a.rec.Condition())
		}
	}
	for id, e := range p.expect {
		if now.Sub(e.at) < ExpectRestartFor {
			continue
		}
		delete(p.expect, id)
		name := id
		if a := p.agents[id]; a != nil {
			name = a.reg.Name
		}
		p.log.Warn("expected restart did not happen: no new agent registered within 10 minutes", "id", id, "name", name, "kind", e.kind)
		p.historyLocked(history.Event{Ts: now.UnixMilli(), Type: history.TNodeAdmin, Severity: history.Warn, Nodes: []string{name},
			Summary: fmt.Sprintf("Expected restart of node %s did not happen within 10 minutes.", name),
			Data:    map[string]any{"id": id, "action": "expect_restart_expired", "kind": e.kind}})
	}
}

// saveStartupLocked queues the write of a snapshot of r; it runs after p.mu
// is released (after.go). The first write that succeeds inserts the record
// and gives r its id; later ones update it.
func (p *Pool) saveStartupLocked(r *startups.Record) {
	snap, st := r.Snapshot(), p.o.Startups
	p.afterLocked(func() {
		p.mu.Lock()
		id := r.ID
		p.mu.Unlock()
		var err error
		if id == 0 {
			if err = st.Insert(&snap); err == nil {
				p.mu.Lock()
				r.ID = snap.ID
				p.mu.Unlock()
			}
		} else {
			snap.ID = id
			err = st.Update(&snap)
		}
		if err != nil {
			p.log.Warn("start-up record not stored", "id", snap.NodeID, "err", err)
		}
	})
}

func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }
