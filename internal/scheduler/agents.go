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
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
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
	// ExpectMiB is the launch's expect_mib (CR-9): the node's share of the
	// profile's need. 0 sends none.
	ExpectMiB int64
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

// NodeGateDetail is implemented by a NodeGate that can say more about a
// reason than its word (agent_unreachable: the address and what to check).
type NodeGateDetail interface {
	// UnplacedDetail maps a node to the message of its reason; a node whose
	// reason has no message is absent.
	UnplacedDetail() map[core.NodeID]string
}

// Reasons a listed node takes no new load (NodeStatus.Unplaced).
const (
	// UnplacedNoTierMatch: the node matches no tier. It is outside the pool:
	// never eligible and not counted in capacity, like an excluded node.
	UnplacedNoTierMatch = "no_tier_match"
	// UnplacedBadFloor: the agent reports a watchdog floor larger than half
	// the node's memory or larger than 16384 MiB (CR-9 review). It is outside
	// the pool like no_tier_match: it does not pass by itself.
	UnplacedBadFloor = "bad_floor"
	// The other reasons pass by themselves. The node stays counted in
	// capacity, so a request that needs it waits in the queue (as for a paused
	// node) and is not refused or backed off.
	UnplacedProgramsNotReady       = "programs_not_ready"       // the agent has not verified its programs (5.1)
	UnplacedModelSourceUnreachable = "model_source_unreachable" // a worker launch would need the model source (8)
	UnplacedOrphanLaunch           = "orphan_launch"            // the agent reports a launch Potluck does not hold (11)
	UnplacedSilent                 = "silent"                   // no heartbeat for 10 s while its launch is kept (4.3, CR-6)
	UnplacedAgentUnreachable       = ReasonAgentUnreachable     // Potluck cannot call the agent back (the call-back check, CR-8)
	// UnplacedHostMachine: the agent runs on the scheduler's own machine. It
	// is shown in the agent's status only: the node is not listed as a remote
	// node at all.
	UnplacedHostMachine = "host_machine"
	// UnplacedPaused: the operator paused the node (7.2). It is shown in the
	// agent's status when the node has no reason of its own (CR-9); the
	// scheduler itself reads the node's Paused flag.
	UnplacedPaused = "paused"
)

// modelSourceHold is how long a node whose launch was refused with
// model_source_unreachable is left out of placements: the request stays
// queued and is tried again (agent contract 6.4).
const modelSourceHold = 5 * time.Second

// pausedRefusalHold is how long a node whose launch its agent refused as
// "paused" is left out of placements. The pool lists a node as paused until
// an answer has told its agent it is unpaused, so this happens only when the
// launch overtook that answer; the next heartbeat (every 2 s) settles it.
const pausedRefusalHold = 3 * time.Second

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
// it is lost: design 8.4's 30 s, or at once with agents (the heartbeat
// silence already happened: 10 s, or the 20 s lease TTL for a node that runs
// a launch, agent contract 4.3 and CR-6).
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
// reach the model source, or because its agent still believed it was paused;
// it is skipped until the hold ends.
func (c *schedCore) agentHeld(id core.NodeID) bool {
	ns := c.nodes[id]
	return ns != nil && c.now.Before(ns.agentHold)
}

// launchWorkers starts the worker launches of a new instance.
func (c *schedCore) launchWorkers(in *inst) {
	in.phase = phWorkers
	in.workers = true
	spec := WorkerLaunchSpec{IID: in.iid, Profile: in.prof.Name, Label: path.Base(in.prof.Model)}
	expect := c.expectShares(in.prof, in.placement)
	for i, pn := range in.placement.Nodes {
		if pn.Local {
			continue
		}
		n, _ := c.node(pn.ID)
		// The node's own watchdog floor when its agent reports one (CR-9).
		floor := int64(c.cfg.Defaults.WatchdogFloorMiB)
		if n.WatchdogFloorMiB > 0 {
			floor = int64(n.WatchdogFloorMiB)
		}
		// Settled as built by CR-5 (agent contract 11.1). The question was: 6.2 says min_mem_available_mib comes "from the
		// budget" without a formula. A node whose device shares system memory
		// needs its budget plus the watchdog floor available; any other node
		// is asked for the floor only (the weights go to its own VRAM).
		min := floor
		if n.SharedMemory {
			min += int64(pn.BudgetMiB)
		}
		wt := WorkerTarget{Node: pn.ID, Addr: pn.Addr, MinMemAvailableMiB: min}
		if expect != nil {
			wt.ExpectMiB = int64(expect[i]) // CR-9: the agent refuses a launch that would not fit
		}
		spec.Workers = append(spec.Workers, wt)
	}
	c.log.Info("starting worker launches", "profile", in.prof.Name, "workers", len(spec.Workers))
	c.effects = append(c.effects, effLaunchWorkers{spec: spec})
}

// EnvRPCFileLoad tells the host llama-server how its RPC workers get the
// model: "required" makes every worker read the file itself and fails the
// load when one cannot; "0" never asks the workers and sends the weights
// over the worker connection.
const EnvRPCFileLoad = "LLAMA_RPC_FILE_LOAD"

// rpcFileLoadEnv is the host child's LLAMA_RPC_FILE_LOAD for a load with
// agent workers: the workers were launched with or without model_dir by the
// same setting (worker_model_dir), so the host must be told the same thing
// and not inherit whatever the scheduler's own environment holds. A load
// without agent workers is left as inherited. (Profiles carry no environment
// of their own, so there is nothing that could override it.)
func (c *schedCore) rpcFileLoadEnv(in *inst) []string {
	if !in.workers || in.prof.Kind == core.KindCommand {
		return nil
	}
	if c.cfg.Defaults.WorkerModelDir {
		return []string{EnvRPCFileLoad + "=required"}
	}
	return []string{EnvRPCFileLoad + "=0"}
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
		// not do; the child starts on the placement as made. CR-9 restores
		// the forced fit check before the launches are sent, from the agents'
		// own figures (forcedfit.go, agentForcedGate).
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
	case agentproto.ReasonPaused:
		// Not a failed load: the node takes no new load for a moment and the
		// request is planned again, with no backoff (the agent had not heard
		// of the unpause yet, or the node was paused meanwhile).
		c.log.Info("worker launch refused: the node's agent still believes it is paused; planning again", "profile", in.prof.Name,
			"node", res.Node, "hold", pausedRefusalHold)
		c.ns(res.Node).agentHold = c.now.Add(pausedRefusalHold)
		c.returnInst(in, "worker launch refused: node paused", nil)
	default:
		// would_not_fit (CR-9) ends here too: a failed load with the agent's
		// reason and message, no quarantine (the node did nothing wrong) and
		// nothing learned (nothing was started, so nothing was measured).
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
	msg := c.noteWorkerExit(in, r)
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

// noteWorkerExit logs and records a worker launch that ended by itself
// (workerExited), applies the memory-kill rule to the learned need and
// returns the failure text.
func (c *schedCore) noteWorkerExit(in *inst, r WorkerReport) string {
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
	return msg
}

// ownWorkerExit picks, from the launches read when the child exited, the one
// that explains it: a memory kill first, else the first that ended by itself.
func ownWorkerExit(rs []WorkerReport) *WorkerReport {
	var first *WorkerReport
	for i := range rs {
		if rs[i].Exit == nil {
			continue
		}
		if rs[i].Exit.Reason == agentproto.ExitWatchdog || rs[i].Exit.Reason == agentproto.ExitOOMKill {
			return &rs[i]
		}
		if first == nil {
			first = &rs[i]
		}
	}
	return first
}

// workerEndedInstance handles a child that exited after (or together with) a
// worker launch that ended by itself: the worker's reason is the failure
// reason in the answer, the log and history, and the child's exit is named
// beside it.
func (c *schedCore) workerEndedInstance(in *inst, r WorkerReport, st instance.ExitStatus) {
	msg := c.noteWorkerExit(in, r)
	if in.state == stStarting {
		c.failLoad(in, msg+"; the host program then ended: "+st.String(), st.Tail, true)
		return
	}
	in.stopReason = "worker exited"
	c.removeInst(in, true)
}

// WorkerExitReader is implemented by a WorkerLauncher that can say which
// launches of a load ended by themselves. The scheduler asks when a child
// exits without having been stopped, before it handles the exit: the agent
// keeps the exit record of its last launch. The call is bounded (2 s);
// launches that were stopped on request are not returned.
type WorkerExitReader interface {
	Exits(ctx context.Context, iid uint64) []WorkerReport
}

// workerExits reads the ended launches of a load whose child just exited.
func (s *Scheduler) workerExits(iid uint64, st instance.ExitStatus) []WorkerReport {
	r, ok := s.opts.Workers.(WorkerExitReader)
	if !ok || st.Requested {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rs := r.Exits(ctx, iid)
	for i := range rs {
		rs[i].IID = iid
	}
	return rs
}

// raiseNeedOnKill raises the profile's learned need after the agent's memory
// watchdog or the kernel's OOM killer ended a worker (agent contract 6.7).
//
// Settled as built by CR-5 (agent contract 11.1). The question was: the contract says such an exit "raises the learned memory
// need" without an amount; the need is raised by a tenth (at least 256 MiB)
// and kept, as any learned need (stored across a restart, needs.go).
func (c *schedCore) raiseNeedOnKill(in *inst, node core.NodeID, ex *agentproto.ExitInfo) {
	if ex == nil || (ex.Reason != agentproto.ExitWatchdog && ex.Reason != agentproto.ExitOOMKill) {
		return
	}
	info, err := c.need(in.prof)
	if err != nil || info.Need.MiB <= 0 {
		return
	}
	raised := info.Need.MiB + max(info.Need.MiB/10, needRaiseMinMiB)
	l := &learnedNeed{TotalMiB: raised, key: needKeyOf(in.prof), at: c.now, release: c.buildRelease, cause: string(ex.Reason),
		placement: in.nodeIDs()}
	if base, ok := c.needs_[in.prof.Name]; ok {
		l.modelBytes = base.FileBytes
	}
	c.learned[in.prof.Name] = l
	c.storeLearned(in.prof.Name)
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
