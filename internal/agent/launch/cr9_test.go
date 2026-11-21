package launch

import (
	"context"
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
	t.Run("no device known: no check", func(t *testing.T) {
		e := newEnv(t, true)
		if _, ref := e.m.Start(ctx, req("a", 50000)); ref != nil {
			t.Fatalf("refused without a device: %+v", ref.Body)
		}
	})
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
