package pool

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Work queued under p.mu that panics (a store write, a history sink) is
// reported; the rest of its batch still runs and the queue is not stuck for
// the next unlock, which may be the scheduler loop's.
func TestAfterQueueSurvivesAPanic(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { t.Errorf("escalated: %s", r) }})()
	p := &Pool{}
	var ran []string
	p.mu.Lock()
	p.afterLocked(func() { panic("store write bug") })
	p.afterLocked(func() { ran = append(ran, "second") })
	p.unlock()
	p.mu.Lock()
	p.afterLocked(func() { ran = append(ran, "next batch") })
	p.unlock()
	if len(ran) != 2 || ran[0] != "second" || ran[1] != "next batch" {
		t.Fatalf("ran %v", ran)
	}
	if supervise.Total() != 1 {
		t.Fatalf("failures %d", supervise.Total())
	}
}

// A launch supervisor that fails restarts alone: the lease is still renewed
// and the launch stays held.
func TestLaunchSupervisorRestartsAndRenews(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { t.Errorf("escalated: %s", r) }})()
	r := newRig(t)
	a := r.agent(1)
	r.waitNode(a.ID)
	rep := &reports{}
	if res := r.p.Launch(context.Background(), spec(6, a), rep.add); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	id := a.LaunchState().LaunchID
	h, fired := supervise.PanicAtForTest("pool.launch", 1)
	defer supervise.SetHookForTest(h)()
	eventually(t, 5*time.Second, func() bool { return fired() == 1 }, "the injected failure")
	before := a.Leases()
	eventually(t, 5*time.Second, func() bool { return a.Leases() >= before+2 }, "leases renewed after the restart")
	if st := a.LaunchState(); st.LaunchID != id || st.State != agentproto.StateReady {
		t.Fatalf("the launch was disturbed: %+v", st)
	}
	for _, s := range supervise.Stats() {
		if s.Name == "pool.launch" && (s.Restarts != 1 || s.Failures != 1) {
			t.Fatalf("stats %+v", s)
		}
	}
}
