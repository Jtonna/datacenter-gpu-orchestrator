package pool

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
)

// The call-back check (CR-8). A node whose firewall drops inbound connections
// registers and sends heartbeats like any other; every load placed on it
// would cost the connect timeout and fail. Until Potluck has reached the
// agent's API once after the registration, the node takes no load, and its
// status says why and what to check.
func TestCallBackCheck_UnreachableAgentIsNotPlaceableUntilItAnswers(t *testing.T) {
	r := newRig(t)
	r.failCallBack("dial tcp 192.0.2.10:1: i/o timeout")
	id := "02:00:00:00:00:a1"
	if ans := r.p.Register(regReq(id)); !ans.Accepted {
		t.Fatalf("a registration is accepted whatever the call-back says: %+v", ans)
	}
	r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	if got := r.unplaced(id); got != scheduler.UnplacedAgentUnreachable {
		t.Fatalf("unplaced %q, want agent_unreachable", got)
	}
	addr := "192.0.2.10:" + strconv.Itoa(r.agentPort)
	f := r.fact(id)
	if f.Placeable || f.State != StateActive || f.Unplaced != scheduler.UnplacedAgentUnreachable {
		t.Fatalf("facts %+v", f)
	}
	for _, want := range []string{addr, "firewall", "i/o timeout", strconv.Itoa(r.workerPort)} {
		if !strings.Contains(f.UnplacedMessage, want) {
			t.Fatalf("message %q does not say %q", f.UnplacedMessage, want)
		}
	}
	if msg := r.p.UnplacedDetail()[core.NodeID(id)]; msg != f.UnplacedMessage {
		t.Fatalf("detail for the scheduler %q, facts %q", msg, f.UnplacedMessage)
	}
	// It is tried again (every 15 s; 20 ms here), and warned about once.
	before := r.cbCalls.Load()
	eventually(t, 5*time.Second, func() bool { return r.cbCalls.Load() >= before+3 }, "the call-back is tried again")
	if n := strings.Count(r.logs.String(), "agent unreachable"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, r.logs.String())
	}
	if !strings.Contains(r.logs.String(), "level=WARN msg=\"agent unreachable") || !strings.Contains(r.logs.String(), addr) {
		t.Fatalf("the warning must be a WARN that names the address:\n%s", r.logs.String())
	}
	// A registration again of the same unreachable agent does not warn again.
	r.p.Register(regReq(id))
	if n := strings.Count(r.logs.String(), "agent unreachable"); n != 1 {
		t.Fatalf("%d warnings after registering again, want 1", n)
	}
	r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	drain(r.p.Changed())

	// The firewall rule was added.
	r.failCallBack("")
	eventually(t, 5*time.Second, func() bool { return r.unplaced(id) == "" }, "placeable after the call-back answered")
	if f := r.fact(id); !f.Placeable || f.Unplaced != "" || f.UnplacedMessage != "" {
		t.Fatalf("facts %+v", f)
	}
	if !drain(r.p.Changed()) {
		t.Fatal("the scheduler was not told that the node became placeable")
	}
	if n := strings.Count(r.logs.String(), "level=INFO msg=\"agent reachable"); n != 1 {
		t.Fatalf("%d INFO lines for the recovery, want 1:\n%s", n, r.logs.String())
	}
	// Once it answered it is not asked again.
	n := r.cbCalls.Load()
	time.Sleep(100 * time.Millisecond)
	if r.cbCalls.Load() != n {
		t.Fatal("the call-back went on after it answered")
	}
}

// The real call-back is GET /launch on the agent port.
func TestCallBackCheck_ReadsTheLaunchStateOfARealAgent(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	eventually(t, 5*time.Second, func() bool { return r.unplaced(a.ID) == "" }, "placeable")
	if !a.Has("GET /launch") {
		t.Fatalf("the agent was not called back: %v", a.Calls())
	}
	if strings.Contains(r.logs.String(), "agent unreachable") || strings.Contains(r.logs.String(), "level=INFO msg=\"agent reachable") {
		t.Fatalf("a reachable agent is not worth a line:\n%s", r.logs.String())
	}
}

// A launch request that gets no answer puts the node back into
// agent_unreachable: the next load is not placed on it.
func TestCallBackCheck_FailedLaunchRequestPutsTheNodeBack(t *testing.T) {
	r := newRig(t)
	id := "02:00:00:00:00:a2"
	q := regReq(id)
	q.Addr = "127.0.0.9" // nothing listens on the agent port there
	r.failCallBack("")
	real := r.p.callBack
	r.p.callBack = func(context.Context, string) error { return nil }
	r.p.Register(q)
	r.p.callBack = real
	r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	if got := r.unplaced(id); got != "" {
		t.Fatalf("unplaced %q before the launch", got)
	}
	spec := scheduler.WorkerLaunchSpec{IID: 3, Profile: "m", Label: "m.gguf", Workers: []scheduler.WorkerTarget{
		{Node: core.NodeID(id), Addr: net.JoinHostPort(q.Addr, strconv.Itoa(r.workerPort))}}}
	res := r.p.Launch(context.Background(), spec, func(scheduler.WorkerReport) {})
	if res.Reason != ReasonAgentUnreachable {
		t.Fatalf("%+v", res)
	}
	if got := r.unplaced(id); got != scheduler.UnplacedAgentUnreachable {
		t.Fatalf("unplaced %q after a launch request without an answer", got)
	}
	if f := r.fact(id); f.Placeable || !strings.Contains(f.UnplacedMessage, "127.0.0.9:"+strconv.Itoa(r.agentPort)) {
		t.Fatalf("facts %+v", f)
	}
	if n := strings.Count(r.logs.String(), "agent unreachable"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, r.logs.String())
	}
	r.p.Stop(3, nil)
	r.p.WaitStops(5 * time.Second)
}

// A state read that gets no answer does the same; the agent that answers
// again is placeable again.
func TestCallBackCheck_FailedStateReadPutsTheNodeBack(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	if res := r.p.Launch(context.Background(), spec(4, a), func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	r.p.mu.Lock()
	var l *launch
	for _, x := range r.p.launches {
		l = x
	}
	r.p.mu.Unlock()
	r.failCallBack("no route to host")
	dead := &launch{p: r.p, iid: l.iid, id: l.id, node: l.node, name: l.name, changed: make(chan struct{}, 1),
		base: "http://127.0.0.9:" + strconv.Itoa(r.agentPort)}
	r.p.poll(context.Background(), dead)
	if got := r.unplaced(a.ID); got != scheduler.UnplacedAgentUnreachable {
		t.Fatalf("unplaced %q after a state read without an answer", got)
	}
	// A read that fails because the launch is being stopped says nothing.
	r.failCallBack("")
	eventually(t, 5*time.Second, func() bool { return r.unplaced(a.ID) == "" }, "placeable again")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.p.poll(ctx, dead)
	if got := r.unplaced(a.ID); got != "" {
		t.Fatalf("unplaced %q after a cancelled read", got)
	}
	r.p.Stop(4, nil)
	r.p.WaitStops(10 * time.Second)
}

// A connection to an agent that cannot be established fails after the
// connect timeout (5 s), not after the request's own 30 s. The dialer here
// never connects: it waits for as long as it is allowed to.
func TestLaunchRequestConnectTimeout(t *testing.T) {
	cfg := &config.Config{Defaults: config.DefaultDefaults()}
	var allowed atomic.Int64
	hang := func(ctx context.Context, _, _ string) (net.Conn, error) {
		if dl, ok := ctx.Deadline(); ok {
			allowed.Store(int64(time.Until(dl)))
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p, err := New(Options{Config: func() *config.Config { return cfg }, PotluckRelease: "0.1.0", Dial: hang})
	if err != nil {
		t.Fatal(err)
	}
	if p.o.ConnectTimeout != 5*time.Second {
		t.Fatalf("default connect timeout %v, want 5 s", p.o.ConnectTimeout)
	}
	p, err = New(Options{Config: func() *config.Config { return cfg }, PotluckRelease: "0.1.0", Dial: hang, ConnectTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	_, ref, err := p.postLaunch(context.Background(), "http://192.0.2.1:50060", agentproto.LaunchRequest{LaunchID: "ln-1"})
	if err == nil || ref != nil {
		t.Fatalf("answer without a connection: %v %v", ref, err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("the launch request took %v to fail, want the connect timeout (50 ms here)", d)
	}
	if d := time.Duration(allowed.Load()); d <= 0 || d > 50*time.Millisecond {
		t.Fatalf("the dialer was allowed %v, want at most the connect timeout (50 ms here), not the request's 30 s", d)
	}
}

// An agent that registers again while a launch of ours runs on it (an agent
// restart that re-adopted the launch, same launch id) keeps the instance. The
// node is not placeable until the call-back answers; that must not end or
// disturb the running launch or its lease renewals.
func TestCallBackCheck_RegisteringAgainKeepsARunningLaunch(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	rep := &reports{}
	if res := r.p.Launch(context.Background(), spec(6, a), rep.add); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	id := a.LaunchState().LaunchID
	r.failCallBack("dial tcp: i/o timeout") // the call-back does not answer for a while
	q := regReq(a.ID)
	q.Name, q.Addr, q.AgentStartID, q.LaunchID = a.Name, a.Addr, a.StartID, &id
	ans := r.p.Register(q)
	if !ans.Accepted || ans.StopLaunch != nil {
		t.Fatalf("registration with our launch: %+v", ans)
	}
	if got := r.unplaced(a.ID); got != scheduler.UnplacedAgentUnreachable {
		t.Fatalf("unplaced %q while the call-back has not answered", got)
	}
	before := a.Leases()
	eventually(t, 5*time.Second, func() bool { return a.Leases() >= before+3 }, "leases renewed while the node is not placeable")
	if f := r.fact(a.ID); f.PotluckLaunchID == nil || *f.PotluckLaunchID != id || f.LaunchState != agentproto.StateReady {
		t.Fatalf("the launch is not held any more: %+v", f)
	}
	r.failCallBack("")
	eventually(t, 5*time.Second, func() bool { return r.unplaced(a.ID) == "" }, "placeable again")
	before = a.Leases()
	eventually(t, 5*time.Second, func() bool { return a.Leases() >= before+2 }, "leases renewed afterwards")
	if st := a.LaunchState(); st.LaunchID != id || st.State != agentproto.StateReady {
		t.Fatalf("the launch was disturbed: %+v", st)
	}
	if len(rep.exits()) != 0 || a.Has("DELETE /launch/") {
		t.Fatalf("the launch was ended: exits %+v calls %v", rep.exits(), a.Calls())
	}
	r.p.Stop(6, nil)
	r.p.WaitStops(10 * time.Second)
	if !a.Has("DELETE /launch/" + id) {
		t.Fatalf("the launch is still stopped on request: %v", a.Calls())
	}
}

// The host machine's agent is not placeable and says why.
func TestHostMachineAgentHasAReason(t *testing.T) {
	r := newRig(t)
	q := regReq("02:00:00:00:00:a3")
	q.Name = "host-1" // fleet.local_node
	r.p.Register(q)
	r.p.Heartbeat(q.ID, hbBody("s1", "", true, "true"))
	f := r.fact(q.ID)
	if !f.HostAgent || f.Placeable || f.Unplaced != scheduler.UnplacedHostMachine {
		t.Fatalf("facts %+v", f)
	}
}

// The local_node setting names the host machine with no fleet section at all;
// while it is empty, fleet.local_node still does (an older database).
func TestHostMachineAgentByLocalNodeSetting(t *testing.T) {
	for _, c := range []struct {
		name, setting, fleet string
		host                 bool
	}{
		{"local_node only", "host-2", "", true},
		{"fleet.local_node fallback", "", "host-2", true},
		{"local_node wins", "host-2", "host-9", true},
		{"no name", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.cfg.Fleet = config.Fleet{LocalNode: c.fleet} // set before any agent registers
			r.cfg.LocalNode = c.setting
			q := regReq("02:00:00:00:00:a4")
			q.Name = "host-2"
			r.p.Register(q)
			r.p.Heartbeat(q.ID, hbBody("s1", "", true, "true"))
			if f := r.fact(q.ID); f.HostAgent != c.host || (c.host && f.Unplaced != scheduler.UnplacedHostMachine) {
				t.Fatalf("facts %+v", f)
			}
		})
	}
}

// launch_id and role in a node's status are what its agent reported in its
// heartbeat. The launch Potluck holds for the node has its own field: it
// exists before the agent has received anything.
func TestStatusLaunchIDIsWhatTheAgentReported(t *testing.T) {
	r := newRig(t)
	hold, entered := make(chan struct{}), make(chan struct{})
	a := r.agent(1, func(a *fakeagent.Agent) {
		a.Refuse = func(agentproto.LaunchRequest) *agentproto.LaunchRefusal {
			close(entered)
			<-hold
			return &agentproto.LaunchRefusal{Reason: agentproto.ReasonPaused, Message: "refused by the test"}
		}
	})
	r.waitNode(a.ID)
	done := make(chan scheduler.WorkerResult, 1)
	go func() { done <- r.p.Launch(context.Background(), spec(5, a), func(scheduler.WorkerReport) {}) }()
	<-entered
	time.Sleep(150 * time.Millisecond) // a few heartbeats (50 ms)
	f := r.fact(a.ID)
	if f.LaunchID != nil {
		t.Errorf("launch_id %q although the agent runs no launch", *f.LaunchID)
	}
	if f.Role != agentproto.RoleIdle {
		t.Errorf("role %q, want what the heartbeat said (idle)", f.Role)
	}
	if f.PotluckLaunchID == nil || !strings.HasPrefix(*f.PotluckLaunchID, "ln-") {
		t.Errorf("potluck_launch_id %v, want the launch Potluck is sending", f.PotluckLaunchID)
	}
	close(hold)
	if res := <-done; res.Reason != agentproto.ReasonPaused {
		t.Fatalf("%+v", res)
	}
	if f := r.fact(a.ID); f.LaunchID != nil || f.PotluckLaunchID != nil {
		t.Fatalf("after the refusal: %+v", f)
	}
}
