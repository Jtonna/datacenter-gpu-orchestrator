package instance

import (
	"io"
	"log/slog"
	"testing"

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
	h, fired := supervise.PanicAtForTest("instance.reap", 1)
	defer supervise.SetHookForTest(h)()
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	c.Stop(StopKill)
	st := recvExit(t, c)
	if st.Err == nil || fired() != 1 {
		t.Fatalf("status %+v after %d injected panics", st, fired())
	}
	if l.Ports.InUse() != 0 || l.KillAll() != 0 {
		t.Fatal("the port or the live entry was kept")
	}
}
