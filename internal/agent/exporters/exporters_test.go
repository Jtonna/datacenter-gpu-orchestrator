package exporters

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ports"
)

func TestMain(m *testing.M) {
	code := m.Run()
	agenttest.Cleanup()
	os.Exit(code)
}

func TestValidate(t *testing.T) {
	for args, ok := range map[string]bool{
		"--web.listen-address=127.0.0.1:50070 --collector.cpu": true,
		"--web.listen-address 127.0.0.1:50071":                 true,
		"--web.listen-address=0.0.0.0:50070":                   false,
		"--web.listen-address=:50070":                          false,
		"--collector.cpu":                                      false,
		"--web.listen-address=127.0.0.1:":                      false,
	} {
		err := Validate(agentproto.ExporterSpec{Name: "node_exporter", Args: strings.Fields(args)})
		if (err == nil) != ok {
			t.Errorf("%q: %v", args, err)
		}
	}
	if UnitName("node_exporter") != "potluck-node-exporter" || UnitName("nvidia_gpu_exporter") != "potluck-nvidia-exporter" {
		t.Fatal("unit names")
	}
}

// freePort is a port the exporter under test binds later: outside the
// ephemeral range (testutil/ports).
func freePort(t *testing.T) int { return ports.Free(t, "127.0.0.1") }

func TestSupervisorRunsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	prog := agenttest.Install(t, dir, "node_exporter")
	fixture := filepath.Join(dir, "metrics.prom")
	os.WriteFile(fixture, []byte("node_load1 0.5\n"), 0o644)
	t.Setenv("FAKEPROG_FIXTURE", fixture)
	port := freePort(t)
	child := runner.NewChild()
	s := &Supervisor{Runner: child, Path: func(string) (string, bool) { return prog, true }, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srcs, refused := s.Set(context.Background(), []agentproto.ExporterSpec{
		{Name: "node_exporter", Args: []string{"--web.listen-address=" + addr}},
		{Name: "nvidia_gpu_exporter", Args: []string{"--web.listen-address=0.0.0.0:1"}},
	})
	defer s.StopAll()
	if len(srcs) != 1 || srcs[0].Name != "node" || srcs[0].URL != "http://"+addr+"/metrics" || len(refused) != 1 {
		t.Fatalf("%v %v", srcs, refused)
	}
	get := func() bool {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	}
	agenttest.Eventually(t, 10*time.Second, "exporter serving", get)
	// It crashes; the supervisor starts it again.
	units, _ := child.List(context.Background(), "potluck-node-exporter")
	if len(units) != 1 {
		t.Fatal(units)
	}
	child.Stop(context.Background(), "potluck-node-exporter")
	agenttest.Eventually(t, 10*time.Second, "restarted", func() bool {
		u, _ := child.List(context.Background(), "potluck-node-exporter")
		return len(u) == 1 && get()
	})
	s.StopAll()
	if u, _ := child.List(context.Background(), "potluck-"); len(u) != 0 {
		t.Fatal("left running", u)
	}
}

func serving(addr string) bool {
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func exporterRig(t *testing.T) (*Supervisor, *runner.Child, string, []agentproto.ExporterSpec) {
	dir := t.TempDir()
	prog := agenttest.Install(t, dir, "node_exporter")
	fixture := filepath.Join(dir, "metrics.prom")
	os.WriteFile(fixture, []byte("node_load1 0.5\n"), 0o644)
	t.Setenv("FAKEPROG_FIXTURE", fixture)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	child := runner.NewChild()
	s := &Supervisor{Runner: child, Path: func(string) (string, bool) { return prog, true }}
	t.Cleanup(s.StopAll)
	return s, child, addr, []agentproto.ExporterSpec{{Name: "node_exporter", Args: []string{"--web.listen-address=" + addr}}}
}

// The context of an earlier Set ended (a registration's pipeline was
// cancelled) and stopped the exporter: the next Set with the same spec must
// start it again, not take it for running.
func TestSetStartsAnExporterWhoseSupervisionEnded(t *testing.T) {
	s, _, addr, specs := exporterRig(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	s.Set(ctx1, specs)
	agenttest.Eventually(t, 10*time.Second, "exporter serving", func() bool { return serving(addr) })
	cancel1()
	agenttest.Eventually(t, 10*time.Second, "exporter stopped", func() bool { return !serving(addr) })
	s.Set(context.Background(), specs)
	agenttest.Eventually(t, 10*time.Second, "exporter serving again", func() bool { return serving(addr) })
}

// Set with an unchanged spec leaves the running exporter alone.
func TestSetKeepsAnUnchangedExporter(t *testing.T) {
	s, child, addr, specs := exporterRig(t)
	var starts atomic.Int32
	s.OnStart = func(string) { starts.Add(1) }
	s.Set(context.Background(), specs)
	agenttest.Eventually(t, 10*time.Second, "exporter serving", func() bool { return serving(addr) })
	s.Set(context.Background(), specs)
	time.Sleep(200 * time.Millisecond)
	if u, _ := child.List(context.Background(), "potluck-node-exporter"); starts.Load() != 1 || len(u) != 1 || !serving(addr) {
		t.Fatalf("starts %d units %v", starts.Load(), u)
	}
}

// An exporter that should run and does not is started again by Revive, at
// most once per ReviveEvery, and only when its scrape fails.
func TestReviveStartsAGoneExporterOncePerPeriod(t *testing.T) {
	s, child, addr, specs := exporterRig(t)
	s.MinBackoff, s.MaxBackoff, s.ReviveEvery = time.Hour, time.Hour, time.Hour // no start but Revive's
	var logs strings.Builder
	s.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s.Set(context.Background(), specs)
	agenttest.Eventually(t, 10*time.Second, "exporter serving", func() bool { return serving(addr) })
	failing := true
	fails := func(src string) bool { return src == agentproto.SourceNode && failing }

	s.Revive(fails) // it runs: nothing to do
	child.Stop(context.Background(), "potluck-node-exporter")
	agenttest.Eventually(t, 10*time.Second, "exporter gone", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.running["node_exporter"].up && !serving(addr)
	})
	failing = false
	s.Revive(fails) // the scrape works: not ours to start
	time.Sleep(100 * time.Millisecond)
	if serving(addr) {
		t.Fatal("revived although the scrape does not fail")
	}
	failing = true
	s.Revive(fails)
	agenttest.Eventually(t, 10*time.Second, "revived", func() bool { return serving(addr) })

	// Gone again within the period: not started a second time.
	child.Stop(context.Background(), "potluck-node-exporter")
	agenttest.Eventually(t, 10*time.Second, "exporter gone again", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.running["node_exporter"].up && !serving(addr)
	})
	s.Revive(fails)
	time.Sleep(200 * time.Millisecond)
	if serving(addr) {
		t.Fatal("revived twice within the period")
	}
	s.StopAll()
	if n := strings.Count(logs.String(), "exporter is not running; starting it again"); n != 1 {
		t.Fatalf("%d log lines, want 1\n%s", n, logs.String())
	}
}
