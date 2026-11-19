package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/fleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
)

// Read-only views of the core. None of these changes scheduling state.

// cmdline is the command line shown for an instance: the child's argv once
// it runs. Before that there is no port, so a llama command is shown without
// its --port flag (the hub's --rpc and --ctx-size patterns still match) and a
// kind: command line, whose port flag is unknown, is not shown.
func (c *schedCore) cmdline(in *inst) string {
	if in.argv != nil {
		return instance.RenderCommand(in.argv)
	}
	if in.prof.Kind == core.KindCommand {
		return ""
	}
	argv, err := instance.BuildArgs(*in.prof, in.placement, 0, c.build)
	if err != nil {
		return ""
	}
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--port" {
			argv = append(argv[:i:i], argv[i+2:]...)
			break
		}
	}
	return instance.RenderCommand(argv)
}

func (c *schedCore) runningList() []RunningInstance {
	var out []RunningInstance
	for _, in := range c.sortedInsts() {
		r := RunningInstance{Profile: in.prof.Name, State: in.state.running(), Cmd: c.cmdline(in)}
		if in.state == stStarting {
			r.LoadProgress = c.loadView(in).progress
			r.Load = c.loadReport(in)
		}
		if in.state == stReady || in.state == stDraining {
			r.ReadySince = in.readyAt
		}
		out = append(out, r)
	}
	return out
}

func (c *schedCore) profileStates() []ProfileState {
	names := make([]string, 0, len(c.cfg.Profiles))
	for n := range c.cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ProfileState, 0, len(names))
	for _, n := range names {
		ps := ProfileState{Name: n, State: RunningStopped}
		if in := c.insts[n]; in != nil {
			ps.State = in.state.running()
			if ps.State == RunningReady {
				ps.ReadySince = in.readyAt
			}
		}
		out = append(out, ps)
	}
	return out
}

// upstream is the address of a ready or draining instance (design 9,
// /upstream). It never waits and never starts anything.
func (c *schedCore) upstream(profile string) (string, bool) {
	in := c.insts[profile]
	if in == nil || (in.state != stReady && in.state != stDraining) || in.child == nil {
		return "", false
	}
	return "127.0.0.1:" + strconv.Itoa(in.child.Port()), true
}

func memOf(d instance.DeviceMem) DeviceMemory {
	return DeviceMemory{Name: d.Name, ModelMiB: d.ModelMiB, KVMiB: d.KVMiB, ComputeMiB: d.ComputeMiB, TotalMiB: d.TotalMiB()}
}

// endpointOf extracts "ip:port" from an RPC buffer name "RPC0[ip:port]".
func endpointOf(name string) string {
	i := strings.IndexByte(name, '[')
	if i < 0 || !strings.HasSuffix(name, "]") {
		return ""
	}
	return name[i+1 : len(name)-1]
}

func (c *schedCore) instStatus(in *inst) InstanceStatus {
	s := InstanceStatus{
		Profile: in.prof.Name, State: in.state.String(), RunningState: in.state.running(),
		Cmd: c.cmdline(in), LogFile: in.logFile, Created: in.created, LastUsed: in.lastUsed, LoadDuration: in.loadDuration,
		InFlight: in.inFlight, Waiters: len(in.waiters), VictimOf: in.victimOf,
		ModelFile: in.prof.Model, RequestsServed: in.served,
	}
	if in.state == stStarting {
		s.Phase = in.phase.String()
		if in.phase == phLoading {
			s.LoadDeadline = in.loadDL
		}
	}
	if in.state == stReady || in.state == stDraining {
		s.ReadySince = in.readyAt
	}
	if in.state == stStarting {
		v := c.loadView(in)
		s.LoadElapsedS, s.LoadBytesTotal, s.LoadBytesSent = &v.elapsed, v.total, v.sent
		s.LoadProgress, s.LoadEtaS, s.LoadProgressSource = v.progress, v.eta, v.source
		s.Load = c.loadReport(in)
	}
	if in.child != nil {
		s.Addr = "127.0.0.1:" + strconv.Itoa(in.child.Port())
	}
	idx := map[string]int{}
	for i, pn := range in.placement.Nodes {
		s.Nodes = append(s.Nodes, PlacedNodeStatus{ID: pn.ID, Addr: pn.Addr, Local: pn.Local, BudgetMiB: pn.BudgetMiB})
		if pn.Local {
			idx[c.build.LocalDevice] = i
		} else {
			idx[pn.Addr] = i
		}
	}
	if in.child != nil {
		for _, d := range in.child.Load().Devices {
			m := memOf(d)
			if d.Host {
				s.HostMemory = append(s.HostMemory, m)
				continue
			}
			s.DeviceTotalMiB += m.TotalMiB
			key := endpointOf(d.Name)
			if key == "" {
				key = d.Name
			}
			if i, ok := idx[key]; ok {
				s.Nodes[i].Memory = append(s.Nodes[i].Memory, m)
			} else {
				s.Unmatched = append(s.Unmatched, m)
			}
		}
	}
	return s
}

func (c *schedCore) statusDoc() Status {
	st := Status{Time: c.now, ShuttingDown: c.shutdown, RecentLoads: c.recentLoads(), LearnedNeeds: c.learnedNeeds()}
	for _, in := range c.sortedInsts() {
		st.Instances = append(st.Instances, c.instStatus(in))
	}
	starting := c.anyStarting()
	for _, e := range c.orderedEntries() {
		st.Queue = append(st.Queue, EntryStatus{
			Profile: e.prof.Name, Created: e.created, Age: c.now.Sub(e.created), Clock: e.clock,
			ClockRunning: !e.claimed() && !starting, Waiters: len(e.waiters), Claimed: e.claimed(),
			Claim: append([]core.NodeID(nil), e.claim...), Victims: append([]string(nil), e.victims...),
			VictimsStopped: e.stoppedAny,
		})
		if last := &st.Queue[len(st.Queue)-1]; !e.claimed() && c.insts[e.prof.Name] == nil {
			_, last.WaitReason = c.waitWhy(e.prof)
		}
	}
	for _, id := range c.sortedNodeIDs() {
		ns := c.nodes[id]
		if ns != nil && c.now.Before(ns.quarUntil) {
			st.Quarantines = append(st.Quarantines, QuarantineStatus{Node: id, Until: ns.quarUntil, Level: ns.quarLevel})
		}
	}
	names := make([]string, 0, len(c.backoffs))
	for n := range c.backoffs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if b, on := c.backoffActive(n); on {
			st.Backoffs = append(st.Backoffs, BackoffStatus{Profile: n, Until: b.until, Level: b.level})
		}
	}
	st.View = ViewStatus{Age: c.age, Connected: c.view.Connected(), Stale: !c.fresh}
	if c.age == fleet.NeverReceived {
		st.View.Age = -1
	}
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok {
			// A node that left the fleet view altogether is still listed while
			// it is tracked as hung after node loss (design 8.4a).
			if h := c.health[id]; h != nil {
				st.View.Nodes = append(st.View.Nodes, NodeStatus{ID: id, Addr: h.addr, Health: c.healthStatus(h),
					Recovery: c.recoveryStatus(c.recov[id])})
			}
			continue
		}
		s := NodeStatus{ID: id, Hostname: n.Hostname, Local: n.Local, Online: n.Online, Addr: n.Addr, Priority: n.Priority,
			Eligible: c.eligible(id), Excluded: c.excluded(n), TotalMiB: n.TotalMiB, FreeMiB: c.freeMiB(id, n), LiveBudgetMiB: c.liveBudget(id, n),
			EmptyMiB: c.emptyMiB(id, n), PlanBudgetMiB: c.planBudget(id, n)}
		if ns := c.nodes[id]; ns != nil {
			if !ns.ovUntil.IsZero() && c.now.Before(ns.ovUntil) {
				s.OverrideFreeMiB, s.OverrideUntil = ns.ovFree, ns.ovUntil
			}
			s.Holder, s.ClaimedBy, s.Settling = ns.holder, ns.claim, ns.settle.String()
			if ns.settle != setNone {
				s.SettleUntil = ns.settleAt
			}
			if c.now.Before(ns.quarUntil) {
				s.Quarantined = ns.quarUntil
			}
			s.MissingSince = ns.missingSince
		}
		if h := c.health[id]; h != nil {
			s.Health = c.healthStatus(h)
		}
		s.Recovery = c.recoveryStatus(c.recov[id])
		st.View.Nodes = append(st.View.Nodes, s)
	}
	return st
}

func (c *schedCore) placedStatus(pl core.Placement) []PlacedNodeStatus {
	var out []PlacedNodeStatus
	for _, pn := range pl.Nodes {
		out = append(out, PlacedNodeStatus{ID: pn.ID, Addr: pn.Addr, Local: pn.Local, BudgetMiB: pn.BudgetMiB})
	}
	return out
}

// dryPlan is POST /api/plan: what a request for profile would meet now.
// It changes nothing (the need provider's result may be cached).
func (c *schedCore) dryPlan(profile string) PlanReport {
	r := PlanReport{Profile: profile}
	p := c.cfg.Profiles[profile]
	if p == nil {
		r.Action, r.Reason = PlanReject, string(ReasonUnknownModel)
		return r
	}
	if c.shutdown {
		r.Action, r.Reason = PlanReject, string(ReasonShuttingDown)
		return r
	}
	if b, on := c.backoffActive(profile); on {
		r.Action, r.Reason = PlanBackoff, fmt.Sprintf("backed off until %s", b.until.Format("15:04:05"))
		return r
	}
	if why, bad := c.forcedExcluded(p); bad && c.insts[profile] == nil {
		r.Action, r.Reason = PlanReject, why
		return r
	}
	if fe := c.forcedGoneErr(p); fe != nil && c.insts[profile] == nil {
		r.Action, r.Reason = PlanReject, fe.Message
		return r
	}
	req, info, err := c.request(p)
	if err != nil {
		r.Action, r.Reason = PlanReject, "memory need unknown: "+err.Error()
		return r
	}
	r.NeedMiB, r.Measured = info.Need.MiB, info.Need.Measured
	r.FitNeedMiB, r.NeedBasis = info.Need.WithMargin(), c.needBasis(info)
	if in := c.insts[profile]; in != nil {
		r.Instance = in.state.String()
		r.Placement = c.placedStatus(in.placement)
		switch in.state {
		case stReady:
			r.Action = PlanServe
		case stStarting:
			r.Action = PlanWait
		default:
			r.Action, r.Reason = PlanQueue, fmt.Sprintf("waits for the %s instance to go", in.state)
		}
		return r
	}
	if err := c.capacity(profile); err != nil {
		var np *placer.NoPlacement
		if errors.As(err, &np) {
			r.RequiredMiB, r.CapacityMiB = np.RequiredMiB, np.CapacityMiB
		}
		r.Action, r.Reason = PlanReject, err.Error()+c.excludedNote(p)
		return r
	}
	if e := c.entries[profile]; e != nil {
		r.Action = PlanQueue
		r.Reason = fmt.Sprintf("joins the queued entry (%d waiting, claimed %v)", len(e.waiters), e.claimed())
		return r
	}
	pl, perr := placer.Place(req, c.freeCandidates(p))
	if perr == nil {
		r.Action, r.Placement = PlanStart, c.placedStatus(pl)
		c.applyForcedFit(&r, p, pl)
		return r
	}
	if ev := c.evictables(p, 0); len(ev) > 0 {
		if res, err := placer.Plan(req, c.freeCandidates(p), ev); err == nil {
			r.Action, r.Placement = PlanEvict, c.placedStatus(res.Placement)
			for _, v := range res.Victims {
				r.Victims = append(r.Victims, PlanVictim{Profile: v, Busy: c.insts[v].inFlight > 0})
			}
			return r
		}
	}
	r.Action = PlanQueue
	_, r.Reason = c.waitWhy(p)
	return r
}

// applyForcedFit adds the forced_fit_check outcome to a plan that would
// start: a refusal in reject mode, a warning in warn mode.
func (c *schedCore) applyForcedFit(r *PlanReport, p *core.Profile, pl core.Placement) {
	ff := c.forcedFitCheck(p, pl)
	if ff == nil {
		return
	}
	if ff.mode == config.ForcedFitReject {
		r.Action, r.Reason = PlanReject, ff.message(p.Name)+" (defaults.forced_fit_check is reject)"
		return
	}
	r.Warning = ff.message(p.Name) + " (defaults.forced_fit_check is warn; it would start anyway)"
}
