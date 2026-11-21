package scheduler

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// Agents membership (agent contract). The pool comes from potluck-agent
// registrations; a load first starts one worker launch on each remote node
// (POST /launch, role worker) and only then starts the host child, which
// connects to the workers. The worker launches replace the pre-flight: no
// --list-devices probe is run, so nothing holds a worker's single client slot
// before the child connects. The launches are stopped after the instance has
// gone (its child exited, or none ran).
//
// The agent side is behind WorkerLauncher (internal/pool implements it); the
// core only says what to launch and stop and takes the outcomes as events.

// WorkerTarget is one worker launch of a load.
type WorkerTarget struct {
	Node core.NodeID
	// Addr is the worker's rpc address (host:50052) as the child names it.
	Addr string
	// MinMemAvailableMiB is the launch's min_mem_available_mib.
	MinMemAvailableMiB int64
}

// WorkerLaunchSpec is every worker launch of one load.
type WorkerLaunchSpec struct {
	IID     uint64
	Profile string
	Label   string // the model file, for the agent's logs and status
	Workers []WorkerTarget
}

// WorkerResult is the outcome of WorkerLauncher.Launch. Reason empty means
// every launch is ready; otherwise Node is the node that failed and Reason is
// the agent's refusal word, ReasonWorkerExited (Exit set) or
// ReasonAgentUnreachable (the agent did not answer).
type WorkerResult struct {
	LaunchIDs map[core.NodeID]string
	Node      core.NodeID
	Reason    string
	Message   string
	Exit      *agentproto.ExitInfo
}

// Reasons of a failed worker launch that are not agent refusal words.
const (
	ReasonWorkerExited     = "worker_exited"
	ReasonAgentUnreachable = "agent_unreachable"
)

// WorkerReport is something a ready (or loading) worker launch reported
// after Launch returned: its load progress, or that it exited.
type WorkerReport struct {
	IID  uint64
	Node core.NodeID
	Addr string // rpc address
	Load *agentproto.LoadProgress
	Exit *agentproto.ExitInfo
}

// WorkerLauncher starts and stops the worker launches of loads.
type WorkerLauncher interface {
	// Launch starts every launch of spec and waits until all are ready or
	// one fails (then the others are left for Stop). It keeps renewing the
	// leases and watching the launches until Stop; report is called with
	// what they report afterwards (never from inside Launch's caller).
	Launch(ctx context.Context, spec WorkerLaunchSpec, report func(WorkerReport)) WorkerResult
	// Stop ends every launch of load iid, also one Launch is still starting;
	// force lists the nodes whose launch is stopped with force (forced
	// DELETE, then /reset). It does not wait.
	Stop(iid uint64, force []core.NodeID)
	// ForceNode stops whatever launch the node runs (forced DELETE, then
	// /reset), for a force unload of a node no load of this run holds.
	ForceNode(node core.NodeID)
	// LoadDone says the load of iid is over (its instance is ready): from now
	// on its launches are read once a second, not four times (agent contract
	// 6.3). It does not wait.
	LoadDone(iid uint64)
}

// NodeGate is implemented by a node view that knows why a node it lists takes
// no new load (agents membership). The scheduler reads it with the node list.
type NodeGate interface {
	// Unplaced maps a node to its reason word; a node that may take a load
	// is absent.
	Unplaced() map[core.NodeID]string
}

// Reasons a listed node takes no new load (NodeStatus.Unplaced).
const (
	// UnplacedNoTierMatch: the node matches no tier. It is outside the pool:
	// never eligible and not counted in capacity, like an excluded node.
	UnplacedNoTierMatch = "no_tier_match"
	// The other reasons pass by themselves. The node stays counted in
	// capacity, so a request that needs it waits in the queue (as for a paused
	// node) and is not refused or backed off.
	UnplacedProgramsNotReady       = "programs_not_ready"       // the agent has not verified its programs (5.1)
	UnplacedModelSourceUnreachable = "model_source_unreachable" // a worker launch would need the model source (8)
	UnplacedOrphanLaunch           = "orphan_launch"            // the agent reports a launch Potluck does not hold (11)
)

// modelSourceHold is how long a node whose launch was refused with
// model_source_unreachable is left out of placements: the request stays
// queued and is tried again (agent contract 6.4).
const modelSourceHold = 5 * time.Second

// needRaise is how much a watchdog or OOM kill of a worker raises the
// profile's learned need: a tenth, at least this many MiB.
const needRaiseMinMiB = 256

type (
	effLaunchWorkers struct{ spec WorkerLaunchSpec }
	effStopWorkers   struct {
		iid   uint64
		force []core.NodeID
	}
	effForceNode     struct{ node core.NodeID }
	effWorkersLoaded struct{ iid uint64 }

	evWorkersLaunched struct {
		iid uint64
		res WorkerResult
	}
	evWorkerReport struct{ r WorkerReport }
	evForceUnload  struct {
		node  core.NodeID
		reply chan forceResult
	}
)

func (effLaunchWorkers) isEffect() {}
func (effStopWorkers) isEffect()   {}
func (effForceNode) isEffect()     {}
func (effWorkersLoaded) isEffect() {}
func (evWorkersLaunched) isEvent() {}
func (evWorkerReport) isEvent()    {}
func (evForceUnload) isEvent()     {}

type forceResult struct {
	profile string
	err     error
}

// nodeLossAfter is how long a remote node may be missing before the load on
// it is lost: design 8.4's 30 s, or at once with agents (the 10 s heartbeat
// silence already happened, agent contract 4.3).
//
// Settled as built by CR-5 (agent contract 11.1). The question was: 11 says a lost node with a launch "fails the instance at
// once". The existing node-loss path is used: the instance is killed at once
// and its other members stopped, but the waiters of a starting instance go
// back to the queue (they get no 502), as design 8.4 settled for node loss;
// the profile is not backed off.
func (c *schedCore) nodeLossAfter() time.Duration {
	if c.agents {
		return 0
	}
	return nodeLossAfter
}

// agentHeld: the node's last worker launch was refused because it could not
// reach the model source; it is skipped until the hold ends.
func (c *schedCore) agentHeld(id core.NodeID) bool {
	ns := c.nodes[id]
	return ns != nil && c.now.Before(ns.agentHold)
}

// launchWorkers starts the worker launches of a new instance.
func (c *schedCore) launchWorkers(in *inst) {
	in.phase = phWorkers
	in.workers = true
	spec := WorkerLaunchSpec{IID: in.iid, Profile: in.prof.Name, Label: path.Base(in.prof.Model)}
	floor := int64(c.cfg.Defaults.WatchdogFloorMiB)
	for _, pn := range in.placement.Nodes {
		if pn.Local {
			continue
		}
		n, _ := c.node(pn.ID)
		// Settled as built by CR-5 (agent contract 11.1). The question was: 6.2 says min_mem_available_mib comes "from the
		// budget" without a formula. A node whose device shares system memory
		// needs its budget plus the watchdog floor available; any other node
		// is asked for the floor only (the weights go to its own VRAM).
		min := floor
		if n.SharedMemory {
			min += int64(pn.BudgetMiB)
		}
		spec.Workers = append(spec.Workers, WorkerTarget{Node: pn.ID, Addr: pn.Addr, MinMemAvailableMiB: min})
	}
	c.log.Info("starting worker launches", "profile", in.prof.Name, "workers", len(spec.Workers))
	c.effects = append(c.effects, effLaunchWorkers{spec: spec})
}

// stopWorkers asks for the instance's worker launches to stop. removeInst
// calls it, so the child has exited (or never ran) by then.
func (c *schedCore) stopWorkers(in *inst) {
	if !in.workers {
		return
	}
	in.workers = false
	c.effects = append(c.effects, effStopWorkers{iid: in.iid, force: in.forceNodes})
}

// workersLaunched takes the outcome of the worker launches.
func (c *schedCore) workersLaunched(iid uint64, res WorkerResult) {
	in := c.instByIID(iid)
	if in == nil || in.state != stStarting || in.phase != phWorkers {
		return // stopped meanwhile; removeInst already asked for the stop
	}
	in.launchIDs = res.LaunchIDs
	if res.Reason == "" {
		// Settled as built by CR-5 (agent contract 11.1). The question was: the contract does not say whether the
		// pre-flight's forced-fit check and fit at exact figures (design 7.5)
		// still run. They need a probe of the workers, which agents mode does
		// not do; the child starts on the placement as made.
		c.log.Info("worker launches ready", "profile", in.prof.Name, "launches", len(res.LaunchIDs))
		c.launch(in)
		return
	}
	name := c.nodeName(res.Node)
	data := map[string]any{"reason": res.Reason, "message": res.Message}
	if res.Exit != nil {
		data["exit"] = res.Exit
	}
	c.log.Warn("worker launch failed", "profile", in.prof.Name, "node", res.Node, "reason", res.Reason, "message", res.Message)
	c.record(history.TWorkerLaunchFailed, history.Warn, in.prof.Name,
		fmt.Sprintf("The worker launch of %s on %s failed: %s (%s).", in.prof.Name, name, res.Reason, res.Message),
		[]string{name}, "", data)
	switch res.Reason {
	case agentproto.ReasonModelSourceUnreachable:
		// The request stays queued and is tried again after the hold, until
		// the queue timeout ends it.
		c.ns(res.Node).agentHold = c.now.Add(modelSourceHold)
		c.returnInst(in, "worker model source unreachable", nil)
	case ReasonAgentUnreachable:
		c.quarantine(res.Node, errors.New(res.Message))
		c.returnInst(in, "agent unreachable", nil)
	default:
		if res.Exit != nil {
			c.raiseNeedOnKill(in, res.Node, res.Exit)
		}
		c.failLoad(in, fmt.Sprintf("worker launch on %s: %s: %s", name, res.Reason, res.Message), nil, false)
	}
}

// workerReported takes a report of a worker launch.
func (c *schedCore) workerReported(r WorkerReport) {
	in := c.instByIID(r.IID)
	if in == nil {
		return
	}
	if r.Load != nil {
		if in.agentLoad == nil {
			in.agentLoad = map[string]*agentproto.LoadProgress{}
		}
		in.agentLoad[r.Addr] = r.Load
	}
	if r.Exit == nil || in.state == stStopping {
		return
	}
	name := c.nodeName(r.Node)
	msg := fmt.Sprintf("worker on %s exited: %s", name, r.Exit.Reason)
	if r.Exit.Message != "" {
		msg += " (" + r.Exit.Message + ")"
	}
	c.log.Error("worker launch exited", "profile", in.prof.Name, "node", r.Node, "reason", r.Exit.Reason, "message", r.Exit.Message,
		"state", in.state.String())
	c.record(history.TWorkerExited, history.Error, in.prof.Name,
		fmt.Sprintf("The worker of %s on %s exited (%s) while the instance was %s.", in.prof.Name, name, r.Exit.Reason, in.state),
		[]string{name}, in.sessionID, map[string]any{"exit": r.Exit, "state": in.state.String(), "in_flight": in.inFlight})
	c.raiseNeedOnKill(in, r.Node, r.Exit)
	switch in.state {
	case stStarting:
		if in.phase == phWorkers {
			return // Launch reports it as its result
		}
		c.failLoadMode(in, msg, nil, false, stopKill)
	default: // ready, draining
		in.stopReason = "worker exited"
		c.stopFor(in, stopKill, "worker exited")
	}
}

// raiseNeedOnKill raises the profile's learned need after the agent's memory
// watchdog or the kernel's OOM killer ended a worker (agent contract 6.7).
//
// Settled as built by CR-5 (agent contract 11.1). The question was: the contract says such an exit "raises the learned memory
// need" without an amount; the need is raised by a tenth (at least 256 MiB)
// and kept for this run, as any learned need.
func (c *schedCore) raiseNeedOnKill(in *inst, node core.NodeID, ex *agentproto.ExitInfo) {
	if ex == nil || (ex.Reason != agentproto.ExitWatchdog && ex.Reason != agentproto.ExitOOMKill) {
		return
	}
	info, err := c.need(in.prof)
	if err != nil || info.Need.MiB <= 0 {
		return
	}
	raised := info.Need.MiB + max(info.Need.MiB/10, needRaiseMinMiB)
	c.learned[in.prof.Name] = &learnedNeed{TotalMiB: raised}
	c.log.Warn("learned need raised after a worker was killed for memory", "profile", in.prof.Name, "node", node,
		"exit", ex.Reason, "from_mib", info.Need.MiB, "to_mib", raised)
	c.record(history.TLearnedNeed, history.Warn, in.prof.Name,
		fmt.Sprintf("The need of %s was raised from %d to %d MiB after its worker on %s was ended by %s.",
			in.prof.Name, info.Need.MiB, raised, c.nodeName(node), ex.Reason),
		[]string{c.nodeName(node)}, in.sessionID, map[string]any{"from_mib": info.Need.MiB, "to_mib": raised, "exit": ex.Reason})
}

// forceUnload is agent contract 7.2's force unload of a node: the instance on
// it is killed and the node's launch is stopped with force. With no instance
// on the node, the node's launch is stopped with force all the same.
//
// Force unload exists to clear a bad state, so requests waiting for that
// instance fail at once with an error naming the force unload and the node
// (answered by the PS5 side to the contract question on 7.2); they are not put
// back in the queue as for node loss. Requests in flight on a ready instance
// are cut off as with any kill. The profile is not held back: the next
// request starts a new load.
func (c *schedCore) forceUnload(id core.NodeID) forceResult {
	for _, in := range c.sortedInsts() {
		for _, pn := range in.placement.Nodes {
			if pn.ID != id {
				continue
			}
			c.log.Warn("force unload", "node", id, "profile", in.prof.Name, "in_flight", in.inFlight)
			if !pn.Local {
				in.forceNodes = append(in.forceNodes, id)
			}
			ws := in.waiters
			in.waiters = nil
			why := fmt.Sprintf("force unload of node %s", id)
			c.stopFor(in, stopKill, why)
			c.failAll(ws, err502(fmt.Sprintf("loading %s was ended by the %s", in.prof.Name, why), nil))
			return forceResult{profile: in.prof.Name}
		}
	}
	if c.agents {
		c.effects = append(c.effects, effForceNode{node: id})
	}
	return forceResult{}
}

// ForceUnloadNode is the force unload of a node (agent contract 7.2). It
// returns the profile whose instance was stopped, "" when none was on it.
func (s *Scheduler) ForceUnloadNode(ctx context.Context, id core.NodeID) (string, error) {
	reply := make(chan forceResult, 1)
	if !s.post(ctx, evForceUnload{node: id, reply: reply}) {
		return "", errShutting()
	}
	select {
	case r := <-reply:
		return r.profile, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// execAgents performs the agent effects.
func (s *Scheduler) execAgents(ef effect) bool {
	w := s.opts.Workers
	if w == nil {
		return false // fleet membership: no agent effects are made
	}
	switch ef := ef.(type) {
	case effLaunchWorkers:
		s.goBg(func(ctx context.Context) {
			res := w.Launch(ctx, ef.spec, func(r WorkerReport) {
				r.IID = ef.spec.IID
				s.post(context.Background(), evWorkerReport{r: r})
			})
			s.post(context.Background(), evWorkersLaunched{iid: ef.spec.IID, res: res})
		})
	case effStopWorkers:
		w.Stop(ef.iid, ef.force)
	case effForceNode:
		w.ForceNode(ef.node)
	case effWorkersLoaded:
		w.LoadDone(ef.iid)
	default:
		return false
	}
	return true
}

// handleAgents applies the agent events.
func (c *schedCore) handleAgents(ev event) bool {
	switch e := ev.(type) {
	case evWorkersLaunched:
		c.workersLaunched(e.iid, e.res)
	case evWorkerReport:
		c.workerReported(e.r)
	case evForceUnload:
		r := c.forceUnload(e.node)
		if e.reply != nil {
			e.reply <- r
		}
	default:
		return false
	}
	return true
}

// agentWorkerLoad puts each worker's own load figures, as its agent reports
// them, in place of the sending-side byte counts.
func (c *schedCore) agentWorkerLoad(in *inst, ws []LoadWorker) {
	for i := range ws {
		lp := in.agentLoad[ws[i].Addr]
		if lp == nil {
			continue
		}
		if lp.BytesTotal != nil && *lp.BytesTotal > 0 {
			ws[i].BytesTotal = *lp.BytesTotal
		}
		if lp.BytesLoaded != nil {
			b := *lp.BytesLoaded
			ws[i].BytesSent = &b
		}
		switch lp.State {
		case agentproto.LoadDone:
			ws[i].State = WorkerDone
		case agentproto.LoadReceiving:
			ws[i].State = WorkerReceiving
		default:
			ws[i].State = WorkerWaiting
		}
	}
}
