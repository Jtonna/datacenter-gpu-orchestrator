package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/hw"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/modelsrc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	agenttest.Cleanup()
	os.Exit(code)
}

// ---- fake Potluck ----

type fakePotluck struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	files     map[string][]byte
	registers []agentproto.RegisterRequest
	beats     []agentproto.Heartbeat
	answer    func(req agentproto.RegisterRequest) agentproto.RegisterAnswer
	hbStatus  int
	hbAnswer  agentproto.HeartbeatAnswer
	downloads int
	deregs    []string // paths of the deregister calls
	// left: the node has deregistered; a second call is answered 404, as the
	// real handler does. deregStatus, when set, is the answer to every call;
	// deregHang, when set, holds the answer until the caller gives up or it
	// is closed.
	left        bool
	deregStatus int
	deregHang   chan struct{}
	// logs, when set, serves the CR-10 log routes, outside mu: a log push
	// that hangs must not hold up the heartbeat. beatAt is when each
	// heartbeat arrived.
	logs   atomic.Pointer[http.HandlerFunc]
	beatAt []time.Time
	// ackStartup: answer a heartbeat's startup object with startup_ack, as a
	// Potluck that takes start-up reports does.
	ackStartup bool
}

func newFakePotluck(t *testing.T) *fakePotluck {
	p := &fakePotluck{t: t, files: map[string][]byte{}, hbStatus: 200, hbAnswer: agentproto.HeartbeatAnswer{Release: "0.1.0"}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePotluck) addr() string { return strings.TrimPrefix(p.srv.URL, "http://") }

func (p *fakePotluck) serve(w http.ResponseWriter, r *http.Request) {
	if h := p.logs.Load(); h != nil && strings.HasSuffix(r.URL.Path, "/logs") {
		(*h)(w, r)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.URL.Path == agentproto.PathRegister:
		var req agentproto.RegisterRequest
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &req)
		// Arrays must be arrays, never null.
		for _, k := range []string{`"programs":null`, `"gpus":null`, `"backends":null`, `"foreign_processes":null`} {
			if bytes.Contains(raw, []byte(k)) {
				p.t.Errorf("registration has %s", k)
			}
		}
		p.registers = append(p.registers, req)
		json.NewEncoder(w).Encode(p.answer(req))
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		var hb agentproto.Heartbeat
		json.NewDecoder(r.Body).Decode(&hb)
		p.beats = append(p.beats, hb)
		p.beatAt = append(p.beatAt, time.Now())
		w.WriteHeader(p.hbStatus)
		if p.hbStatus == 200 {
			ans := p.hbAnswer
			if p.ackStartup && hb.Startup != nil {
				ans.StartupAck = hb.Startup.Count()
			}
			json.NewEncoder(w).Encode(ans)
		}
	case strings.HasSuffix(r.URL.Path, "/deregister"):
		// As internal/api agentDeregister: 200 {"left":true}, then 404.
		p.deregs = append(p.deregs, r.Method+" "+r.URL.Path)
		switch {
		case p.deregHang != nil:
			hang := p.deregHang
			p.mu.Unlock()
			select {
			case <-r.Context().Done():
			case <-hang:
			}
			p.mu.Lock()
		case p.deregStatus != 0:
			w.WriteHeader(p.deregStatus)
		case p.left:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":"no registered agent %s"}`, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/pool/nodes/"), "/deregister"))
		default:
			p.left = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"left":true}`))
		}
	case strings.HasPrefix(r.URL.Path, agentproto.PathProgramsPrefix):
		b, ok := p.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		p.downloads++
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func (p *fakePotluck) addFile(name string, body []byte) agentproto.ProgramEntry {
	path := "/api/pool/programs/0.1.0/linux-amd64-vulkan/" + name
	s := sha256.Sum256(body)
	p.mu.Lock()
	p.files[path] = body
	p.mu.Unlock()
	return agentproto.ProgramEntry{Name: name, Path: path, SHA256: hex.EncodeToString(s[:]), Bytes: int64(len(body)), Build: "a01665d30"}
}

func (p *fakePotluck) set(f func()) { p.mu.Lock(); f(); p.mu.Unlock() }

func (p *fakePotluck) nRegisters() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.registers) }
func (p *fakePotluck) lastRegister() agentproto.RegisterRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.registers[len(p.registers)-1]
}
func (p *fakePotluck) nBeats() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.beats) }
func (p *fakePotluck) lastBeat() agentproto.Heartbeat {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.beats[len(p.beats)-1]
}

// ---- fakes for the OS ----

type fakeNet struct{}

func (fakeNet) SourceFor(net.IP) (net.IP, error)   { return net.ParseIP("127.0.0.1"), nil }
func (fakeNet) InterfaceOf(net.IP) (string, error) { return "eth0", nil }
func (fakeNet) DefaultRouteInterface() string      { return "eth0" }
func (fakeNet) IsPhysical(string) bool             { return true }
func (fakeNet) MAC(string) (string, error)         { return "02:00:00:00:00:AB", nil }

type fakeHW struct{}

func (fakeHW) GPUs() []hw.GPUInfo {
	return []hw.GPUInfo{{GPU: agentproto.GPU{Vendor: "amd", Model: "AMD Custom GPU 0405", Integrated: true}, PCI: "0000:20:00.0"}}
}
func (fakeHW) Backends([]hw.GPUInfo) []string       { return []string{"vulkan"} }
func (fakeHW) MissingLibs(string) ([]string, error) { return nil, nil }

type fakeMem struct{ avail int64 }

func (m fakeMem) MemInfo() (osx.MemInfo, error) {
	return osx.MemInfo{TotalMiB: 15258, AvailableMiB: m.avail}, nil
}

type fakeProcs struct {
	mu        sync.Mutex
	list      []osx.ProcInfo
	signalled []int
	onSignal  func(pid int)
}

func (f *fakeProcs) List() ([]osx.ProcInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list, nil
}
func (f *fakeProcs) Signal(pid int, _ osx.Sig) error {
	f.mu.Lock()
	f.signalled = append(f.signalled, pid)
	on := f.onSignal
	f.mu.Unlock()
	if on != nil {
		on(pid)
	}
	return nil
}
func (f *fakeProcs) Self() int { return os.Getpid() }

// dialSockets answers Listening by connecting (tests only; the real agent
// reads the socket table and never connects).
type dialSockets struct{}

func (dialSockets) Listening(port int) (bool, error) {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return false, nil
	}
	c.Close()
	return true, nil
}
func (dialSockets) Established(int) (bool, error) { return false, nil }

type okProber struct{}

type doneProbe struct{ ch chan struct{} }

func (d doneProbe) Done() <-chan struct{} { return d.ch }
func (d doneProbe) Err() error            { return nil }
func (d doneProbe) Kill()                 {}

func (okProber) Start(string) (modelsrc.ProbeHandle, error) {
	ch := make(chan struct{})
	close(ch)
	return doneProbe{ch}, nil
}

type fakeUpdater struct {
	mu    sync.Mutex
	calls []string
}

func (u *fakeUpdater) Update(ctx context.Context, r string) error {
	u.mu.Lock()
	u.calls = append(u.calls, r)
	u.mu.Unlock()
	return nil
}
func (u *fakeUpdater) n() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.calls) }

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// ---- harness ----

type tenv struct {
	t        *testing.T
	pl       *fakePotluck
	a        *Agent
	clock    *osx.FakeClock
	procs    *fakeProcs
	upd      *fakeUpdater
	entries  []agentproto.ProgramEntry
	expSpecs []agentproto.ExporterSpec
	nodePort int
	stateDir string
	opt      Options
	deps     Deps
	cancel   context.CancelFunc
	done     chan struct{}
	runner   runner.Runner
	devOut   atomic.Value // string printed by llama-server --list-devices
	devPath  atomic.Value // last program asked for its devices
}

const devicesOut = "Available devices:\n  Vulkan0: AMD Custom GPU 0405 (RADV GFX1013) (14336 MiB, 14010 MiB free)\n"

func newTenv(t *testing.T) *tenv { t.Helper(); return newTenvWith(t, nil) }

// newTenvWith lets a test change the options and the OS boundaries.
func newTenvWith(t *testing.T, mod func(*Options, *Deps)) *tenv {
	t.Helper()
	pl := newFakePotluck(t)
	dir := t.TempDir()
	e := &tenv{t: t, pl: pl, clock: osx.NewFakeClock(time.Hour), procs: &fakeProcs{}, upd: &fakeUpdater{}}
	e.devOut.Store(devicesOut)
	// Programs: real fake binaries served by the fake Potluck.
	binDir := t.TempDir()
	rpc, _ := os.ReadFile(agenttest.Install(t, binDir, "rpc-server"))
	exp, _ := os.ReadFile(agenttest.Install(t, binDir, "node_exporter"))
	e.entries = []agentproto.ProgramEntry{
		pl.addFile("rpc-server", rpc),
		pl.addFile("llama-server", []byte("LLAMA")),
		pl.addFile("node_exporter", exp),
	}
	fixture := filepath.Join(dir, "metrics.prom")
	os.WriteFile(fixture, []byte("node_load1 0.5\nnode_memory_MemTotal_bytes 1.6e10\nnode_memory_MemAvailable_bytes 1.2e10\n"), 0o644)
	t.Setenv("FAKEPROG_FIXTURE", fixture)
	e.nodePort = freePort(t)
	e.expSpecs = []agentproto.ExporterSpec{{Name: "node_exporter", Args: []string{fmt.Sprintf("--web.listen-address=127.0.0.1:%d", e.nodePort)}}}
	pl.answer = func(req agentproto.RegisterRequest) agentproto.RegisterAnswer {
		return agentproto.RegisterAnswer{Accepted: true, Release: "0.1.0", Platform: "linux-amd64-vulkan",
			HeartbeatEveryS: 0.02, Programs: e.entries, Exporters: e.expSpecs}
	}
	models := filepath.Join(dir, "models")
	os.MkdirAll(models, 0o755)
	os.WriteFile(filepath.Join(models, "m.gguf"), []byte("GGUF"), 0o644)
	e.runner = runner.NewChild()
	e.stateDir = filepath.Join(dir, "run")
	opt := Options{Potluck: pl.addr(), Release: "0.1.0", Name: "console-5", Install: agentproto.InstallDocker,
		RAMOnly: true, ModelSource: modelsrc.Spec{Kind: modelsrc.KindShare, Target: "192.0.2.1:/"},
		WatchdogFloorMiB: 1000, AllowFrom: []netip.Prefix{netip.MustParsePrefix("172.30.0.0/24")},
		DataDir: filepath.Join(dir, "shm"), StateDir: e.stateDir, ModelsDir: models,
	}
	deps := Deps{Clock: e.clock, Runner: e.runner, Mem: fakeMem{avail: 12000}, Sockets: dialSockets{},
		Procs: e.procs, KLog: osx.Kmsg{}, Net: fakeNet{}, Hardware: fakeHW{}, Prober: okProber{},
		DiskFree: func(string) (uint64, error) { return 1 << 40, nil },
		DevicesRun: func(ctx context.Context, path string, args, env []string) ([]byte, error) {
			e.devPath.Store(filepath.Base(path))
			return []byte(e.devOut.Load().(string)), nil
		},
		Updater: e.upd, RegisterRetry: 30 * time.Millisecond, ErrorRetry: 20 * time.Millisecond,
		ProgramsRetry: 30 * time.Millisecond, ProgramsRetryIn: time.Millisecond, LaunchTick: 5 * time.Millisecond,
		ProbeEvery: 20 * time.Millisecond, ProbeTimeout: time.Second, StatsMin: 30 * time.Millisecond,
		ExporterPorts: []int{e.nodePort}, WorkerPort: freePort(t), HostPort: freePort(t), Logger: testLogger()}
	if mod != nil {
		mod(&opt, &deps)
		e.runner, e.stateDir, e.clock = deps.Runner, opt.StateDir, deps.Clock.(*osx.FakeClock)
	}
	e.opt, e.deps = opt, deps
	a, err := New(opt, deps)
	if err != nil {
		t.Fatal(err)
	}
	e.a = a
	return e
}

func (e *tenv) start() {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	go func() { e.a.Run(ctx); close(e.done) }()
	e.t.Cleanup(e.stop)
}

func (e *tenv) stop() {
	if e.cancel != nil {
		e.cancel()
		<-e.done
		e.cancel = nil
		// Leave no child processes behind.
		if ids := e.a.Launch.CurrentID(); ids != nil {
			e.a.Launch.Stop(*ids, true)
		}
		e.a.Launch.Reset(context.Background())
		agenttest.Eventually(e.t, 10*time.Second, "children gone", func() bool {
			u, _ := e.runner.List(context.Background(), "potluck-")
			return len(u) == 0
		})
	}
}

func (e *tenv) waitReady() {
	agenttest.Eventually(e.t, 20*time.Second, "programs_ready in a heartbeat", func() bool {
		return e.pl.nBeats() > 0 && e.pl.lastBeat().ProgramsReady
	})
}

func (e *tenv) do(method, path string, body any, remote string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	e.a.Handler().ServeHTTP(w, r)
	return w
}

const fromPotluck = "127.0.0.1:40000"

// ---- tests ----

func TestRegistrationHeartbeatAndPipeline(t *testing.T) {
	e := newTenv(t)
	e.start()
	e.waitReady()
	req := e.pl.lastRegister()
	if req.ID != "02:00:00:00:00:ab" || req.Name != "console-5" || req.AgentRelease != "0.1.0" || req.Install != "docker" ||
		!req.RAMOnly || req.Addr != "127.0.0.1" || req.MemTotalMiB != 15258 || req.LaunchID != nil ||
		len(req.GPUs) != 1 || !req.GPUs[0].Integrated || req.Backends[0] != "vulkan" ||
		req.ModelSource.Kind != "share" || req.ModelSource.Target != "192.0.2.1:/" || len(req.AgentStartID) != 6 {
		t.Fatalf("%+v", req)
	}
	hb := e.pl.lastBeat()
	if hb.AgentStartID != req.AgentStartID || hb.Role != "idle" || hb.LaunchID != nil || hb.ProgramsError != nil ||
		hb.MemAvailableMiB != 12000 || hb.ModelSource.Kind != "share" || hb.Stats == nil {
		t.Fatalf("%+v", hb)
	}
	if len(hb.Devices) != 1 || hb.Devices[0].Name != "Vulkan0" || hb.Devices[0].TotalMiB != 14336 || !hb.Devices[0].SharesSystemMemory {
		t.Fatalf("devices %+v", hb.Devices)
	}
	// Sequence numbers grow; the clock never goes into the body as a time.
	n := hb.Seq
	agenttest.Eventually(t, 5*time.Second, "next heartbeat", func() bool { return e.pl.lastBeat().Seq > n })
	// Stats: the exporter runs and is scraped.
	agenttest.Eventually(t, 20*time.Second, "node source ok", func() bool {
		s := e.pl.lastBeat().Stats
		return s != nil && s.Sources["node"].Status == "ok" && s.Load != nil && s.Load.Load1 == 0.5
	})
	// Status on 127.0.0.1.
	w := httptest.NewRecorder()
	e.a.LocalHandler().ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
	var st agentproto.Status
	json.Unmarshal(w.Body.Bytes(), &st)
	if !st.Registered || st.Potluck != e.pl.addr() || st.Role != "idle" || st.State != "none" || st.LastError != nil {
		t.Fatalf("%s", w.Body.String())
	}
	// An agent restart does not fetch again: the files are re-hashed.
	if got := e.a.Programs.Held(); len(got) != 3 {
		t.Fatal(got)
	}
}

func TestRefusals(t *testing.T) {
	for _, reason := range []string{agentproto.ReasonPotluckTooOld, agentproto.ReasonUnsupportedHardware,
		agentproto.ReasonDuplicateID, agentproto.ReasonRemoved, agentproto.ReasonForeignProcess} {
		t.Run(reason, func(t *testing.T) {
			e := newTenv(t)
			e.pl.set(func() {
				e.pl.answer = func(agentproto.RegisterRequest) agentproto.RegisterAnswer {
					return agentproto.RegisterAnswer{Accepted: false, Reason: reason, Message: "no"}
				}
			})
			e.start()
			agenttest.Eventually(t, 5*time.Second, "registers again after the retry wait", func() bool { return e.pl.nRegisters() >= 3 })
			st := e.a.Status()
			if st.Registered || st.LastError == nil || !strings.HasPrefix(*st.LastError, reason+": ") {
				t.Fatalf("%+v", st)
			}
			e.pl.mu.Lock()
			dl, beats := e.pl.downloads, len(e.pl.beats)
			e.pl.mu.Unlock()
			if dl != 0 || beats != 0 {
				t.Fatal("a refused agent fetches nothing and sends no heartbeat")
			}
			if u, _ := e.runner.List(context.Background(), "potluck-"); len(u) != 0 {
				t.Fatal("a refused agent starts no exporter")
			}
			if e.upd.n() != 0 {
				t.Fatal("updated")
			}
		})
	}
}

func TestAgentTooOldUpdatesOnlyWithoutLaunch(t *testing.T) {
	e := newTenv(t)
	e.pl.set(func() {
		e.pl.answer = func(agentproto.RegisterRequest) agentproto.RegisterAnswer {
			return agentproto.RegisterAnswer{Accepted: false, Reason: agentproto.ReasonAgentTooOld, Message: "old", RequiredRelease: "0.2.0"}
		}
	})
	e.start()
	agenttest.Eventually(t, 5*time.Second, "update", func() bool { return e.upd.n() >= 1 })
	if e.upd.calls[0] != "0.2.0" {
		t.Fatal(e.upd.calls)
	}
}

func TestForeignProcessReported(t *testing.T) {
	e := newTenv(t)
	e.procs.list = []osx.ProcInfo{
		{PID: 4242, PPID: 1, Exe: "/usr/local/bin/rpc-server"},
		{PID: 4243, PPID: 1, Exe: "/dev/shm/potluck/programs/0.1.0/rpc-server", Cgroup: "0::/system.slice/potluck-launch-ln-1.service"},
		{PID: 4244, PPID: 1, Exe: "/usr/bin/bash"},
	}
	req := e.a.RegisterRequest()
	if len(req.ForeignProcesses) != 1 || req.ForeignProcesses[0].PID != 4242 {
		t.Fatalf("%+v", req.ForeignProcesses)
	}
}

func TestHeartbeatFailureCases(t *testing.T) {
	e := newTenv(t)
	e.start()
	e.waitReady()
	regs := e.pl.nRegisters()
	// 5xx: nothing happens; heartbeats continue on time.
	e.pl.set(func() { e.pl.hbStatus = 503 })
	n := e.pl.nBeats()
	agenttest.Eventually(t, 5*time.Second, "heartbeats continue", func() bool { return e.pl.nBeats() > n+3 })
	if e.pl.nRegisters() != regs || !e.a.Status().Registered {
		t.Fatal("a failed heartbeat must not re-register")
	}
	// 410: registers again at once.
	e.pl.set(func() { e.pl.hbStatus = 410 })
	agenttest.Eventually(t, 5*time.Second, "re-register on 410", func() bool { return e.pl.nRegisters() > regs })
	e.pl.set(func() { e.pl.hbStatus = 200 })
	regs = e.pl.nRegisters()
	// A different release: registers again.
	e.pl.set(func() { e.pl.hbAnswer.Release = "0.2.0" })
	agenttest.Eventually(t, 5*time.Second, "re-register on release change", func() bool { return e.pl.nRegisters() > regs })
}

func TestSourceAddressRule(t *testing.T) {
	e := newTenv(t)
	e.a.RecoverLaunch(context.Background())
	if w := e.do("GET", "/launch", nil, fromPotluck); w.Code != 200 || w.Body.String() != "{\"state\":\"none\"}\n" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "/launch", nil, "172.30.0.5:1234"); w.Code != 200 {
		t.Fatal("allow-from network refused")
	}
	if w := e.do("GET", "/launch", nil, "192.0.2.50:1234"); w.Code != 403 {
		t.Fatal("other address accepted")
	}
	if w := e.do("GET", "/status", nil, fromPotluck); w.Code == 200 {
		t.Fatal("/status must be answered on 127.0.0.1 only")
	}
	w := httptest.NewRecorder()
	e.a.LocalHandler().ServeHTTP(w, httptest.NewRequest("GET", "/launch", nil))
	if w.Code != 403 {
		t.Fatal("local listener answers only GET /status")
	}
}

func workerReq(id string) agentproto.LaunchRequest {
	return agentproto.LaunchRequest{LaunchID: id, Role: "worker", Release: "0.1.0", Label: "Devstral.gguf",
		LeaseTTLS: 20, MinMemAvailableMiB: 9000, Worker: &agentproto.WorkerSpec{ModelDir: true, ExtraArgs: []string{}}}
}

func TestPauseRace(t *testing.T) {
	e := newTenv(t)
	e.pl.set(func() { e.pl.hbAnswer.Paused = true })
	e.start()
	e.waitReady()
	agenttest.Eventually(t, 5*time.Second, "paused", func() bool { return e.a.Status().Paused })
	w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck)
	var ref agentproto.LaunchRefusal
	json.Unmarshal(w.Body.Bytes(), &ref)
	if w.Code != 409 || ref.Reason != "paused" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestWorkerLaunchEndToEnd(t *testing.T) {
	e := newTenv(t)
	e.start()
	e.waitReady()
	w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck)
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var st agentproto.LaunchState
	agenttest.Eventually(t, 20*time.Second, "ready", func() bool {
		json.Unmarshal(e.do("GET", "/launch", nil, fromPotluck).Body.Bytes(), &st)
		return st.State == "ready"
	})
	if st.Role != "worker" || st.Build != "a01665d30" || st.Command[0] != "rpc-server" || st.LeaseExpiresInS == nil {
		t.Fatalf("%+v", st)
	}
	agenttest.Eventually(t, 5*time.Second, "heartbeat role worker", func() bool {
		hb := e.pl.lastBeat()
		return hb.Role == "worker" && hb.LaunchID != nil && *hb.LaunchID == "ln-1"
	})
	// Log with absolute offsets.
	lw := e.do("GET", "/launch/log?since=0", nil, fromPotluck)
	next, _ := strconv.Atoi(lw.Header().Get("X-Next-Offset"))
	if lw.Code != 200 || !strings.Contains(lw.Body.String(), "Starting RPC server") || next != lw.Body.Len() {
		t.Fatalf("%d %q next=%d", lw.Code, lw.Body.String(), next)
	}
	// Lease renew, and for a wrong id.
	if w := e.do("POST", "/launch/ln-1/lease", nil, fromPotluck); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := e.do("POST", "/launch/ln-x/lease", nil, fromPotluck); w.Code != 404 {
		t.Fatal(w.Code)
	}
	// Busy.
	if w := e.do("POST", "/launch", workerReq("ln-2"), fromPotluck); w.Code != 409 || !strings.Contains(w.Body.String(), `"launch_id":"ln-1"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Stop with a wrong id stops nothing.
	if w := e.do("DELETE", "/launch/ln-x?force=1", nil, fromPotluck); w.Code != 409 {
		t.Fatal(w.Code)
	}
	// Forced stop.
	if w := e.do("DELETE", "/launch/ln-1?force=1", nil, fromPotluck); w.Code != 200 {
		t.Fatal(w.Code)
	}
	agenttest.Eventually(t, 20*time.Second, "exited", func() bool {
		json.Unmarshal(e.do("GET", "/launch", nil, fromPotluck).Body.Bytes(), &st)
		return st.State == "exited"
	})
	if st.Exit == nil || st.Exit.Reason != "forced" {
		t.Fatalf("%+v", st.Exit)
	}
	agenttest.Eventually(t, 5*time.Second, "idle again", func() bool { return e.pl.lastBeat().Role == "idle" })
	// Status names the exited launch's model as the last launch's, not as the
	// current model.
	s := e.a.Status()
	if s.Role != "idle" || s.Model != nil || s.State != "exited" || s.LastLaunch == nil {
		t.Fatalf("%+v", s)
	}
	if ll := s.LastLaunch; ll.ID != "ln-1" || ll.Model == nil || *ll.Model != "Devstral.gguf" || ll.Reason != "forced" || ll.EndedAgeS < 0 {
		t.Fatalf("%+v", ll)
	}
}

func TestStopLaunchAtRegistration(t *testing.T) {
	e := newTenv(t)
	e.start()
	e.waitReady()
	if w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	// Potluck restarts: 410, then the registration carries the launch and
	// Potluck answers stop_launch.
	e.pl.set(func() {
		e.pl.hbStatus = 410
		old := e.pl.answer
		e.pl.answer = func(req agentproto.RegisterRequest) agentproto.RegisterAnswer {
			a := old(req)
			if req.LaunchID != nil {
				a.StopLaunch = req.LaunchID
				e.pl.hbStatus = 200
			}
			return a
		}
	})
	agenttest.Eventually(t, 20*time.Second, "stopped by stop_launch", func() bool {
		st := e.a.Launch.State()
		return st.State == "exited" && st.Exit.Reason == "stopped"
	})
	found := false
	e.pl.mu.Lock()
	for _, r := range e.pl.registers {
		if r.LaunchID != nil && *r.LaunchID == "ln-1" {
			found = true
		}
	}
	e.pl.mu.Unlock()
	if !found {
		t.Fatal("registration did not carry launch_id")
	}
}

func TestWrongChecksumReported(t *testing.T) {
	e := newTenv(t)
	e.entries[0].SHA256 = strings.Repeat("0", 64)
	e.start()
	agenttest.Eventually(t, 20*time.Second, "programs_error", func() bool {
		if e.pl.nBeats() == 0 {
			return false
		}
		hb := e.pl.lastBeat()
		return !hb.ProgramsReady && hb.ProgramsError != nil && strings.Contains(*hb.ProgramsError, "sha256 mismatch")
	})
	if w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck); w.Code != 409 || !strings.Contains(w.Body.String(), "release_mismatch") {
		t.Fatal(w.Body.String())
	}
}

func TestPortTakenBlocksRegistration(t *testing.T) {
	e := newTenv(t)
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(e.nodePort))
	if err != nil {
		t.Skip("port raced")
	}
	defer l.Close()
	e.start()
	agenttest.Eventually(t, 5*time.Second, "plain reason", func() bool {
		s := e.a.Status()
		return s.LastError != nil && strings.Contains(*s.LastError, "is taken")
	})
	if e.pl.nRegisters() != 0 {
		t.Fatal("registered with a port taken")
	}
}

func TestSelfUpdater(t *testing.T) {
	body := []byte("NEW AGENT")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pool/release" {
			fmt.Fprintf(w, `{"release":"0.2.0","agents":{"%s":{"path":"/bin/agent","sha256":"%s"}}}`, goosArch(), hex.EncodeToString(sum[:]))
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "potluck-agent")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	restarted := ""
	u := SelfUpdater{Base: srv.URL, HTTP: srv.Client(), Exe: exe, Restart: func(p string) error { restarted = p; return nil }}
	if err := u.Update(context.Background(), "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW AGENT" || restarted != exe {
		t.Fatal(string(b), restarted)
	}
	if err := u.Update(context.Background(), "0.3.0"); err == nil {
		t.Fatal("wrong release accepted")
	}
	code := 0
	DockerUpdater{Exit: func(c int) { code = c }}.Update(context.Background(), "0.2.0")
	if code != 75 {
		t.Fatal(code)
	}
	_ = errors.New
}

func goosArch() string { return runtimeGOOS + "-" + runtimeGOARCH }

func TestNoDeviceAccess(t *testing.T) {
	e := newTenv(t)
	e.devOut.Store("==\n== CUDA ==\nlicence banner\nAvailable devices:\n  (none)\n")
	e.start()
	agenttest.Eventually(t, 20*time.Second, "programs_error says why", func() bool {
		if e.pl.nBeats() == 0 {
			return false
		}
		hb := e.pl.lastBeat()
		return !hb.ProgramsReady && hb.ProgramsError != nil && strings.Contains(*hb.ProgramsError, "render and video groups") && len(hb.Devices) == 0
	})
	if s := e.a.Status(); s.LastError == nil || !strings.Contains(*s.LastError, "root") {
		t.Fatalf("%+v", s)
	}
	if p, _ := e.devPath.Load().(string); p != "llama-server" {
		t.Fatalf("devices read with %q, want llama-server", p)
	}
	// Access granted (for example the agent restarted in the right groups):
	// the next read clears the error.
	e.devOut.Store(devicesOut)
	e.waitReady()
	if hb := e.pl.lastBeat(); hb.ProgramsError != nil || len(hb.Devices) != 1 {
		t.Fatalf("%+v", hb)
	}
}
