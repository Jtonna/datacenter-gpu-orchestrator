package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
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
	// The point names this launch: the rigs of earlier tests leave their
	// pools (and launch supervisors) running, and a bare "pool.launch" hook
	// could fire in one of theirs (seen under -race).
	h, fired := supervise.PanicAtForTest(supervise.PointName("pool.launch", id), 1)
	defer supervise.SetHookForTest(h)()
	eventually(t, 5*time.Second, func() bool { return fired() == 1 }, "the injected failure")
	// Wait for the state asserted: restarted once, out of its backoff.
	var got supervise.TaskStats
	eventually(t, 5*time.Second, func() bool {
		got = supervise.TaskStats{}
		for _, s := range supervise.Stats() {
			if s.Name == "pool.launch" {
				got = s
			}
		}
		return got.Restarts == 1 && got.State == "running"
	}, "the launch supervisor restarted")
	if got.Failures != 1 {
		t.Fatalf("stats %+v", got)
	}
	before := a.Leases()
	eventually(t, 5*time.Second, func() bool { return a.Leases() >= before+2 }, "leases renewed after the restart")
	if st := a.LaunchState(); st.LaunchID != id || st.State != agentproto.StateReady {
		t.Fatalf("the launch was disturbed: %+v", st)
	}
}

// fatalCounter configures supervise for a test whose failure must end the
// process: it counts the Fatal calls instead.
func fatalCounter(t *testing.T) func() int {
	t.Helper()
	supervise.ResetForTest()
	var mu sync.Mutex
	n := 0
	restore := supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(string) { mu.Lock(); n++; mu.Unlock() }})
	t.Cleanup(func() { restore(); supervise.ResetForTest() })
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// lockFree says whether mu can be taken now (it is released at once).
func lockFree(mu *sync.Mutex) bool {
	if !mu.TryLock() {
		return false
	}
	mu.Unlock()
	return true
}

// Review IB1: a panic INSIDE a section under the pool's lock (p.mu or a
// launch's l.mu), in a task that recovers, must neither leave the lock held
// (Potluck would hang instead of restarting) nor let the task go on with the
// half-written state: the lock is released and the fatal path runs, once.
func TestPanicInsideThePoolLockIsFatalAndFreesIt(t *testing.T) {
	launched := func(t *testing.T, mod ...func(*fakeagent.Agent)) (*rig, *fakeagent.Agent, string) {
		r := newRig(t)
		a := r.agent(1, mod...)
		r.waitNode(a.ID)
		if res := r.p.Launch(context.Background(), spec(6, a), (&reports{}).add); res.Reason != "" {
			t.Fatalf("%+v", res)
		}
		return r, a, a.LaunchState().LaunchID
	}
	arm := func(t *testing.T, point string, inst any) func() int {
		h, fired := supervise.PanicAtForTest(supervise.PointName(point, inst), 1)
		t.Cleanup(supervise.SetHookForTest(h))
		return fired
	}
	check := func(t *testing.T, r *rig, fatals func() int, fired func() int, extra ...*sync.Mutex) {
		t.Helper()
		eventually(t, 10*time.Second, func() bool { return fired() == 1 && fatals() == 1 }, "the fatal path")
		if !lockFree(&r.p.mu) {
			t.Fatal("p.mu is still held after the panic")
		}
		for _, mu := range extra {
			if !lockFree(mu) {
				t.Fatal("a launch's l.mu is still held after the panic")
			}
		}
		if n := fatals(); n != 1 {
			t.Fatalf("fatal %d times", n)
		}
	}
	t.Run("stop ends the launch (pool.stop)", func(t *testing.T) {
		fatals := fatalCounter(t)
		r, _, id := launched(t)
		fired := arm(t, "pool.end_launch", id)
		r.p.Stop(6, nil)
		r.p.WaitStops(10 * time.Second)
		check(t, r, fatals, fired)
	})
	t.Run("agent unreachable (scheduler.launch_workers)", func(t *testing.T) {
		fatals := fatalCounter(t)
		r := newRig(t)
		a := r.agent(1)
		r.waitNode(a.ID)
		fired := arm(t, "pool.unreachable", a.ID)
		done := make(chan struct{})
		go supervise.Once("scheduler.launch_workers", func() {
			defer close(done)
			r.p.agentUnreachable(core.NodeID(a.ID), errors.New("dial tcp: refused"))
		}, nil)
		<-done
		check(t, r, fatals, fired)
	})
	t.Run("poll under l.mu (pool.launch)", func(t *testing.T) {
		fatals := fatalCounter(t)
		r, _, id := launched(t)
		l := r.launch(id)
		fired := arm(t, "pool.poll", id)
		check(t, r, fatals, fired, &l.mu)
	})
	t.Run("report under l.mu (pool.launch)", func(t *testing.T) {
		fatals := fatalCounter(t)
		r, _, id := launched(t)
		l := r.launch(id)
		fired := arm(t, "pool.report", id)
		l.wake()
		check(t, r, fatals, fired, &l.mu)
	})
	t.Run("forced stop's reset record (pool.force_stop)", func(t *testing.T) {
		fatals := fatalCounter(t)
		r, a, _ := launched(t, func(a *fakeagent.Agent) { a.IgnoreForce = true })
		eventually(t, 5*time.Second, func() bool { f := r.p.Facts(); return len(f) > 0 && f[0].LaunchID != nil }, "heartbeat names the launch")
		fired := arm(t, "pool.force_tail", a.ID)
		r.p.ForceNode(core.NodeID(a.ID))
		r.p.WaitStops(20 * time.Second)
		check(t, r, fatals, fired)
	})
}

// launch is the pool's launch id.
func (r *rig) launch(id string) *launch {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return r.p.launches[id]
}

// Review IS1: one bad input that fails every launch of a three-node load is
// one failure toward escalation, not three: the load fails, the process
// stays up.
func TestLaunchFanoutFailsOnceTowardEscalation(t *testing.T) {
	fatals := fatalCounter(t)
	r := newRig(t)
	as := []*fakeagent.Agent{r.agent(1), r.agent(2), r.agent(3)}
	for _, a := range as {
		r.waitNode(a.ID)
	}
	h, fired := supervise.PanicAtForTest(supervise.PointName("pool.launch_send", uint64(9)), -1)
	restore := supervise.SetHookForTest(h)
	res := r.p.Launch(context.Background(), spec(9, as...), (&reports{}).add)
	restore()
	if fired() != 3 || res.Reason == "" {
		t.Fatalf("fired %d, result %+v", fired(), res)
	}
	// A member reports its failure after its result is in (Launch may
	// return first): wait for the three.
	var got supervise.TaskStats
	eventually(t, 5*time.Second, func() bool {
		for _, s := range supervise.Stats() {
			if s.Name == "pool.launch_send" {
				got = s
			}
		}
		return got.Failures == 3
	}, "three failures reported")
	supervise.WaitRecords(time.Second)
	if got.FailuresLastMin != 1 || fatals() != 0 {
		t.Fatalf("one load's failures escalated: stats %+v, %d fatal", got, fatals())
	}
}

// A load's stop whose members panic (fan-out pool.stop): the stop's
// WaitGroup is done only after each failure is counted, so WaitStops
// returns with the failures in (supervise.Fanout.Go; run it under -race),
// and one load counts once toward escalation.
func TestStopFanoutMemberPanicIsCountedBeforeTheStopEnds(t *testing.T) {
	fatals := fatalCounter(t)
	r := newRig(t)
	as := []*fakeagent.Agent{r.agent(1), r.agent(2)}
	for _, a := range as {
		r.waitNode(a.ID)
	}
	if res := r.p.Launch(context.Background(), spec(5, as...), (&reports{}).add); res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	h, fired := supervise.PanicAtForTest(supervise.PointName("pool.stop", uint64(5)), 2)
	restore := supervise.SetHookForTest(h)
	r.p.Stop(5, nil)
	r.p.WaitStops(10 * time.Second)
	restore()
	if fired() != 2 {
		t.Fatalf("fired %d", fired())
	}
	var got supervise.TaskStats
	for _, s := range supervise.Stats() {
		if s.Name == "pool.stop" {
			got = s
		}
	}
	if got.Failures != 2 || got.FailuresLastMin != 1 || fatals() != 0 {
		t.Fatalf("stats %+v, %d fatal", got, fatals())
	}
}
