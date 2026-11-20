package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/harness"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// The same-build rule (agent contract 5.1) against fakellama's --version.
func TestHostBuildCheck(t *testing.T) {
	fake := harness.BuildFakeLlama(t)
	full := "a01665d30de2885879a64eae999067ef3ef5fd4b"
	got, problem := hostBuildCheck(context.Background(), fake, full)
	if got != "a01665d30" || problem != "" {
		t.Fatalf("same build: %q %q", got, problem)
	}
	t.Setenv("FAKELLAMA_BUILD", "deadbeef1")
	got, problem = hostBuildCheck(context.Background(), fake, full)
	if got != "deadbeef1" || !strings.Contains(problem, "deadbeef1") || !strings.Contains(problem, full) {
		t.Fatalf("other build: %q %q", got, problem)
	}
	if _, problem := hostBuildCheck(context.Background(), "", full); problem == "" {
		t.Fatal("no server_bin passed the check")
	}
}

func TestLoadProgramsReportsWhyNothingIsServed(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Defaults: config.DefaultDefaults()}
	if pr := loadPrograms(context.Background(), cfg, log); pr.Release != nil || !strings.Contains(pr.Err, "--programs-dir") {
		t.Fatalf("%+v", pr)
	}
	cfg.ProgramsDir = progtree.Write(t, t.TempDir(), progtree.Tree{Release: potluckRelease, Llama: "a01665d30de2885879a64eae999067ef3ef5fd4b",
		Folders: map[string]map[string][]byte{"linux-amd64-vulkan": {"llama-server": []byte("l"), "rpc-server": []byte("r")}}})
	cfg.Defaults.ServerBin = harness.BuildFakeLlama(t)
	pr := loadPrograms(context.Background(), cfg, log)
	if pr.Release == nil || pr.HostErr != "" || pr.HostBuild != "a01665d30" {
		t.Fatalf("%+v", pr)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	harness.Cleanup()
	os.Exit(code)
}
