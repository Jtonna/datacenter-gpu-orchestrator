package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The rolling windows keep each slot's longest step; a slot older than the
// window does not count; a step over the threshold is counted every time
// and recorded at most once a minute.
func TestLoopHealth_WindowsAndSlowStepRateLimit(t *testing.T) {
	var l loopHealth
	base := time.Unix(1_800_000_000, 0)
	name := func(n string) func() string { return func() string { return n } }
	warn := 100 * time.Millisecond
	if l.end(base.Add(-50*time.Minute), 900*time.Millisecond, name("old"), warn) != true {
		t.Fatal("the first slow step is not recorded")
	}
	l.end(base.Add(-5*time.Minute), 300*time.Millisecond, name("five minutes ago"), warn)
	l.end(base.Add(-20*time.Second), 40*time.Millisecond, name("this minute"), warn)
	l.end(base.Add(-19*time.Second), 10*time.Millisecond, name("shorter, same slot"), warn)
	if d, ev := l.windowMax(base, time.Minute); d != 40*time.Millisecond || ev != "this minute" {
		t.Fatalf("last minute %v %q", d, ev)
	}
	if d, ev := l.windowMax(base, 10*time.Minute); d != 300*time.Millisecond || ev != "five minutes ago" {
		t.Fatalf("ten minutes %v %q", d, ev)
	}
	if d, ev := l.windowMax(base, time.Hour); d != 900*time.Millisecond || ev != "old" {
		t.Fatalf("hour %v %q", d, ev)
	}
	if d, _ := l.windowMax(base.Add(2*time.Hour), time.Hour); d != 0 {
		t.Fatalf("slots of two hours ago count: %v", d)
	}
	// rate limit: the slow step five minutes ago was recorded (more than a
	// minute after the first); one 10 s later is counted, not recorded
	if l.slow.Load() != 2 || l.steps.Load() != 4 {
		t.Fatalf("slow %d steps %d", l.slow.Load(), l.steps.Load())
	}
	if l.end(base.Add(-5*time.Minute+10*time.Second), 200*time.Millisecond, name("again"), warn) {
		t.Fatal("a second slow step within a minute was recorded")
	}
	if !l.end(base, 200*time.Millisecond, name("later"), warn) || l.slow.Load() != 4 {
		t.Fatal("a slow step a minute later was not recorded")
	}
	if l.end(base.Add(time.Hour), time.Second, name("off"), 0) {
		t.Fatal("a zero threshold records")
	}
}

type histCap struct {
	mu  sync.Mutex
	evs []history.Event
}

func (h *histCap) Record(e history.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evs = append(h.evs, e)
}

func (h *histCap) of(typ string) []history.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []history.Event
	for _, e := range h.evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// A step that stalls the loop is visible while it runs (busy_now and its
// event, read without the loop), then lands in the windows, counts as slow
// and is recorded as a loopSlowStep history event.
func TestLoopHealth_StalledStepIsReadableWithoutTheLoop(t *testing.T) {
	clk := newFakeClock()
	view := newFakeView(clk)
	view.set(ps5("a", 2, 15000))
	cfg := testConfig()
	cfg.LoopStepWarnMs = 20
	hist := &histCap{}
	s, err := New(cfg, Options{Clock: clk, View: view, Prober: newFakeProber(view), Launcher: &fakeLauncher{nextPort: 5800, autoHealthy: true, autoExit: true},
		WorkerProber: newFakeWorkerProber(), History: hist,
		Needs:  NeedFunc(func(p *core.Profile) (NeedInfo, error) { return NeedInfo{Need: placer.Measured(p.NeedsMiB)}, nil }),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	go s.Run(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	if st := s.LoopStats(); st.WarnMs != 20 || st.BusyNowMs > 20 {
		t.Fatalf("at start: %+v", st)
	}
	release := make(chan struct{})
	answered := make(chan error, 1)
	go func() {
		_, err := askRead(s, ctxT(t), "stall test", func(c *schedCore) int { <-release; return 1 })
		answered <- err
	}()
	waitUntil(t, func() bool { st := s.LoopStats(); return st.BusyNowMs >= 60 && st.CurrentEvent == "read stall test" })
	close(release)
	if err := <-answered; err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return len(hist.of("loopSlowStep")) == 1 })
	st := s.LoopStats()
	if st.MaxLastMinuteMs < 60 || st.MaxLastMinuteEv != "read stall test" || st.Max10mMs < 60 || st.MaxLastHourMs < 60 || st.MaxMs < 60 || st.SlowSteps < 1 || st.Steps < 1 {
		t.Fatalf("after the stall: %+v", st)
	}
	e := hist.of("loopSlowStep")[0]
	if e.Severity != "warn" || e.Data["event"] != "read stall test" || e.Data["warn_ms"] != int64(20) || e.Data["ms"].(int64) < 60 || e.Summary == "" {
		t.Fatalf("event %+v", e)
	}
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
