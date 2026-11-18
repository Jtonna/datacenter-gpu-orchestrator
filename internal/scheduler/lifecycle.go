package scheduler

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
)

const (
	stopGraceful = instance.StopGraceful
	stopKill     = instance.StopKill
)

// startInstance holds the placement's nodes and creates the instance in
// starting (8.3 steps 1 and 4). Its first act is the pre-flight (7.5).
func (c *schedCore) startInstance(p *core.Profile, pl core.Placement, waiters []*waiter, e *entry) {
	if c.insts[p.Name] != nil {
		panic(fmt.Sprintf("scheduler: second instance for %s", p.Name))
	}
	in := &inst{iid: c.next(), prof: p, state: stStarting, phase: phPreflight, placement: pl,
		created: c.now, waiters: waiters, entryCreated: e.created, entrySeq: e.seq, entryClock: e.clock}
	for _, n := range pl.Nodes {
		ns := c.ns(n.ID)
		switch {
		case ns.holder != "":
			panic(fmt.Sprintf("scheduler: node %s placed for %s but held by %s", n.ID, p.Name, ns.holder))
		case ns.claim != "" && ns.claim != p.Name:
			panic(fmt.Sprintf("scheduler: node %s placed for %s but claimed by %s", n.ID, p.Name, ns.claim))
		case ns.settle != setNone:
			panic(fmt.Sprintf("scheduler: node %s placed for %s while %s", n.ID, p.Name, ns.settle))
		case c.quarantined(n.ID):
			panic(fmt.Sprintf("scheduler: node %s placed for %s while quarantined", n.ID, p.Name))
		}
		ns.holder = p.Name
		ns.claim = ""
	}
	c.insts[p.Name] = in
	c.changed = true
	c.emitState(in, RunningStarting)
	c.logPlacement(in)

	var targets []probeTarget
	for _, n := range pl.Nodes {
		if !n.Local {
			targets = append(targets, probeTarget{id: n.ID, addr: n.Addr})
		}
	}
	if len(targets) == 0 {
		c.launch(in) // the local node is not probed (7.5)
		return
	}
	c.effects = append(c.effects, effProbe{iid: in.iid, targets: targets})
}

// preflightDone applies design 7.5 to the probe results.
func (c *schedCore) preflightDone(iid uint64, results []probeOutcome) {
	in := c.instByIID(iid)
	if in == nil || in.state != stStarting || in.phase != phPreflight {
		return // stopped meanwhile; its nodes are already released
	}
	failed := false
	for _, r := range results {
		ns := c.ns(r.id)
		if r.err != nil {
			var pe *instance.ProbeError
			kind := "unknown"
			if errors.As(r.err, &pe) {
				kind = pe.Kind.String()
			}
			c.log.Warn("pre-flight failed", "profile", in.prof.Name, "node", r.id, "addr", r.addr, "kind", kind, "duration", r.took, "err", r.err)
			if pe != nil && pe.Stderr != "" {
				c.log.Debug("pre-flight raw output", "node", r.id, "addr", r.addr, "stderr", pe.Stderr)
			}
			if pe != nil && pe.Kind == instance.ProbeCanceled {
				failed = true // not a verdict on the node
				continue
			}
			if errors.As(r.err, &pe) && pe.Kind == instance.ProbeMultipleRPCLines {
				c.log.Error("worker exposes more than one RPC device; pin one with -d on the worker", "node", r.id, "addr", r.addr)
			}
			c.quarantine(r.id, r.err)
			failed = true
			continue
		}
		c.log.Info("pre-flight ok", "profile", in.prof.Name, "node", r.id, "addr", r.addr,
			"free_mib", r.result.FreeMiB, "total_mib", r.result.TotalMiB, "duration", r.took)
		ns.quarLevel = 0
		ns.quarUntil = c.now // a successful pre-flight resets the quarantine
		ns.ovFree, ns.ovUntil = r.result.FreeMiB, c.now.Add(overrideTTL)
		ns.emptyPF, ns.hasPF = r.result.FreeMiB, true
		ns.fetched = nil
	}
	if failed {
		c.returnInst(in, "pre-flight failed", nil)
		return
	}
	req, _, err := c.request(in.prof)
	if err != nil {
		c.failLoad(in, err.Error(), nil, false)
		return
	}
	pl := in.placement
	pl.Nodes = slices.Clone(pl.Nodes)
	budgets := make([]int, len(pl.Nodes))
	for i, pn := range pl.Nodes {
		n, _ := c.node(pn.ID)
		budgets[i] = c.liveBudget(pn.ID, n)
		pl.Nodes[i].BudgetMiB = budgets[i]
	}
	if !placer.Fits(req, budgets) {
		c.returnInst(in, "exact pre-flight figures do not fit", budgets)
		return
	}
	in.placement = pl
	c.launch(in)
}

// returnInst is design 7.5's "returned": straight to absent, nodes released
// without settling, waiters back into the profile's entry with its original
// creation time and accumulated clock.
func (c *schedCore) returnInst(in *inst, reason string, budgets []int) {
	c.log.Info("instance returned", "profile", in.prof.Name, "reason", reason, "nodes", nodeIDStrings(in.nodeIDs()),
		"budgets", budgets, "waiters", len(in.waiters))
	ws := in.waiters
	in.waiters = nil
	c.removeInst(in, false)
	c.requeue(in, ws)
}

// requeue puts waiters back into the profile's queue entry, restoring the
// entry that started the instance.
func (c *schedCore) requeue(in *inst, ws []*waiter) {
	if len(ws) == 0 {
		return
	}
	e := c.entries[in.prof.Name]
	if e == nil {
		e = &entry{prof: in.prof, seq: in.entrySeq, created: in.entryCreated, clock: in.entryClock}
		c.entries[in.prof.Name] = e
	}
	e.waiters = append(e.waiters, ws...)
	c.changed = true
}

// launch starts the child (after a successful or skipped pre-flight).
func (c *schedCore) launch(in *inst) {
	p := *in.prof
	pl := in.placement
	info, err := c.need(in.prof)
	if err != nil {
		c.failLoad(in, err.Error(), nil, false)
		return
	}
	shares := instance.RemoteShares(p, pl, func(id core.NodeID) float64 {
		n, _ := c.node(id)
		return n.LinkMBs
	})
	in.loadTimeout = instance.EffectiveLoadTimeout(p, c.cfg.Defaults.CommandLoadTimeout, info.FileBytes, shares)
	opts := c.build
	health := defaultHealthCheck
	if p.Kind == core.KindCommand && p.CheckEndpoint != "" {
		health = p.CheckEndpoint
	}
	in.phase = phLaunching
	c.effects = append(c.effects, effStart{iid: in.iid, spec: instance.StartSpec{
		Build:      func(port int) ([]string, error) { return instance.BuildArgs(p, pl, port, opts) },
		HealthPath: health,
		Env:        instance.ChildEnvOverrides(p, pl),
	}})
}

func (c *schedCore) childStarted(iid uint64, ch instance.Child, err error) {
	in := c.instByIID(iid)
	if in == nil || in.phase != phLaunching {
		if ch != nil {
			c.effects = append(c.effects, effStop{iid: iid, child: ch, mode: stopKill})
		}
		return
	}
	if err != nil {
		if in.state == stStopping {
			c.removeInst(in, false)
			return
		}
		c.failLoad(in, "starting the child: "+err.Error(), nil, true)
		return
	}
	in.child, in.childRan, in.argv = ch, true, ch.Argv()
	c.log.Info("child started", "profile", in.prof.Name, "pid", ch.PID(), "port", ch.Port(), "cmd", instance.RenderCommand(in.argv))
	if in.state == stStopping {
		c.effects = append(c.effects, effStop{iid: in.iid, child: ch, mode: in.stopMode})
		return
	}
	in.phase = phLoading
	in.loadDL = c.now.Add(in.loadTimeout)
	c.changed = true
}

func (c *schedCore) healthy(iid uint64, err error) {
	in := c.instByIID(iid)
	if in == nil || in.state != stStarting || in.phase != phLoading || err != nil {
		return // exits and timeouts arrive as their own events
	}
	in.state, in.phase = stReady, phRunning
	in.readyAt, in.lastUsed = c.now, c.now
	in.loadDuration = c.now.Sub(in.created)
	delete(c.backoffs, in.prof.Name) // reset when the profile next reaches ready
	for _, n := range in.placement.Nodes {
		ns := c.ns(n.ID)
		ns.hasPF, ns.hasClaim = false, false // forgotten when a load on the node succeeds
	}
	c.emitState(in, RunningReady)
	load := in.child.Load()
	c.log.Info("instance ready", "profile", in.prof.Name, "load_duration", in.loadDuration,
		"memory_mib", load.TotalMiB(), "devices", load.DeviceCount(), "waiters", len(in.waiters))
	ws := in.waiters
	in.waiters = nil
	for _, w := range ws {
		c.admit(in, w)
	}
	c.admitEntry(in)
	c.changed = true
}

// stopInst moves an instance to stopping. Without a child (pre-flight) it
// goes straight to absent and its nodes are released without settling.
// Waiters are the caller's business.
func (c *schedCore) stopInst(in *inst, mode instance.StopMode) {
	if in.victimOf != "" {
		if e := c.entries[in.victimOf]; e != nil {
			e.stoppedAny = true
		}
	}
	if in.state == stStopping {
		if mode == stopKill && in.stopMode != stopKill {
			in.stopMode = stopKill
			if in.child != nil {
				c.effects = append(c.effects, effStop{iid: in.iid, child: in.child, mode: stopKill})
			}
		}
		return
	}
	if in.phase == phPreflight {
		c.removeInst(in, false)
		return
	}
	if in.phase == phLoading {
		c.effects = append(c.effects, effCancelHealth{iid: in.iid})
	}
	in.state, in.stopMode = stStopping, mode
	c.changed = true
	c.log.Info("instance stopping", "profile", in.prof.Name, "reason", c.stopReasonOf(in), "mode", modeName(mode), "in_flight", in.inFlight)
	c.emitState(in, RunningStopping)
	if in.child != nil {
		c.effects = append(c.effects, effStop{iid: in.iid, child: in.child, mode: mode})
	}
}

// removeInst makes the instance absent and releases its nodes; settle says
// whether a child ran on them (then they settle, 7.1).
func (c *schedCore) removeInst(in *inst, settle bool) {
	if len(in.waiters) > 0 {
		panic(fmt.Sprintf("scheduler: removing %s with %d waiters", in.prof.Name, len(in.waiters)))
	}
	if in.phase == phLoading && in.state == stStarting {
		c.effects = append(c.effects, effCancelHealth{iid: in.iid})
	}
	if in.victimOf != "" {
		if e := c.entries[in.victimOf]; e != nil {
			e.stoppedAny = true // gone counts as stopped (8.3 step 6)
			e.victims = slices.DeleteFunc(e.victims, func(v string) bool { return v == in.prof.Name })
		}
	}
	for _, n := range in.placement.Nodes {
		c.releaseNode(n.ID, in.prof.Name, settle)
	}
	delete(c.insts, in.prof.Name)
	c.changed = true
	if in.childRan {
		var loadedFor time.Duration
		if !in.readyAt.IsZero() {
			loadedFor = c.now.Sub(in.readyAt)
		}
		c.log.Info("instance stopped", "profile", in.prof.Name, "reason", c.stopReasonOf(in), "loaded_for", loadedFor,
			"requests_served", in.served, "exit", in.exit)
	}
	c.emitState(in, RunningStopped)
}

func (c *schedCore) releaseNode(id core.NodeID, holder string, settle bool) {
	ns := c.ns(id)
	if ns.holder != holder {
		return
	}
	ns.holder = ""
	if !settle {
		// DESIGN: 7.1 ends the pre-flight override "when an instance is
		// next released from the node", but 7.5 says a returned instance
		// releases its nodes and "the next pass sees ... the override, so
		// it does not repeat the same placement". Read literally, the
		// release by the returned instance would erase the override it
		// just set (pre-flight measures a: 9000 free, fleet says 15000;
		// fit fails; release clears the override; the next pass places on
		// a by 15000 again, forever). Only a release after a child ran
		// (the settling kind) ends the override.
		return
	}
	n, _ := c.node(id)
	ns.settle, ns.settleAt = setWaiting, c.now.Add(n.Settle)
	ns.ovUntil = timeZero // an instance released from the node ends the override
	ns.failLog = false
	ns.fetched = nil
}

func (c *schedCore) exited(iid uint64, st instance.ExitStatus) {
	in := c.instByIID(iid)
	if in == nil || in.child == nil {
		return
	}
	in.exit = st.String()
	switch in.state {
	case stStarting:
		c.failLoad(in, "child exited during load: "+st.String(), st.Tail, true)
	default:
		if in.state != stStopping {
			c.log.Error("instance exited unexpectedly", "profile", in.prof.Name, "exit", in.exit, "in_flight", in.inFlight)
			in.stopReason = "child exited"
		}
		// ready or draining: in-flight requests fail on their own (the
		// connection is gone); stopping: the stop completed.
		c.removeInst(in, true)
	}
}

// failLoad is design 8.4's load failure: waiters get 502 with the child's
// output tail, the profile is backed off, the instance stops. gone says
// no process exists any more (it exited, or Start failed); the nodes then
// settle only if a child ran on them.
func (c *schedCore) failLoad(in *inst, msg string, tail []string, gone bool) {
	if tail == nil && in.child != nil {
		tail = in.child.Tail(failureTailLines)
	}
	if len(tail) > failureTailLines {
		tail = tail[len(tail)-failureTailLines:]
	}
	c.log.Error("load failed", "profile", in.prof.Name, "reason", msg, "exit", in.exit,
		"waiters", len(in.waiters), "last_lines", strings.Join(lastLines(tail, 10), " | "))
	if in.stopReason == "" {
		in.stopReason = "load failed"
	}
	ws := in.waiters
	in.waiters = nil
	c.failAll(ws, err502(fmt.Sprintf("loading %s failed: %s", in.prof.Name, msg), tail))
	c.startBackoff(in.prof.Name)
	switch {
	case gone:
		c.removeInst(in, in.childRan)
	case in.child != nil || in.phase == phLaunching:
		c.stopInst(in, stopGraceful)
	default:
		c.removeInst(in, false)
	}
}

// logPlacement logs the placement chosen for a new instance.
func (c *schedCore) logPlacement(in *inst) {
	need, measured := 0, false
	if info, err := c.need(in.prof); err == nil {
		need, measured = info.Need.MiB, info.Need.Measured
	}
	budgets := make([]int, len(in.placement.Nodes))
	for i, n := range in.placement.Nodes {
		budgets[i] = n.BudgetMiB
	}
	c.log.Info("placement chosen", "profile", in.prof.Name, "nodes", nodeIDStrings(in.nodeIDs()), "budgets", budgets,
		"need_mib", need, "measured", measured, "waiters", len(in.waiters))
}

func nodeIDStrings(ids []core.NodeID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

func modeName(m instance.StopMode) string {
	if m == stopKill {
		return "kill"
	}
	return "graceful"
}

func lastLines(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// stopFor stops an instance and remembers why (the first reason wins), so
// the "instance stopping" and "instance stopped" lines can say.
func (c *schedCore) stopFor(in *inst, mode instance.StopMode, reason string) {
	if in.stopReason == "" {
		in.stopReason = reason
	}
	c.stopInst(in, mode)
}

// stopReasonOf is the recorded reason, else "evicted for X" for a victim.
func (c *schedCore) stopReasonOf(in *inst) string {
	switch {
	case in.stopReason != "":
		return in.stopReason
	case in.victimOf != "":
		return "evicted for " + in.victimOf
	}
	return "unspecified"
}
