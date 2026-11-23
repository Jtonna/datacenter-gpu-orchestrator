package launch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// Host launches of release 0.1.9 (phase 6): expect_mib, rpc_file_load, the
// node's own device first, the per-launch key, read progress.

const testKey = "k0123456789abcdef-ABCDEF_x.y"

func consoleDevice() *agentproto.Device {
	return &agentproto.Device{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14000, FreeMiB: 12000, SharesSystemMemory: true}
}

// hostEnv is a manager with a model file and a known device.
func hostEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, true)
	e.device = consoleDevice()
	e.m = e.newManager()
	os.WriteFile(filepath.Join(e.cfg.ModelDir, "m.gguf"), make([]byte, 4096), 0o644)
	return e
}

func hostReq(id string, workers ...string) agentproto.LaunchRequest {
	h := &agentproto.HostSpec{Model: "m.gguf", Ctx: 4096, Parallel: 1, Workers: workers,
		RPCFileLoad: agentproto.RPCFileLoadRequired, ExtraArgs: []string{"--n-gpu-layers", "999", "-lv", "4"}}
	if len(workers) > 0 {
		h.TensorSplit = make([]float64, len(workers)+1)
		for i := range h.TensorSplit {
			h.TensorSplit[i] = 1
		}
	}
	return agentproto.LaunchRequest{LaunchID: id, Role: agentproto.RoleHost, Release: "0.1.0", LeaseTTLS: 20,
		MinMemAvailableMiB: 1000, Host: h}
}

func TestHostExpectMiB(t *testing.T) {
	ctx := context.Background()
	t.Run("MemAvailable minus expect below the floor", func(t *testing.T) {
		e := hostEnv(t)
		e.mem.set(9000)
		r := hostReq("h1")
		r.Host.ExpectMiB = 8500 // 9000 - 8500 < 1000
		_, ref := e.m.Start(ctx, r)
		if ref == nil || ref.Status != 409 || ref.Body.Reason != agentproto.ReasonWouldNotFit || !strings.Contains(ref.Body.Message, "MemAvailable") {
			t.Fatalf("%+v", ref)
		}
		if len(e.runner.specs) != 0 {
			t.Fatal("a refused host was started")
		}
	})
	t.Run("device free below expect", func(t *testing.T) {
		e := hostEnv(t)
		e.device.FreeMiB = 5000
		e.mem.set(20000)
		r := hostReq("h1")
		r.Host.ExpectMiB = 6000
		_, ref := e.m.Start(ctx, r)
		if ref == nil || ref.Body.Reason != agentproto.ReasonWouldNotFit || !strings.Contains(ref.Body.Message, "Vulkan0") {
			t.Fatalf("%+v", ref)
		}
	})
	t.Run("fits", func(t *testing.T) {
		e := hostEnv(t)
		r := hostReq("h1")
		r.Host.ExpectMiB = 6000 // 12000 - 6000 >= 1000, device 12000 free
		if _, ref := e.m.Start(ctx, r); ref != nil {
			t.Fatal(ref)
		}
	})
	t.Run("no expect_mib makes no test (a request without the field)", func(t *testing.T) {
		e := hostEnv(t)
		e.device.FreeMiB = 10
		if _, ref := e.m.Start(ctx, hostReq("h1")); ref != nil {
			t.Fatal(ref)
		}
	})
}

func TestHostRPCFileLoad(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		mode, strict string
		want         string
	}{
		{agentproto.RPCFileLoadRequired, "", "required"},
		{agentproto.RPCFileLoadOff, "", "0"},
		{agentproto.RPCFileLoadOff, "strict", "0"}, // rpc_file_load wins
		{"", "strict", "required"},                 // a request without the field
		{"", "", "1"},
	} {
		e := hostEnv(t)
		r := hostReq("h1")
		r.Host.RPCFileLoad, r.Host.StrictFileLoad = c.mode, c.strict != ""
		if _, ref := e.m.Start(ctx, r); ref != nil {
			t.Fatal(ref)
		}
		if got := strings.Join(e.runner.lastSpec().Env, " "); got != EnvRPCFileLoad+"="+c.want {
			t.Errorf("%+v: env %q", c, got)
		}
	}
	e := hostEnv(t)
	r := hostReq("h1")
	r.Host.RPCFileLoad = "1" // the silent fallback to pushed weights is not asked for this way
	if _, ref := e.m.Start(ctx, r); ref == nil || ref.Body.Reason != agentproto.ReasonInvalidRequest {
		t.Fatalf("%+v", ref)
	}
}

func TestHostNamesItsOwnDeviceFirst(t *testing.T) {
	ctx := context.Background()
	e := hostEnv(t)
	e.setProbe("192.0.2.122:50052", true)
	e.setProbe("192.0.2.123:50052", true)
	if _, ref := e.m.Start(ctx, hostReq("h1", "192.0.2.122:50052", "192.0.2.123:50052")); ref != nil {
		t.Fatal(ref)
	}
	got := strings.Join(e.runner.lastSpec().Args, " ")
	if !strings.Contains(got, "--rpc 192.0.2.122:50052,192.0.2.123:50052 --device Vulkan0,RPC0,RPC1 --tensor-split 1,1,1") {
		t.Fatal(got)
	}
	// A single-node host names no device: llama-server uses its own.
	e = hostEnv(t)
	if _, ref := e.m.Start(ctx, hostReq("h2")); ref != nil {
		t.Fatal(ref)
	}
	if got := strings.Join(e.runner.lastSpec().Args, " "); strings.Contains(got, "--device") || strings.Contains(got, "--rpc") {
		t.Fatal(got)
	}
	// With workers and no known device the order cannot be made: refused.
	e = hostEnv(t)
	e.device = nil
	e.setProbe("192.0.2.122:50052", true)
	if _, ref := e.m.Start(ctx, hostReq("h3", "192.0.2.122:50052")); ref == nil || ref.Body.Reason != agentproto.ReasonInvalidRequest {
		t.Fatalf("%+v", ref)
	}
	// The flags the agent sets itself are refused in extra_args.
	for _, f := range [][]string{{"--device", "CUDA0"}, {"-dev", "Vulkan0"}, {"--device=Vulkan0"}, {"--api-key", "x"}, {"--api-key-file", "/tmp/k"}} {
		e = hostEnv(t)
		r := hostReq("h4")
		r.Host.ExtraArgs = f
		if _, ref := e.m.Start(ctx, r); ref == nil || ref.Status != 400 || ref.Body.Reason != agentproto.ReasonInvalidRequest {
			t.Errorf("%v: %+v", f, ref)
		}
	}
}

func TestHostAPIKey(t *testing.T) {
	ctx := context.Background()
	e := hostEnv(t)
	e.run()
	r := hostReq("h1")
	r.Host.APIKey = testKey
	st, ref := e.m.Start(ctx, r)
	if ref != nil {
		t.Fatal(ref)
	}
	keyPath := filepath.Join(e.cfg.StateDir, "launch", "h1.key")
	sp := e.runner.lastSpec()
	args := strings.Join(sp.Args, " ")
	if !strings.Contains(args, "--api-key-file "+keyPath) {
		t.Fatal(args)
	}
	b, err := os.ReadFile(keyPath)
	if err != nil || string(b) != testKey+"\n" {
		t.Fatalf("%q %v", b, err)
	}
	if fi, _ := os.Stat(keyPath); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	// The key is nowhere the agent reports or keeps its launch.
	stj, _ := json.Marshal(st)
	rec, _ := os.ReadFile(filepath.Join(e.cfg.StateDir, "launch", "h1.json"))
	for what, s := range map[string]string{"argv": args, "env": strings.Join(sp.Env, " "), "state": string(stj),
		"record": string(rec), "GET /launch": func() string { b, _ := json.Marshal(e.m.State()); return string(b) }()} {
		if strings.Contains(s, testKey) {
			t.Errorf("the key is in the %s: %s", what, s)
		}
	}
	// It is removed when the launch ends.
	if _, err := e.m.Stop("h1", false); err != nil {
		t.Fatal(err)
	}
	e.runner.proc("potluck-launch-h1").finish(runner.Exit{Known: true, Code: 0})
	e.waitState(agentproto.StateExited)
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key file left after exit: %v", err)
	}

	// A bad key is refused without being echoed.
	for _, k := range []string{"short", "has space in it 0123", strings.Repeat("a", 257), "quote\"0123456789abcdef"} {
		e := hostEnv(t)
		r := hostReq("h2")
		r.Host.APIKey = k
		_, ref := e.m.Start(ctx, r)
		if ref == nil || ref.Body.Reason != agentproto.ReasonInvalidRequest || strings.Contains(ref.Body.Message, k) {
			t.Errorf("%q: %+v", k, ref)
		}
	}
}

// The key file goes with the launch's files when the launch is replaced or
// a start fails.
func TestHostKeyRemovedOnStartFailure(t *testing.T) {
	e := hostEnv(t)
	e.runner.startErr = errors.New("systemd-run failed")
	r := hostReq("h1")
	r.Host.APIKey = testKey
	st, ref := e.m.Start(context.Background(), r)
	if ref != nil || st.State != agentproto.StateExited || st.Exit.Reason != agentproto.ExitStartFailed {
		t.Fatalf("%+v %+v", st, ref)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.StateDir, "launch", "h1.key")); !os.IsNotExist(err) {
		t.Fatalf("key file left after a failed start: %v", err)
	}
}

func TestHostReadProgress(t *testing.T) {
	e := hostEnv(t)
	var rchar atomic.Int64
	m := New(e.cfg, Deps{Clock: e.clock, Runner: e.runner, Mem: e.mem, Sockets: e.sockets, Procs: e.procs, KLog: e.klog,
		PortFree: func(string, int) error { return nil },
		Device:   func() (agentproto.Device, bool) { return *e.device, true },
		Gate:     e.gate, Programs: fakePrograms{dir: e.cfg.ProgramRoot},
		ReadChars: func(pid int) (int64, error) { return rchar.Load(), nil }})
	e.m = m
	e.run()
	if _, ref := m.Start(context.Background(), hostReq("h1")); ref != nil {
		t.Fatal(ref)
	}
	if st := m.State(); st.Load == nil || st.Load.State != agentproto.LoadWaiting || st.Load.BytesLoaded != nil {
		t.Fatalf("%+v", st.Load)
	}
	rchar.Store(1000)
	agenttest.Eventually(t, 5*time.Second, "bytes", func() bool {
		l := m.State().Load
		return l.BytesLoaded != nil && *l.BytesLoaded == 1000 && *l.BytesTotal == 4096 && l.State == agentproto.LoadReceiving
	})
	// More than the file (libraries, other reads) is capped at its size.
	rchar.Store(1 << 30)
	agenttest.Eventually(t, 5*time.Second, "capped", func() bool {
		l := m.State().Load
		return l.BytesLoaded != nil && *l.BytesLoaded == 4096
	})
}

func TestParseRChar(t *testing.T) {
	n, err := osx.ParseRChar("rchar: 323934931\nwchar: 323929600\nsyscr: 632687\n")
	if err != nil || n != 323934931 {
		t.Fatal(n, err)
	}
	if _, err := osx.ParseRChar("wchar: 1\n"); err == nil {
		t.Fatal("no rchar line is an error")
	}
}

// The watchdog covers a host as any launch: SIGKILL at once below the floor.
func TestHostWatchdog(t *testing.T) {
	e := hostEnv(t)
	e.run()
	if _, ref := e.m.Start(context.Background(), hostReq("h1")); ref != nil {
		t.Fatal(ref)
	}
	e.mem.set(900)
	p := e.runner.proc("potluck-launch-h1")
	agenttest.Eventually(t, 5*time.Second, "SIGKILL", func() bool {
		for _, s := range p.signals() {
			if s == osx.SigKill {
				return true
			}
		}
		return false
	})
	p.finish(runner.Exit{Known: true, Code: 137, Signal: "KILL"})
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitWatchdog {
		t.Fatalf("%+v", st.Exit)
	}
}

// A forced stop of a host kills at once, like a worker's.
func TestHostForcedStop(t *testing.T) {
	e := hostEnv(t)
	e.run()
	if _, ref := e.m.Start(context.Background(), hostReq("h1")); ref != nil {
		t.Fatal(ref)
	}
	st, err := e.m.Stop("h1", true)
	if err != nil || st.State != agentproto.StateStopping || st.StopReason != agentproto.ExitForced {
		t.Fatalf("%+v %v", st, err)
	}
	p := e.runner.proc("potluck-launch-h1")
	agenttest.Eventually(t, 5*time.Second, "SIGKILL", func() bool { return len(p.signals()) > 0 && p.signals()[0] == osx.SigKill })
	p.finish(runner.Exit{Known: true, Code: 137, Signal: "KILL"})
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitForced {
		t.Fatalf("%+v", st.Exit)
	}
}
