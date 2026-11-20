package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	agenttest.Cleanup()
	os.Exit(code)
}

func TestParseExitFile(t *testing.T) {
	cases := map[string]Exit{
		"exited 0\n":     {Known: true, Code: 0},
		"exited 1":       {Known: true, Code: 1},
		"killed KILL\n":  {Known: true, Code: 137, Signal: "KILL"},
		"killed SIGTERM": {Known: true, Code: 143, Signal: "TERM"},
		"dumped 11":      {Known: true, Code: 139, Signal: "SEGV"},
		"":               {},
		"exited x":       {},
	}
	for in, want := range cases {
		if got := ParseExitFile(in); got != want {
			t.Errorf("%q: got %+v want %+v", in, got, want)
		}
	}
}

// fakeSystemctl simulates systemd-run and systemctl for one host.
type fakeSystemctl struct {
	mu    sync.Mutex
	calls [][]string
	units map[string]string // unit.service -> ActiveState
	pids  map[string]int
}

func (f *fakeSystemctl) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	switch {
	case name == "systemd-run":
		u := strings.TrimPrefix(args[0], "--unit=")
		f.units[u] = "active"
		f.pids[u] = 4242
		return []byte("Running as unit: " + u), nil
	case name == "systemctl" && args[0] == "show":
		st, ok := f.units[args[1]]
		if !ok {
			return []byte("LoadState=not-found\nActiveState=inactive\nMainPID=0\n"), nil
		}
		return []byte(fmt.Sprintf("LoadState=loaded\nActiveState=%s\nSubState=running\nMainPID=%d\nExecMainCode=0\nExecMainStatus=0\n", st, f.pids[args[1]])), nil
	case name == "systemctl" && args[0] == "list-units":
		var b strings.Builder
		for u, st := range f.units {
			fmt.Fprintf(&b, "%s loaded %s running potluck\n", u, st)
		}
		return []byte(b.String()), nil
	case name == "systemctl" && args[0] == "stop":
		delete(f.units, args[1])
	}
	return nil, nil
}

func (f *fakeSystemctl) set(unit, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == "" {
		delete(f.units, unit)
	} else {
		f.units[unit] = state
	}
}

func (f *fakeSystemctl) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

func TestSystemdStartArgs(t *testing.T) {
	s := &Systemd{}
	got := s.StartArgs(Spec{Unit: "potluck-launch-ln-1", Path: "/dev/shm/potluck/programs/0.1.0/rpc-server",
		Args: []string{"--host", "192.0.2.101", "--port", "50052"}, Env: []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID"}, OOMScoreAdjust: 900,
		LogPath: "/dev/shm/potluck/launch/ln-1.log", ExitPath: "/run/potluck/launch/ln-1.exit"})
	line := strings.Join(got, " ")
	for _, want := range []string{
		"--unit=potluck-launch-ln-1.service",
		"--setenv=CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"-p OOMScoreAdjust=900",
		"StandardOutput=append:/dev/shm/potluck/launch/ln-1.log",
		"StandardError=append:/dev/shm/potluck/launch/ln-1.log",
		`ExecStopPost=/bin/sh -c "echo $$EXIT_CODE $$EXIT_STATUS > /run/potluck/launch/ln-1.exit"`,
		"-- /dev/shm/potluck/programs/0.1.0/rpc-server --host 192.0.2.101 --port 50052",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in\n%s", want, line)
		}
	}
}

func TestSystemdLifecycle(t *testing.T) {
	f := &fakeSystemctl{units: map[string]string{}, pids: map[string]int{}}
	s := &Systemd{Cmd: f, Poll: 5 * time.Millisecond}
	exitPath := filepath.Join(t.TempDir(), "ln-1.exit")
	ctx := context.Background()
	p, err := s.Start(ctx, Spec{Unit: "potluck-launch-ln-1", Path: "/x/rpc-server", ExitPath: exitPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, Spec{Unit: "potluck-launch-ln-1", Path: "/x/rpc-server"}); err != ErrExists {
		t.Fatalf("second start: %v", err)
	}
	agenttest.Eventually(t, 2*time.Second, "pid", func() bool { return p.PID() == 4242 })
	units, _ := s.List(ctx, "potluck-launch-")
	if len(units) != 1 || units[0] != "potluck-launch-ln-1" {
		t.Fatalf("list: %v", units)
	}
	if err := p.Signal(osx.SigTerm); err != nil || !f.called("systemctl kill --signal=SIGTERM potluck-launch-ln-1.service") {
		t.Fatalf("signal: %v %v", err, f.calls)
	}
	// The unit ends; ExecStopPost wrote the status; systemd collects it.
	os.WriteFile(exitPath, []byte("killed KILL\n"), 0o644)
	f.set("potluck-launch-ln-1.service", "")
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not done")
	}
	if e := p.Exit(); e != (Exit{Known: true, Code: 137, Signal: "KILL"}) {
		t.Fatalf("exit %+v", e)
	}
}

func TestSystemdAdopt(t *testing.T) {
	f := &fakeSystemctl{units: map[string]string{"potluck-launch-ln-2.service": "active"}, pids: map[string]int{"potluck-launch-ln-2.service": 99}}
	s := &Systemd{Cmd: f, Poll: 5 * time.Millisecond}
	ctx := context.Background()
	p, err := s.Adopt(ctx, "potluck-launch-ln-2", "")
	if err != nil || p == nil || p.PID() != 99 {
		t.Fatalf("adopt: %v %v", p, err)
	}
	// A unit that is gone, with an exit file left behind.
	exitPath := filepath.Join(t.TempDir(), "gone.exit")
	os.WriteFile(exitPath, []byte("exited 1\n"), 0o644)
	p2, err := s.Adopt(ctx, "potluck-launch-gone", exitPath)
	if err != nil || p2 == nil {
		t.Fatalf("adopt gone: %v %v", p2, err)
	}
	<-p2.Done()
	if p2.Exit().Code != 1 {
		t.Fatal(p2.Exit())
	}
	// Gone with no file: nothing to adopt.
	if p3, _ := s.Adopt(ctx, "potluck-launch-none", filepath.Join(t.TempDir(), "x")); p3 != nil {
		t.Fatal("adopted nothing")
	}
}

func TestChildRunner(t *testing.T) {
	dir := t.TempDir()
	prog := agenttest.Install(t, dir, "rpc-server")
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c := NewChild()
	logPath := filepath.Join(dir, "out.log")
	exitPath := filepath.Join(dir, "out.exit")
	p, err := c.Start(context.Background(), Spec{Unit: "potluck-launch-a", Path: prog,
		Args: []string{"--host", "127.0.0.1", "--port", fmt.Sprint(port)}, LogPath: logPath, ExitPath: exitPath,
		Env: []string{"FAKEPROG_PRINT_ENV=LLAMA_RPC_FILE_LOAD", "LLAMA_RPC_FILE_LOAD=required"}})
	if err != nil {
		t.Fatal(err)
	}
	agenttest.Eventually(t, 10*time.Second, "start line in log", func() bool {
		b, _ := os.ReadFile(logPath)
		return strings.Contains(string(b), "Starting RPC server")
	})
	if b, _ := os.ReadFile(logPath); !strings.Contains(string(b), "env LLAMA_RPC_FILE_LOAD=required\n") {
		t.Fatalf("the launch environment did not reach the process: %q", b)
	}
	if u, _ := c.List(context.Background(), "potluck-launch-"); len(u) != 1 {
		t.Fatalf("list %v", u)
	}
	if p, _ := c.Adopt(context.Background(), "potluck-launch-a", ""); p != nil {
		t.Fatal("children are never adopted")
	}
	p.Signal(osx.SigKill)
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("not done")
	}
	e := p.Exit()
	if !e.Known {
		t.Fatal("exit unknown")
	}
	if runtime.GOOS != "windows" && (e.Signal != "KILL" || e.Code != 137) {
		t.Fatalf("exit %+v", e)
	}
	b, _ := os.ReadFile(exitPath)
	if len(b) == 0 {
		t.Fatal("no exit file")
	}
	if u, _ := c.List(context.Background(), "potluck-launch-"); len(u) != 0 {
		t.Fatalf("list after exit %v", u)
	}
}
