package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/harness"
)

const waitFor = 15 * time.Second

func newLauncher(t *testing.T) *ProcessLauncher {
	t.Helper()
	base := contiguousRange(t, 8)
	ports, err := NewPortAllocator(base, base+7)
	if err != nil {
		t.Fatal(err)
	}
	return &ProcessLauncher{Ports: ports, StopGrace: 400 * time.Millisecond, HealthInterval: 20 * time.Millisecond}
}

func fakeSpec(t *testing.T, extra ...string) StartSpec {
	t.Helper()
	bin := harness.BuildFakeLlama(t)
	return StartSpec{
		HealthPath: "/health",
		Build: func(port int) ([]string, error) {
			return append([]string{bin, "--host", "127.0.0.1", "--port", fmt.Sprint(port)}, extra...), nil
		},
	}
}

func start(t *testing.T, l *ProcessLauncher, spec StartSpec) Child {
	t.Helper()
	c, err := l.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Stop(StopKill)
		select {
		case <-doneOf(c):
		case <-time.After(waitFor):
			t.Error("child not reaped at cleanup")
		}
	})
	return c
}

// doneOf returns a channel closed when the child has exited, without
// consuming the single exit status.
func doneOf(c Child) <-chan struct{} {
	return c.(*proc).done
}

func waitHealthy(t *testing.T, c Child) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := c.WaitHealthy(ctx); err != nil {
		t.Fatalf("WaitHealthy: %v\ntail: %q", err, c.Tail(20))
	}
}

func recvExit(t *testing.T, c Child) ExitStatus {
	t.Helper()
	select {
	case st, ok := <-c.Exited():
		if !ok {
			t.Fatal("Exited closed without a status")
		}
		return st
	case <-time.After(waitFor):
		t.Fatalf("child did not exit; tail: %q", c.Tail(20))
	}
	return ExitStatus{}
}

func TestStartBecomesHealthyAfterLoadDelay(t *testing.T) {
	t.Setenv("FAKELLAMA_LOAD_DELAY", "400ms")
	l := newLauncher(t)
	t0 := time.Now()
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	if el := time.Since(t0); el < 300*time.Millisecond {
		t.Fatalf("healthy after only %v; the 503 loading phase was not honoured", el)
	}
	if c.Port() < 1 || c.PID() < 1 {
		t.Fatalf("port %d pid %d", c.Port(), c.PID())
	}
	if !strings.Contains(strings.Join(c.Argv(), " "), fmt.Sprintf("--port %d", c.Port())) {
		t.Fatalf("argv = %q", c.Argv())
	}
}

func TestWaitHealthyHonoursDeadline(t *testing.T) {
	t.Setenv("FAKELLAMA_NEVER_HEALTHY", "1")
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	err := c.WaitHealthy(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(t0); el > 5*time.Second {
		t.Fatalf("took %v", el)
	}
}

func TestWaitHealthyUsesCheckEndpoint(t *testing.T) {
	l := newLauncher(t)
	spec := fakeSpec(t)
	spec.HealthPath = "/v1/health"
	c := start(t, l, spec)
	waitHealthy(t, c)

	spec.HealthPath = "/no-such-endpoint"
	c2 := start(t, l, spec)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if err := c2.WaitHealthy(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("404 endpoint treated as healthy: %v", err)
	}
}

func TestChildExitIsReportedAsyncWithTail(t *testing.T) {
	t.Setenv("FAKELLAMA_EXIT_CODE", "3")
	t.Setenv("FAKELLAMA_EXIT_AFTER", "100ms")
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	st := recvExit(t, c)
	if st.Code != 3 || st.Requested {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(strings.Join(st.Tail, "\n"), "simulated load failure") {
		t.Fatalf("tail = %q", st.Tail)
	}
	if _, ok := <-c.Exited(); ok {
		t.Fatal("Exited should be closed after the single status")
	}
}

func TestWaitHealthyReturnsWhenChildExits(t *testing.T) {
	t.Setenv("FAKELLAMA_NEVER_HEALTHY", "1")
	t.Setenv("FAKELLAMA_EXIT_CODE", "4")
	t.Setenv("FAKELLAMA_EXIT_AFTER", "100ms")
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	t0 := time.Now()
	err := c.WaitHealthy(ctx)
	if !errors.Is(err, ErrChildExited) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatal("did not return promptly on exit")
	}
}

func TestPortReleasedAfterExit(t *testing.T) {
	t.Setenv("FAKELLAMA_EXIT_CODE", "1")
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	if l.Ports.InUse() != 1 {
		t.Fatalf("InUse = %d while child lives", l.Ports.InUse())
	}
	recvExit(t, c)
	if l.Ports.InUse() != 0 {
		t.Fatalf("InUse = %d after exit", l.Ports.InUse())
	}
}

func TestStartFailureReleasesPort(t *testing.T) {
	l := newLauncher(t)
	_, err := l.Start(StartSpec{Build: func(port int) ([]string, error) { return []string{"definitely-not-a-binary-xyz"}, nil }})
	if err == nil {
		t.Fatal("expected error")
	}
	_, err = l.Start(StartSpec{Build: func(port int) ([]string, error) { return nil, errors.New("boom") }})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	_, err = l.Start(StartSpec{Build: func(port int) ([]string, error) { return nil, nil }})
	if err == nil {
		t.Fatal("empty argv accepted")
	}
	if l.Ports.InUse() != 0 {
		t.Fatalf("InUse = %d", l.Ports.InUse())
	}
}

func TestStopKillsChild(t *testing.T) {
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	c.Stop(StopGraceful)
	st := recvExit(t, c)
	if !st.Requested {
		t.Fatalf("status = %+v", st)
	}
	// the child is gone: its port no longer answers
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", c.Port()), time.Second); err == nil {
		conn.Close()
		t.Fatal("child still accepting connections after Stop")
	}
	if l.Ports.InUse() != 0 {
		t.Fatal("port not released")
	}
}

func TestStopIsIdempotentAndConcurrencySafe(t *testing.T) {
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				c.Stop(StopGraceful)
			} else {
				c.Stop(StopKill)
			}
		}(i)
	}
	wg.Wait()
	recvExit(t, c)
	c.Stop(StopGraceful) // after exit: no-op, no panic
	c.Stop(StopKill)
}

func TestStopRacingWithExit(t *testing.T) {
	t.Setenv("FAKELLAMA_EXIT_CODE", "0")
	t.Setenv("FAKELLAMA_EXIT_AFTER", "30ms")
	l := newLauncher(t)
	for i := 0; i < 5; i++ {
		c := start(t, l, fakeSpec(t))
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(j*15) * time.Millisecond)
				c.Stop(StopGraceful)
			}()
		}
		wg.Wait()
		recvExit(t, c)
	}
	if l.Ports.InUse() != 0 {
		t.Fatalf("InUse = %d", l.Ports.InUse())
	}
}

func TestChildEnvironmentIsScrubbed(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "start.jsonl")
	t.Setenv("FAKELLAMA_START_LOG", logPath)
	t.Setenv("LLAMA_ARG_MODEL", "/leak")
	t.Setenv("LLAMA_ARG_PORT", "1")
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t))
	waitHealthy(t, c)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct{ Env map[string]string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatal(err)
	}
	for k := range rec.Env {
		if strings.HasPrefix(k, "LLAMA_ARG_") {
			t.Errorf("child saw %s", k)
		}
	}
	if rec.Env["FAKELLAMA_START_LOG"] == "" {
		t.Error("other variables must pass through")
	}
}

func TestLoadStatsFromChildOutput(t *testing.T) {
	t.Setenv("FAKELLAMA_LOAD_LOG", "auto")
	l := newLauncher(t)
	c := start(t, l, fakeSpec(t, "--rpc", "10.0.0.2:50052,10.0.0.3:50052", "--device", "RPC0,RPC1,CUDA0"))
	waitHealthy(t, c)
	// output is read asynchronously; the lines precede listening, but poll anyway
	deadline := time.Now().Add(waitFor)
	for len(c.Load().Devices) < 4 && time.Now().After(deadline) == false {
		time.Sleep(10 * time.Millisecond)
	}
	s := c.Load()
	if !near(s.TotalMiB(), 3*1300) {
		t.Fatalf("TotalMiB = %v; %+v", s.TotalMiB(), s.Devices)
	}
	if !near(s.HostMiB(), 360) {
		t.Fatalf("HostMiB = %v", s.HostMiB())
	}
	var names []string
	for _, d := range s.Devices {
		names = append(names, d.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "RPC0[10.0.0.3:50052]") {
		t.Fatalf("names = %v", names)
	}
}

func TestTailIsBounded(t *testing.T) {
	l := newLauncher(t)
	l.TailLines = 3
	bin := harness.BuildFakeLlama(t)
	t.Setenv("FAKELLAMA_EXIT_CODE", "1")
	c := start(t, l, StartSpec{Build: func(port int) ([]string, error) {
		return []string{bin, "--port", fmt.Sprint(port), "--a", "--b"}, nil
	}})
	st := recvExit(t, c)
	if len(st.Tail) > 3 || len(st.Tail) == 0 {
		t.Fatalf("tail = %q", st.Tail)
	}
}

func TestNoGoroutineLeakAfterManyChildren(t *testing.T) {
	l := newLauncher(t)
	spec := fakeSpec(t)
	// warm up the build and runtime before sampling
	c0 := start(t, l, spec)
	waitHealthy(t, c0)
	c0.Stop(StopKill)
	recvExit(t, c0)
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 4; i++ {
		c, err := l.Start(spec)
		if err != nil {
			t.Fatal(err)
		}
		waitHealthy(t, c)
		c.Stop(StopGraceful)
		recvExit(t, c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+1 {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutines %d -> %d\n%s", before, after, buf[:n])
	}
}
