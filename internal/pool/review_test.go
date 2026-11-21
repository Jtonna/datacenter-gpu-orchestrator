package pool

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// hbBody is a heartbeat of regReq's agent (agent_start_id s1). launch "" is
// launch_id null; reachable "" leaves model_source.reachable out.
func hbBody(startID, launch string, ready bool, reachable string) []byte {
	lid := "null"
	if launch != "" {
		lid = `"` + launch + `"`
	}
	src := `{"kind":"share"}`
	if reachable != "" {
		src = `{"kind":"share","reachable":` + reachable + `}`
	}
	return []byte(fmt.Sprintf(`{"agent_start_id":%q,"seq":1,"uptime_s":3,"role":"idle","launch_id":%s,"programs_ready":%v,
		"programs_error":null,"model_source":%s,"mem_available_mib":9000,
		"devices":[{"name":"Vulkan0","backend":"vulkan","total_mib":14336,"free_mib":14000,"shares_system_memory":true}]}`,
		startID, lid, ready, src))
}

func (r *rig) unplaced(id string) string { return r.p.Unplaced()[core.NodeID(id)] }

func (r *rig) fact(id string) NodeFacts {
	r.t.Helper()
	for _, f := range r.p.Facts() {
		if f.ID == id {
			return f
		}
	}
	r.t.Fatalf("no facts for %s", id)
	return NodeFacts{}
}

func drain(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// A node whose programs are not verified, or whose model source a worker
// launch would need and cannot reach, takes no load: the agent would refuse
// the launch (6.2) and the profile would be backed off. The reason is in the
// node's status.
func TestNodeIsNotPlaceableUntilProgramsReadyAndModelSourceReachable(t *testing.T) {
	r := newRig(t)
	const id = "02:00:00:00:00:73"
	r.p.Register(regReq(id))
	if _, ok := r.node(id); !ok {
		t.Fatal("a registered node is not in the view")
	}
	if got := r.unplaced(id); got != scheduler.UnplacedProgramsNotReady {
		t.Fatalf("before the first heartbeat: unplaced %q", got)
	}
	if f := r.fact(id); f.Unplaced != scheduler.UnplacedProgramsNotReady || f.Placeable {
		t.Fatalf("facts %+v", f)
	}
	r.p.Heartbeat(id, hbBody("s1", "", false, "true"))
	if got := r.unplaced(id); got != scheduler.UnplacedProgramsNotReady {
		t.Fatalf("programs_ready false: unplaced %q", got)
	}
	drain(r.p.Changed())
	r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	if got := r.unplaced(id); got != "" {
		t.Fatalf("ready: unplaced %q", got)
	}
	if !drain(r.p.Changed()) {
		t.Fatal("the scheduler is not woken when a node becomes placeable")
	}
	if f := r.fact(id); f.Unplaced != "" || !f.Placeable {
		t.Fatalf("facts %+v", f)
	}

	// The model source matters only for a launch that reads model files
	// itself (worker_model_dir, contract 6.2 and 8).
	r.p.Heartbeat(id, hbBody("s1", "", true, "false"))
	if got := r.unplaced(id); got != "" {
		t.Fatalf("unreachable source without worker_model_dir: unplaced %q", got)
	}
	r.cfg.Defaults.WorkerModelDir = true
	r.p.Refresh()
	if got := r.unplaced(id); got != scheduler.UnplacedModelSourceUnreachable {
		t.Fatalf("unreachable source with worker_model_dir: unplaced %q", got)
	}
	r.p.Heartbeat(id, hbBody("s1", "", true, "")) // not probed yet
	if got := r.unplaced(id); got != scheduler.UnplacedModelSourceUnreachable {
		t.Fatalf("no probe result with worker_model_dir: unplaced %q", got)
	}
	r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	if got := r.unplaced(id); got != "" {
		t.Fatalf("reachable: unplaced %q", got)
	}
}

// A heartbeat that names a launch this run does not hold (the node was lost
// and its load given up, then the node came back): the agent is told to stop
// it, once, and the node takes no load until the agent reports no launch.
func TestOrphanLaunchInAHeartbeatIsStoppedOnce(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	if res := r.p.Launch(context.Background(), spec(9, a), func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	lid := a.LaunchState().LaunchID
	a.SetHeartbeats(false)
	time.Sleep(100 * time.Millisecond) // a heartbeat on its way
	r.clk.advance(11 * time.Second)
	r.p.CheckLost()
	r.p.Stop(9, nil) // the lost node is not waited for; its launch stays
	r.p.WaitStops(10 * time.Second)
	if a.Has("DELETE") || a.LaunchState().State != agentproto.StateReady || len(r.p.HeldLaunches()) != 0 {
		t.Fatalf("setup: calls %v state %+v held %v", a.Calls(), a.LaunchState(), r.p.HeldLaunches())
	}

	// The node is back, still running the launch.
	if res := r.p.Heartbeat(a.ID, hbBody(a.StartID, lid, true, "true")); res.Status != 200 {
		t.Fatalf("%+v", res)
	}
	if n, _ := r.node(a.ID); !n.Online {
		t.Fatal("not back online")
	}
	if got := r.unplaced(a.ID); got != scheduler.UnplacedOrphanLaunch {
		t.Fatalf("unplaced %q", got)
	}
	r.p.WaitStops(10 * time.Second)
	if st := a.LaunchState(); st.State != agentproto.StateExited || st.Exit.Reason != agentproto.ExitStopped {
		t.Fatalf("the orphan launch was not stopped: %+v, calls %v", st, a.Calls())
	}
	// The agent's next heartbeat may still name it: no second stop, still not placeable.
	r.p.Heartbeat(a.ID, hbBody(a.StartID, lid, true, "true"))
	r.p.WaitStops(10 * time.Second)
	n := 0
	for _, c := range a.Calls() {
		if strings.HasPrefix(c, "DELETE /launch/"+lid) {
			n++
			if strings.Contains(c, "force") {
				t.Fatalf("forced: %v", a.Calls())
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d stops sent, want 1: %v", n, a.Calls())
	}
	if got := r.unplaced(a.ID); got != scheduler.UnplacedOrphanLaunch {
		t.Fatalf("still reporting the launch: unplaced %q", got)
	}
	if !strings.Contains(r.logs.String(), "launch Potluck does not hold") {
		t.Fatalf("no log line:\n%s", r.logs.String())
	}
	// No launch reported: placeable again.
	r.p.Heartbeat(a.ID, hbBody(a.StartID, "", true, "true"))
	if got := r.unplaced(a.ID); got != "" {
		t.Fatalf("unplaced %q after the agent reported no launch", got)
	}
	// A launch this run holds is never taken for an orphan.
	a.SetHeartbeats(true)
	if res := r.p.Launch(context.Background(), spec(10, a), func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	eventually(t, 5*time.Second, func() bool { f := r.fact(a.ID); return f.Role == agentproto.RoleWorker }, "heartbeat names the held launch")
	if got := r.unplaced(a.ID); got != "" {
		t.Fatalf("held launch: unplaced %q", got)
	}
	r.p.Stop(10, nil)
	r.p.WaitStops(10 * time.Second)
}

func (r *rig) launchReads(a interface{ Calls() []string }) int {
	n := 0
	for _, c := range a.Calls() {
		if c == "GET /launch" {
			n++
		}
	}
	return n
}

// Contract 6.3: the launch state is read four times a second while a load is
// in progress on the node, and once a second during the rest of the launch. A
// worker is ready before its load starts, so ready must not slow the reads.
func TestLaunchStateIsReadFourTimesASecondDuringTheLoad(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	if res := r.p.Launch(context.Background(), spec(11, a), func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	time.Sleep(300 * time.Millisecond) // the supervisor has seen ready
	before := r.launchReads(a)
	time.Sleep(1500 * time.Millisecond)
	if got := r.launchReads(a) - before; got < 4 {
		t.Fatalf("%d reads in 1.5 s while the load is in progress, want about 6 (4 Hz)", got)
	}
	r.p.LoadDone(11)
	time.Sleep(400 * time.Millisecond) // the supervisor's next read slows it down
	before = r.launchReads(a)
	time.Sleep(1500 * time.Millisecond)
	if got := r.launchReads(a) - before; got > 3 {
		t.Fatalf("%d reads in 1.5 s after the load, want 1 or 2 (1 Hz)", got)
	}
	r.p.Stop(11, nil)
	r.p.WaitStops(10 * time.Second)
}

// A tier for agent nodes matches on the platform Potluck gave the node and on
// a device name it reports, so it does not depend on the hostname.
func TestAgentNodeMatchesItsTierOnPlatformAndDevice(t *testing.T) {
	r := newRig(t)
	r.cfg.Tiers = []config.TierRule{
		{Match: config.Match{Platform: "linux-amd64-cuda"}, Tier: core.Tier{Priority: 2, HeadroomMiB: 256}},
		{Match: config.Match{Platform: "linux-amd64-vulkan", Device: "amd custom gpu*"}, Tier: core.Tier{Priority: 1, HeadroomMiB: 1024, LinkMBs: 100}},
		{Match: config.Match{Device: "Vulkan1"}, Tier: core.Tier{Priority: 7}},
	}
	q := regReq("02:00:00:00:00:74")
	q.Name = "x7f3a9c" // a console gets a new hostname at each flash
	q.GPUs = []agentproto.GPU{{Vendor: "amd", Model: "AMD Custom GPU 0405", Integrated: true}}
	r.p.Register(q)
	if n, ok := r.node(q.ID); !ok || n.Priority != 1 || n.HeadroomMiB != 1024 || n.LinkMBs != 100 {
		t.Fatalf("console: %+v %v", n, ok)
	}
	q = regReq("02:00:00:00:00:75")
	q.Name, q.Backends = "y1", []string{"vulkan", "cuda"}
	q.GPUs = []agentproto.GPU{{Vendor: "nvidia", Model: "RTX 5060"}}
	r.p.Register(q)
	if n, ok := r.node(q.ID); !ok || n.Priority != 2 {
		t.Fatalf("laptop: %+v %v", n, ok)
	}
	// A device of the heartbeat's list matches too, once it is reported.
	q = regReq("02:00:00:00:00:76")
	q.Name, q.GPUs = "z1", []agentproto.GPU{{Vendor: "intel", Model: "Arc"}}
	r.p.Register(q)
	if got := r.unplaced(q.ID); got != scheduler.UnplacedNoTierMatch {
		t.Fatalf("no matching device yet: unplaced %q", got)
	}
	r.p.Heartbeat(q.ID, []byte(strings.Replace(string(hbBody("s1", "", true, "true")), "Vulkan0", "Vulkan1", 1)))
	if n, _ := r.node(q.ID); n.Priority != 7 || r.unplaced(q.ID) != "" {
		t.Fatalf("heartbeat device: %+v unplaced %q", n, r.unplaced(q.ID))
	}
}

// An agent node that matches no tier is never silent: it is in the node list
// with the reason no_tier_match, takes no load, and one log line is written
// per registration (not per heartbeat).
func TestNoTierMatchIsListedAndLoggedOncePerRegistration(t *testing.T) {
	r := newRig(t)
	r.cfg.Tiers = []config.TierRule{{Match: config.Match{Hostname: "console-*"}, Tier: core.Tier{Priority: 1}}}
	const id = "02:00:00:00:00:77"
	const line = "agent node matches no tier"
	r.p.Register(regReq(id))
	n, ok := r.node(id)
	if !ok || !n.Online || n.Priority != 0 {
		t.Fatalf("the node is not listed: %+v %v", n, ok)
	}
	if got := r.unplaced(id); got != scheduler.UnplacedNoTierMatch {
		t.Fatalf("unplaced %q", got)
	}
	for i := 0; i < 3; i++ {
		r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	}
	if f := r.fact(id); f.Unplaced != scheduler.UnplacedNoTierMatch || f.Placeable || f.State != StateActive {
		t.Fatalf("facts %+v", f)
	}
	if got := strings.Count(r.logs.String(), line); got != 1 {
		t.Fatalf("%d log lines after one registration and three heartbeats, want 1:\n%s", got, r.logs.String())
	}
	if !strings.Contains(r.logs.String(), "linux-amd64-vulkan") {
		t.Fatalf("the log line does not say what the node reported:\n%s", r.logs.String())
	}
	r.p.Register(regReq(id))
	if got := strings.Count(r.logs.String(), line); got != 2 {
		t.Fatalf("%d log lines after a second registration, want 2", got)
	}
	// A tier added for it takes it into the pool.
	r.cfg.Tiers = append(r.cfg.Tiers, config.TierRule{Match: config.Match{Platform: "linux-amd64-vulkan"}, Tier: core.Tier{Priority: 3}})
	r.p.Refresh()
	r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	if n, _ := r.node(id); n.Priority != 3 || r.unplaced(id) != "" {
		t.Fatalf("after the tier was added: %+v unplaced %q", n, r.unplaced(id))
	}
}
