package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// A panic in the scheduler's event loop ends the process through the
// bounded fatal path: exit code 3 well within the watchdog, the failure
// logged at error level with its task and recorded in the history together
// with the stopping event. The loop is not restarted.
func TestSchedulerLoopPanicTakesTheFatalPath(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	prevExit := fatalExit
	fatalExit = func(code int) { t.Errorf("the watchdog fired (exit %d): the fatal path overran its bound", code) }
	defer func() { fatalExit = prevExit }()

	dir := t.TempDir()
	file := filepath.Join(dir, "seed.yaml")
	yaml := "listen: \"127.0.0.1:0\"\nlocal_node: host-1\ndefaults: {membership: agents}\nprofiles: {}\n"
	if err := os.WriteFile(file, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	data, models, hdir := filepath.Join(dir, "data"), filepath.Join(dir, "models"), filepath.Join(dir, "history")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := realMain([]string{"import-config", "--data-dir", data, "--models-dir", models, file}, &out, &errb); code != 0 {
		t.Fatalf("import: exit %d: %s", code, errb.String())
	}
	var logs syncBuf
	sigs := make(chan os.Signal, 2)
	done := make(chan int, 1)
	boot := config.Bootstrap{Listen: "127.0.0.1:0", DataDir: data, ModelsDir: models, HistoryDir: hdir}
	go func() { done <- serveUntil(boot, slog.New(slog.NewTextHandler(&logs, nil)), sigs) }()
	listen := regexp.MustCompile(`msg="llm-scheduler started".* listen=(\S+)`)
	var addr string
	for deadline := time.Now().Add(60 * time.Second); addr == ""; {
		if m := listen.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
			break
		}
		select {
		case code := <-done:
			t.Fatalf("stopped (exit %d) before starting:\n%s", code, logs.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not start:\n%s", logs.String())
		}
	}

	h, fired := supervise.PanicAtForTest("scheduler.loop", 1)
	defer supervise.SetHookForTest(h)()
	began := time.Now()
	go func() { // any request the loop answers runs a step
		if resp, err := http.Get("http://" + addr + "/api/needs"); err == nil {
			resp.Body.Close()
		}
	}()
	var code int
	select {
	case code = <-done:
	case <-time.After(fatalWatchdog + 5*time.Second):
		t.Fatalf("the scheduler did not stop:\n%s", logs.String())
	}
	took := time.Since(began)
	if code != supervise.ExitCode || fired() != 1 {
		t.Fatalf("exit %d after %d injected panics, want %d after 1:\n%s", code, fired(), supervise.ExitCode, logs.String())
	}
	if took > fatalWatchdog {
		t.Fatalf("the fatal path took %s", took)
	}
	l := logs.String()
	for _, want := range []string{`level=ERROR msg="background task failed" task=scheduler.loop policy=exit`, "escalating=true",
		"Potluck stops now so that it is started again (exit code 3)"} {
		if !strings.Contains(l, want) {
			t.Errorf("the log lacks %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "msg=stopped") {
		t.Error("the graceful shutdown ran")
	}
	var events strings.Builder
	files, _ := filepath.Glob(filepath.Join(hdir, "events-*.jsonl"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		events.Write(b)
	}
	ev := events.String()
	if !strings.Contains(ev, `"type":"internalFailure"`) || !strings.Contains(ev, `"task":"scheduler.loop"`) ||
		!strings.Contains(ev, `internal failure in scheduler.loop`) {
		t.Fatalf("the history lacks the failure or the stopping event:\n%s", ev)
	}
}

// Review IS2: once the graceful shutdown has begun, a task failure does not
// cut it short. It is recorded (the exit code is 3), but nothing is sent on
// ch (main no longer reads it) and the 5 s watchdog is not armed: only a
// watchdog past the shutdown's own bound is, in case the shutdown hangs.
func TestFailureDuringTheGracefulShutdownKeepsTheGrace(t *testing.T) {
	exited := make(chan int, 1)
	prevExit := fatalExit
	fatalExit = func(code int) { exited <- code }
	defer func() { fatalExit = prevExit }()

	f := newFatalPath()
	f.beginShutdown(fatalWatchdog + 300*time.Millisecond) // a grace longer than the 5 s watchdog
	f.trigger("bench.manager: boom")
	if !f.fired.Load() {
		t.Fatal("the failure was not recorded")
	}
	select {
	case r := <-f.ch:
		t.Fatalf("sent %q on ch during the shutdown", r)
	default:
	}
	select {
	case code := <-exited:
		t.Fatalf("exit %d within the grace", code)
	case <-time.After(fatalWatchdog + 100*time.Millisecond):
	}
	select { // the shutdown's bound, then the watchdog, ends a shutdown that hangs
	case code := <-exited:
		if code != supervise.ExitCode {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(fatalWatchdog + 2*time.Second):
		t.Fatal("no watchdog past the shutdown's bound")
	}

	g := newFatalPath() // a shutdown that finishes disarms it
	g.beginShutdown(50 * time.Millisecond)
	g.trigger("x")
	g.disarm()
	select {
	case code := <-exited:
		t.Fatalf("exit %d after disarm", code)
	case <-time.After(fatalWatchdog + 300*time.Millisecond):
	}
}
