package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/hw"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// CR-9: the agent reports its watchdog floor at registration and in every
// heartbeat.
func TestWatchdogFloorReported(t *testing.T) {
	e := newTenvWith(t, func(o *Options, d *Deps) { o.WatchdogFloorMiB = 1500 })
	e.start()
	e.waitReady()
	if got := e.pl.lastRegister().WatchdogFloorMiB; got != 1500 {
		t.Fatalf("registration watchdog_floor_mib = %d, want 1500", got)
	}
	if got := e.pl.lastBeat().WatchdogFloorMiB; got != 1500 {
		t.Fatalf("heartbeat watchdog_floor_mib = %d, want 1500", got)
	}
}

// CR-9: POST /launch with an expect_mib that does not fit the node's device
// (here one that shares system memory: MemAvailable 12000, floor 1000) is
// answered 409 would_not_fit and starts nothing.
func TestWouldNotFitRefusal(t *testing.T) {
	e := newTenv(t)
	e.start()
	e.waitReady()
	req := workerReq("ln-1")
	req.Worker.ExpectMiB = 11500
	w := e.do("POST", "/launch", req, fromPotluck)
	var ref agentproto.LaunchRefusal
	json.Unmarshal(w.Body.Bytes(), &ref)
	if w.Code != 409 || ref.Reason != agentproto.ReasonWouldNotFit {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var st agentproto.LaunchState
	json.Unmarshal(e.do("GET", "/launch", nil, fromPotluck).Body.Bytes(), &st)
	if st.State != agentproto.StateNone {
		t.Fatalf("state %s after the refusal", st.State)
	}
	// The same launch without expect_mib (a release 0.1.0 scheduler) starts.
	if w := e.do("POST", "/launch", workerReq("ln-2"), fromPotluck); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// CR-9: a stop while the leave marker exists deregisters first, so the node
// shows left and not lost; without the marker a stop stays silent.
func TestLeaveOnPurpose(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "disabled")
	deregs := func(e *tenv) []string {
		e.pl.mu.Lock()
		defer e.pl.mu.Unlock()
		return append([]string(nil), e.pl.deregs...)
	}
	t.Run("no marker", func(t *testing.T) {
		e := newTenvWith(t, func(o *Options, d *Deps) { o.LeaveMarker = marker })
		e.start()
		e.waitReady()
		e.stop()
		if got := deregs(e); len(got) != 0 {
			t.Fatalf("deregistered without the marker: %v", got)
		}
	})
	t.Run("marker", func(t *testing.T) {
		e := newTenvWith(t, func(o *Options, d *Deps) { o.LeaveMarker = marker })
		e.start()
		e.waitReady()
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		e.stop()
		got := deregs(e)
		if len(got) != 1 || got[0] != "POST /api/pool/nodes/02:00:00:00:00:ab/deregister" {
			t.Fatalf("deregister calls: %v", got)
		}
	})
}

// mutMem is a MemAvailable the test can change.
type mutMem struct {
	mu    sync.Mutex
	avail int64
}

func (m *mutMem) MemInfo() (osx.MemInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return osx.MemInfo{TotalMiB: 15258, AvailableMiB: m.avail}, nil
}
func (m *mutMem) set(v int64) { m.mu.Lock(); m.avail = v; m.mu.Unlock() }

// discreteHW is a machine whose one AMD GPU is not integrated, so the
// heuristic does not mark its Vulkan device as sharing system memory.
type discreteHW struct{ fakeHW }

func (discreteHW) GPUs() []hw.GPUInfo {
	return []hw.GPUInfo{{GPU: agentproto.GPU{Vendor: "amd", Model: "AMD Custom GPU 0405", Integrated: false}, PCI: "0000:20:00.0"}}
}

func launchCode(t *testing.T, e *tenv, id string, expect int64) (int, agentproto.LaunchRefusal) {
	t.Helper()
	req := workerReq(id)
	req.MinMemAvailableMiB = 0
	req.Worker.ExpectMiB = expect
	w := e.do("POST", "/launch", req, fromPotluck)
	var ref agentproto.LaunchRefusal
	json.Unmarshal(w.Body.Bytes(), &ref)
	return w.Code, ref
}

// CR-9 review, A1: a device read that fails with something other than "no
// devices" leaves the agent ready with no device. A launch with expect_mib is
// then held to the MemAvailable test, not started unchecked.
func TestWouldNotFitWithNoDeviceRead(t *testing.T) {
	e := newTenv(t)
	e.devOut.Store("llama-server: error while loading shared libraries\n") // no "Available devices:" line
	e.start()
	e.waitReady()
	if d := e.pl.lastBeat().Devices; len(d) != 0 {
		t.Fatalf("devices %+v, want none", d)
	}
	code, ref := launchCode(t, e, "ln-1", 11500) // MemAvailable 12000, floor 1000
	if code != 409 || ref.Reason != agentproto.ReasonWouldNotFit || !strings.Contains(ref.Message, "no device figure") {
		t.Fatalf("%d %+v, want 409 would_not_fit with no device figure", code, ref)
	}
	if code, ref := launchCode(t, e, "ln-2", 11000); code != 202 {
		t.Fatalf("%d %+v: fits MemAvailable minus the floor", code, ref)
	}
}

// CR-9 review, A2: --shared-memory decides how the device is reported and
// tested. Auto keeps the heuristic's true, and reports a non-CUDA device that
// the heuristic left false as sharing too (any doubt); false reports false.
func TestSharedMemoryOption(t *testing.T) {
	shares := func(t *testing.T, mod func(*Options, *Deps)) bool {
		t.Helper()
		e := newTenvWith(t, mod)
		e.start()
		e.waitReady()
		d := e.pl.lastBeat().Devices
		if len(d) != 1 {
			t.Fatalf("devices %+v", d)
		}
		return d[0].SharesSystemMemory
	}
	if !shares(t, nil) {
		t.Fatal("auto, integrated console GPU: want shares_system_memory true")
	}
	if !shares(t, func(o *Options, d *Deps) { d.Hardware = discreteHW{} }) {
		t.Fatal("auto, Vulkan device not known to have memory of its own: want true")
	}
	if shares(t, func(o *Options, d *Deps) { o.SharedMemory = "false"; d.Hardware = discreteHW{} }) {
		t.Fatal("--shared-memory false: want false")
	}
	if shares(t, func(o *Options, d *Deps) { o.SharedMemory = "false" }) {
		t.Fatal("--shared-memory false wins over the heuristic: want false")
	}
}

// CR-9 review, B7: the device figure is read at registration and after a
// launch exits, so it can be old. An old figure is still used, and on a
// device held to system memory the MemAvailable test catches what happened
// since.
func TestStaleDeviceFigure(t *testing.T) {
	mem := &mutMem{avail: 12000}
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Mem = mem })
	e.start()
	e.waitReady()
	e.clock.Advance(time.Hour) // the device read is an hour old
	mem.set(6000)
	agenttest.Eventually(t, 5*time.Second, "an old device figure in the heartbeat", func() bool {
		d := e.pl.lastBeat().Devices
		return len(d) == 1 && d[0].ReadAgeS >= 3600 && d[0].FreeMiB == 14010
	})
	code, ref := launchCode(t, e, "ln-1", 8000) // the device says 14010 MiB free
	if code != 409 || ref.Reason != agentproto.ReasonWouldNotFit || !strings.Contains(ref.Message, "6000") {
		t.Fatalf("%d %+v, want would_not_fit from MemAvailable 6000", code, ref)
	}
	// The device figure itself is still a bound.
	mem.set(30000)
	code, ref = launchCode(t, e, "ln-2", 14011)
	if code != 409 || !strings.Contains(ref.Message, "Vulkan0") {
		t.Fatalf("%d %+v, want would_not_fit from the device's 14010 MiB", code, ref)
	}
}

// String is the log so far (syncBuf is in fix3a_test.go).
func (l *syncBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// leaveEnv is an agent with the leave marker in place and its log captured.
func leaveEnv(t *testing.T, mod func(*Options, *Deps)) (*tenv, *syncBuf) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "disabled")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	e := newTenvWith(t, func(o *Options, d *Deps) {
		o.LeaveMarker = marker
		d.Logger = slog.New(slog.NewTextHandler(logs, nil))
		if mod != nil {
			mod(o, d)
		}
	})
	return e, logs
}

// CR-9 review, B4 and B7: a deregister that fails is logged as a failure
// with its status, at Warn, not as "deregistered".
func TestLeaveDeregisterFailureIsLoggedAsSuch(t *testing.T) {
	e, logs := leaveEnv(t, nil)
	e.pl.set(func() { e.pl.deregStatus = http.StatusServiceUnavailable })
	e.start()
	e.waitReady()
	e.stop()
	out := logs.String()
	if strings.Contains(out, "deregistered before stopping") {
		t.Fatalf("a 503 was logged as a success:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "status=503") {
		t.Fatalf("want a WARN line with status=503:\n%s", out)
	}
}

// CR-9 review, B4: a deregister answered 200 is logged at Info.
func TestLeaveDeregisterSuccessIsLoggedAtInfo(t *testing.T) {
	e, logs := leaveEnv(t, nil)
	e.start()
	e.waitReady()
	e.stop()
	if out := logs.String(); !strings.Contains(out, "level=INFO msg=\"leave marker found: deregistered before stopping\"") {
		t.Fatalf("no Info line for the deregister:\n%s", out)
	}
}

// CR-9 review, B7: the second deregister (the uninstall script already
// deregistered) is answered 404; the agent only logs it, as already left.
func TestLeaveSecondDeregisterAnswers404(t *testing.T) {
	e, logs := leaveEnv(t, nil)
	e.pl.set(func() { e.pl.left = true }) // the uninstall script came first
	e.start()
	e.waitReady()
	e.stop()
	out := logs.String()
	if len(e.pl.deregs) != 1 || !strings.Contains(out, "status=404") || !strings.Contains(out, "already") {
		t.Fatalf("calls %v, log:\n%s", e.pl.deregs, out)
	}
	if strings.Contains(out, "level=WARN msg=\"leave marker") {
		t.Fatalf("a 404 (already left) is not a failure:\n%s", out)
	}
}

// CR-9 review, B7: a deregister that hangs is given up after 3 s; the agent
// still stops.
func TestLeaveDeregisterHangIsBounded(t *testing.T) {
	e, logs := leaveEnv(t, nil)
	hang := make(chan struct{})
	e.pl.set(func() { e.pl.deregHang = hang })
	defer close(hang)
	e.start()
	e.waitReady()
	began := time.Now()
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(6 * time.Second):
		t.Fatal("the agent did not stop within 6 s of a hanging deregister")
	}
	if d := time.Since(began); d < 2500*time.Millisecond || d > 5*time.Second {
		t.Fatalf("stopped after %v, want about the 3 s bound", d)
	}
	e.cancel = nil
	e.a.Launch.Reset(context.Background())
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "deregister") {
		t.Fatalf("no warning for the hanging deregister:\n%s", out)
	}
}

// CR-9 review, B3: a leave with a launch running deregisters and leaves the
// launch to Potluck's stop and the machine: the agent does not stop it on the
// way out (on a bare install the launch unit outlives the agent, section
// 6.1; the uninstall script stops it).
func TestLeaveWithALaunchDoesNotStopIt(t *testing.T) {
	sr := newSurvRunner("potluck-launch-ln-1")
	e, _ := leaveEnv(t, func(o *Options, d *Deps) { d.Runner = sr })
	e.leftLaunch(time.Hour)
	e.start()
	agenttest.Eventually(t, 5*time.Second, "registration with the launch", func() bool {
		return e.pl.nRegisters() >= 1 && e.pl.lastRegister().LaunchID != nil
	})
	e.cancel()
	<-e.done
	e.cancel = nil
	if len(e.pl.deregs) != 1 {
		t.Fatalf("deregister calls %v", e.pl.deregs)
	}
	p := sr.proc("potluck-launch-ln-1")
	if !p.alive() || len(p.signals()) != 0 {
		t.Fatalf("the agent stopped the launch on the way out: alive %v, signals %v", p.alive(), p.signals())
	}
	p.finish()
}

// CR-9 review, B7: while its launch is stopping, the agent's heartbeat
// carries stop_reason (and stop_message when there is one).
func TestHeartbeatCarriesStopReason(t *testing.T) {
	sr := newSurvRunner("potluck-launch-ln-1")
	sr.setStubborn("potluck-launch-ln-1") // SIGTERM is ignored: the launch stays stopping
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Runner = sr })
	e.leftLaunch(time.Hour)
	e.start()
	agenttest.Eventually(t, 5*time.Second, "a heartbeat", func() bool { return e.pl.nBeats() > 0 })
	if hb := e.pl.lastBeat(); hb.StopReason != "" || hb.StopMessage != "" {
		t.Fatalf("stop_reason %q on a running launch", hb.StopReason)
	}
	if _, err := e.a.Launch.Stop("ln-1", false); err != nil {
		t.Fatal(err)
	}
	agenttest.Eventually(t, 5*time.Second, "stop_reason in the heartbeat", func() bool {
		return e.pl.lastBeat().StopReason == agentproto.ExitStopped
	})
	sr.proc("potluck-launch-ln-1").finish()
}
