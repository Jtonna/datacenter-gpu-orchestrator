package instance

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// KillAll (the scheduler's fatal path) kills every live child, and a reaped
// child leaves the live set.
func TestKillAllKillsLiveChildren(t *testing.T) {
	l := newLauncher(t)
	a, b := start(t, l, fakeSpec(t)), start(t, l, fakeSpec(t))
	waitHealthy(t, a)
	waitHealthy(t, b)
	if n := l.KillAll(); n != 2 {
		t.Fatalf("KillAll killed %d, want 2", n)
	}
	for _, c := range []Child{a, b} {
		if st := recvExit(t, c); !st.Requested {
			t.Fatalf("status %+v", st)
		}
	}
	if n := l.KillAll(); n != 0 {
		t.Fatalf("reaped children still live: %d", n)
	}
}

// A failure while reaping still publishes the exit (with Err set), releases
// the port and leaves the live set: the core is never left waiting.
func TestReapFailureStillPublishesTheExit(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { t.Errorf("escalated: %s", r) }})()
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	h, fired := supervise.PanicAtForTest(supervise.PointName("instance.reap", c), 1)
	defer supervise.SetHookForTest(h)()
	c.Stop(StopKill)
	st := recvExit(t, c)
	if st.Err == nil || fired() != 1 {
		t.Fatalf("status %+v after %d injected panics", st, fired())
	}
	if l.Ports.InUse() != 0 || l.KillAll() != 0 {
		t.Fatal("the port or the live entry was kept")
	}
}

// Review IB1: a panic inside the child's lock while reaping releases the
// lock and takes the fatal path (the exit is still published on the way:
// the deferred finisher runs).
func TestReapPanicInsideTheLockIsFatal(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	var mu sync.Mutex
	fatals := 0
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(string) { mu.Lock(); fatals++; mu.Unlock() }})()
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	h, fired := supervise.PanicAtForTest(supervise.PointName("instance.reaped", c), 1)
	defer supervise.SetHookForTest(h)()
	c.Stop(StopKill)
	recvExit(t, c)
	n := 0
	for deadline := time.Now().Add(5 * time.Second); n == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		n = fatals
		mu.Unlock()
	}
	if fired() != 1 || n != 1 {
		t.Fatalf("fired %d, fatal %d", fired(), n)
	}
	p := c.(*proc)
	if !p.mu.TryLock() {
		t.Fatal("the child's lock is still held")
	}
	p.mu.Unlock()
}
