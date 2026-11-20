package exporters

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
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

func freePort(t *testing.T) int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

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
