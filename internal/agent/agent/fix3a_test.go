package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// Tests of the defects seen when the agent first ran on a real machine.

// countRunner counts the starts of each unit.
type countRunner struct {
	runner.Runner
	mu     sync.Mutex
	starts map[string]int
}

func (c *countRunner) Start(ctx context.Context, s runner.Spec) (runner.Proc, error) {
	c.mu.Lock()
	if c.starts == nil {
		c.starts = map[string]int{}
	}
	c.starts[s.Unit]++
	c.mu.Unlock()
	return c.Runner.Start(ctx, s)
}

func (c *countRunner) n(unit string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.starts[unit] }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *syncBuf) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// A. Potluck restarted: the heartbeat answers 410 and the agent registers
// again. The exporter of this agent process keeps running; it is neither
// stopped nor started a second time, and the stats stay fresh.
func TestReRegistrationKeepsTheExporterRunning(t *testing.T) {
	cr := &countRunner{Runner: runner.NewChild()}
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Runner = cr })
	e.start()
	e.waitReady()
	listening := func() bool { ok, _ := dialSockets{}.Listening(e.nodePort); return ok }
	agenttest.Eventually(t, 10*time.Second, "exporter listening", listening)
	for round := 0; round < 2; round++ {
		regs := e.pl.nRegisters()
		e.pl.set(func() { e.pl.hbStatus = 410 })
		agenttest.Eventually(t, 5*time.Second, "re-registration on 410", func() bool { return e.pl.nRegisters() > regs })
		e.pl.set(func() { e.pl.hbStatus = 200 })
		n := e.pl.nBeats()
		agenttest.Eventually(t, 10*time.Second, "ready again", func() bool { return e.pl.nBeats() > n+2 && e.pl.lastBeat().ProgramsReady })
		for i := 0; i < 10; i++ {
			if !listening() {
				t.Fatalf("round %d: the exporter port is closed after the re-registration", round)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	if n := cr.n(agentproto.NodeExporterUnit); n != 1 {
		t.Fatalf("the exporter was started %d times; it must keep running across a re-registration", n)
	}
	agenttest.Eventually(t, 5*time.Second, "node stats ok", func() bool {
		s := e.pl.lastBeat().Stats
		return s != nil && s.Sources["node"].Status == "ok"
	})
}

// C. GET /status right after an agent restart: the answer waits for the
// re-adoption, so a launch that runs is never shown as role idle.
func TestStatusWaitsForReAdoption(t *testing.T) {
	sr := newSurvRunner("potluck-launch-ln-1")
	sr.adoptErrs = 3
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Runner, d.RecoverRetry = sr, 50*time.Millisecond })
	e.leftLaunch(time.Hour)
	e.start()
	w := httptest.NewRecorder()
	e.a.LocalHandler().ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
	var st agentproto.Status
	json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != 200 || st.Role != "worker" || st.State != "ready" || st.Model == nil || *st.Model != "m.gguf" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// C. When the re-adoption takes longer than the wait, GET /status answers
// 503 like the Potluck-facing API; it never answers from an unread state.
func TestStatusAnswers503WhileTheLaunchStateIsNotRead(t *testing.T) {
	e := newTenv(t)
	e.leftLaunch(time.Hour)
	e.a.statusWait = 20 * time.Millisecond
	w := httptest.NewRecorder()
	e.a.LocalHandler().ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), ReasonStarting) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// C. last_error of an abnormal exit and the last launch survive an agent
// restart: both come from the launch's exit record.
func TestLastErrorAndLastLaunchSurviveAnAgentRestart(t *testing.T) {
	sr := newSurvRunner()
	e1, api := runningLaunch(t, sr)
	agenttest.Eventually(t, 10*time.Second, "unit", func() bool { return sr.proc(launchUnit) != nil })
	sr.proc(launchUnit).finish() // the worker dies by itself
	agenttest.Eventually(t, 10*time.Second, "exited", func() bool {
		st, _ := launchState(t, api)
		return st.State == agentproto.StateExited
	})
	before := e1.a.Status()
	if before.LastError == nil || !strings.Contains(*before.LastError, "launch ln-1 exited") {
		t.Fatalf("%+v", before)
	}
	e1.die()
	e1.clock.Advance(30 * time.Second)

	e2 := restartOn(t, e1, sr, nil)
	if err := e2.a.RecoverLaunch(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e2.a.Status()
	if st.LastError == nil || *st.LastError != *before.LastError || st.LastErrorAgeS == nil || *st.LastErrorAgeS < 30 {
		t.Fatalf("last_error after the restart: %+v", st)
	}
	if st.Role != "idle" || st.Model != nil || st.LastLaunch == nil || st.LastLaunch.ID != "ln-1" ||
		st.LastLaunch.Model == nil || *st.LastLaunch.Model != "Devstral.gguf" || st.LastLaunch.EndedAgeS < 30 {
		t.Fatalf("last_launch after the restart: %+v %+v", st, st.LastLaunch)
	}
}

// E. The log lines an operator needs: programs, devices, the first failed
// heartbeat (once), the heartbeat working again, one line per registration.
func TestOperatorLogLines(t *testing.T) {
	var logs syncBuf
	e := newTenvWith(t, func(o *Options, d *Deps) { d.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	e.start()
	e.waitReady()
	for _, want := range []string{`msg="programs downloaded and verified" count=3 downloaded=3 bytes=`, `msg="devices read" count=1 devices="Vulkan0 (14336 MiB, 14010 MiB free)"`,
		`msg=registered release=0.1.0 platform=linux-amd64-vulkan paused=false why="agent start"`} {
		if logs.count(want) != 1 {
			t.Errorf("want once: %s\n%s", want, logs.b.String())
		}
	}
	// Heartbeats fail: one WARN, however many fail.
	e.pl.set(func() { e.pl.hbStatus = 503 })
	n := e.pl.nBeats()
	agenttest.Eventually(t, 5*time.Second, "failed heartbeats", func() bool { return e.pl.nBeats() > n+5 })
	if c := logs.count("level=WARN msg=\"Potluck unreachable"); c != 1 {
		t.Fatalf("%d lines for the failing heartbeat, want 1", c)
	}
	if logs.count("Potluck reachable again") != 0 {
		t.Fatal("reachable too early")
	}
	e.pl.set(func() { e.pl.hbStatus = 200 })
	agenttest.Eventually(t, 5*time.Second, "works again", func() bool { return logs.count("level=INFO msg=\"Potluck reachable again") == 1 })
	// A second outage is said again.
	e.pl.set(func() { e.pl.hbStatus = 503 })
	agenttest.Eventually(t, 5*time.Second, "second outage", func() bool { return logs.count("Potluck unreachable") == 2 })
	// Re-registration: one line.
	e.pl.set(func() { e.pl.hbStatus = 410 })
	agenttest.Eventually(t, 5*time.Second, "registered again", func() bool {
		return logs.count(`msg=registered release=0.1.0 platform=linux-amd64-vulkan paused=false why="heartbeat answered 410"`) >= 1
	})
	e.pl.set(func() { e.pl.hbStatus = 200 })
	if c := logs.count("level=INFO msg=\"heartbeat answered 410"); c != 0 {
		t.Fatalf("%d extra INFO lines per re-registration", c)
	}
}

// I. After a launch has exited GET /status shows no load: the node loads
// nothing. The launch's last progress is kept under last_launch.
func TestStatusHasNoLoadAfterTheLaunchExited(t *testing.T) {
	e := newTenv(t)
	e.start()
	e.waitReady()
	if w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	f, err := os.OpenFile(filepath.Join(e.opt.DataDir, "launch", "ln-1.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("[file_load] progress bytes=100 total=400\n")
	f.Close()
	agenttest.Eventually(t, 10*time.Second, "load in status", func() bool {
		s := e.a.Status()
		return s.Load != nil && s.Load.BytesLoaded != nil && *s.Load.BytesLoaded == 100 && s.Role == "worker"
	})
	if w := e.do("DELETE", "/launch/ln-1?force=1", nil, fromPotluck); w.Code != 200 {
		t.Fatal(w.Code)
	}
	agenttest.Eventually(t, 20*time.Second, "exited", func() bool { return e.a.Status().State == "exited" })
	s := e.a.Status()
	if s.Load != nil || s.Role != "idle" || s.LastLaunch == nil || s.LastLaunch.Load == nil ||
		*s.LastLaunch.Load.BytesLoaded != 100 || *s.LastLaunch.Load.BytesTotal != 400 {
		t.Fatalf("%+v last_launch %+v", s, s.LastLaunch)
	}
}
