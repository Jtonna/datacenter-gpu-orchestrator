package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// End-to-end tests of the review fixes: a whole agent (Run, the program
// pipeline with the fake programs, the launch manager) against the fake
// Potluck, called over real HTTP as Potluck calls it. The units are faked by
// survRunner, which keeps them alive across an agent death as systemd does.

// httpAPI serves the agent's Potluck-facing API on a real listener.
func (e *tenv) httpAPI() string {
	srv := httptest.NewServer(e.a.Handler())
	e.t.Cleanup(srv.Close)
	return srv.URL
}

// die ends the agent process without any of its shutdown steps having an
// effect on the units: what tenv.stop cleans up is left alone.
func (e *tenv) die() {
	e.cancel()
	<-e.done
	e.cancel = nil
}

// restartOn starts a new agent process on the state, the programs, the clock
// and the units of e, whose process is dead.
func restartOn(t *testing.T, e *tenv, sr *survRunner, mod func(*Options, *Deps)) *tenv {
	t.Helper()
	return newTenvWith(t, func(o *Options, d *Deps) {
		o.StateDir, o.DataDir, o.ModelsDir = e.opt.StateDir, e.opt.DataDir, e.opt.ModelsDir
		d.Clock, d.Runner = e.clock, sr
		d.ExporterPorts, d.WorkerPort, d.HostPort = e.deps.ExporterPorts, e.deps.WorkerPort, e.deps.HostPort
		if mod != nil {
			mod(o, d)
		}
	})
}

func httpDo(t *testing.T, method, url string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// launchState reads GET /launch; ok is false while the agent answers 503.
func launchState(t *testing.T, api string) (st agentproto.LaunchState, ok bool) {
	t.Helper()
	code, body := httpDo(t, "GET", api+"/launch", nil)
	if code == http.StatusServiceUnavailable {
		return st, false
	}
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil {
		t.Fatalf("GET /launch: %d %s", code, body)
	}
	return st, true
}

// runningLaunch brings up an agent with launch ln-1 started over HTTP.
func runningLaunch(t *testing.T, sr *survRunner) (*tenv, string) {
	t.Helper()
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Runner = sr })
	e.start()
	e.waitReady()
	api := e.httpAPI()
	if code, body := httpDo(t, "POST", api+"/launch", workerReq("ln-1")); code != 202 {
		t.Fatalf("POST /launch: %d %s", code, body)
	}
	return e, api
}

const launchUnit = "potluck-launch-ln-1"

// Items 1 and 3. The agent dies without stopping anything: the launch unit
// and the exporter unit keep running, the exporter holds its port. The
// restarted agent must stop its own leftover exporter, register with the
// launch it still runs, never answer "none" for it, and enforce its lease.
func TestE2ERestartAfterUngracefulDeath(t *testing.T) {
	sr := newSurvRunner()
	e1, _ := runningLaunch(t, sr)
	e1.die()
	left := sr.revive(agentproto.NodeExporterUnit) // still running, still on its port
	sr.mu.Lock()
	sr.adoptErrs = 3 // systemd answers late: the re-adoption takes a moment
	sr.mu.Unlock()

	e2 := restartOn(t, e1, sr, func(o *Options, d *Deps) {
		d.RecoverRetry = 100 * time.Millisecond
		d.PortFree = func(addr string, port int) error {
			if port == e1.nodePort && left.alive() {
				return errors.New("bind: address already in use")
			}
			return nil
		}
	})
	api := e2.httpAPI()
	e2.start()
	sawStarting := false
	agenttest.Eventually(t, 10*time.Second, "launch state after the restart", func() bool {
		st, ok := launchState(t, api)
		if !ok {
			sawStarting = true
			return false
		}
		if st.State == agentproto.StateNone || st.LaunchID != "ln-1" {
			t.Fatalf("the restarted agent answers %q %q for a launch that runs", st.State, st.LaunchID)
		}
		return true
	})
	if !sawStarting {
		t.Fatal("no 503 while the launch was not re-adopted yet")
	}
	agenttest.Eventually(t, 10*time.Second, "registration", func() bool { return e2.pl.nRegisters() >= 1 })
	for i := 0; i < e2.pl.nRegisters(); i++ {
		e2.pl.mu.Lock()
		id := e2.pl.registers[i].LaunchID
		e2.pl.mu.Unlock()
		if id == nil || *id != "ln-1" {
			t.Fatalf("registration %d does not name the running launch: %v", i, id)
		}
	}
	if left.alive() || !sr.wasStopped(agentproto.NodeExporterUnit) {
		t.Fatal("the leftover exporter was not stopped")
	}
	p := sr.proc(launchUnit)
	if len(p.signals()) != 0 || !p.alive() {
		t.Fatal("the running launch was touched")
	}
	// The lease of the first process goes on: 20 s without a renewal end it.
	e2.clock.Advance(21 * time.Second)
	agenttest.Eventually(t, 10*time.Second, "lease expiry", func() bool {
		st, _ := launchState(t, api)
		return st.State == agentproto.StateExited && st.Exit.Reason == agentproto.ExitLeaseExpired
	})
}

// Item 1. The agent's own leftover exporter cannot be stopped (systemctl
// fails) and keeps its port: the agent still comes up and registers.
func TestE2ERestartWhenOwnExporterCannotBeStopped(t *testing.T) {
	sr := newSurvRunner()
	e1, _ := runningLaunch(t, sr)
	e1.die()
	left := sr.revive(agentproto.NodeExporterUnit)
	sr.setStubborn(agentproto.NodeExporterUnit)
	e2 := restartOn(t, e1, sr, func(o *Options, d *Deps) {
		d.PortFree = func(addr string, port int) error {
			if port == e1.nodePort && left.alive() {
				return errors.New("bind: address already in use")
			}
			return nil
		}
	})
	e2.start()
	agenttest.Eventually(t, 10*time.Second, "registration", func() bool { return e2.pl.nRegisters() >= 1 })
	if id := e2.pl.lastRegister().LaunchID; id == nil || *id != "ln-1" {
		t.Fatalf("registration does not name the running launch: %v", id)
	}
	left.finish()
}

// A port held by something that is not the agent's still blocks registration.
func TestForeignListenerOnExporterPortStillBlocks(t *testing.T) {
	sr := newSurvRunner()
	e := newTenvWith(t, func(o *Options, d *Deps) {
		d.Runner = sr
		d.PortFree = func(string, int) error { return errors.New("bind: address already in use") }
	})
	e.start()
	agenttest.Eventually(t, 5*time.Second, "plain reason", func() bool {
		s := e.a.Status()
		return s.LastError != nil && strings.Contains(*s.LastError, "is taken")
	})
	if e.pl.nRegisters() != 0 {
		t.Fatal("registered with a foreign listener on the exporter port")
	}
}

// Item 2. A launch that ignores SIGTERM, on a machine where systemctl kill
// and systemctl stop fail. The agent dies during the 10 s wait. The
// restarted agent goes on: SIGKILL, stop of the unit, then SIGKILL sent
// directly to the processes of the launch's unit, until the launch is gone.
func TestE2EStubbornLaunchIsKilledAcrossRestart(t *testing.T) {
	sr := newSurvRunner()
	e1, api1 := runningLaunch(t, sr)
	sr.setStubborn(launchUnit)
	p := sr.proc(launchUnit)
	if code, body := httpDo(t, "DELETE", api1+"/launch/ln-1", nil); code != 200 || !strings.Contains(body, `"state":"stopping"`) {
		t.Fatalf("DELETE: %d %s", code, body)
	}
	agenttest.Eventually(t, 5*time.Second, "TERM", func() bool { return len(p.signals()) == 1 })
	e1.die()

	e2 := restartOn(t, e1, sr, nil)
	e2.procs.mu.Lock()
	e2.procs.list = []osx.ProcInfo{
		{PID: 9001, Cgroup: "0::/system.slice/" + launchUnit + ".service"},
		{PID: 9002, Cgroup: "0::/system.slice/ssh.service"},
		{PID: 9003, Cgroup: "0::/system.slice/potluck-launch-ln-12.service"},
	}
	e2.procs.onSignal = func(pid int) {
		if pid == 9001 {
			p.finish()
		}
	}
	e2.procs.mu.Unlock()
	api2 := e2.httpAPI()
	e2.start()
	agenttest.Eventually(t, 10*time.Second, "stopping after the restart", func() bool {
		st, ok := launchState(t, api2)
		return ok && st.State == agentproto.StateStopping && st.LaunchID == "ln-1"
	})
	agenttest.Eventually(t, 20*time.Second, "the launch is gone", func() bool {
		e2.clock.Advance(250 * time.Millisecond)
		st, _ := launchState(t, api2)
		return st.State == agentproto.StateExited
	})
	st, _ := launchState(t, api2)
	if st.Exit.Reason != agentproto.ExitStopped {
		t.Fatal(st.Exit.Reason)
	}
	if s := p.signals(); len(s) < 2 || s[0] != osx.SigTerm || s[1] != osx.SigKill {
		t.Fatalf("signals %v, want TERM then KILL", s)
	}
	if !sr.wasStopped(launchUnit) {
		t.Fatal("no stop of the unit was tried")
	}
	e2.procs.mu.Lock()
	defer e2.procs.mu.Unlock()
	if len(e2.procs.signalled) != 1 || e2.procs.signalled[0] != 9001 {
		t.Fatalf("signalled directly: %v, want only the launch unit's pid 9001", e2.procs.signalled)
	}
}

// Item 4. Potluck's POST /launch is cancelled while systemd-run runs. The
// unit starts all the same, so the agent must hold it as its launch, with a
// record and a lease, and not report start_failed over a running unit.
func TestE2ECancelledLaunchRequest(t *testing.T) {
	sr := newSurvRunner()
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Runner = sr })
	e.start()
	e.waitReady()
	api := e.httpAPI()
	sr.mu.Lock()
	sr.launchGate, sr.launchEntered = make(chan struct{}), make(chan struct{}, 1)
	sr.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		b, _ := json.Marshal(workerReq("ln-1"))
		req, _ := http.NewRequestWithContext(ctx, "POST", api+"/launch", bytes.NewReader(b))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-sr.launchEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the unit start was not reached")
	}
	cancel() // Potluck gives up on the request
	<-sent
	time.Sleep(300 * time.Millisecond) // the server has seen the closed connection
	close(sr.launchGate)

	var st agentproto.LaunchState
	agenttest.Eventually(t, 10*time.Second, "the launch is held", func() bool {
		var ok bool
		st, ok = launchState(t, api)
		return ok && st.State != agentproto.StateNone
	})
	if st.LaunchID != "ln-1" || st.State != agentproto.StateStarting || st.LeaseExpiresInS == nil {
		t.Fatalf("launch after a cancelled request: %+v", st)
	}
	p := sr.proc(launchUnit)
	if p == nil || !p.alive() {
		t.Fatal("no unit")
	}
	// It has a lease: with no renewal it is stopped.
	e.clock.Advance(21 * time.Second)
	agenttest.Eventually(t, 10*time.Second, "lease expiry", func() bool { return !p.alive() })
}
