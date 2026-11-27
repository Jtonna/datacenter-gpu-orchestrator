package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// syncBuf is a log sink that the scheduler's goroutines and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// startLog imports yaml into a fresh database, starts the scheduler, waits
// until want has been logged (or the scheduler has started, when want is
// empty), stops it and returns everything it logged.
func startLog(t *testing.T, yaml, want string) string {
	t.Helper()
	// Test names stay clear of the word fleet: the temporary folders carry them into the log.
	dir := t.TempDir()
	file := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(file, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	data, models := filepath.Join(dir, "data"), filepath.Join(dir, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := realMain([]string{"import-config", "--data-dir", data, "--models-dir", models, file}, &out, &errb); code != 0 {
		t.Fatalf("import: exit %d: %s", code, errb.String())
	}
	if want == "" {
		want = "llm-scheduler started"
	}
	var logs syncBuf
	sigs := make(chan os.Signal, 2)
	done := make(chan int, 1)
	boot := config.Bootstrap{Listen: "127.0.0.1:0", DataDir: data, ModelsDir: models}
	go func() {
		done <- serveUntil(boot, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), sigs)
	}()
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(logs.String(), want) {
		select {
		case code := <-done:
			t.Fatalf("the scheduler stopped (exit %d) before logging %q:\n%s", code, want, logs.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q within a minute:\n%s", want, logs.String())
		}
	}
	sigs <- os.Interrupt
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("the scheduler did not stop:\n%s", logs.String())
	}
	return logs.String()
}

// Membership agents stands alone: whether fleet.url is empty or set, no log
// line at any level names the fleet or its console, and one INFO line says
// where the nodes come from. A configuration with no fleet section starts.
func TestAgentsStartStandsAlone(t *testing.T) {
	const base = "listen: \"127.0.0.1:0\"\nlocal_node: host-1\ndefaults: {membership: agents}\nprofiles: {}\n"
	for name, yaml := range map[string]string{
		"no section": base,
		"empty url":  base + "fleet: {url: \"\", local_node: host-old}\n",
		"url set":    base + "fleet: {url: \"http://192.0.2.1:8090\", stale_after: 30s}\n",
	} {
		t.Run(name, func(t *testing.T) {
			out := startLog(t, yaml, "")
			t.Logf("start log:\n%s", out)
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(strings.ToLower(line), "fleet") {
					t.Errorf("a log line names the fleet: %s", line)
				}
			}
			if !strings.Contains(out, `level=INFO msg="membership is agents: nodes come from potluck-agent registrations"`) {
				t.Errorf("no INFO line on where the nodes come from:\n%s", out)
			}
			if !strings.Contains(out, "msg=stopped") {
				t.Errorf("no clean stop:\n%s", out)
			}
		})
	}
}

// Fleet membership keeps its start-up checks and their wording.
func TestFleetStartStillChecksTheFleetConsole(t *testing.T) {
	out := startLog(t, "listen: \"127.0.0.1:0\"\ndefaults: {membership: fleet}\nprofiles: {}\n", "no fleet.url configured: only the local node can be used")
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("the missing fleet.url is no longer an ERROR:\n%s", out)
	}
}
