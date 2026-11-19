package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/ggufgen"
)

func writeConfig(t *testing.T, models, profiles string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	y := "listen: \"127.0.0.1:0\"\nmodels_dir: " + filepath.ToSlash(models) + "\ntiers:\n  - match: {local: true}\n    priority: 1\nprofiles:\n" + profiles
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVersionFlag(t *testing.T) {
	var out, errb bytes.Buffer
	if code := realMain([]string{"--version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "llm-scheduler ") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestImportConfigCheckAndRefuse(t *testing.T) {
	models := t.TempDir()
	os.WriteFile(filepath.Join(models, "a.gguf"), []byte("x"), 0o644)
	good := writeConfig(t, models, "  a:\n    model: a.gguf\n    ctx: 1024\n    needs_mib: 100\n")
	var out, errb bytes.Buffer
	if code := realMain([]string{"import-config", "--check", good}, &out, &errb); code != 0 {
		t.Fatalf("good config: exit %d: %s", code, errb.String())
	}
	bad := writeConfig(t, models, "  a:\n    model: missing.gguf\n    ctx: 1024\n    needs_mib: 100\n")
	out.Reset()
	errb.Reset()
	if code := realMain([]string{"import-config", "--check", bad}, &out, &errb); code != 1 {
		t.Fatalf("bad config: exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "missing.gguf") {
		t.Fatalf("errors not printed: %q", errb.String())
	}
	if code := realMain([]string{"import-config", "--check", filepath.Join(models, "nope.yaml")}, &out, &errb); code != 1 {
		t.Fatalf("missing file: exit %d", code)
	}
}

func TestImportConfigWritesRevisionOneAndRefusesSecond(t *testing.T) {
	models := t.TempDir()
	os.WriteFile(filepath.Join(models, "a.gguf"), []byte("x"), 0o644)
	file := writeConfig(t, models, "  a:\n    model: a.gguf\n    ctx: 1024\n    needs_mib: 100\n")
	data := t.TempDir()
	var out, errb bytes.Buffer
	args := []string{"import-config", "--data-dir", data, "--models-dir", models, file}
	if code := realMain(args, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "revision 1") {
		t.Fatalf("stdout %q", out.String())
	}
	out.Reset()
	errb.Reset()
	if code := realMain(args, &out, &errb); code != 1 || !strings.Contains(errb.String(), "--replace") {
		t.Fatalf("second import: exit %d %q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	file2 := writeConfig(t, models, "  a:\n    model: a.gguf\n    ctx: 2048\n    needs_mib: 100\n")
	if code := realMain([]string{"import-config", "--replace", "--data-dir", data, "--models-dir", models, file2}, &out, &errb); code != 0 || !strings.Contains(out.String(), "revision 2") {
		t.Fatalf("replace: exit %d %q %q", code, out.String(), errb.String())
	}
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	if code := realMain([]string{"--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestSlidingWindowWarning(t *testing.T) {
	models := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ggufgen.Write(filepath.Join(models, "gemma.gguf"), ggufgen.Spec{Arch: "gemma3", SlidingWindow: 1024}))
	must(ggufgen.Write(filepath.Join(models, "plain.gguf"), ggufgen.Spec{}))
	must(ggufgen.Write(filepath.Join(models, "gemma-measured.gguf"), ggufgen.Spec{Arch: "gemma3", SlidingWindow: 1024}))
	must(ggufgen.Write(filepath.Join(models, "gemma-forced.gguf"), ggufgen.Spec{Arch: "gemma3", SlidingWindow: 1024}))
	path := writeConfig(t, models, `  gemma:
    model: gemma.gguf
    ctx: 8192
  gemma-forced:
    model: gemma-forced.gguf
    ctx: 8192
    force_nodes: [ps5-a, omen]
  plain:
    model: plain.gguf
    ctx: 8192
  gemma-measured:
    model: gemma-measured.gguf
    ctx: 8192
    needs_mib: 7800
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	warnSlidingWindow(cfg, slog.New(slog.NewTextHandler(&buf, nil)))
	out := buf.String()
	if !strings.Contains(out, "profile=gemma ") || !strings.Contains(out, "needs_mib") || !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a warning for gemma:\n%s", out)
	}
	if strings.Contains(out, "profile=plain") || strings.Contains(out, "gemma-measured") {
		t.Fatalf("unexpected warning:\n%s", out)
	}
	// Forced profiles skip the fit check and never use the estimate (7.3).
	if strings.Contains(out, "gemma-forced") {
		t.Fatalf("warned for a force_nodes profile:\n%s", out)
	}
}

// A shutdown that overran its grace (children were killed) is not a clean
// stop: exit non-zero and say why.
func TestExitCodeAfterShutdown(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if got := exitCodeAfterShutdown(0, nil, log); got != 0 {
		t.Fatalf("clean shutdown: exit %d", got)
	}
	if got := exitCodeAfterShutdown(1, nil, log); got != 1 {
		t.Fatalf("earlier failure must stay: exit %d", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("clean shutdown logged: %s", buf.String())
	}
	if got := exitCodeAfterShutdown(0, context.DeadlineExceeded, log); got != 1 {
		t.Fatalf("overrun shutdown: exit %d, want 1", got)
	}
	if out := buf.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "killed") {
		t.Fatalf("no explanation logged: %q", out)
	}
}

func TestHistoryStartedAndStoppingEvents(t *testing.T) {
	models := t.TempDir()
	os.WriteFile(filepath.Join(models, "a.gguf"), []byte("x"), 0o644)
	cfg, err := config.Load(writeConfig(t, models, "  a:\n    model: a.gguf\n    ctx: 1024\n    needs_mib: 100\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e := startedEvent(cfg, "9.9.9", now)
	if e.Type != "schedulerStarted" || e.Data["version"] != "9.9.9" || e.Data["profiles"] != 1 || !strings.Contains(e.Summary, "9.9.9") {
		t.Fatalf("%+v", e)
	}
	safety := e.Data["safety"].(map[string]any)
	if safety["load_memory_floor_mib"] != 768 || safety["split_skew_margin"] != 0.15 || safety["recovery_enabled"] != false {
		t.Fatalf("safety %+v", safety)
	}
	if _, err := json.Marshal(e); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	t.Logf("%s", b)
	s := stoppingEvent("terminated", now)
	if s.Type != "schedulerStopping" || s.Data["reason"] != "terminated" {
		t.Fatalf("%+v", s)
	}
	b, _ = json.Marshal(s)
	t.Logf("%s", b)
	// Written through a store, they survive a restart.
	dir := t.TempDir()
	st := history.Open(history.Options{Dir: dir})
	st.Record(e)
	st.Record(s)
	st.Close()
	st = history.Open(history.Options{Dir: dir})
	defer st.Close()
	if r := st.Query(history.Query{Asc: true}); len(r.Events) != 2 || r.Events[0].Type != "schedulerStarted" {
		t.Fatalf("%+v", r)
	}
}
