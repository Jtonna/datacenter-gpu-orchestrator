//go:build !windows

package instance

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A child that ignores SIGTERM is killed after the grace period.
func TestStopEscalatesToSIGKILLAfterGrace(t *testing.T) {
	t.Setenv("FAKELLAMA_IGNORE_SIGTERM", "1")
	l := newLauncher(t)
	l.StopGrace = 600 * time.Millisecond
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	t0 := time.Now()
	c.Stop(StopGraceful)
	st := recvExit(t, c)
	el := time.Since(t0)
	if el < 500*time.Millisecond {
		t.Fatalf("exited after %v, before the grace period; SIGTERM was meant to be ignored", el)
	}
	if st.Signal != "killed" || !st.Requested {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(strings.Join(c.Tail(50), "\n"), "ignoring") {
		t.Fatalf("child never saw SIGTERM; tail %q", c.Tail(50))
	}
}

// A child that handles SIGTERM exits promptly, well inside the grace period.
func TestStopGracefulSIGTERMExitsCleanly(t *testing.T) {
	l := newLauncher(t)
	l.StopGrace = 30 * time.Second
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	t0 := time.Now()
	c.Stop(StopGraceful)
	st := recvExit(t, c)
	if time.Since(t0) > 5*time.Second || st.Code != 0 || st.Signal != "" {
		t.Fatalf("status %+v after %v", st, time.Since(t0))
	}
}

func TestStopKillSkipsGrace(t *testing.T) {
	t.Setenv("FAKELLAMA_IGNORE_SIGTERM", "1")
	l := newLauncher(t)
	l.StopGrace = 30 * time.Second
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	c.Stop(StopGraceful)
	c.Stop(StopKill) // escalate at once, as node loss does
	st := recvExit(t, c)
	if st.Signal != "killed" {
		t.Fatalf("status = %+v", st)
	}
}

// The whole process group dies, including grandchildren.
func TestStopKillsProcessGroup(t *testing.T) {
	l := newLauncher(t)
	c := start(t, l, StartSpec{Build: func(port int) ([]string, error) {
		return []string{"sh", "-c", "sleep 300 & echo CHILD=$!; wait"}, nil
	}})
	var pid int
	deadline := time.Now().Add(waitFor)
	for pid == 0 && time.Now().Before(deadline) {
		for _, line := range c.Tail(10) {
			if v, ok := strings.CutPrefix(line, "CHILD="); ok {
				pid, _ = strconv.Atoi(v)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("no grandchild pid; tail %q", c.Tail(10))
	}
	c.Stop(StopGraceful)
	recvExit(t, c)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild %d survived Stop", pid)
}
