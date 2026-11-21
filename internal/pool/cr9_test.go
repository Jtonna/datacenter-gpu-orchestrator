package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
)

// CR-9, 1a and 1c: the launch request carries expect_mib, and the agent's
// would_not_fit refusal comes back as the launch's reason and message. A
// target without a figure sends no expect_mib (release 0.1.0 behaviour).
func TestLaunchSendsExpectMiBAndWouldNotFitComesBack(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	got := map[string]int64{}
	seen := func(a *fakeagent.Agent) {
		a.Refuse = func(req agentproto.LaunchRequest) *agentproto.LaunchRefusal {
			mu.Lock()
			got[a.ID] = req.Worker.ExpectMiB
			mu.Unlock()
			return nil
		}
	}
	a1, a2 := r.agent(1, seen), r.agent(2, seen) // MemAvailable 14000, floor 1000
	r.waitNode(a1.ID)
	r.waitNode(a2.ID)
	s := spec(31, a1, a2)
	s.Workers[0].ExpectMiB = 6000
	if res := r.p.Launch(context.Background(), s, func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	mu.Lock()
	if len(got) != 2 || got[a1.ID] != 6000 || got[a2.ID] != 0 {
		t.Fatalf("expect_mib sent: %v", got)
	}
	mu.Unlock()
	r.p.Stop(31, nil)
	r.p.WaitStops(10 * time.Second)

	s = spec(32, a1, a2)
	s.Workers[1].ExpectMiB = 13500
	res := r.p.Launch(context.Background(), s, func(scheduler.WorkerReport) {})
	if res.Reason != agentproto.ReasonWouldNotFit || res.Node != core.NodeID(a2.ID) ||
		!strings.Contains(res.Message, "14000") || !strings.Contains(res.Message, "13500") || !strings.Contains(res.Message, "1000") {
		t.Fatalf("%+v", res)
	}
	r.p.Stop(32, nil)
	r.p.WaitStops(10 * time.Second)
	if f := r.fact(a2.ID); !f.Placeable || f.Unplaced != "" {
		t.Fatalf("a would_not_fit refusal must leave the node placeable: %+v", f)
	}
}

// CR-9, 2: the node's reported watchdog floor is used for its placement
// memory; a difference from Potluck's setting is in the node's status and
// logged once per registration. A node that reports none uses the setting.
func TestReportedWatchdogFloorIsUsedAndAMismatchIsShown(t *testing.T) {
	r := newRig(t) // defaults.watchdog_floor_mib 1000
	a1 := r.agent(1, func(a *fakeagent.Agent) { a.WatchdogFloorMiB = 2500 })
	a2 := r.agent(2)
	n1, n2 := r.waitNode(a1.ID), r.waitNode(a2.ID)
	// usable = min(device free 14000, mem_available 14000 - floor)
	if n1.FreeMiB != 11500 || n1.WatchdogFloorMiB != 2500 {
		t.Fatalf("node with a reported floor: %+v", n1)
	}
	if n2.FreeMiB != 13000 || n2.WatchdogFloorMiB != 1000 {
		t.Fatalf("node without a reported floor: %+v", n2)
	}
	f1, f2 := r.fact(a1.ID), r.fact(a2.ID)
	if f1.WatchdogFloorMiB == nil || *f1.WatchdogFloorMiB != 2500 || !strings.Contains(f1.FloorMismatch, "2500") || !strings.Contains(f1.FloorMismatch, "1000") {
		t.Fatalf("facts %+v", f1)
	}
	if f2.WatchdogFloorMiB != nil || f2.FloorMismatch != "" {
		t.Fatalf("facts of a node that reports no floor: %+v", f2)
	}
	time.Sleep(200 * time.Millisecond) // several more heartbeats
	if n := strings.Count(r.logs.String(), "watchdog floor differs"); n != 1 {
		t.Fatalf("the mismatch was logged %d times, want once per registration\n%s", n, r.logs.String())
	}
}

// CR-9, 2: the registration's floor counts before the first heartbeat and
// when a heartbeat names none, and a floor equal to the setting is no
// mismatch.
func TestWatchdogFloorFromTheRegistration(t *testing.T) {
	r := newRig(t)
	req := regReq("02:00:00:00:00:70")
	req.WatchdogFloorMiB = 1000
	r.p.Register(req)
	r.p.Heartbeat("02:00:00:00:00:70", hbBody("s1", "", true, "true")) // a 0.1.0-shaped heartbeat: no floor
	f := r.fact("02:00:00:00:00:70")
	if f.WatchdogFloorMiB == nil || *f.WatchdogFloorMiB != 1000 || f.FloorMismatch != "" {
		t.Fatalf("facts %+v", f)
	}
	if n, _ := r.node("02:00:00:00:00:70"); n.FreeMiB != 8000 {
		t.Fatalf("node %+v", n)
	}
}

// CR-9, 4: a launch the agent is stopping for a cause of its own (watchdog,
// lease_expired) explains the host program's exit before the launch has
// exited; a stop on request does not.
func TestExitsAcceptsAStoppingLaunchWithItsOwnCause(t *testing.T) {
	r := newRig(t)
	a1, a2 := r.agent(1), r.agent(2)
	r.waitNode(a1.ID)
	r.waitNode(a2.ID)
	if res := r.p.Launch(context.Background(), spec(41, a1, a2), func(scheduler.WorkerReport) {}); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	a1.BeginStop(agentproto.ExitStopped, "")
	a2.BeginStop(agentproto.ExitWatchdog, "MemAvailable 900 MiB fell below the watchdog floor of 1000 MiB")
	began := time.Now()
	rs := r.p.Exits(context.Background(), 41)
	if len(rs) != 1 || string(rs[0].Node) != a2.ID || rs[0].Exit == nil || rs[0].Exit.Reason != agentproto.ExitWatchdog ||
		!strings.Contains(rs[0].Exit.Message, "900 MiB") {
		t.Fatalf("%+v", rs)
	}
	if d := time.Since(began); d > time.Second {
		t.Fatalf("waited %v for a cause that was readable at once", d)
	}
	eventually(t, 3*time.Second, func() bool { return r.fact(a2.ID).StopReason == agentproto.ExitWatchdog },
		"stop_reason from the heartbeat in the node's status")
	a1.ExitLaunch(agentproto.ExitStopped, "")
	a2.ExitLaunch(agentproto.ExitWatchdog, "")
	r.p.Stop(41, nil)
	r.p.WaitStops(10 * time.Second)
}

// CR-9, 5: a paused node says why it is not placeable.
func TestPausedNodeHasAnUnplacedReason(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq("02:00:00:00:00:70"))
	r.p.Heartbeat("02:00:00:00:00:70", hbBody("s1", "", true, "true"))
	if f := r.fact("02:00:00:00:00:70"); !f.Placeable || f.Unplaced != "" {
		t.Fatalf("before the pause: %+v", f)
	}
	if _, err := r.p.SetPaused("02:00:00:00:00:70", true); err != nil {
		t.Fatal(err)
	}
	if f := r.fact("02:00:00:00:00:70"); f.Placeable || f.Unplaced != scheduler.UnplacedPaused {
		t.Fatalf("paused: placeable %v, unplaced %q", f.Placeable, f.Unplaced)
	}
	// Another reason of the node itself comes first.
	r.p.Heartbeat("02:00:00:00:00:70", hbBody("s1", "", false, "true"))
	if f := r.fact("02:00:00:00:00:70"); f.Unplaced != scheduler.UnplacedProgramsNotReady {
		t.Fatalf("paused and programs not ready: %q", f.Unplaced)
	}
}

// CR-9 review, A2: a node that a tier says shares system memory is placed on
// min(device free, mem_available - floor) even when its device does not say
// so, the figure the agent tests a device held to MemAvailable against.
func TestTierSharedMemoryUsesTheSmallerFigure(t *testing.T) {
	r := newRig(t)
	r.cfg.Tiers[0].Tier.SharedMemory = true
	r.p.Register(regReq("02:00:00:00:00:70"))
	body := strings.Replace(string(hbBody("s1", "", true, "true")), `"shares_system_memory":true`, `"shares_system_memory":false`, 1)
	r.p.Heartbeat("02:00:00:00:00:70", []byte(body))
	n, ok := r.node("02:00:00:00:00:70")
	if !ok || !n.SharedMemory || n.FreeMiB != 8000 { // min(14000, 9000 - 1000)
		t.Fatalf("node %+v, want shared with 8000 MiB usable", n)
	}
}

// CR-9 review, B2: a reported watchdog floor larger than half the node's
// memory, or larger than 16384 MiB, makes the node unplaced (bad_floor) with
// a message, instead of driving its placement memory to 0 in silence.
func TestBadWatchdogFloorIsUnplaced(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int64
		floor int64
		bad   bool
	}{
		{"half of the memory is fine", 16000, 8000, false},
		{"more than half", 16000, 8001, true},
		{"more than 16384 MiB", 64000, 16385, true},
		{"16384 MiB on a big node is fine", 64000, 16384, false},
		{"no total known: only the absolute bound", 0, 9000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			req := regReq("02:00:00:00:00:70")
			req.MemTotalMiB, req.WatchdogFloorMiB = tc.total, tc.floor
			r.p.Register(req)
			r.p.Heartbeat("02:00:00:00:00:70", hbBody("s1", "", true, "true"))
			f := r.fact("02:00:00:00:00:70")
			if !tc.bad {
				if f.Unplaced == scheduler.UnplacedBadFloor {
					t.Fatalf("unplaced bad_floor: %+v", f)
				}
				return
			}
			if f.Unplaced != scheduler.UnplacedBadFloor || f.Placeable || !strings.Contains(f.UnplacedMessage, fmt.Sprint(tc.floor)) {
				t.Fatalf("facts %+v, want bad_floor naming %d", f, tc.floor)
			}
			if r.unplaced("02:00:00:00:00:70") != scheduler.UnplacedBadFloor {
				t.Fatalf("the scheduler's gate says %q", r.unplaced("02:00:00:00:00:70"))
			}
			if msg := r.p.UnplacedDetail()[core.NodeID("02:00:00:00:00:70")]; !strings.Contains(msg, "watchdog floor") {
				t.Fatalf("detail %q", msg)
			}
		})
	}
}
