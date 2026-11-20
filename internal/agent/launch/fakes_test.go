package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

type fakeProc struct {
	unit       string
	pid        int
	mu         sync.Mutex
	done       chan struct{}
	exit       runner.Exit
	sigs       []osx.Sig
	ignoreTerm bool
	killErrs   int // this many KILLs fail with an error
	killDeaf   int // this many KILLs are accepted but do not end the process
}

func (p *fakeProc) Unit() string          { return p.unit }
func (p *fakeProc) PID() int              { return p.pid }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Exit() runner.Exit     { <-p.done; return p.exit }

func (p *fakeProc) Signal(s osx.Sig) error {
	p.mu.Lock()
	p.sigs = append(p.sigs, s)
	ignore := p.ignoreTerm
	if s == osx.SigKill && p.killErrs > 0 {
		p.killErrs--
		p.mu.Unlock()
		return errors.New("systemctl kill: failed to connect to bus")
	}
	if s == osx.SigKill && p.killDeaf > 0 {
		p.killDeaf--
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	switch {
	case s == osx.SigKill:
		p.finish(runner.Exit{Known: true, Code: 137, Signal: "KILL"})
	case s == osx.SigTerm && !ignore:
		p.finish(runner.Exit{Known: true, Code: 0})
	}
	return nil
}

func (p *fakeProc) signals() []osx.Sig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]osx.Sig(nil), p.sigs...)
}

func (p *fakeProc) finish(e runner.Exit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return
	default:
	}
	p.exit = e
	close(p.done)
}

func (p *fakeProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

type fakeRunner struct {
	mu         sync.Mutex
	procs      map[string]*fakeProc
	specs      []runner.Spec
	survives   bool
	startErr   error
	ignoreTerm bool
	nextPID    int
	stopped    []string
	stopErr    error
	stopKills  bool // Stop ends the unit, as systemctl stop does
}

func newFakeRunner(survives bool) *fakeRunner {
	return &fakeRunner{procs: map[string]*fakeProc{}, survives: survives, nextPID: 1000}
}

func (r *fakeRunner) Start(ctx context.Context, s runner.Spec) (runner.Proc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err // as exec.CommandContext does
	}
	if r.startErr != nil {
		return nil, r.startErr
	}
	if p, ok := r.procs[s.Unit]; ok && p.alive() {
		return nil, runner.ErrExists
	}
	r.nextPID++
	p := &fakeProc{unit: s.Unit, pid: r.nextPID, done: make(chan struct{}), ignoreTerm: r.ignoreTerm}
	r.procs[s.Unit] = p
	r.specs = append(r.specs, s)
	return p, nil
}

func (r *fakeRunner) Adopt(ctx context.Context, unit, exitPath string) (runner.Proc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.survives {
		return nil, nil
	}
	p, ok := r.procs[unit]
	if !ok {
		return nil, nil
	}
	return p, nil
}

func (r *fakeRunner) List(ctx context.Context, prefix string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for u, p := range r.procs {
		if strings.HasPrefix(u, prefix) && p.alive() {
			out = append(out, u)
		}
	}
	return out, nil
}

func (r *fakeRunner) Stop(ctx context.Context, unit string) error {
	r.mu.Lock()
	r.stopped = append(r.stopped, unit)
	p, err, kills := r.procs[unit], r.stopErr, r.stopKills
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if kills && p != nil {
		p.finish(runner.Exit{Known: true, Code: 137, Signal: "KILL"})
	}
	return ctx.Err()
}

func (r *fakeRunner) stops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stopped...)
}
func (r *fakeRunner) Kind() string   { return "fake" }
func (r *fakeRunner) Survives() bool { return r.survives }

func (r *fakeRunner) proc(unit string) *fakeProc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[unit]
}

func (r *fakeRunner) lastSpec() runner.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.specs[len(r.specs)-1]
}

type fakeSockets struct {
	mu          sync.Mutex
	listening   map[int]bool
	established map[int]bool
}

func (s *fakeSockets) Listening(p int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listening[p], nil
}
func (s *fakeSockets) Established(p int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.established[p], nil
}
func (s *fakeSockets) set(p int, listen, est bool) {
	s.mu.Lock()
	s.listening[p], s.established[p] = listen, est
	s.mu.Unlock()
}

type fakeMem struct {
	mu    sync.Mutex
	avail int64
}

func (m *fakeMem) MemInfo() (osx.MemInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return osx.MemInfo{TotalMiB: 16000, AvailableMiB: m.avail}, nil
}
func (m *fakeMem) set(v int64) { m.mu.Lock(); m.avail = v; m.mu.Unlock() }

type fakeGate struct {
	mu        sync.Mutex
	paused    bool
	ready     bool
	reachable bool
}

func (g *fakeGate) Paused() bool        { g.mu.Lock(); defer g.mu.Unlock(); return g.paused }
func (g *fakeGate) Release() string     { return "0.1.0" }
func (g *fakeGate) ProgramsReady() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.ready }
func (g *fakeGate) ModelSourceReachable() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reachable
}
func (g *fakeGate) WaitDevicesRead() {}

type fakePrograms struct{ dir string }

func (p fakePrograms) Path(name string) (string, bool) { return filepath.Join(p.dir, name), true }
func (p fakePrograms) Build(name string) string        { return "a01665d30" }

type fakeProcs struct {
	mu     sync.Mutex
	list   []osx.ProcInfo
	killed []int
	lists  int
	onKill func(pid int)
}

func (f *fakeProcs) List() ([]osx.ProcInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return f.list, nil
}
func (f *fakeProcs) Signal(pid int, s osx.Sig) error {
	f.mu.Lock()
	f.killed = append(f.killed, pid)
	on := f.onKill
	f.mu.Unlock()
	if on != nil {
		on(pid)
	}
	return nil
}
func (f *fakeProcs) kills() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.killed...)
}
func (f *fakeProcs) set(l []osx.ProcInfo) { f.mu.Lock(); f.list = l; f.mu.Unlock() }
func (f *fakeProcs) Self() int            { return 1 }

type fakeKLog struct{ oom map[int]bool }

func (k fakeKLog) OOMKilled(pid int) bool { return k.oom[pid] }

// env is one test's manager with all its fakes.
type env struct {
	t       *testing.T
	m       *Manager
	clock   *osx.FakeClock
	runner  *fakeRunner
	sockets *fakeSockets
	mem     *fakeMem
	gate    *fakeGate
	procs   *fakeProcs
	klog    fakeKLog
	cfg     Config
	portErr error
	probeOK map[string]bool
	healthy bool
	exits   chan agentproto.LaunchState
	errs    []string
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

func newEnv(t *testing.T, survives bool) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, clock: osx.NewFakeClock(100 * time.Second), runner: newFakeRunner(survives),
		sockets: &fakeSockets{listening: map[int]bool{}, established: map[int]bool{}},
		mem:     &fakeMem{avail: 12000}, gate: &fakeGate{ready: true, reachable: true},
		procs: &fakeProcs{}, klog: fakeKLog{oom: map[int]bool{}}, probeOK: map[string]bool{},
		exits: make(chan agentproto.LaunchState, 16)}
	e.cfg = Config{StateDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "shm", "launch"),
		ProgramRoot: filepath.Join(dir, "shm", "programs"), BindAddr: "192.0.2.101",
		ModelDir: filepath.Join(dir, "models"), WatchdogFloorMiB: 1000,
		Tick: 2 * time.Millisecond, PortRetry: 50 * time.Millisecond, PortCloseWait: 10 * time.Second}
	os.MkdirAll(e.cfg.ModelDir, 0o755)
	e.m = e.newManager()
	return e
}

func (e *env) newManager() *Manager {
	return New(e.cfg, Deps{
		Clock: e.clock, Runner: e.runner, Mem: e.mem, Sockets: e.sockets, Procs: e.procs, KLog: e.klog,
		PortFree: func(addr string, port int) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.portErr
		},
		Probe: func(ctx context.Context, addr string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.probeOK[addr] {
				return nil
			}
			return errors.New("refused")
		},
		HostHealthy: func(ctx context.Context, addr string) bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.healthy
		},
		Gate: e.gate, Programs: fakePrograms{dir: e.cfg.ProgramRoot},
		OnExit:  func(st agentproto.LaunchState) { e.exits <- st },
		OnError: func(s string) { e.mu.Lock(); e.errs = append(e.errs, s); e.mu.Unlock() },
	})
}

func (e *env) run() {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	go func() { e.m.Run(ctx); close(e.done) }()
	e.t.Cleanup(e.stop)
}

func (e *env) stop() {
	if e.cancel != nil {
		e.cancel()
		<-e.done
		e.cancel = nil
	}
}

func (e *env) setProbe(addr string, ok bool) { e.mu.Lock(); e.probeOK[addr] = ok; e.mu.Unlock() }

func workerReq(id string) agentproto.LaunchRequest {
	return agentproto.LaunchRequest{LaunchID: id, Role: agentproto.RoleWorker, Release: "0.1.0",
		Label: "Devstral.gguf", LeaseTTLS: 20, MinMemAvailableMiB: 9000,
		Worker: &agentproto.WorkerSpec{ModelDir: true, ExtraArgs: []string{}}}
}

func (e *env) appendLog(id, text string) {
	p := filepath.Join(e.cfg.LogDir, id+".log")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	f.WriteString(text)
	f.Close()
}

func (e *env) waitState(want string) agentproto.LaunchState {
	e.t.Helper()
	var st agentproto.LaunchState
	agenttest.Eventually(e.t, 5*time.Second, "state "+want, func() bool {
		st = e.m.State()
		return st.State == want
	})
	return st
}

// startReady starts a worker and brings it to ready.
func (e *env) startReady(id string) agentproto.LaunchState {
	e.t.Helper()
	st, ref := e.m.Start(context.Background(), workerReq(id))
	if ref != nil {
		e.t.Fatalf("refused: %v", ref)
	}
	if st.State != agentproto.StateStarting {
		e.t.Fatalf("state %s", st.State)
	}
	e.appendLog(id, "Starting RPC server v3.0.0\n  endpoint : 192.0.2.101:50052\n")
	e.setProbe("192.0.2.101:50052", true)
	e.sockets.set(50052, true, false)
	return e.waitState(agentproto.StateReady)
}

// processGone simulates the process exiting and its port closing.
func (e *env) portClosed() { e.sockets.set(50052, false, false) }

// settle waits until the loop has completed n more rounds, for checks that
// something did NOT happen.
func (e *env) settle(n int64) {
	e.t.Helper()
	start := e.m.ticks.Load()
	agenttest.Eventually(e.t, 5*time.Second, "loop rounds", func() bool { return e.m.ticks.Load() >= start+n })
}
