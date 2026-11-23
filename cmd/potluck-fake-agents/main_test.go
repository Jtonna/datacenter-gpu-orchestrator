package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/api"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

const testConfig = `
listen: ":0"
fleet: {local_node: sched-host}
models_dir: /models
defaults: {membership: agents, ttl: 0s}
tiers:
  - match: {hostname: "ps5-*"}
    priority: 1
    headroom_mib: 300
    shared_memory: true
profiles:
  forced:
    model: forced.gguf
    ctx: 4096
    needs_mib: 4000
    force_nodes: [ps5-b]
  auto:
    model: auto.gguf
    ctx: 4096
    needs_mib: 20000
`

type noProber struct{}

func (noProber) Probe(context.Context, string) (instance.ProbeResult, error) {
	return instance.ProbeResult{}, errors.New("no probe in this test")
}

type noLauncher struct{}

func (noLauncher) Start(instance.StartSpec) (instance.Child, error) {
	return nil, errors.New("nothing is started in this test")
}

// scheduler is a real scheduler in agent membership, in process: the pool
// (with a verified program release), the scheduler on it and the REST API.
func startScheduler(t *testing.T, agentPort, workerPort int) string {
	t.Helper()
	cfg, err := config.ParseWithOptions([]byte(testConfig), config.Options{FileExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: agentproto.Release, Llama: "a01665d30",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("AGENT")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("R")},
		}})
	rel, err := release.Load(root, agentproto.Release)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pl, err := pool.New(pool.Options{Config: func() *config.Config { return cfg }, Programs: pool.Programs{Release: rel},
		PotluckRelease: agentproto.Release, AgentPort: agentPort, WorkerPort: workerPort, Logger: log,
		CallBackEvery: 50 * time.Millisecond,
		RunProbe:      func(context.Context, string) ([]byte, error) { return []byte("24576, 1024\n"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go pl.Run(ctx)
	sched, err := scheduler.New(cfg, scheduler.Options{View: pl, Workers: pl, LoadGate: pl.LoadGate, Prober: noProber{}, Launcher: noLauncher{},
		Needs: scheduler.NeedFunc(func(p *core.Profile) (scheduler.NeedInfo, error) {
			return scheduler.NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1 << 20}, nil
		}), Logger: log, Release: agentproto.Release})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	a := api.New(cfg, sched, api.Options{Logger: log, Pool: pl})
	srv := httptest.NewServer(a)
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close(); cancel(); <-done; a.Close() })
	return srv.URL
}

func freeLoopbackPort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("no second loopback address on this machine: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func getMap(t *testing.T, url string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := getJSON(context.Background(), url, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The fake agents of nodes.sample.json register with a real scheduler; the
// scheduler lists all six active and placeable, a profile forcing one of
// them plans "start", and a launch is refused by default.
func TestFakeAgentsAreActiveAndPlaceable(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	base := startScheduler(t, agentPort, workerPort)
	nodes, err := readNodes("nodes.sample.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := assignAddrs(nodes, "127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	f, err := Start(context.Background(), Options{Scheduler: base, Release: "auto", AgentPort: agentPort, WorkerPort: workerPort,
		Heartbeat: 100 * time.Millisecond}, nodes, &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	t.Logf("%s", out.String())
	if !strings.Contains(out.String(), "6 of 6 fake agents registered") || !strings.Contains(out.String(), "as release "+agentproto.Release) {
		t.Fatalf("start-up output:\n%s", out.String())
	}
	var st map[string]any
	eventually(t, "six active, placeable agents", func() bool {
		st = getMap(t, base+"/api/status")
		n := 0
		for _, x := range st["agents"].([]any) {
			a := x.(map[string]any)
			if a["state"] == "active" && a["placeable"] == true && a["release"] == agentproto.Release {
				n++
			}
		}
		return n == 6
	})
	for _, x := range st["agents"].([]any) {
		a := x.(map[string]any)
		if a["platform"] != "linux-amd64-vulkan" || a["programs_ready"] != true || toJSON(a["capabilities"]) != `["file_cache_evict"]` {
			t.Fatalf("agent %v", a)
		}
	}
	plan := postPlan(t, base, "forced")
	if pl, _ := plan["placement"].([]any); plan["action"] != "start" || len(pl) != 1 || pl[0].(map[string]any)["id"] != "02:00:00:00:00:02" {
		t.Fatalf("plan for the forced profile: %v", plan)
	}
	// 20000 MiB fits only across nodes: the automatic profile places on them too.
	if plan := postPlan(t, base, "auto"); plan["action"] != "start" {
		t.Fatalf("plan for the automatic profile: %v", plan)
	}
	// Nothing can load: a launch is refused, and says why.
	resp, err := http.Post("http://"+net.JoinHostPort("127.0.0.3", strconv.Itoa(agentPort))+"/launch", "application/json",
		strings.NewReader(`{"launch_id":"l1","role":"worker"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "launches are refused") {
		t.Fatalf("launch: %d %s", resp.StatusCode, b)
	}

	// --from-status mirrors this scheduler's agents: ids, names, memory; never addresses.
	file := filepath.Join(t.TempDir(), "status.json")
	raw, _ := json.Marshal(st)
	os.WriteFile(file, raw, 0o644)
	for _, src := range []string{file, base + "/api/status", base + "/api/nodes"} {
		got, err := nodesFromStatus(src)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 6 || got[1].ID != "02:00:00:00:00:02" || got[1].Name != "ps5-b" || got[1].MemAvailableMiB != 14200 ||
			len(got[1].Devices) != 1 || got[1].Devices[0].TotalMiB != 14336 || got[1].Addr != "" || got[1].WatchdogFloorMiB != 1000 {
			t.Fatalf("%s: %+v", src, got)
		}
	}
}

// --accept-launches: a launch is accepted and its fake worker becomes ready.
func TestAcceptLaunches(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	base := startScheduler(t, agentPort, workerPort)
	nodes := []Node{{ID: "02:00:00:00:00:01", Name: "ps5-a"}}
	fillDefaults(&nodes[0])
	assignAddrs(nodes, "127.0.0.2")
	f, err := Start(context.Background(), Options{Scheduler: base, Release: "auto", AgentPort: agentPort, WorkerPort: workerPort,
		AcceptLaunches: true, Heartbeat: 100 * time.Millisecond}, nodes, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	resp, err := http.Post("http://"+net.JoinHostPort("127.0.0.2", strconv.Itoa(agentPort))+"/launch", "application/json",
		strings.NewReader(`{"launch_id":"l1","role":"worker","lease_ttl_s":30}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("launch: %d", resp.StatusCode)
	}
	eventually(t, "the fake worker ready", func() bool { return f.Agents[0].LaunchState().State == agentproto.StateReady })
}

func TestPrintMirrorsWithoutAddresses(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--nodes", "nodes.sample.json", "--print"}, &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"addr": "127.0.0.7"`) || strings.Count(out.String(), `"id"`) != 6 {
		t.Fatal(out.String())
	}
	if code := run([]string{"--print"}, &out, &errb); code != 2 {
		t.Fatalf("no node source: %d", code)
	}
}

func postPlan(t *testing.T, base, model string) map[string]any {
	t.Helper()
	resp, err := http.Post(base+"/api/plan", "application/json", strings.NewReader(`{"model":"`+model+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
