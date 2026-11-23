package scheduler

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
)

func (h *harness) runHostStart(ef effStart) event {
	h.hosts = append(h.hosts, *ef.host)
	if err := h.hostFail; err != nil {
		h.hostFail = nil
		return evChildStarted{iid: ef.iid, err: err}
	}
	addr := ""
	for _, n := range h.view.Nodes() {
		if n.ID == ef.host.Node {
			addr = strings.TrimSuffix(n.Addr, ":50052") + ":50061"
		}
	}
	ch := &fakeChild{addr: addr, key: ef.host.APIKey, port: 50061, argv: []string{"llama-server", "--port", "50061"},
		exitCh: make(chan instance.ExitStatus, 1), healthyCh: make(chan struct{}), gone: make(chan struct{}),
		tail: []string{"loading model"}}
	h.children[ef.iid] = ch
	return evChildStarted{iid: ef.iid, child: ch}
}

func (h *harness) lastHost() HostLaunchSpec {
	h.t.Helper()
	if len(h.hosts) == 0 {
		h.t.Fatalf("no host launch")
	}
	return h.hosts[len(h.hosts)-1]
}

// workersUp reports the worker launch on b ready.
func (h *harness) workersUp() {
	h.t.Helper()
	h.step(evWorkersLaunched{iid: h.lastLaunch().spec.IID, res: WorkerResult{LaunchIDs: map[core.NodeID]string{"b": "l-b"}}})
}

// hostReady loads m and returns its released lease.
func (h *harness) hostReady() *grant {
	h.t.Helper()
	w := h.request("m")
	h.workersUp()
	h.healthy("m")
	g := h.granted(w)
	h.release(g)
	return g
}

// hostHarness: agents membership, two consoles, a profile hosted on the
// first and split over both.
func hostHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL, p.Host = []string{"a", "b"}, []int{1, 1}, ptrDur(0), core.HostNode
	})
	return h
}

func TestHost_TheFirstForcedNodeHostsAndIsNotAWorker(t *testing.T) {
	h := hostHarness(t)
	w := h.request("m")
	l := h.lastLaunch()
	if len(l.spec.Workers) != 1 || l.spec.Workers[0].Node != "b" {
		t.Fatalf("only b is a worker launch: %+v", l.spec.Workers)
	}
	if len(h.hosts) != 0 {
		t.Fatal("the host starts after the workers are ready")
	}
	h.workersUp()
	hs := h.lastHost()
	if hs.Node != "a" || !slices.Equal(hs.Workers, []string{"192.0.2.4:50052"}) || len(hs.TensorSplit) != 2 {
		t.Fatalf("%+v", hs)
	}
	if hs.RPCFileLoad != "0" || len(hs.APIKey) < 32 || hs.Model != "m.gguf" || hs.Ctx != 4096 || hs.Parallel != 1 {
		t.Fatalf("%+v", hs)
	}
	if slices.Contains(hs.ExtraArgs, "--no-mmap") || !slices.Contains(hs.ExtraArgs, "--cache-ram") {
		t.Fatalf("no-mmap is off by default; no prompt cache in RAM: %v", hs.ExtraArgs)
	}
	// The host node's memory figures carry the host process.
	if hs.ExpectMiB <= 0 || hs.MinMemAvailableMiB <= int64(h.cfg.Defaults.WatchdogFloorMiB)+int64(h.cfg.Defaults.HostOverheadMiB) {
		t.Fatalf("%+v", hs)
	}
	h.healthy("m")
	g := h.granted(w)
	if g.Addr != "192.0.2.2:50061" || g.APIKey != hs.APIKey || g.Gone == nil {
		t.Fatalf("%+v", g)
	}
	if a, ok := h.c.upstream("m"); !ok || a != "192.0.2.2:50061" {
		t.Fatal(a, ok)
	}
	h.release(g)
	if err := h.c.unload("m", false); err != nil {
		t.Fatal(err)
	}
	h.step(evTick{})
	h.wantState("m", "absent")
	select {
	case <-g.Gone:
	default:
		t.Fatal("the lease's Gone is closed when the instance goes")
	}
	// Each launch gets a key of its own.
	if a, b := newAPIKey(), newAPIKey(); a == b || len(a) != 32 {
		t.Fatal(a, b)
	}
}

func TestHost_NoMmapSetting(t *testing.T) {
	h := hostHarness(t)
	h.cfg.Defaults.HostNoMmap = true
	h.request("m")
	h.workersUp()
	if !slices.Contains(h.lastHost().ExtraArgs, "--no-mmap") {
		t.Fatal(h.lastHost().ExtraArgs)
	}
}

func TestHost_WorkerModelDirMeansFileLoadRequired(t *testing.T) {
	h := hostHarness(t)
	h.cfg.Defaults.WorkerModelDir = true
	h.request("m")
	h.workersUp()
	if h.lastHost().RPCFileLoad != "required" {
		t.Fatal(h.lastHost().RPCFileLoad)
	}
}

func TestHost_SingleNodeHasNoWorkers(t *testing.T) {
	h := hostHarness(t)
	h.cfg.Profiles["m"].ForceNodes, h.cfg.Profiles["m"].TensorSplit = []string{"a"}, nil
	w := h.request("m")
	if l := h.lastLaunch(); len(l.spec.Workers) != 0 {
		t.Fatalf("%+v", l.spec)
	}
	h.step(evWorkersLaunched{iid: h.lastLaunch().spec.IID})
	hs := h.lastHost()
	if len(hs.Workers) != 0 || len(hs.TensorSplit) != 0 || hs.Node != "a" {
		t.Fatalf("%+v", hs)
	}
	h.healthy("m")
	h.granted(w)
}

func TestHost_TheSchedulersMachineCannotBeADevice(t *testing.T) {
	h := hostHarness(t)
	h.nodes(ps5("a", 2, 15000), hostNode(7000))
	h.cfg.Profiles["m"].ForceNodes = []string{"a", "host-1"}
	w := h.request("m")
	e := h.failed(w, 502, ReasonLoadFailed)
	if !strings.Contains(e.Message, "scheduler's machine") {
		t.Fatal(e.Message)
	}
	if len(h.wlaunches) != 0 || len(h.hosts) != 0 {
		t.Fatal("nothing is launched")
	}
}

type hostGateView struct {
	*fakeView
	no map[core.NodeID]string
}

func (v hostGateView) CanHost(id core.NodeID) (bool, string) {
	if why, ok := v.no[id]; ok {
		return false, why
	}
	return true, ""
}

// An agent that does not advertise host_launch (0.1.6, or a push node) is
// never asked to host: the load fails before anything is launched.
func TestHost_ANodeThatCannotHostIsNeverAsked(t *testing.T) {
	h := hostHarness(t)
	h.c.view = hostGateView{fakeView: h.view, no: map[core.NodeID]string{"a": "its agent does not advertise host_launch"}}
	w := h.request("m")
	e := h.failed(w, 502, ReasonLoadFailed)
	if !strings.Contains(e.Message, ReasonHostNotCapable) || !strings.Contains(e.Message, "host_launch") {
		t.Fatal(e.Message)
	}
	if len(h.wlaunches) != 0 || len(h.hosts) != 0 {
		t.Fatal("nothing is launched")
	}
}

// The agent refuses the host launch: the load fails with its reason and the
// worker launches are stopped.
func TestHost_RefusedLaunchFailsTheLoad(t *testing.T) {
	h := hostHarness(t)
	w := h.request("m")
	h.hostFail = errors.New("agent refused the host launch: low_memory: MemAvailable 900 MiB")
	h.workersUp()
	e := h.failed(w, 502, ReasonLoadFailed)
	if !strings.Contains(e.Message, "low_memory") {
		t.Fatal(e.Message)
	}
	h.step(evTick{})
	h.wantState("m", "absent")
	if len(h.wstops) != 1 {
		t.Fatalf("worker launches stopped: %+v", h.wstops)
	}
}

// The host process dies (a watchdog kill): the instance goes, the workers
// are stopped, and the learned need is raised as for a worker's kill.
func TestHost_HostProcessDies(t *testing.T) {
	h := hostHarness(t)
	h.hostReady()
	in := h.inst("m")
	before, _ := h.c.need(in.prof)
	h.exitChild(in.iid, instance.ExitStatus{Code: -1, Signal: "KILL", Reason: string(agentproto.ExitWatchdog) + ": MemAvailable below the floor"})
	h.step(evTick{})
	h.wantState("m", "absent")
	if len(h.wstops) != 1 {
		t.Fatalf("worker launches stopped: %+v", h.wstops)
	}
	after, _ := h.c.need(h.cfg.Profiles["m"])
	if after.Need.MiB <= before.Need.MiB || after.Source != NeedLearned {
		t.Fatalf("need %d -> %d (%v)", before.Need.MiB, after.Need.MiB, after.Source)
	}
}

// A crash is not a memory kill: no raise.
func TestHost_CrashDoesNotRaiseTheNeed(t *testing.T) {
	h := hostHarness(t)
	h.hostReady()
	in := h.inst("m")
	before, _ := h.c.need(in.prof)
	h.exitChild(in.iid, instance.ExitStatus{Code: 139, Reason: string(agentproto.ExitCrashed) + ": segfault"})
	h.step(evTick{})
	h.wantState("m", "absent")
	after, _ := h.c.need(h.cfg.Profiles["m"])
	if after.Need.MiB != before.Need.MiB {
		t.Fatalf("need %d -> %d", before.Need.MiB, after.Need.MiB)
	}
}

// The host node goes offline: the instance ends at once.
func TestHost_HostNodeLost(t *testing.T) {
	h := hostHarness(t)
	g := h.hostReady()
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.step(evTick{})
	h.wantState("m", "absent")
	select {
	case <-g.Gone:
	default:
		t.Fatal("leases end with the instance")
	}
}

// Stop and forced stop reach the host child (the pool turns them into a
// DELETE or a forced DELETE of the host launch).
func TestHost_StopAndForcedStop(t *testing.T) {
	h := hostHarness(t)
	h.autoExit = false
	h.hostReady()
	ch := h.child("m")
	iid := h.inst("m").iid
	if err := h.c.unload("m", false); err != nil {
		t.Fatal(err)
	}
	h.step(evTick{})
	if err := h.c.unload("m", true); err != nil {
		t.Fatal(err)
	}
	h.step(evTick{})
	modes := ch.stopModes()
	if len(modes) == 0 || modes[0] != instance.StopGraceful || modes[len(modes)-1] != instance.StopKill {
		t.Fatalf("%v", modes)
	}
	h.exitChild(iid, instance.ExitStatus{Code: -1, Signal: "KILL", Requested: true})
	h.step(evTick{})
	h.wantState("m", "absent")
}

// Measurements of a host on a node are a shape of their own.
func TestHost_NeedShapeIsSeparate(t *testing.T) {
	p := &core.Profile{Name: "m", Kind: core.KindLlama, Model: "m.gguf", Ctx: 4096, Parallel: 1}
	q := *p
	q.Host = core.HostNode
	ks, kn := needKeyOf(p), needKeyOf(&q)
	if ks == kn || ks.shape() == kn.shape() {
		t.Fatal("same shape")
	}
	// The server shape is the one stored before 0.1.9.
	if ks.shape() != needstore.ShapeKey("llama", "m.gguf", 4096, 1, "", false, nil) {
		t.Fatal(ks.shape())
	}
	r := needstore.Record{Kind: "llama", Model: "m.gguf", Ctx: 4096, Parallel: 1, Host: core.HostNode}
	if needKeyOfRecord(r) != kn || r.Shape() != kn.shape() {
		t.Fatal("record round trip")
	}
}

// The reservation on the host node carries host_overhead_mib.
func TestHost_ReservationCarriesTheHostProcess(t *testing.T) {
	h := hostHarness(t)
	p := h.cfg.Profiles["m"]
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.2:50052"}, {ID: "b", Addr: "192.0.2.4:50052"}}}
	withHost := h.c.expectShares(p, pl)
	q := *p
	q.Host = ""
	without := h.c.expectShares(&q, pl)
	if withHost == nil || withHost[0]-without[0] != h.cfg.Defaults.HostOverheadMiB || withHost[1] != without[1] {
		t.Fatalf("%v %v", withHost, without)
	}
	rs, _ := h.c.reservedShares(p, pl)
	if rs == nil || rs[0].Host != h.cfg.Defaults.HostOverheadMiB || rs[1].Host != 0 {
		t.Fatalf("%+v", rs)
	}
}

// The host's own buffers (Vulkan0) count on the host node, the workers' by
// their endpoint, the host's CPU-side buffers on none.
func TestHost_LoadByNodeNamesTheHostNode(t *testing.T) {
	h := hostHarness(t)
	in := &inst{prof: h.cfg.Profiles["m"], placement: core.Placement{Host: "a",
		Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.2:50052"}, {ID: "b", Addr: "192.0.2.4:50052"}}}}
	ll := instance.NewLoadLog()
	for _, line := range []string{
		"load_tensors:      Vulkan0 model buffer size =  2000.00 MiB",
		"load_tensors: RPC0[192.0.2.4:50052] model buffer size =  1000.00 MiB",
		"llama_kv_cache:    Vulkan0 KV buffer size =   256.00 MiB",
		"llama_context:  Vulkan_Host compute buffer size =  40.00 MiB",
	} {
		ll.Feed(line)
	}
	mib, seen := h.c.loadByNode(in, ll.Snapshot())
	if !seen[0] || !seen[1] || mib[0] != 2256 || mib[1] != 1000 {
		t.Fatal(mib, seen)
	}
}

// POST /api/plan names the host node and its overhead, on the plan and on
// the host node's memory row.
func TestHost_PlanShowsTheHostAndItsOverhead(t *testing.T) {
	h := hostHarness(t)
	h.viewChanged()
	r := h.c.dryPlan("m")
	if r.Host != core.HostNode || r.HostNode != "a" || r.HostOverheadMiB != h.cfg.Defaults.HostOverheadMiB || r.HostProblem != "" {
		t.Fatalf("%+v", r)
	}
	if r.Memory == nil || len(r.Memory.Nodes) != 2 || r.Memory.Nodes[0].HostOverheadMiB != h.cfg.Defaults.HostOverheadMiB ||
		r.Memory.Nodes[1].HostOverheadMiB != 0 {
		t.Fatalf("%+v %+v", r, r.Memory)
	}
	// A node that cannot host says so in the plan.
	h.c.view = hostGateView{fakeView: h.view, no: map[core.NodeID]string{"a": "its agent does not advertise host_launch"}}
	if r := h.c.dryPlan("m"); !strings.Contains(r.HostProblem, ReasonHostNotCapable) {
		t.Fatalf("%+v", r)
	}
	// A server-hosted profile: host server, no overhead.
	h.llama("s", 4000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
	if r := h.c.dryPlan("s"); r.Host != core.HostServer || r.HostNode != "" || r.HostOverheadMiB != 0 {
		t.Fatalf("%+v", r)
	}
}

// A node-hosted load has no byte counter (the host process is not
// Potluck's child), so the stall rule never applies to it: the load timeout
// alone ends a stuck load. That timeout is the design 7.6 formula with the
// host node's own share counted over its link, since the host reads its
// part from the share: 120 s + 2 x (file x fraction / link) per node.
func TestHost_LoadTimeoutCountsTheHostsShare(t *testing.T) {
	h := hostHarness(t)
	h.request("m")
	h.workersUp()
	in := h.c.insts["m"]
	if in == nil {
		t.Fatal("no instance")
	}
	want := instance.LoadTimeoutFor(1<<30, []instance.NodeShare{{Fraction: 0.5, LinkMBs: 11}, {Fraction: 0.5, LinkMBs: 11}})
	if in.loadTimeout != want {
		t.Fatalf("load timeout %v, want %v (both nodes' shares)", in.loadTimeout, want)
	}
	// The real host child counts no bytes; the harness fake does, so hide it.
	if _, ok := any(&instance.RemoteChild{}).(instance.ByteCounter); ok {
		t.Fatal("RemoteChild became a byte counter: the stall rule now applies, update the docs")
	}
	in.child = struct{ instance.Child }{in.child}
	if h.c.wantsSample(in) {
		t.Fatal("a node-hosted load is never sampled, so the stall rule cannot fire")
	}
	if _, stalled := h.c.loadStalled(in); stalled {
		t.Fatal("stalled without a measurement")
	}
}
