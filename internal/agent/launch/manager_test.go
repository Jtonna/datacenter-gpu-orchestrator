package launch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

func TestWorkerLifecycleAndCommand(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	st := e.startReady("ln-1")
	if st.Port != 50052 || st.Build != "a01665d30" || st.Command[0] != "rpc-server" {
		t.Fatalf("%+v", st)
	}
	sp := e.runner.lastSpec()
	if sp.Unit != "potluck-launch-ln-1" {
		t.Fatal(sp.Unit)
	}
	want := []string{"--host", "192.0.2.101", "--port", "50052", "--model-dir", e.cfg.ModelDir}
	if strings.Join(sp.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("args %v", sp.Args)
	}
	if st.Load != nil {
		t.Fatal("worker load is null until a progress line")
	}
	if *st.ClientConnected {
		t.Fatal("no client yet")
	}
	// A client connects and the worker reads from the share.
	e.sockets.set(50052, true, true)
	e.appendLog("ln-1", "[file_load] progress bytes=100 total=400\n")
	agenttest.Eventually(t, 5*time.Second, "progress", func() bool {
		s := e.m.State()
		return s.Load != nil && *s.Load.BytesLoaded == 100 && *s.ClientConnected && s.Load.State == agentproto.LoadReceiving
	})
	e.appendLog("ln-1", "[file_load] progress bytes=400 total=400\n")
	agenttest.Eventually(t, 5*time.Second, "done", func() bool {
		s := e.m.State()
		return s.Load != nil && s.Load.State == agentproto.LoadDone
	})
	if e.m.Role() != agentproto.RoleWorker || e.m.CurrentID() == nil || *e.m.Model() != "Devstral.gguf" {
		t.Fatal("role/current/model")
	}
	// Busy.
	_, ref := e.m.Start(context.Background(), workerReq("ln-2"))
	if ref == nil || ref.Status != 409 || ref.Body.Reason != agentproto.ReasonBusy || ref.Body.LaunchID != "ln-1" {
		t.Fatalf("busy: %+v", ref)
	}
	// Stop: TERM, the process exits, the port closes, then exited.
	st, err := e.m.Stop("ln-1", false)
	if err != nil || st.State != agentproto.StateStopping {
		t.Fatalf("stop: %v %v", st.State, err)
	}
	p := e.runner.proc("potluck-launch-ln-1")
	agenttest.Eventually(t, 5*time.Second, "TERM", func() bool { return len(p.signals()) == 1 && p.signals()[0] == osx.SigTerm })
	// Port still open: still stopping.
	e.settle(5)
	if s := e.m.State().State; s != agentproto.StateStopping {
		t.Fatalf("port open must keep stopping, got %s", s)
	}
	e.portClosed()
	st = e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitStopped || st.LeaseExpiresInS != nil || *st.Exit.Code != 0 {
		t.Fatalf("exit %+v", st.Exit)
	}
	if st.Load == nil || st.Load.State != agentproto.LoadDone {
		t.Fatal("load keeps its final values")
	}
	if len(st.Exit.LogTail) == 0 || st.Exit.LogTail[0] != "Starting RPC server v3.0.0" {
		t.Fatalf("tail %v", st.Exit.LogTail)
	}
	<-e.exits
	// Idempotent stop of an exited launch.
	st2, err := e.m.Stop("ln-1", true)
	if err != nil || st2.State != agentproto.StateExited || st2.Exit.Reason != agentproto.ExitStopped {
		t.Fatalf("idempotent: %v %v", st2, err)
	}
	if e.m.Busy() || e.m.CurrentID() != nil || e.m.Role() != agentproto.RoleIdle {
		t.Fatal("exited launch does not count")
	}
	// The next launch starts and the previous log is gone.
	if _, ref := e.m.Start(context.Background(), workerReq("ln-2")); ref != nil {
		t.Fatal(ref)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.LogDir, "ln-1.log")); !os.IsNotExist(err) {
		t.Fatal("previous log kept")
	}
}

func TestStopWrongID(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	if _, err := e.m.Stop("ln-x", true); !errors.Is(err, ErrNotCurrent) {
		t.Fatal(err)
	}
	if len(e.runner.proc("potluck-launch-ln-1").signals()) != 0 {
		t.Fatal("wrong id stopped something")
	}
	if e.m.State().State != agentproto.StateReady {
		t.Fatal("state changed")
	}
}

func TestForcedStop(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	e.m.Stop("ln-1", true)
	p := e.runner.proc("potluck-launch-ln-1")
	agenttest.Eventually(t, 5*time.Second, "KILL", func() bool { s := p.signals(); return len(s) == 1 && s[0] == osx.SigKill })
	e.portClosed()
	st := e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitForced || *st.Exit.Signal != "KILL" || *st.Exit.Code != 137 {
		t.Fatalf("%+v", st.Exit)
	}
}

func TestStopThatHangs(t *testing.T) {
	e := newEnv(t, true)
	e.runner.ignoreTerm = true
	e.run()
	e.startReady("ln-1")
	e.m.Stop("ln-1", false)
	p := e.runner.proc("potluck-launch-ln-1")
	agenttest.Eventually(t, 5*time.Second, "TERM", func() bool { return len(p.signals()) == 1 })
	e.settle(5)
	if s := e.m.State(); s.State != agentproto.StateStopping || !e.m.Busy() {
		t.Fatalf("hanging stop must stay stopping and busy: %s", s.State)
	}
	if _, ref := e.m.Start(context.Background(), workerReq("ln-2")); ref == nil || ref.Body.Reason != agentproto.ReasonBusy {
		t.Fatal("stopping is busy")
	}
	e.clock.Advance(10 * time.Second) // the 10 s wait
	agenttest.Eventually(t, 5*time.Second, "KILL", func() bool { s := p.signals(); return len(s) == 2 && s[1] == osx.SigKill })
	e.portClosed()
	st := e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitStopped || *st.Exit.Signal != "KILL" {
		t.Fatalf("%+v", st.Exit)
	}
}

func TestLeaseRenewAndExpiry(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	leasePath := filepath.Join(e.cfg.StateDir, "launch", "ln-1.lease")
	e.clock.Advance(15 * time.Second)
	if !e.m.Renew("ln-1") {
		t.Fatal("renew")
	}
	if e.m.Renew("ln-x") {
		t.Fatal("renew of another id")
	}
	b, _ := os.ReadFile(leasePath)
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if time.Duration(n) != e.clock.Now()+20*time.Second {
		t.Fatalf("lease file %d, want boottime deadline %d", n, e.clock.Now()+20*time.Second)
	}
	if st := e.m.State(); *st.LeaseExpiresInS != 20 {
		t.Fatalf("lease_expires_in_s %v", *st.LeaseExpiresInS)
	}
	e.clock.Advance(19 * time.Second)
	e.settle(5)
	if e.m.State().State != agentproto.StateReady {
		t.Fatal("expired early")
	}
	e.clock.Advance(2 * time.Second)
	e.waitState(agentproto.StateStopping)
	e.portClosed()
	st := e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitLeaseExpired {
		t.Fatal(st.Exit.Reason)
	}
	if e.m.Renew("ln-1") {
		t.Fatal("renew of an exited launch")
	}
	if _, err := os.Stat(leasePath); !os.IsNotExist(err) {
		t.Fatal("lease file left")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.errs) != 1 || !strings.Contains(e.errs[0], "lease_expired") {
		t.Fatalf("last_error %v", e.errs)
	}
}

func TestLeaseKeptAcrossAgentRestart(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	e.clock.Advance(5 * time.Second)
	e.m.Renew("ln-1") // deadline now+20
	e.stop()
	// The agent restarts; the unit keeps running; a new manager adopts it.
	e.clock.Advance(12 * time.Second)
	e.m = e.newManager()
	if err := e.m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.m.State()
	if st.State != agentproto.StateReady || st.LaunchID != "ln-1" || *st.LeaseExpiresInS != 8 {
		t.Fatalf("after restart: %s %v", st.State, st.LeaseExpiresInS)
	}
	if e.m.CurrentID() == nil || *e.m.CurrentID() != "ln-1" {
		t.Fatal("current id")
	}
	e.run()
	e.clock.Advance(9 * time.Second)
	e.waitState(agentproto.StateStopping)
	e.portClosed()
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitLeaseExpired {
		t.Fatal(st.Exit.Reason)
	}
}

func TestRestartWithoutLeaseFileExpires(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	e.stop()
	os.Remove(filepath.Join(e.cfg.StateDir, "launch", "ln-1.lease"))
	e.m = e.newManager()
	e.m.Recover(context.Background())
	e.run()
	e.waitState(agentproto.StateStopping)
	e.portClosed()
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitLeaseExpired {
		t.Fatal(st.Exit.Reason)
	}
}

func TestRestartDockerInstallDropsLaunch(t *testing.T) {
	e := newEnv(t, false) // child runner: launches die with the container
	e.run()
	e.startReady("ln-1")
	e.stop()
	e.m = e.newManager()
	e.m.Recover(context.Background())
	if st := e.m.State(); st.State != agentproto.StateNone {
		t.Fatalf("docker restart keeps no launch: %s", st.State)
	}
	if files, _ := filepath.Glob(filepath.Join(e.cfg.StateDir, "launch", "*")); len(files) != 0 {
		t.Fatalf("state files left: %v", files)
	}
}

func TestRestartAfterUnitEnded(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	e.stop()
	// The unit is killed while the agent is away (not by the agent).
	e.runner.proc("potluck-launch-ln-1").finish(runner.Exit{Known: true, Code: 139, Signal: "SEGV"})
	e.portClosed()
	e.m = e.newManager()
	e.m.Recover(context.Background())
	e.run()
	st := e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitCrashed || *st.Exit.Signal != "SEGV" {
		t.Fatalf("%+v", st.Exit)
	}
}

func TestExitedStateSurvivesRestart(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	e.m.Stop("ln-1", true)
	e.portClosed()
	e.waitState(agentproto.StateExited)
	e.stop()
	e.m = e.newManager()
	e.m.Recover(context.Background())
	if st := e.m.State(); st.State != agentproto.StateExited || st.Exit.Reason != agentproto.ExitForced {
		t.Fatalf("%+v", st)
	}
}

func TestWatchdog(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	e.mem.set(5000)
	agenttest.Eventually(t, 5*time.Second, "low water", func() bool { return e.m.State().MemLowWaterMiB == 5000 })
	e.mem.set(999)
	p := e.runner.proc("potluck-launch-ln-1")
	agenttest.Eventually(t, 5*time.Second, "KILL", func() bool { s := p.signals(); return len(s) == 1 && s[0] == osx.SigKill })
	e.portClosed()
	st := e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitWatchdog || !strings.Contains(st.Exit.Message, "999") || st.MemLowWaterMiB != 999 {
		t.Fatalf("%+v low=%d", st.Exit, st.MemLowWaterMiB)
	}
}

func TestWatchdogRate(t *testing.T) {
	// The watchdog must read MemAvailable at least four times a second with
	// the default tick.
	m := New(Config{}, Deps{})
	if m.cfg.Tick > 250*time.Millisecond {
		t.Fatalf("default tick %v is slower than 4 Hz", m.cfg.Tick)
	}
	if m.cfg.WatchdogFloorMiB != 1000 || m.cfg.StopWait != 10*time.Second || m.cfg.PortRetry != 3*time.Second {
		t.Fatal("contract defaults")
	}
}

func TestReset(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	// Another launch unit (left over) and processes in the program folder.
	e.runner.Start(context.Background(), runner.Spec{Unit: "potluck-launch-old"})
	root := e.cfg.ProgramRoot
	e.procs.list = []osx.ProcInfo{
		{PID: 501, Exe: filepath.Join(root, "0.1.0", "rpc-server")},
		{PID: 502, Exe: filepath.Join(root, "0.0.9", "llama-server") + " (deleted)"},
		{PID: 503, Exe: filepath.Join(root, "0.1.0", "node_exporter")},
		{PID: 504, Exe: "/usr/bin/rpc-server"},
	}
	ans := e.m.Reset(context.Background())
	if len(ans.Killed) != 4 {
		t.Fatalf("killed %+v", ans.Killed)
	}
	if strings.Join(intsToStr(e.procs.killed), ",") != "501,502" {
		t.Fatalf("procs killed %v", e.procs.killed)
	}
	if e.runner.proc("potluck-launch-old").alive() {
		t.Fatal("other unit not killed")
	}
	e.portClosed()
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitReset {
		t.Fatal(st.Exit.Reason)
	}
	b, _ := json.Marshal(ans)
	if !strings.Contains(string(b), `"killed":[`) {
		t.Fatal(string(b))
	}
}

func intsToStr(v []int) []string {
	var out []string
	for _, x := range v {
		out = append(out, strconv.Itoa(x))
	}
	return out
}

func TestStartFailedAndCrashed(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	// Exits before ready and before any load: start_failed.
	e.m.Start(context.Background(), workerReq("ln-1"))
	e.appendLog("ln-1", "error: invalid argument --bogus\n")
	e.runner.proc("potluck-launch-ln-1").finish(runner.Exit{Known: true, Code: 1})
	st := e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitStartFailed || st.Exit.LogTail[0] != "error: invalid argument --bogus" {
		t.Fatalf("%+v", st.Exit)
	}
	// Exits after ready on its own: crashed.
	e.startReady("ln-2")
	e.runner.proc("potluck-launch-ln-2").finish(runner.Exit{Known: true, Code: 134, Signal: "ABRT"})
	e.portClosed()
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitCrashed {
		t.Fatal(st.Exit.Reason)
	}
	// Killed by a signal the agent did not send, named by the kernel log: oom_kill.
	e.startReady("ln-3")
	p := e.runner.proc("potluck-launch-ln-3")
	e.klog.oom[p.pid] = true
	p.finish(runner.Exit{Known: true, Code: 137, Signal: "KILL"})
	e.portClosed()
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitOOMKill {
		t.Fatal(st.Exit.Reason)
	}
	// Runner refuses to start: start_failed at once.
	e.runner.startErr = errors.New("systemd-run failed")
	st, ref := e.m.Start(context.Background(), workerReq("ln-4"))
	if ref != nil || st.State != agentproto.StateExited || st.Exit.Reason != agentproto.ExitStartFailed {
		t.Fatalf("%+v %v", st, ref)
	}
}

func TestDecideExitOrder(t *testing.T) {
	tok := func(timeout, required bool) *logScan {
		return &logScan{timeoutTok: timeout, requiredTok: required, requiredWhy: "no_capability"}
	}
	yes := func() bool { return true }
	cases := []struct {
		name, role, cause string
		scan              *logScan
		ready, began      bool
		sig               string
		want, wantMsg     string
	}{
		{"agent cause beats tokens", agentproto.RoleHost, agentproto.ExitWatchdog, tok(true, true), true, true, "KILL", agentproto.ExitWatchdog, ""},
		{"timeout token", agentproto.RoleHost, "", tok(true, true), false, false, "SEGV", agentproto.ExitFileLoadTimeout, ""},
		{"required token", agentproto.RoleHost, "", tok(false, true), false, false, "", agentproto.ExitFileLoadRequired, "no_capability"},
		{"tokens are host only", agentproto.RoleWorker, "", tok(true, true), false, false, "", agentproto.ExitStartFailed, ""},
		{"start_failed beats oom", agentproto.RoleWorker, "", tok(false, false), false, false, "KILL", agentproto.ExitStartFailed, ""},
		{"oom", agentproto.RoleWorker, "", tok(false, false), true, false, "KILL", agentproto.ExitOOMKill, ""},
		{"oom needs a signal", agentproto.RoleWorker, "", tok(false, false), true, false, "", agentproto.ExitCrashed, ""},
		{"load began counts as started", agentproto.RoleWorker, "", tok(false, false), false, true, "", agentproto.ExitCrashed, ""},
	}
	for _, c := range cases {
		got, msg := decideExit(c.role, c.cause, "", c.scan, c.ready, c.began, c.sig, yes)
		if got != c.want || msg != c.wantMsg {
			t.Errorf("%s: got %s %q", c.name, got, msg)
		}
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	host := func(id string) agentproto.LaunchRequest {
		return agentproto.LaunchRequest{LaunchID: id, Role: agentproto.RoleHost, Release: "0.1.0", LeaseTTLS: 20,
			MinMemAvailableMiB: 100, Host: &agentproto.HostSpec{Model: "m.gguf", Ctx: 8192, Parallel: 2,
				Workers: []string{"192.0.2.122:50052", "192.0.2.123:50052"}, TensorSplit: []float64{40, 30, 30}}}
	}
	check := func(t *testing.T, ref *Refusal, status int, reason string) {
		t.Helper()
		if ref == nil {
			t.Fatalf("not refused, want %s", reason)
		}
		if ref.Status != status || ref.Body.Reason != reason {
			t.Fatalf("got %d %s (%s), want %d %s", ref.Status, ref.Body.Reason, ref.Body.Message, status, reason)
		}
	}
	t.Run("paused (the pause race)", func(t *testing.T) {
		e := newEnv(t, true)
		e.gate.paused = true
		_, ref := e.m.Start(ctx, workerReq("a"))
		check(t, ref, 409, agentproto.ReasonPaused)
	})
	t.Run("release", func(t *testing.T) {
		e := newEnv(t, true)
		r := workerReq("a")
		r.Release = "0.2.0"
		_, ref := e.m.Start(ctx, r)
		check(t, ref, 409, agentproto.ReasonReleaseMismatch)
		e.gate.ready = false
		_, ref = e.m.Start(ctx, workerReq("a"))
		check(t, ref, 409, agentproto.ReasonReleaseMismatch)
	})
	t.Run("model source", func(t *testing.T) {
		e := newEnv(t, true)
		e.gate.reachable = false
		_, ref := e.m.Start(ctx, workerReq("a"))
		check(t, ref, 503, agentproto.ReasonModelSourceUnreachable)
		_, ref = e.m.Start(ctx, host("b"))
		check(t, ref, 503, agentproto.ReasonModelSourceUnreachable)
		// A worker without model_dir does not need the share.
		r := workerReq("c")
		r.Worker.ModelDir = false
		if _, ref := e.m.Start(ctx, r); ref != nil {
			t.Fatal(ref)
		}
	})
	t.Run("model not found", func(t *testing.T) {
		e := newEnv(t, true)
		_, ref := e.m.Start(ctx, host("a"))
		check(t, ref, 404, agentproto.ReasonModelNotFound)
	})
	t.Run("low memory", func(t *testing.T) {
		e := newEnv(t, true)
		e.mem.set(8999)
		_, ref := e.m.Start(ctx, workerReq("a"))
		check(t, ref, 507, agentproto.ReasonLowMemory)
	})
	t.Run("worker unreachable", func(t *testing.T) {
		e := newEnv(t, true)
		os.WriteFile(filepath.Join(e.cfg.ModelDir, "m.gguf"), []byte("GGUF"), 0o644)
		e.setProbe("192.0.2.122:50052", true)
		_, ref := e.m.Start(ctx, host("a"))
		check(t, ref, 502, agentproto.ReasonWorkerUnreachable)
		if len(ref.Body.FailedWorkers) != 1 || ref.Body.FailedWorkers[0] != "192.0.2.123:50052" {
			t.Fatal(ref.Body.FailedWorkers)
		}
	})
	t.Run("port in use, retried", func(t *testing.T) {
		e := newEnv(t, true)
		e.portErr = errors.New("address in use")
		start := time.Now()
		_, ref := e.m.Start(ctx, workerReq("a"))
		check(t, ref, 409, agentproto.ReasonPortInUse)
		if time.Since(start) < e.cfg.PortRetry {
			t.Fatal("bind failure not retried for the retry window")
		}
		// Frees during the retry window: accepted.
		e.cfg.PortRetry = 5 * time.Second
		e.m = e.newManager()
		go func() { time.Sleep(30 * time.Millisecond); e.mu.Lock(); e.portErr = nil; e.mu.Unlock() }()
		if _, ref := e.m.Start(ctx, workerReq("b")); ref != nil {
			t.Fatal(ref)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		e := newEnv(t, true)
		bad := []agentproto.LaunchRequest{
			{LaunchID: "../x", Role: agentproto.RoleWorker, Worker: &agentproto.WorkerSpec{}},
			{LaunchID: "a", Role: "chef"},
			{LaunchID: "a", Role: agentproto.RoleWorker},
			{LaunchID: "a", Role: agentproto.RoleWorker, Worker: &agentproto.WorkerSpec{ExtraArgs: []string{"--port=1"}}},
		}
		h := host("a")
		h.Host.Model = "../../etc/passwd"
		bad = append(bad, h)
		h2 := host("a")
		h2.Host.TensorSplit = []float64{50, 50}
		bad = append(bad, h2)
		h3 := host("a")
		h3.Host.ExtraArgs = []string{"--rpc", "x"}
		bad = append(bad, h3)
		for i, r := range bad {
			r.Release = "0.1.0"
			_, ref := e.m.Start(ctx, r)
			if ref == nil || ref.Status != 400 || ref.Body.Reason != agentproto.ReasonInvalidRequest {
				t.Errorf("case %d: %+v", i, ref)
			}
		}
		e.cfg.PushOnly = true
		e.m = e.newManager()
		_, ref := e.m.Start(ctx, workerReq("a"))
		check(t, ref, 400, agentproto.ReasonInvalidRequest)
	})
}

func TestHostLaunch(t *testing.T) {
	e := newEnv(t, true)
	e.cfg.CUDA = true
	e.m = e.newManager()
	e.run()
	os.WriteFile(filepath.Join(e.cfg.ModelDir, "m.gguf"), []byte("GGUF"), 0o644)
	e.setProbe("192.0.2.122:50052", true)
	req := agentproto.LaunchRequest{LaunchID: "h1", Role: agentproto.RoleHost, Release: "0.1.0", LeaseTTLS: 20,
		Host: &agentproto.HostSpec{Model: "m.gguf", Ctx: 8192, Parallel: 2, Workers: []string{"192.0.2.122:50052"},
			TensorSplit: []float64{50, 50}, StrictFileLoad: true, ExtraArgs: []string{"--flash-attn"}}}
	st, ref := e.m.Start(context.Background(), req)
	if ref != nil {
		t.Fatal(ref)
	}
	if st.Port != 50061 || st.Load == nil || st.Load.State != agentproto.LoadWaiting || st.ClientConnected != nil {
		t.Fatalf("%+v", st)
	}
	sp := e.runner.lastSpec()
	got := strings.Join(sp.Args, " ")
	want := "--host 192.0.2.101 --port 50061 -m " + filepath.Join(e.cfg.ModelDir, "m.gguf") + " -c 16384 --parallel 2 --rpc 192.0.2.122:50052 --tensor-split 50,50 --flash-attn"
	if got != want {
		t.Fatalf("args\n got %s\nwant %s", got, want)
	}
	// strict_file_load is the fork's LLAMA_RPC_FILE_LOAD in the host's environment.
	if strings.Join(sp.Env, " ") != "CUDA_DEVICE_ORDER=PCI_BUS_ID LLAMA_RPC_FILE_LOAD=required" {
		t.Fatal(sp.Env)
	}
	e.appendLog("h1", "load_progress: fraction=0.25\n")
	agenttest.Eventually(t, 5*time.Second, "fraction", func() bool {
		s := e.m.State()
		return s.Load.Fraction != nil && *s.Load.Fraction == 0.25 && s.Load.State == agentproto.LoadReceiving
	})
	e.mu.Lock()
	e.healthy = true
	e.mu.Unlock()
	e.waitState(agentproto.StateReady)
	// A strict-mode refusal token wins over the exit status.
	e.appendLog("h1", "rpc_file_load_required: endpoint=192.0.2.122:50052 reason=identity_mismatch\n")
	e.settle(5)
	e.runner.proc("potluck-launch-h1").finish(runner.Exit{Known: true, Code: 1})
	st = e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitFileLoadRequired || st.Exit.Message != "identity_mismatch" {
		t.Fatalf("%+v", st.Exit)
	}
}

func TestNoneState(t *testing.T) {
	e := newEnv(t, true)
	b, _ := json.Marshal(e.m.State())
	if string(b) != `{"state":"none"}` {
		t.Fatal(string(b))
	}
}

func TestHostFileLoadEnv(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	os.WriteFile(filepath.Join(e.cfg.ModelDir, "m.gguf"), []byte("GGUF"), 0o644)
	req := agentproto.LaunchRequest{LaunchID: "h2", Role: agentproto.RoleHost, Release: "0.1.0", LeaseTTLS: 20,
		Host: &agentproto.HostSpec{Model: "m.gguf", Ctx: 4096, Parallel: 1, StrictFileLoad: false}}
	if _, ref := e.m.Start(context.Background(), req); ref != nil {
		t.Fatal(ref)
	}
	sp := e.runner.lastSpec()
	if strings.Join(sp.Env, " ") != "LLAMA_RPC_FILE_LOAD=1" {
		t.Fatal(sp.Env)
	}
	for _, a := range sp.Args {
		if strings.Contains(a, "strict") {
			t.Fatal("no strict flag exists:", sp.Args)
		}
	}
}

func TestWorkerHasNoFileLoadEnv(t *testing.T) {
	e := newEnv(t, false)
	e.run()
	if _, ref := e.m.Start(context.Background(), workerReq("w9")); ref != nil {
		t.Fatal(ref)
	}
	for _, v := range e.runner.lastSpec().Env {
		if strings.HasPrefix(v, "LLAMA_RPC_FILE_LOAD=") {
			t.Fatal(v)
		}
	}
}

// advanceUntil moves the fake clock in steps until cond holds.
func (e *env) advanceUntil(what string, step time.Duration, cond func() bool) {
	e.t.Helper()
	agenttest.Eventually(e.t, 10*time.Second, what, func() bool {
		if cond() {
			return true
		}
		e.clock.Advance(step)
		e.settle(2)
		return cond()
	})
}

var unitProcs = []osx.ProcInfo{
	{PID: 700, Cgroup: "0::/system.slice/potluck-launch-ln-1.service"},
	{PID: 701, Cgroup: "0::/system.slice/potluck-launch-ln-10.service"},
	{PID: 702, Cgroup: "0::/system.slice/ssh.service"},
	{PID: 1, Cgroup: "0::/init.scope"},
}

// An ordinary stop escalates: SIGTERM, after the 10 s wait SIGKILL, then a
// stop of the unit, then SIGKILL sent directly to the processes of the
// launch's own unit, and only those.
func TestStopEscalation(t *testing.T) {
	e := newEnv(t, true)
	e.runner.ignoreTerm = true
	e.runner.stopErr = errors.New("systemctl stop: failed to connect to bus")
	e.run()
	e.startReady("ln-1")
	p := e.runner.proc("potluck-launch-ln-1")
	p.mu.Lock()
	p.killDeaf = 1 << 30 // systemctl kill is accepted and does nothing
	p.mu.Unlock()
	e.procs.set(unitProcs)
	e.procs.mu.Lock()
	e.procs.onKill = func(pid int) {
		if pid == 700 {
			p.finish(runner.Exit{Known: true, Code: 137, Signal: "KILL"})
		}
	}
	e.procs.mu.Unlock()

	e.m.Stop("ln-1", false)
	agenttest.Eventually(t, 5*time.Second, "TERM", func() bool { return len(p.signals()) == 1 })
	e.settle(5)
	if s := p.signals(); len(s) != 1 || s[0] != osx.SigTerm {
		t.Fatalf("before the 10 s wait: %v", s)
	}
	e.clock.Advance(10 * time.Second)
	agenttest.Eventually(t, 5*time.Second, "KILL", func() bool { s := p.signals(); return len(s) == 2 && s[1] == osx.SigKill })
	e.settle(5)
	if len(p.signals()) != 2 || len(e.runner.stops()) != 0 {
		t.Fatal("the next attempt came before the retry wait")
	}
	e.clock.Advance(time.Second)
	agenttest.Eventually(t, 5*time.Second, "stop of the unit", func() bool { return len(e.runner.stops()) == 1 })
	e.settle(5)
	if len(e.procs.kills()) != 0 {
		t.Fatal("direct kill before the retry wait")
	}
	e.clock.Advance(250 * time.Millisecond) // the stop failed: the short wait
	e.portClosed()
	st := e.waitState(agentproto.StateExited)
	if k := e.procs.kills(); len(k) != 1 || k[0] != 700 {
		t.Fatalf("signalled directly: %v, want only pid 700 of the launch unit", k)
	}
	if st.Exit.Reason != agentproto.ExitStopped || len(p.signals()) != 2 || e.runner.stops()[0] != "potluck-launch-ln-1" {
		t.Fatalf("%s, signals %v, stops %v", st.Exit.Reason, p.signals(), e.runner.stops())
	}
}

// The watchdog sends SIGKILL at once, with no SIGTERM, and escalates in the
// same way when that does not work.
func TestWatchdogKillsFirstThenEscalates(t *testing.T) {
	e := newEnv(t, true)
	e.runner.stopKills = true
	e.run()
	e.startReady("ln-1")
	p := e.runner.proc("potluck-launch-ln-1")
	p.mu.Lock()
	p.killErrs = 1 << 30 // systemctl kill never works
	p.mu.Unlock()
	e.mem.set(900)
	agenttest.Eventually(t, 5*time.Second, "KILL", func() bool { return len(p.signals()) == 1 })
	e.settle(5)
	if s := p.signals(); len(s) != 1 || s[0] != osx.SigKill || len(e.runner.stops()) != 0 {
		t.Fatalf("signals %v, stops %v", s, e.runner.stops())
	}
	if e.m.State().State != agentproto.StateStopping {
		t.Fatal("exited before the process is gone")
	}
	e.clock.Advance(250 * time.Millisecond)
	e.portClosed()
	st := e.waitState(agentproto.StateExited)
	if st.Exit.Reason != agentproto.ExitWatchdog || len(e.runner.stops()) != 1 {
		t.Fatalf("%s, stops %v", st.Exit.Reason, e.runner.stops())
	}
}

// When nothing works the kill is tried for ever, but never more than about
// four times a second.
func TestKillNeverGivesUpAndIsRateBounded(t *testing.T) {
	e := newEnv(t, true)
	e.runner.stopErr = errors.New("systemctl stop: failed to connect to bus")
	e.run()
	e.startReady("ln-1")
	p := e.runner.proc("potluck-launch-ln-1")
	p.mu.Lock()
	p.killErrs = 1 << 30
	p.mu.Unlock()
	attempts := func() int {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return e.m.cur.killTries
	}
	e.m.Stop("ln-1", true)
	for i := 0; i < 100; i++ { // one second, 10 ms at a time
		e.clock.Advance(10 * time.Millisecond)
		e.settle(2)
	}
	if n := attempts(); n < 4 || n > 5 {
		t.Fatalf("%d kill attempts in one second, want four or five", n)
	}
	for i := 0; i < 240; i++ { // one more minute
		e.clock.Advance(250 * time.Millisecond)
		e.settle(2)
	}
	if n := attempts(); n < 200 {
		t.Fatalf("the kill was given up: %d attempts after a minute", n)
	}
	// Attempt 2 is the one stop of the unit; from attempt 4 on every attempt
	// is SIGKILL together with the direct kill.
	e.procs.mu.Lock()
	lists := e.procs.lists
	e.procs.mu.Unlock()
	if n := attempts(); len(e.runner.stops()) != 1 || len(p.signals()) < n-3 || lists < n-3 {
		t.Fatalf("%d attempts: %d stops, %d SIGKILLs, %d direct tries", n, len(e.runner.stops()), len(p.signals()), lists)
	}
	if e.m.State().State != agentproto.StateStopping {
		t.Fatal("not stopping")
	}
	p.mu.Lock()
	p.killErrs = 0
	p.mu.Unlock()
	e.portClosed()
	e.advanceUntil("exited", 250*time.Millisecond, func() bool { return e.m.State().State == agentproto.StateExited })
}

// A restarted agent does not trust a kill recorded by the earlier process.
func TestRestartKillsStoppingLaunchAgain(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.startReady("ln-1")
	p := e.runner.proc("potluck-launch-ln-1")
	p.mu.Lock()
	p.killDeaf = 1 // the kill of the first agent process had no effect
	p.mu.Unlock()
	e.m.Stop("ln-1", true)
	agenttest.Eventually(t, 5*time.Second, "first KILL", func() bool { return len(p.signals()) == 1 })
	e.stop()
	e.m = e.newManager()
	if err := e.m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := e.m.State(); st.State != agentproto.StateStopping {
		t.Fatal(st.State)
	}
	e.run()
	e.portClosed()
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitForced || len(p.signals()) != 2 {
		t.Fatalf("%s after %d signals", st.Exit.Reason, len(p.signals()))
	}
}

// The unit start is not tied to the HTTP request that asked for it.
func TestStartSurvivesCancelledRequest(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st, ref := e.m.Start(ctx, workerReq("ln-1"))
	if ref != nil || st.State != agentproto.StateStarting {
		t.Fatalf("%v %+v", ref, st)
	}
	if e.m.CurrentID() == nil {
		t.Fatal("the launch has no record")
	}
}

// A failed start stops the unit name, so a unit that started anyway does not
// keep the port with no lease or watchdog.
func TestStartFailureStopsUnit(t *testing.T) {
	e := newEnv(t, true)
	e.run()
	e.runner.startErr = errors.New("systemd-run: timed out")
	st, ref := e.m.Start(context.Background(), workerReq("ln-1"))
	if ref != nil || st.State != agentproto.StateExited || st.Exit.Reason != agentproto.ExitStartFailed {
		t.Fatalf("%v %+v", ref, st)
	}
	e.runner.mu.Lock()
	defer e.runner.mu.Unlock()
	if len(e.runner.stopped) != 1 || e.runner.stopped[0] != "potluck-launch-ln-1" {
		t.Fatalf("stopped %v", e.runner.stopped)
	}
}

func TestLaunchUnitHardening(t *testing.T) {
	e := newEnv(t, true)
	e.cfg.LibraryPath = "/opt/llama/lib"
	e.m = e.newManager()
	e.run()
	e.startReady("ln-1")
	sp := e.runner.lastSpec()
	if sp.OOMScoreAdjust != 900 {
		t.Fatalf("OOM score adjust %d", sp.OOMScoreAdjust)
	}
	if !strings.Contains(strings.Join(sp.Env, " "), "LD_LIBRARY_PATH=/opt/llama/lib") {
		t.Fatalf("env %v", sp.Env)
	}
}

func TestInUnitCgroup(t *testing.T) {
	for cg, want := range map[string]bool{
		"0::/system.slice/potluck-launch-ln-1.service":                   true,
		"1:name=systemd:/system.slice/potluck-launch-ln-1.service\n0::/": true,
		"0::/system.slice/potluck-launch-ln-1.service/sub":               true,
		"0::/system.slice/potluck-launch-ln-10.service":                  false,
		"0::/system.slice/xpotluck-launch-ln-1.service":                  false,
		"": false,
	} {
		if got := InUnitCgroup(cg, "potluck-launch-ln-1"); got != want {
			t.Errorf("%q: %v", cg, got)
		}
	}
}

// verifyingProcs is a process table in which a pid can have gone to another
// process since the list was read.
type verifyingProcs struct {
	*fakeProcs
	now map[int]string // pid -> its cgroup at the moment of the signal
}

func (v verifyingProcs) SignalVerified(pid int, s osx.Sig, check func(string) bool) (bool, error) {
	if !check(v.now[pid]) {
		return false, nil
	}
	return true, v.fakeProcs.Signal(pid, s)
}

// The direct kill reads a process's cgroup again right before the signal: a
// pid that was reused by a process outside the launch unit gets nothing.
func TestDirectKillRechecksCgroup(t *testing.T) {
	e := newEnv(t, true)
	unit := "0::/system.slice/potluck-launch-ln-1.service"
	e.procs.set([]osx.ProcInfo{{PID: 700, Cgroup: unit}, {PID: 703, Cgroup: unit}})
	m := New(e.cfg, Deps{Clock: e.clock, Runner: e.runner, Procs: verifyingProcs{e.procs, map[int]string{
		700: "0::/system.slice/ssh.service", // pid 700 ended and was given to sshd
		703: unit,
	}}})
	if n := m.killUnitProcs("potluck-launch-ln-1"); n != 1 {
		t.Fatalf("%d processes counted as signalled", n)
	}
	if k := e.procs.kills(); len(k) != 1 || k[0] != 703 {
		t.Fatalf("signalled %v, want only 703", k)
	}
}

// The stop of the unit can block for many seconds (systemctl stop waits for
// the unit). It runs outside the manager's lock: meanwhile GET /launch
// answers, the loop goes on reading memory, a lease call is answered, and no
// second kill attempt is started beside it.
func TestHangingStopStepBlocksNothing(t *testing.T) {
	e := newEnv(t, true)
	e.runner.stopHang = make(chan struct{})
	e.runner.stopKills = true
	e.run()
	e.startReady("ln-1")
	p := e.runner.proc("potluck-launch-ln-1")
	p.mu.Lock()
	p.killDeaf = 1 << 30
	p.mu.Unlock()
	e.m.Stop("ln-1", true) // attempt 1: SIGKILL, no effect
	agenttest.Eventually(t, 5*time.Second, "KILL", func() bool { return len(p.signals()) == 1 })
	e.clock.Advance(time.Second) // attempt 2: the stop of the unit, which hangs
	agenttest.Eventually(t, 5*time.Second, "stop of the unit", func() bool { return len(e.runner.stops()) == 1 })

	answered := make(chan agentproto.LaunchState, 1)
	go func() { answered <- e.m.State() }()
	select {
	case st := <-answered:
		if st.State != agentproto.StateStopping {
			t.Fatal(st.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GET /launch does not answer while the stop of the unit hangs")
	}
	e.mem.set(4321)
	agenttest.Eventually(t, 2*time.Second, "the loop still reads memory", func() bool { return e.m.State().MemAvailableMiB == 4321 })
	if !e.m.Renew("ln-1") || e.m.CurrentID() == nil || !e.m.Busy() {
		t.Fatal("lease and state calls")
	}
	e.clock.Advance(time.Minute)
	e.settle(10)
	if len(p.signals()) != 1 || len(e.runner.stops()) != 1 {
		t.Fatalf("a second attempt ran beside the hanging one: %d signals, %d stops", len(p.signals()), len(e.runner.stops()))
	}
	close(e.runner.stopHang)
	e.portClosed()
	if st := e.waitState(agentproto.StateExited); st.Exit.Reason != agentproto.ExitForced {
		t.Fatal(st.Exit.Reason)
	}
}
