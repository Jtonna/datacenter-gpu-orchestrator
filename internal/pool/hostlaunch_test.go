package pool

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
)

const testHostKey = "k0123456789abcdef0123456789abcde"

// hostAgent is a node whose agent takes host launches.
func (r *rig) hostAgent(n int, mod ...func(*fakeagent.Agent)) *fakeagent.Agent {
	port := freePort(r.t)
	return r.agent(n, append([]func(*fakeagent.Agent){func(a *fakeagent.Agent) {
		a.Capabilities, a.HostPort = []string{agentproto.CapHostLaunch}, port
	}}, mod...)...)
}

func hostSpec(iid uint64, a *fakeagent.Agent, workers ...*fakeagent.Agent) scheduler.HostLaunchSpec {
	s := scheduler.HostLaunchSpec{IID: iid, Profile: "m", Label: "m.gguf", Node: core.NodeID(a.ID), Model: "m.gguf",
		Ctx: 4096, Parallel: 1, RPCFileLoad: agentproto.RPCFileLoadOff, ExtraArgs: []string{"--jinja"},
		ExpectMiB: 3000, MinMemAvailableMiB: 2000, APIKey: testHostKey}
	for _, w := range workers {
		s.Workers = append(s.Workers, net.JoinHostPort(w.Addr, "50052"))
	}
	if len(workers) > 0 {
		s.TensorSplit = make([]float64, len(workers)+1)
		for i := range s.TensorSplit {
			s.TensorSplit[i] = 1 / float64(len(s.TensorSplit))
		}
	}
	return s
}

func waitExit(t *testing.T, ch instance.Child) instance.ExitStatus {
	t.Helper()
	select {
	case st := <-ch.Exited():
		return st
	case <-time.After(15 * time.Second):
		t.Fatal("the host launch did not end")
	}
	return instance.ExitStatus{}
}

func startHost(t *testing.T, r *rig, s scheduler.HostLaunchSpec) instance.Child {
	t.Helper()
	ch, err := r.p.StartHost(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ch.WaitHealthy(ctx); err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestHostLaunchServesWithItsKeyAndStops(t *testing.T) {
	r := newRig(t)
	a, b := r.hostAgent(1), r.agent(2)
	r.waitNode(a.ID)
	r.waitNode(b.ID)
	ch := startHost(t, r, hostSpec(21, a, b))
	ls := a.Launches()
	if len(ls) != 1 || ls[0].Role != agentproto.RoleHost || ls[0].Host == nil || ls[0].Host.APIKey != testHostKey ||
		ls[0].Host.ExpectMiB != 3000 || ls[0].MinMemAvailableMiB != 2000 || len(ls[0].Host.Workers) != 1 ||
		ls[0].Host.RPCFileLoad != agentproto.RPCFileLoadOff || ls[0].Host.StrictFileLoad {
		t.Fatalf("%+v", ls)
	}
	addr := instance.AddrOf(ch)
	if addr != net.JoinHostPort(a.Addr, strconv.Itoa(a.HostPort)) || instance.APIKeyOf(ch) != testHostKey || ch.PID() != 0 {
		t.Fatal(addr)
	}
	// The buffer sizes come from the copied launch log.
	if ld := ch.Load(); ld.TotalMiB() < 3000 {
		t.Fatalf("%+v", ld)
	}
	// Without the key the host answers 401; with it, 200.
	for _, c := range []struct {
		key  string
		want int
	}{{"", 401}, {testHostKey, 200}} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader("{}"))
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("key %q: %d", c.key, resp.StatusCode)
		}
	}
	eventually(t, 5*time.Second, func() bool { return a.Leases() >= 2 }, "lease renewed")
	if f := r.fact(a.ID); f.Role != agentproto.RoleHost {
		t.Fatalf("heartbeat role %q", f.Role)
	}
	if got := r.unplaced(a.ID); got == scheduler.UnplacedOrphanLaunch {
		t.Fatal("a held host launch is no orphan")
	}
	ch.Stop(instance.StopGraceful)
	st := waitExit(t, ch)
	if !st.Requested || !strings.HasPrefix(st.Reason, agentproto.ExitStopped) {
		t.Fatalf("%+v", st)
	}
	if a.Has("DELETE /launch/" + ls[0].LaunchID + "?force") {
		t.Fatal(a.Calls())
	}
	eventually(t, 5*time.Second, func() bool { return len(r.p.HeldLaunches()) == 0 }, "launch let go")
	// The port is closed with the launch.
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("host port still open")
	}
}

func TestHostLaunchForcedStop(t *testing.T) {
	r := newRig(t)
	a := r.hostAgent(1)
	r.waitNode(a.ID)
	ch := startHost(t, r, hostSpec(22, a))
	id := a.LaunchState().LaunchID
	ch.Stop(instance.StopKill)
	st := waitExit(t, ch)
	if !st.Requested || !strings.HasPrefix(st.Reason, agentproto.ExitForced) || !a.Has("DELETE /launch/"+id+"?force=1") {
		t.Fatalf("%+v %v", st, a.Calls())
	}
}

// A forced stop the agent does not act on is followed by /reset.
func TestHostLaunchForcedStopThenReset(t *testing.T) {
	r := newRig(t)
	a := r.hostAgent(1, func(a *fakeagent.Agent) { a.IgnoreForce, a.IgnoreStop = true, true })
	r.waitNode(a.ID)
	ch := startHost(t, r, hostSpec(23, a))
	ch.Stop(instance.StopGraceful)
	ch.Stop(instance.StopKill)
	st := waitExit(t, ch)
	if !st.Requested || !a.Has("POST /reset") {
		t.Fatalf("%+v %v", st, a.Calls())
	}
}

// The host process dies on the node (the watchdog): the child ends with the
// agent's reason, not as requested.
func TestHostLaunchProcessDies(t *testing.T) {
	r := newRig(t)
	a := r.hostAgent(1)
	r.waitNode(a.ID)
	ch := startHost(t, r, hostSpec(24, a))
	a.ExitLaunch(agentproto.ExitWatchdog, "MemAvailable 900 MiB fell below the watchdog floor")
	st := waitExit(t, ch)
	if st.Requested || !strings.HasPrefix(st.Reason, agentproto.ExitWatchdog+": MemAvailable 900") {
		t.Fatalf("%+v", st)
	}
	eventually(t, 5*time.Second, func() bool { return len(r.p.HeldLaunches()) == 0 }, "launch let go")
}

// The host launch exits before it is ready: WaitHealthy fails.
func TestHostLaunchExitsWhileStarting(t *testing.T) {
	r := newRig(t)
	a := r.hostAgent(1, func(a *fakeagent.Agent) { a.ReadyAfter = 3 * time.Second })
	r.waitNode(a.ID)
	ch, err := r.p.StartHost(context.Background(), hostSpec(25, a))
	if err != nil {
		t.Fatal(err)
	}
	a.ExitLaunch(agentproto.ExitStartFailed, "llama-server: unknown argument")
	if err := ch.WaitHealthy(context.Background()); !errors.Is(err, instance.ErrChildExited) || !strings.Contains(err.Error(), "unknown argument") {
		t.Fatal(err)
	}
}

// An agent without host_launch (release 0.1.6) is never sent a host launch.
func TestHostLaunchNeverSentToAnAgentWithoutTheCapability(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	if ok, why := r.p.CanHost(core.NodeID(a.ID)); ok || !strings.Contains(why, agentproto.CapHostLaunch) {
		t.Fatal(ok, why)
	}
	_, err := r.p.StartHost(context.Background(), hostSpec(26, a))
	if err == nil || !strings.Contains(err.Error(), scheduler.ReasonHostNotCapable) {
		t.Fatal(err)
	}
	if a.Has("POST /launch") || len(r.p.HeldLaunches()) != 0 {
		t.Fatal(a.Calls())
	}
}

// The agent refuses the host launch: the error carries its reason and
// nothing is held.
func TestHostLaunchRefused(t *testing.T) {
	r := newRig(t)
	a := r.hostAgent(1, func(a *fakeagent.Agent) { a.MemAvail = 2500 })
	r.waitNode(a.ID)
	_, err := r.p.StartHost(context.Background(), hostSpec(27, a)) // expects 3000 MiB with a floor of 1000
	if err == nil || !strings.Contains(err.Error(), agentproto.ReasonWouldNotFit) {
		t.Fatal(err)
	}
	if len(r.p.HeldLaunches()) != 0 {
		t.Fatal(r.p.HeldLaunches())
	}
}

// The host node is lost: a stop is not sent to it; the child ends at once.
func TestHostLaunchNodeLost(t *testing.T) {
	r := newRig(t)
	a := r.hostAgent(1)
	r.waitNode(a.ID)
	ch := startHost(t, r, hostSpec(28, a))
	a.SetHeartbeats(false)
	time.Sleep(100 * time.Millisecond)
	r.clk.advance(21 * time.Second) // past the lease TTL (CR-6)
	r.p.CheckLost()
	if n, _ := r.node(a.ID); n.Online {
		t.Fatal("not lost")
	}
	ch.Stop(instance.StopKill)
	st := waitExit(t, ch)
	if !strings.HasPrefix(st.Reason, ReasonHostNodeLost) || a.Has("DELETE") {
		t.Fatalf("%+v %v", st, a.Calls())
	}
	eventually(t, 5*time.Second, func() bool { return len(r.p.HeldLaunches()) == 0 }, "launch let go")
}

// The scheduler restarts while a node hosts: the new run does not hold the
// launch and stops it (owner decision: a console-hosted model is not kept
// across a restart).
func TestHostLaunchStoppedAfterASchedulerRestart(t *testing.T) {
	r := newRig(t)
	a := r.hostAgent(1)
	r.waitNode(a.ID)
	ch := startHost(t, r, hostSpec(29, a))
	addr := instance.AddrOf(ch)
	old := r.p
	r.start()
	// The old run is gone: its supervisors stop with it.
	old.mu.Lock()
	for _, l := range old.launches {
		if l.cancel != nil {
			l.cancel()
		}
	}
	old.unlock()
	eventually(t, 10*time.Second, func() bool { return a.LaunchState().State == agentproto.StateExited }, "host launch stopped by the new run")
	if st := a.LaunchState(); st.Exit == nil || st.Exit.Reason != agentproto.ExitStopped {
		t.Fatalf("%+v", st)
	}
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("host port still open")
	}
}
