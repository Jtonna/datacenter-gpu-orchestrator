package launch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// CR-9: a worker launch whose expect_mib does not fit is refused before
// anything is started.
func TestWouldNotFit(t *testing.T) {
	ctx := context.Background()
	req := func(id string, expect int64) agentproto.LaunchRequest {
		r := workerReq(id)
		r.MinMemAvailableMiB = 0
		r.Worker.ExpectMiB = expect
		return r
	}
	withDevice := func(d agentproto.Device) *env {
		e := newEnv(t, true)
		e.device = &d
		e.m = e.newManager()
		return e
	}
	refused := func(t *testing.T, e *env, ref *Refusal, parts ...string) {
		t.Helper()
		if ref == nil || ref.Status != 409 || ref.Body.Reason != agentproto.ReasonWouldNotFit {
			t.Fatalf("got %+v, want 409 would_not_fit", ref)
		}
		for _, p := range parts {
			if !strings.Contains(ref.Body.Message, p) {
				t.Fatalf("message %q does not name %s", ref.Body.Message, p)
			}
		}
		if st := e.m.State(); st.State != agentproto.StateNone {
			t.Fatalf("state %s after a refusal: something was started", st.State)
		}
	}
	shared := agentproto.Device{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14000, FreeMiB: 13500, SharesSystemMemory: true}
	dedicated := agentproto.Device{Name: "CUDA0", Backend: "cuda", TotalMiB: 8192, FreeMiB: 8000}

	t.Run("shared memory: MemAvailable minus expect below the floor", func(t *testing.T) {
		e := withDevice(shared) // MemAvailable 12000, floor 1000
		_, ref := e.m.Start(ctx, req("a", 11001))
		refused(t, e, ref, "12000", "11001", "1000")
		if _, ref := e.m.Start(ctx, req("b", 11000)); ref != nil {
			t.Fatalf("exactly at the floor must start: %+v", ref.Body)
		}
	})
	t.Run("dedicated memory: the device's free memory", func(t *testing.T) {
		e := withDevice(dedicated)
		e.mem.set(2000) // system memory is not what the weights take
		_, ref := e.m.Start(ctx, req("a", 8001))
		refused(t, e, ref, "8000", "8001", "CUDA0")
		if _, ref := e.m.Start(ctx, req("b", 8000)); ref != nil {
			t.Fatalf("exactly the free memory must start: %+v", ref.Body)
		}
	})
	t.Run("without expect_mib the agent behaves as before", func(t *testing.T) {
		e := withDevice(shared)
		e.mem.set(1500)
		if _, ref := e.m.Start(ctx, req("a", 0)); ref != nil {
			t.Fatalf("refused without expect_mib: %+v", ref.Body)
		}
	})
}

// CR-9 review, A1: with expect_mib and no device figure the agent never
// starts unchecked. It falls back to the MemAvailable test; when MemAvailable
// cannot be read either, it refuses.
func TestWouldNotFitWithoutADevice(t *testing.T) {
	ctx := context.Background()
	req := func(id string, expect int64) agentproto.LaunchRequest {
		r := workerReq(id)
		r.MinMemAvailableMiB = 0
		r.Worker.ExpectMiB = expect
		return r
	}
	t.Run("no device known: the MemAvailable test applies", func(t *testing.T) {
		e := newEnv(t, true) // no device; MemAvailable 12000, floor 1000
		_, ref := e.m.Start(ctx, req("a", 11001))
		if ref == nil || ref.Status != 409 || ref.Body.Reason != agentproto.ReasonWouldNotFit {
			t.Fatalf("got %+v, want 409 would_not_fit", ref)
		}
		for _, p := range []string{"12000", "11001", "1000"} {
			if !strings.Contains(ref.Body.Message, p) {
				t.Fatalf("message %q does not name %s", ref.Body.Message, p)
			}
		}
		if st := e.m.State(); st.State != agentproto.StateNone {
			t.Fatalf("state %s after a refusal", st.State)
		}
		if _, ref := e.m.Start(ctx, req("b", 11000)); ref != nil {
			t.Fatalf("exactly at the floor must start: %+v", ref.Body)
		}
	})
	t.Run("no device and no MemAvailable: refused", func(t *testing.T) {
		e := newEnv(t, true)
		e.mem.setErr(errors.New("meminfo unreadable"))
		_, ref := e.m.Start(ctx, req("a", 100))
		if ref == nil || ref.Status != 409 || ref.Body.Reason != agentproto.ReasonWouldNotFit ||
			!strings.Contains(ref.Body.Message, "no memory figure could be read") {
			t.Fatalf("got %+v, want 409 would_not_fit saying no memory figure could be read", ref)
		}
		if st := e.m.State(); st.State != agentproto.StateNone {
			t.Fatalf("state %s after a refusal", st.State)
		}
	})
	t.Run("without expect_mib an unreadable MemAvailable stays low_memory", func(t *testing.T) {
		e := newEnv(t, true)
		e.mem.setErr(errors.New("meminfo unreadable"))
		_, ref := e.m.Start(ctx, req("a", 0))
		if ref == nil || ref.Body.Reason != agentproto.ReasonLowMemory {
			t.Fatalf("got %+v, want low_memory", ref)
		}
	})
}

// CR-9 review, A2: the agent takes the stricter of the device test and the
// MemAvailable test. The MemAvailable test applies unless the device is known
// to have memory of its own (a CUDA device, or --shared-memory false).
func TestWouldNotFitTakesTheStricterTest(t *testing.T) {
	ctx := context.Background()
	req := func(id string, expect int64) agentproto.LaunchRequest {
		r := workerReq(id)
		r.MinMemAvailableMiB = 0
		r.Worker.ExpectMiB = expect
		return r
	}
	start := func(t *testing.T, mode string, d agentproto.Device, avail, expect int64) *Refusal {
		t.Helper()
		e := newEnv(t, true)
		e.cfg.SharedMemory = mode
		e.device = &d
		e.mem.set(avail)
		e.m = e.newManager()
		_, ref := e.m.Start(ctx, req("a", expect))
		if ref != nil && ref.Body.Reason != agentproto.ReasonWouldNotFit {
			t.Fatalf("refused with %s, want would_not_fit or a start: %+v", ref.Body.Reason, ref.Body)
		}
		return ref
	}
	// A console whose device the heuristic did not mark as shared: Vulkan
	// reports nearly all the RAM as free, so only MemAvailable is true.
	console := agentproto.Device{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14947, FreeMiB: 14900, SharesSystemMemory: false}
	cuda := agentproto.Device{Name: "CUDA0", Backend: "cuda", TotalMiB: 8192, FreeMiB: 8000}
	shared := agentproto.Device{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14000, FreeMiB: 5000, SharesSystemMemory: true}

	t.Run("console device reporting shares_system_memory false: MemAvailable applies", func(t *testing.T) {
		ref := start(t, "", console, 9000, 8500) // device free 14900 would pass
		if ref == nil || !strings.Contains(ref.Body.Message, "9000") || !strings.Contains(ref.Body.Message, "watchdog floor") {
			t.Fatalf("got %+v, want the MemAvailable refusal", ref)
		}
		if ref := start(t, "auto", console, 9000, 8000); ref != nil {
			t.Fatalf("fits MemAvailable minus the floor: %+v", ref.Body)
		}
	})
	t.Run("shared device: its own free figure applies too", func(t *testing.T) {
		ref := start(t, "", shared, 12000, 6000) // MemAvailable would pass
		if ref == nil || !strings.Contains(ref.Body.Message, "Vulkan0") || !strings.Contains(ref.Body.Message, "5000") {
			t.Fatalf("got %+v, want the device refusal", ref)
		}
	})
	t.Run("cuda device in auto: the device figure only", func(t *testing.T) {
		if ref := start(t, "", cuda, 2000, 8000); ref != nil {
			t.Fatalf("a dedicated device is not held to system memory: %+v", ref.Body)
		}
	})
	t.Run("--shared-memory true: a cuda device is held to MemAvailable too", func(t *testing.T) {
		if ref := start(t, "true", cuda, 2000, 8000); ref == nil {
			t.Fatal("started with MemAvailable 2000 and expect 8000")
		}
	})
	t.Run("--shared-memory false: a vulkan device is held to its own figure only", func(t *testing.T) {
		if ref := start(t, "false", console, 2000, 14000); ref != nil {
			t.Fatalf("refused: %+v", ref.Body)
		}
		if ref := start(t, "false", console, 2000, 14901); ref == nil {
			t.Fatal("started beyond the device's free memory")
		}
	})
}

func TestTestsSystemMemory(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		backend string
		shares  bool
		want    bool
	}{
		{"", "cuda", false, false},
		{"auto", "cuda", false, false},
		{"auto", "cuda", true, true},
		{"auto", "vulkan", false, true},
		{"auto", "vulkan", true, true},
		{"auto", "metal", false, true},
		{"true", "cuda", false, true},
		{"false", "vulkan", true, false},
	} {
		d := agentproto.Device{Name: "X0", Backend: tc.backend, SharesSystemMemory: tc.shares}
		if got := TestsSystemMemory(tc.mode, d); got != tc.want {
			t.Errorf("mode %q backend %s shares %v: got %v, want %v", tc.mode, tc.backend, tc.shares, got, tc.want)
		}
	}
}

// CR-9: the cause is reported as soon as the stop is decided, while the
// launch is still stopping.
func TestStopReasonWhileStopping(t *testing.T) {
	t.Run("watchdog", func(t *testing.T) {
		e := newEnv(t, true)
		e.run()
		e.startReady("ln-1")
		if st := e.m.State(); st.StopReason != "" {
			t.Fatalf("stop_reason %q on a ready launch", st.StopReason)
		}
		p := e.runner.proc("potluck-launch-ln-1")
		p.mu.Lock()
		p.killErrs = 1 << 30 // the process does not go: the launch stays stopping
		p.mu.Unlock()
		e.mem.set(900)
		var st agentproto.LaunchState
		agenttest.Eventually(t, 5*time.Second, "stopping", func() bool {
			st = e.m.State()
			return st.State == agentproto.StateStopping
		})
		if st.StopReason != agentproto.ExitWatchdog || !strings.Contains(st.StopMessage, "900") || st.Exit != nil {
			t.Fatalf("stopping launch reports stop_reason %q (%q), exit %v", st.StopReason, st.StopMessage, st.Exit)
		}
	})
	t.Run("stop on request", func(t *testing.T) {
		e := newEnv(t, true)
		e.run()
		e.startReady("ln-1")
		st, err := e.m.Stop("ln-1", false)
		if err != nil || st.State != agentproto.StateStopping || st.StopReason != agentproto.ExitStopped {
			t.Fatalf("%+v %v", st, err)
		}
	})
}
