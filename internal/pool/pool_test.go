package pool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// These tests are agent contract 13 item 2 at the pool level: the pool
// against fake agents (internal/testutil/fakeagent, built on agentproto, not
// the real agent).

type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.off) }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

type memStore struct {
	mu sync.Mutex
	m  map[string]cfgstore.NodeAdmin
}

func (s *memStore) NodeAdmins() (map[string]cfgstore.NodeAdmin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]cfgstore.NodeAdmin{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}

func (s *memStore) SetNodeAdmin(id string, a cfgstore.NodeAdmin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a == (cfgstore.NodeAdmin{}) {
		delete(s.m, id)
	} else {
		s.m[id] = a
	}
	return nil
}

type hist struct {
	mu sync.Mutex
	ev []history.Event
}

func (h *hist) Record(e history.Event) { h.mu.Lock(); h.ev = append(h.ev, e); h.mu.Unlock() }

const llamaBuild = "a01665d30de2885879a64eae999067ef3ef5fd4b"

type rig struct {
	t *testing.T
	// p is the pool, for the test's own goroutine. serve runs on HTTP
	// goroutines and reads cur, which start swaps (a Potluck restart).
	p          *Pool
	cur        atomic.Pointer[Pool]
	clk        *clock
	store      *memStore
	hist       *hist
	srv        *httptest.Server
	agentPort  int
	workerPort int
	cfg        *config.Config
	rel        *release.Release

	mu     sync.Mutex
	forced []core.NodeID
	logs   logBuf

	// cbErr, when set, is what the call-back check of every agent answers;
	// cbCalls counts the checks. Unset, an agent at 192.0.2.x (a registration
	// made by the test itself, with nothing listening) counts as reached and
	// any other agent is really called.
	cbErr   atomic.Pointer[error]
	cbCalls atomic.Int64
}

func (r *rig) failCallBack(msg string) {
	if msg == "" {
		r.cbErr.Store(nil)
		return
	}
	err := errors.New(msg)
	r.cbErr.Store(&err)
}

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func testRelease(t *testing.T) *release.Release {
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0", Llama: llamaBuild,
		Builds: map[string]string{"node_exporter": "1.13.1"},
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("A")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("R"), "node_exporter": []byte("N")},
			"linux-amd64-cuda":   {"llama-server": []byte("LC"), "rpc-server": []byte("RC")},
		}})
	rel, err := release.Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, clk: &clock{}, store: &memStore{m: map[string]cfgstore.NodeAdmin{}}, hist: &hist{},
		agentPort: freePort(t), workerPort: freePort(t), rel: testRelease(t)}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	r.cfg = &config.Config{Defaults: d, Fleet: config.Fleet{LocalNode: "host-1"},
		Tiers: []config.TierRule{{Match: config.Match{Hostname: "*"}, Tier: core.Tier{Priority: 1, HeadroomMiB: 500, LinkMBs: 11}}}}
	r.start()
	t.Cleanup(func() { r.srv.Close() })
	return r
}

// start (re)creates the pool on the same store, as a Potluck restart does.
func (r *rig) start() {
	p, err := New(Options{Config: func() *config.Config { return r.cfg }, Programs: Programs{Release: r.rel},
		PotluckRelease: "0.1.0", Store: r.store, History: r.hist, Clock: r.clk.now,
		AgentPort: r.agentPort, WorkerPort: r.workerPort, LeaseEvery: 100 * time.Millisecond, LogDir: r.t.TempDir(),
		CallBackEvery: 20 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(&r.logs, nil))})
	if err != nil {
		r.t.Fatal(err)
	}
	p.inlineCallBack = true
	real := p.callBack
	p.callBack = func(ctx context.Context, base string) error {
		r.cbCalls.Add(1)
		if e := r.cbErr.Load(); e != nil {
			return *e
		}
		if strings.HasPrefix(base, "http://192.0.2.") {
			return nil
		}
		return real(ctx, base)
	}
	p.SetForceUnload(func(_ context.Context, id core.NodeID) (string, error) {
		r.mu.Lock()
		r.forced = append(r.forced, id)
		r.mu.Unlock()
		return "", nil
	})
	r.p = p
	r.cur.Store(p)
	if r.srv == nil {
		r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	}
}

// serve stands in for the API's agent endpoints (internal/api/nodes.go).
func (r *rig) serve(w http.ResponseWriter, req *http.Request) {
	p := r.cur.Load()
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/api/pool/nodes/"), "/")
	switch {
	case req.URL.Path == agentproto.PathRegister:
		var rr agentproto.RegisterRequest
		json.NewDecoder(req.Body).Decode(&rr)
		json.NewEncoder(w).Encode(p.Register(rr))
	case len(parts) == 2 && parts[1] == "heartbeat":
		b, _ := io.ReadAll(req.Body)
		res := p.Heartbeat(parts[0], b)
		w.WriteHeader(res.Status)
		if res.Status == 200 {
			json.NewEncoder(w).Encode(res.Answer)
		}
	case len(parts) == 2 && parts[1] == "deregister":
		p.Deregister(req.Context(), parts[0])
	default:
		w.WriteHeader(404)
	}
}

func (r *rig) agent(n int, mod ...func(*fakeagent.Agent)) *fakeagent.Agent {
	a := &fakeagent.Agent{ID: "02:00:00:00:00:0" + strconv.Itoa(n), Name: "node-" + strconv.Itoa(n),
		Addr: "127.0.0." + strconv.Itoa(n+1), AgentPort: r.agentPort, WorkerPort: r.workerPort, Potluck: r.srv.URL,
		GPUs: []agentproto.GPU{{Vendor: "amd", Integrated: true}}, MemAvail: 14000, HeartbeatEvery: 50 * time.Millisecond,
		Devices: []agentproto.Device{{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14336, FreeMiB: 14000, SharesSystemMemory: true}}}
	for _, m := range mod {
		m(a)
	}
	if err := a.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(a.Close)
	return a
}

func (r *rig) node(id string) (core.Node, bool) {
	for _, n := range r.p.Nodes() {
		if string(n.ID) == id {
			return n, true
		}
	}
	return core.Node{}, false
}

func (r *rig) waitNode(id string) core.Node {
	r.t.Helper()
	var n core.Node
	eventually(r.t, 5*time.Second, func() bool {
		var ok bool
		n, ok = r.node(id)
		return ok && !n.FreeUnknown
	}, "node "+id+" in the view")
	return n
}

func regReq(id string) agentproto.RegisterRequest {
	r := agentproto.RegisterRequest{ID: id, Name: "n", AgentRelease: "0.1.0", AgentStartID: "s1", OS: "linux", Arch: "amd64",
		Install: "bare", Addr: "192.0.2.10", Backends: []string{"vulkan"}, GPUs: []agentproto.GPU{{Vendor: "amd"}}}
	r.FillEmpty()
	return r
}

func TestRegisterRefusals(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		name   string
		mod    func(*agentproto.RegisterRequest)
		reason string
	}{
		{"agent too old", func(q *agentproto.RegisterRequest) { q.AgentRelease = "0.0.9" }, agentproto.ReasonAgentTooOld},
		{"potluck too old", func(q *agentproto.RegisterRequest) { q.AgentRelease = "0.10.0" }, agentproto.ReasonPotluckTooOld},
		{"unsupported", func(q *agentproto.RegisterRequest) { q.Arch = "arm64" }, agentproto.ReasonUnsupportedHardware},
		{"no backend", func(q *agentproto.RegisterRequest) { q.Backends = []string{} }, agentproto.ReasonUnsupportedHardware},
		{"foreign", func(q *agentproto.RegisterRequest) {
			q.ForeignProcesses = []agentproto.ForeignProcess{{PID: 7, Exe: "/usr/bin/rpc-server"}}
		}, agentproto.ReasonForeignProcess},
	}
	for _, c := range cases {
		q := regReq("02:00:00:00:00:99")
		c.mod(&q)
		a := r.p.Register(q)
		if a.Accepted || a.Reason != c.reason || a.Message == "" {
			t.Errorf("%s: %+v", c.name, a)
		}
		if c.reason == agentproto.ReasonAgentTooOld && a.RequiredRelease != "0.1.0" {
			t.Errorf("required_release %+v", a)
		}
	}
	// duplicate_id: another live agent with a different addr holds the id.
	if a := r.p.Register(regReq("02:00:00:00:00:50")); !a.Accepted {
		t.Fatalf("first: %+v", a)
	}
	q := regReq("02:00:00:00:00:50")
	q.Addr = "192.0.2.11"
	if a := r.p.Register(q); a.Accepted || a.Reason != agentproto.ReasonDuplicateID {
		t.Fatalf("duplicate: %+v", a)
	}
	// The same addr registers again (agent restart): accepted.
	if a := r.p.Register(regReq("02:00:00:00:00:50")); !a.Accepted {
		t.Fatalf("again: %+v", a)
	}
	// A lost agent's id may be taken from another addr.
	r.clk.advance(11 * time.Second)
	r.p.CheckLost()
	if a := r.p.Register(q); !a.Accepted {
		t.Fatalf("after lost: %+v", a)
	}
	// No release served.
	r.rel = nil
	r.start()
	r.p.o.Programs.Err = "release 0.1.0: open release.json: no such file"
	if a := r.p.Register(regReq("02:00:00:00:00:51")); a.Accepted || a.Reason != ReasonReleaseUnavailable || !strings.Contains(a.Message, "release.json") {
		t.Fatalf("no release: %+v", a)
	}
}

func TestRegisterAnswerAndPlatform(t *testing.T) {
	r := newRig(t)
	q := regReq("02:00:00:00:00:60")
	q.Backends = []string{"vulkan", "cuda"}
	q.GPUs = []agentproto.GPU{{Vendor: "nvidia", Model: "RTX 5060"}}
	a := r.p.Register(q)
	if !a.Accepted || a.Platform != "linux-amd64-cuda" || a.Release != "0.1.0" || a.HeartbeatEveryS != 2 || len(a.Programs) != 2 || a.StopLaunch != nil {
		t.Fatalf("%+v", a)
	}
	q = regReq("02:00:00:00:00:61")
	a = r.p.Register(q)
	if a.Platform != "linux-amd64-vulkan" || len(a.Exporters) != 1 || a.Exporters[0].Name != "node_exporter" {
		t.Fatalf("%+v", a)
	}
}

func TestHeartbeatAndPlacementMemory(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq("02:00:00:00:00:70"))
	if res := r.p.Heartbeat("02:00:00:00:00:71", []byte(`{}`)); res.Status != http.StatusGone {
		t.Fatalf("unknown agent: %+v", res)
	}
	if res := r.p.Heartbeat("02:00:00:00:00:70", []byte(`{"agent_start_id":"other"}`)); res.Status != http.StatusGone {
		t.Fatalf("new start id: %+v", res)
	}
	hb := `{"agent_start_id":"s1","seq":1,"uptime_s":3,"role":"idle","launch_id":null,"programs_ready":true,"programs_error":null,
		"model_source":{"kind":"share","reachable":true},"mem_available_mib":9000,
		"devices":[{"name":"Vulkan0","backend":"vulkan","total_mib":14336,"free_mib":14000,"shares_system_memory":true,"read_age_s":1}],
		"stats":{"stats_version":99,"cpu":{}}}`
	res := r.p.Heartbeat("02:00:00:00:00:70", []byte(hb))
	if res.Status != 200 || res.Answer.Release != "0.1.0" || res.Answer.Paused {
		t.Fatalf("%+v", res)
	}
	n, _ := r.node("02:00:00:00:00:70")
	// usable = min(device free 14000, mem_available 9000 - floor 1000)
	if n.FreeMiB != 8000 || n.TotalMiB != 14336 || !n.SharedMemory || n.FreeUnknown || n.Addr != "192.0.2.10:"+strconv.Itoa(r.workerPort) {
		t.Fatalf("node %+v", n)
	}
	f := r.p.Facts()[0]
	if f.Stats != nil || f.StatsNote != "stats version not supported" || !f.ProgramsReady {
		t.Fatalf("facts %+v", f)
	}
	// A bad stats object never rejects the heartbeat; a good one is kept raw.
	if res := r.p.Heartbeat("02:00:00:00:00:70", []byte(strings.Replace(hb, `{"stats_version":99,"cpu":{}}`, `[1,2`, 1))); res.Status == 200 {
		t.Fatal("broken JSON accepted") // the whole body is broken JSON here
	}
	if res := r.p.Heartbeat("02:00:00:00:00:70", []byte(strings.Replace(hb, `{"stats_version":99,"cpu":{}}`, `"x"`, 1))); res.Status != 200 {
		t.Fatalf("stats of the wrong type rejected the heartbeat: %+v", res)
	}
	if f := r.p.Facts()[0]; f.Stats != nil || f.StatsNote == "" {
		t.Fatalf("facts %+v", f)
	}
	r.p.Heartbeat("02:00:00:00:00:70", []byte(strings.Replace(hb, `"stats_version":99`, `"stats_version":1`, 1)))
	if f := r.p.Facts()[0]; string(f.Stats) != `{"stats_version":1,"cpu":{}}` || f.StatsNote != "" {
		t.Fatalf("facts %+v %s", f, f.Stats)
	}
	// A dGPU device counts its own free memory only.
	r.p.Heartbeat("02:00:00:00:00:70", []byte(strings.Replace(hb, `"shares_system_memory":true`, `"shares_system_memory":false`, 1)))
	if n, _ := r.node("02:00:00:00:00:70"); n.FreeMiB != 14000 || n.SharedMemory {
		t.Fatalf("node %+v", n)
	}
}

func TestLostNodeAndReturn(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	n := r.waitNode(a.ID)
	if !n.Online {
		t.Fatal("offline")
	}
	a.SetHeartbeats(false)
	r.clk.advance(9 * time.Second)
	r.p.CheckLost()
	if n, _ := r.node(a.ID); !n.Online {
		t.Fatal("lost before 10 s")
	}
	r.clk.advance(2 * time.Second)
	r.p.CheckLost()
	if n, _ := r.node(a.ID); n.Online {
		t.Fatal("not lost after 10 s")
	}
	if r.p.Facts()[0].State != StateLost {
		t.Fatalf("facts %+v", r.p.Facts()[0])
	}
	a.SetHeartbeats(true)
	eventually(t, 5*time.Second, func() bool { n, _ := r.node(a.ID); return n.Online }, "back online")
}

func TestPotluckRestartStopsUnknownLaunch(t *testing.T) {
	r := newRig(t)
	q := regReq("02:00:00:00:00:80")
	id := "ln-old-1"
	q.LaunchID = &id
	a := r.p.Register(q)
	if !a.Accepted || a.StopLaunch == nil || *a.StopLaunch != id {
		t.Fatalf("%+v", a)
	}
}

func spec(iid uint64, as ...*fakeagent.Agent) scheduler.WorkerLaunchSpec {
	s := scheduler.WorkerLaunchSpec{IID: iid, Profile: "m", Label: "m.gguf"}
	for _, a := range as {
		s.Workers = append(s.Workers, scheduler.WorkerTarget{Node: core.NodeID(a.ID), Addr: net.JoinHostPort(a.Addr, strconv.Itoa(a.WorkerPort)), MinMemAvailableMiB: 2000})
	}
	return s
}

type reports struct {
	mu sync.Mutex
	rs []scheduler.WorkerReport
}

func (x *reports) add(r scheduler.WorkerReport) { x.mu.Lock(); x.rs = append(x.rs, r); x.mu.Unlock() }
func (x *reports) exits() []scheduler.WorkerReport {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []scheduler.WorkerReport
	for _, r := range x.rs {
		if r.Exit != nil {
			out = append(out, r)
		}
	}
	return out
}

func TestWorkerLaunchesAroundALoad(t *testing.T) {
	r := newRig(t)
	a1, a2 := r.agent(1), r.agent(2)
	r.waitNode(a1.ID)
	r.waitNode(a2.ID)
	rep := &reports{}
	res := r.p.Launch(context.Background(), spec(1, a1, a2), rep.add)
	if res.Reason != "" || len(res.LaunchIDs) != 2 {
		t.Fatalf("%+v", res)
	}
	for _, a := range []*fakeagent.Agent{a1, a2} {
		if st := a.LaunchState(); st.State != agentproto.StateReady {
			t.Fatalf("%s: %+v", a.ID, st)
		}
	}
	eventually(t, 5*time.Second, func() bool { return a1.Leases() >= 2 && a2.Leases() >= 2 }, "leases renewed")
	if f := r.p.Facts(); f[0].PotluckLaunchID == nil || f[0].LaunchState != agentproto.StateReady {
		t.Fatalf("facts %+v", f[0])
	}
	r.p.Stop(1, nil)
	r.p.WaitStops(10 * time.Second)
	for _, a := range []*fakeagent.Agent{a1, a2} {
		st := a.LaunchState()
		if st.State != agentproto.StateExited || st.Exit.Reason != agentproto.ExitStopped || a.Has("DELETE /launch/"+st.LaunchID+"?force") {
			t.Fatalf("%s: %+v %v", a.ID, st, a.Calls())
		}
	}
	if len(rep.exits()) != 0 {
		t.Fatalf("a requested stop is not reported as an exit: %+v", rep.exits())
	}
	if len(r.p.HeldLaunches()) != 0 {
		t.Fatal("launches still held")
	}
}

func TestWorkerLaunchRefusedCarriesTheAgentsReason(t *testing.T) {
	r := newRig(t)
	a1 := r.agent(1)
	a2 := r.agent(2, func(a *fakeagent.Agent) { a.MemAvail = 1000 })
	r.waitNode(a1.ID)
	r.waitNode(a2.ID)
	res := r.p.Launch(context.Background(), spec(2, a1, a2), func(scheduler.WorkerReport) {})
	if res.Reason != agentproto.ReasonLowMemory || res.Node != core.NodeID(a2.ID) || !strings.Contains(res.Message, "MemAvailable") {
		t.Fatalf("%+v", res)
	}
	r.p.Stop(2, nil)
	r.p.WaitStops(10 * time.Second)
	if st := a1.LaunchState(); st.State != agentproto.StateExited {
		t.Fatalf("the other member still runs: %+v", st)
	}
}

func TestWorkerExitDuringStartAndAfterReady(t *testing.T) {
	r := newRig(t)
	a1 := r.agent(1, func(a *fakeagent.Agent) { a.ReadyAfter = 2 * time.Second })
	r.waitNode(a1.ID)
	go func() {
		eventually(t, 5*time.Second, func() bool { return a1.LaunchState().State == agentproto.StateStarting }, "starting")
		a1.ExitLaunch(agentproto.ExitWatchdog, "MemAvailable fell below 1000 MiB")
	}()
	res := r.p.Launch(context.Background(), spec(3, a1), func(scheduler.WorkerReport) {})
	if res.Reason != scheduler.ReasonWorkerExited || res.Exit == nil || res.Exit.Reason != agentproto.ExitWatchdog {
		t.Fatalf("%+v", res)
	}
	r.p.Stop(3, nil)
	r.p.WaitStops(10 * time.Second)

	a2 := r.agent(2)
	r.waitNode(a2.ID)
	rep := &reports{}
	if res := r.p.Launch(context.Background(), spec(4, a2), rep.add); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	a2.ExitLaunch(agentproto.ExitOOMKill, "")
	eventually(t, 5*time.Second, func() bool { return len(rep.exits()) == 1 }, "exit reported")
	if e := rep.exits()[0]; e.Exit.Reason != agentproto.ExitOOMKill || e.Node != core.NodeID(a2.ID) {
		t.Fatalf("%+v", e)
	}
	r.p.Stop(4, nil)
	r.p.WaitStops(10 * time.Second)
}

func TestForcedStopResetsAfterFiveSeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("waits the contract's 5 s")
	}
	r := newRig(t)
	a1 := r.agent(1, func(a *fakeagent.Agent) { a.IgnoreForce = true })
	r.waitNode(a1.ID)
	if res := r.p.Launch(context.Background(), spec(5, a1), func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	id := a1.LaunchState().LaunchID
	r.p.Stop(5, []core.NodeID{core.NodeID(a1.ID)})
	r.p.WaitStops(20 * time.Second)
	if !a1.Has("DELETE /launch/"+id+"?force=1") || !a1.Has("POST /reset") {
		t.Fatalf("calls %v", a1.Calls())
	}
	if st := a1.LaunchState(); st.State != agentproto.StateExited || st.Exit.Reason != agentproto.ExitReset {
		t.Fatalf("%+v", st)
	}
}

func TestStopSkipsALostNode(t *testing.T) {
	r := newRig(t)
	a1, a2 := r.agent(1), r.agent(2)
	r.waitNode(a1.ID)
	r.waitNode(a2.ID)
	if res := r.p.Launch(context.Background(), spec(6, a1, a2), func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	a1.SetHeartbeats(false)
	r.clk.advance(21 * time.Second) // a node with a launch is lost after the lease TTL (CR-6)
	r.p.CheckLost()
	if f := r.p.Facts(); f[0].State != StateLost || f[1].State != StateActive {
		// a2 kept sending; re-check after its next heartbeat
		eventually(t, 2*time.Second, func() bool { f := r.p.Facts(); return f[0].State == StateLost && f[1].State == StateActive }, "a1 lost only")
	}
	r.p.Stop(6, nil)
	r.p.WaitStops(10 * time.Second)
	if a1.Has("DELETE") {
		t.Fatalf("the lost node was waited for: %v", a1.Calls())
	}
	if st := a2.LaunchState(); st.State != agentproto.StateExited {
		t.Fatalf("the other member: %+v", st)
	}
}

func TestPauseIsStoredAndSentInTheHeartbeatAnswer(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	if _, err := r.p.SetPaused(a.ID, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, a.Paused, "paused in the heartbeat answer")
	if n, _ := r.node(a.ID); !n.Paused {
		t.Fatal("node not paused")
	}
	if _, err := r.p.SetPaused("02:00:00:00:00:99", true); err != ErrUnknownNode {
		t.Fatalf("unknown node: %v", err)
	}
	// A Potluck restart keeps the pause; the agent re-registers on 410.
	r.start()
	eventually(t, 5*time.Second, func() bool { n, ok := r.node(a.ID); return ok && n.Paused }, "paused after restart")
	// The agent stores the new registration's answer a moment after the pool
	// lists the node.
	eventually(t, 5*time.Second, func() bool { ans := a.Answer(); return ans != nil && ans.Paused }, "paused in the registration answer")
	r.p.SetPaused(a.ID, false)
	eventually(t, 5*time.Second, func() bool { return !a.Paused() }, "unpaused")
}

func TestDeregisterRemovalAndReadmission(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	r.p.SetPaused(a.ID, true)
	a.SetHeartbeats(false)
	if code, err := a.Deregister(); err != nil || code != 200 {
		t.Fatalf("%d %v", code, err)
	}
	if _, ok := r.node(a.ID); ok {
		t.Fatal("a left node stays in the view")
	}
	if f := r.p.Facts()[0]; f.State != StateLeft || f.Paused {
		t.Fatalf("facts %+v (pause is forgotten)", f)
	}
	r.mu.Lock()
	forced := len(r.forced)
	r.mu.Unlock()
	if forced != 1 {
		t.Fatal("deregister did not stop the node's instance")
	}
	a.SetHeartbeats(true) // heartbeat gets 410, registers again as new
	r.waitNode(a.ID)

	if err := r.p.Remove(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool {
		rs := a.Refusals()
		return len(rs) > 0 && rs[len(rs)-1].Reason == agentproto.ReasonRemoved
	}, "refused as removed")
	if _, ok := r.node(a.ID); ok {
		t.Fatal("removed node in the view")
	}
	if m, _ := r.store.NodeAdmins(); m[a.ID] != (cfgstore.NodeAdmin{Removed: true}) {
		t.Fatalf("store %+v", r.store.m)
	}
	if err := r.p.Readmit(a.ID); err != nil {
		t.Fatal(err)
	}
	r.waitNode(a.ID)
}

func TestDockerAgentRestartEndsItsLaunch(t *testing.T) {
	r := newRig(t)
	a := r.agent(1, func(a *fakeagent.Agent) { a.Install = agentproto.InstallDocker })
	r.waitNode(a.ID)
	rep := &reports{}
	if res := r.p.Launch(context.Background(), spec(7, a), rep.add); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	// The container restarted: a new agent process registers without the launch.
	q := regReq(a.ID)
	q.Addr, q.AgentStartID, q.Install = a.Addr, "s-new", agentproto.InstallDocker
	if ans := r.p.Register(q); !ans.Accepted {
		t.Fatalf("%+v", ans)
	}
	eventually(t, 5*time.Second, func() bool { return len(rep.exits()) == 1 }, "exit reported")
	if e := rep.exits()[0]; e.Exit.Reason != agentproto.InstanceReasonAgentRestarted {
		t.Fatalf("%+v", e.Exit)
	}
	r.p.Stop(7, nil)
	r.p.WaitStops(10 * time.Second)
}

func TestForceNodeWithoutAnInstance(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	// A launch this run does not hold (as after a restart, before the agent re-registered).
	if res := r.p.Launch(context.Background(), spec(8, a), func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	id := a.LaunchState().LaunchID
	eventually(t, 5*time.Second, func() bool { f := r.p.Facts(); return f[0].LaunchID != nil }, "heartbeat names the launch")
	r.p.ForceNode(core.NodeID(a.ID))
	r.p.WaitStops(10 * time.Second)
	if !a.Has("DELETE /launch/" + id + "?force=1") {
		t.Fatalf("calls %v", a.Calls())
	}
	r.p.Stop(8, nil)
	r.p.WaitStops(10 * time.Second)
}

func eventually(t testing.TB, d time.Duration, cond func() bool, what string) {
	t.Helper()
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A worker serves one device. A node listing more than one device of its
// platform's backend uses the first; the rest are named in its status and
// warned about once per registration, never guessed at silently.
func TestExtraDevicesOfTheBackendAreReported(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq("02:00:00:00:00:72"))
	hb := `{"agent_start_id":"s1","seq":1,"programs_ready":true,"mem_available_mib":30000,"devices":[
		{"name":"Vulkan0","backend":"vulkan","total_mib":8000,"free_mib":7000},
		{"name":"Vulkan1","backend":"vulkan","total_mib":16000,"free_mib":15000},
		{"name":"Vulkan2","backend":"vulkan","total_mib":16000,"free_mib":15000},
		{"name":"CPU","backend":"cpu","total_mib":30000,"free_mib":30000}]}`
	for i := 0; i < 3; i++ {
		if res := r.p.Heartbeat("02:00:00:00:00:72", []byte(hb)); res.Status != 200 {
			t.Fatalf("%+v", res)
		}
	}
	if n, _ := r.node("02:00:00:00:00:72"); n.TotalMiB != 8000 || n.FreeMiB != 7000 {
		t.Fatalf("not the first vulkan device: %+v", n)
	}
	if f := r.p.Facts()[0]; f.ExtraDevicesIgnored != 2 {
		t.Fatalf("facts %+v", f)
	}
	b, _ := json.Marshal(r.p.Facts()[0])
	if !strings.Contains(string(b), `"extra_devices_ignored":2`) {
		t.Fatalf("status %s", b)
	}
	if n := strings.Count(r.logs.String(), "extra devices ignored"); n != 1 {
		t.Fatalf("%d warnings for one registration:\n%s", n, r.logs.String())
	}
	r.p.Register(regReq("02:00:00:00:00:72")) // registers again: warned again
	r.p.Heartbeat("02:00:00:00:00:72", []byte(hb))
	if n := strings.Count(r.logs.String(), "extra devices ignored"); n != 2 {
		t.Fatalf("%d warnings for two registrations", n)
	}
	one := strings.Replace(hb, `"backend":"vulkan","total_mib":16000`, `"backend":"cuda","total_mib":16000`, 2)
	r.p.Heartbeat("02:00:00:00:00:72", []byte(one))
	if f := r.p.Facts()[0]; f.ExtraDevicesIgnored != 0 {
		t.Fatalf("one vulkan device left: %+v", f)
	}
}

// The worker launches of a load must be ready within worker_start_timeout.
func TestWorkerStartTimeoutFailsTheLoad(t *testing.T) {
	r := newRig(t)
	r.cfg.Defaults.WorkerStartTimeout = 300 * time.Millisecond
	a1 := r.agent(1)
	a2 := r.agent(2, func(a *fakeagent.Agent) { a.ReadyAfter = time.Minute })
	r.waitNode(a1.ID)
	r.waitNode(a2.ID)
	start := time.Now()
	res := r.p.Launch(context.Background(), spec(3, a1, a2), func(scheduler.WorkerReport) {})
	if res.Reason != ReasonWorkerStartTimeout || res.Node != core.NodeID(a2.ID) || !strings.Contains(res.Message, "300ms") {
		t.Fatalf("%+v", res)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
	r.p.Stop(3, nil)
	r.p.WaitStops(10 * time.Second)
}
