package pool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Host launches (phase 6, a console as host; release 0.1.9). A profile with
// host: node has its llama-server host process started on a node by that
// node's agent (role host, agent contract 6.2). The pool sends the launch,
// renews its lease, reads its state and copies its log as for a worker
// launch, and hands the scheduler an instance.RemoteChild it drives from
// what the agent reports: the output (buffer sizes, tail), ready, and the
// exit. The launch is held in p.launches like a worker launch (so the
// heartbeat's launch_id is ours and a silent node keeps it until the lease
// ends) but is not one of the load's worker launches (byIID): the load's
// Stop, Exits and LoadDone are about workers; the host is the load's child.

var (
	_ scheduler.HostLauncher = (*Pool)(nil)
	_ scheduler.HostGate     = (*Pool)(nil)
)

// CanHost is scheduler.HostGate: only an agent that advertises
// host_launch is ever sent a host launch (an agent of release 0.1.6 does not).
func (p *Pool) CanHost(id core.NodeID) (bool, string) {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	return p.canHostLocked(id)
}

func (p *Pool) canHostLocked(id core.NodeID) (bool, string) {
	a := p.agents[string(id)]
	switch {
	case a == nil || a.left:
		return false, "its agent is not registered"
	case a.lost:
		return false, "its agent is lost"
	case !slices.Contains(a.reg.Capabilities, agentproto.CapHostLaunch):
		return false, fmt.Sprintf("its agent (release %s) does not advertise %s", a.reg.AgentRelease, agentproto.CapHostLaunch)
	}
	return true, ""
}

// ReasonHostNodeLost is the exit reason of a host launch whose node was
// lost: nobody could ask the node what happened.
const ReasonHostNodeLost = "node_lost"

// hostPollEvery is how often a host launch's state is read while it loads,
// and after (a variable for the tests).
var hostPollEvery = [2]time.Duration{pollLoading, pollAfterLoad}

// StartHost is scheduler.HostLauncher: it sends the host launch and returns
// its child once the agent accepted it. A refusal is an error that carries
// the agent's reason and message.
func (p *Pool) StartHost(ctx context.Context, spec scheduler.HostLaunchSpec) (instance.Child, error) {
	l, addrIP, err := p.holdHost(spec)
	if err != nil {
		return nil, err
	}
	req := agentproto.LaunchRequest{LaunchID: l.id, Role: agentproto.RoleHost, Release: p.o.PotluckRelease,
		Label: spec.Label, LeaseTTLS: LeaseTTL.Seconds(), MinMemAvailableMiB: spec.MinMemAvailableMiB,
		Host: &agentproto.HostSpec{Model: spec.Model, Ctx: spec.Ctx, Parallel: spec.Parallel,
			Workers: nonNil(spec.Workers), TensorSplit: spec.TensorSplit, ExtraArgs: nonNil(spec.ExtraArgs),
			StrictFileLoad: spec.RPCFileLoad == agentproto.RPCFileLoadRequired, RPCFileLoad: spec.RPCFileLoad,
			ExpectMiB: spec.ExpectMiB, APIKey: spec.APIKey}}
	if req.Host.TensorSplit == nil {
		req.Host.TensorSplit = []float64{}
	}
	st, ref, err := p.postLaunch(ctx, l.base, req)
	switch {
	case ref != nil:
		p.forget(l, true)
		return nil, fmt.Errorf("the agent of %s refused the host launch: %s: %s", l.name, ref.Reason, ref.Message)
	case err != nil:
		p.forget(l, false)
		var ue *url.Error
		if errors.As(err, &ue) && ctx.Err() == nil {
			p.agentUnreachable(l.node, err)
		}
		return nil, fmt.Errorf("%s: host launch on %s: %v", ReasonAgentUnreachable, l.name, err)
	}
	port := st.Port
	if port <= 0 {
		port = agentproto.PortHost
	}
	func() {
		l.mu.Lock()
		defer supervise.Unlocked("pool", l.mu.Unlock)
		l.st, l.have = st, true
	}()
	h := &hostRun{p: p, l: l}
	ch := instance.NewRemoteChild(net.JoinHostPort(addrIP, strconv.Itoa(port)), st.Command, spec.APIKey, h.stop, nil)
	h.ch = ch
	l.feed = ch.Feed // set before the supervisor runs; never changed
	p.log.Info("host launch sent", "profile", spec.Profile, "node", l.node, "launch", l.id, "addr", ch.Addr(), "workers", len(spec.Workers))
	h.supervise()
	return ch, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// holdHost checks that the node can host and holds a new launch for it.
func (p *Pool) holdHost(spec scheduler.HostLaunchSpec) (*launch, string, error) {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	if ok, why := p.canHostLocked(spec.Node); !ok {
		return nil, "", fmt.Errorf("%s: node %s cannot host: %s", scheduler.ReasonHostNotCapable, spec.Node, why)
	}
	a := p.agents[string(spec.Node)]
	p.seq++
	l := &launch{p: p, iid: spec.IID, node: spec.Node, name: a.reg.Name, profile: spec.Profile,
		id:      fmt.Sprintf("ln-%d-%d-%d", p.o.Clock().UnixMilli(), spec.IID, p.seq),
		base:    p.agentBase(a),
		changed: make(chan struct{}, 1), done: make(chan struct{})}
	p.launches[l.id] = l
	p.publishLogSnapLocked()
	return l, a.reg.Addr, nil
}

// hostRun is one host launch and its child.
type hostRun struct {
	p  *Pool
	l  *launch
	ch *instance.RemoteChild
}

// supervise renews the lease, reads the state and copies the log until the
// launch has exited (or its agent no longer has it); it then reports the
// exit to the child. Only this goroutine copies the log, so the child sees
// every byte once.
//
// Task pool.host_launch, policy restart (as pool.launch): its state is the
// agent's, so after a failure it renews the lease and reads the launch state
// at once; ready is kept across the restart so the child is told once. A
// failure inside l.mu or p.mu stops Potluck (supervise.Unlocked).
func (h *hostRun) supervise() {
	p, l := h.p, h.l
	ctx, cancel := context.WithCancel(context.Background())
	func() {
		l.mu.Lock()
		defer supervise.Unlocked("pool", l.mu.Unlock)
		l.cancel = cancel
	}()
	restarted, ready := false, false
	supervise.Go(ctx, supervise.Spec{Name: "pool.host_launch", Policy: supervise.Restart, Rebuild: func(any) { restarted = true },
		Done: func() { cancel(); close(l.done) }}, func(ctx context.Context) {
		if restarted {
			p.renew(ctx, l)
			p.poll(ctx, l)
		}
		lease := time.NewTicker(p.o.LeaseEvery)
		defer lease.Stop()
		logs := time.NewTicker(2 * time.Second)
		defer logs.Stop()
		every := hostPollEvery[0]
		if ready {
			every = hostPollEvery[1]
		}
		poll := time.NewTicker(every)
		defer poll.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.ch.Done():
				return // finished by the stop path (a lost node)
			case <-lease.C:
				p.renew(ctx, l)
			case <-logs.C:
				p.copyLog(ctx, l)
			case <-l.changed:
			case <-poll.C:
				supervise.Point("pool.host_launch", l.id) // test hook, per host launch
				p.poll(ctx, l)
			}
			if ex := l.exitInfo(); ex != nil {
				p.copyLog(context.Background(), l)
				h.finish(ex, true)
				return
			}
			if !ready && l.stateName() == agentproto.StateReady {
				ready = true
				p.copyLog(ctx, l) // the buffer sizes are in the log by now
				h.ch.SetReady()
				poll.Reset(hostPollEvery[1])
			}
		}
	})
}

// finish reports the launch's end to the child and lets the launch go.
func (h *hostRun) finish(ex *agentproto.ExitInfo, confirmed bool) {
	st := instance.ExitStatus{Code: -1, Reason: ex.Reason}
	if ex.Code != nil {
		st.Code = *ex.Code
	}
	if ex.Signal != nil {
		st.Signal = *ex.Signal
	}
	if ex.Message != "" {
		st.Reason += ": " + ex.Message
	}
	if len(h.ch.Tail(1)) == 0 && len(ex.LogTail) > 0 {
		h.ch.Feed([]byte(strings.Join(ex.LogTail, "\n") + "\n"))
	}
	h.p.log.Info("host launch ended", "launch", h.l.id, "node", h.l.node, "reason", ex.Reason, "message", ex.Message)
	h.ch.Finish(st)
	h.p.endHost(h.l, confirmed)
}

func (p *Pool) endHost(l *launch, confirmed bool) {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	p.endLaunchLocked(l, confirmed)
	p.publishLogSnapLocked()
}

// stop is the child's stop function (instance.RemoteChild runs it on its
// own goroutine, a graceful stop before a kill). A plain stop is DELETE; the
// supervisor then sees the exit. A kill is the forced DELETE and, when that
// does not end the launch within 5 s, /reset (agent contract 7.2). A lost
// node is not called: the launch ends with it (its lease runs out there).
func (h *hostRun) stop(mode instance.StopMode) {
	p, l := h.p, h.l
	if lost, why := p.hostNodeGone(l.node); lost {
		p.log.Info("host launch not stopped: its node is lost", "launch", l.id, "node", l.node)
		var cancel context.CancelFunc
		func() {
			l.mu.Lock()
			defer supervise.Unlocked("pool", l.mu.Unlock)
			cancel = l.cancel
		}()
		if cancel != nil {
			cancel()
			<-l.done
		}
		h.finish(&agentproto.ExitInfo{Reason: ReasonHostNodeLost, Message: why, LogTail: []string{}}, false)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	force := mode == instance.StopKill
	p.log.Info("stopping host launch", "launch", l.id, "node", l.node, "force", force)
	if err := p.deleteLaunch(ctx, l.base, l.id, force); err != nil {
		p.log.Warn("stopping host launch failed", "launch", l.id, "node", l.node, "err", err)
	}
	if force && !p.forceTail(ctx, l.base, l.id, string(l.node)) {
		// Neither the forced stop nor the reset was confirmed: the
		// supervisor keeps reading; the lease ends the launch on the node.
		p.log.Warn("forced stop of the host launch not confirmed; its lease ends it on the node", "launch", l.id, "node", l.node)
	}
	l.wake() // the supervisor reads the state now
}

// hostNodeGone reports whether the node of a host launch is lost or gone.
func (p *Pool) hostNodeGone(id core.NodeID) (bool, string) {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	a := p.agents[string(id)]
	switch {
	case a == nil || a.left:
		return true, "its agent left"
	case a.lost:
		return true, "no heartbeat from its agent"
	}
	return false, ""
}
