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
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ports"
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

// freeLoopbackPort is a port on the second loopback address that a fake
// agent binds later: outside the ephemeral range (testutil/ports).
func freeLoopbackPort(t *testing.T) int {
	p, err := ports.FreeOn("127.0.0.2")
	if err != nil {
		t.Skipf("no second loopback address on this machine: %v", err)
	}
	return p
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
		// The stats block of a real agent, with its headline temperature
		// (50 when nodes.json gives none), as /api/nodes passes it on.
		s, _ := a["stats"].(map[string]any)
		mem, _ := s["memory"].(map[string]any)
		if s["stats_version"] != float64(1) || s["headline_temp_c"] != float64(50) || mem["total_bytes"] == nil ||
			toJSON(s["sources"]) != `{"node":{"age_ms":500,"scrape_ms":20,"status":"ok"}}` || len(s["temps"].([]any)) != 1 {
			t.Fatalf("stats %v (note %v)", toJSON(s), a["stats_note"])
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
	// The fake agent marks the launch ready once its worker port answers a
	// TCP dial (fakeagent becomeReady): 0.16 s alone, but under a full
	// parallel race run the machine can stall for seconds. Wait on that
	// condition with a generous bound, and say the last state seen.
	var last agentproto.LaunchState
	deadline := time.Now().Add(60 * time.Second)
	for {
		if last = f.Agents[0].LaunchState(); last.State == agentproto.StateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake worker did not become ready in 60 s (launch l1 state %q, port %d): the agent dials its worker port before it marks the launch ready", last.State, last.Port)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --from-status mirrors the headline temperature of each node's stats; a
// node without one gets the fixed 50 degrees.
func TestFromStatusMirrorsHeadlineTemp(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nodes.json")
	os.WriteFile(file, []byte(`{"nodes":[
	  {"id":"02:00:00:00:00:01","name":"ps5-a","state":"active","stats":{"stats_version":1,"headline_temp_c":61.5}},
	  {"id":"02:00:00:00:00:02","name":"ps5-b","state":"lost"},
	  {"id":"02:00:00:00:00:03","name":"ps5-c","state":"active","stats":{"stats_version":1}}]}`), 0o644)
	nodes, err := nodesFromStatus(file)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{61.5, 50, 50}
	for i, n := range nodes {
		var s struct {
			Version  int      `json:"stats_version"`
			Headline *float64 `json:"headline_temp_c"`
			Temps    []struct {
				Celsius float64 `json:"celsius"`
			} `json:"temps"`
			Memory struct {
				Total int64 `json:"total_bytes"`
				Avail int64 `json:"available_bytes"`
			} `json:"memory"`
			GPUs []any `json:"gpus"`
		}
		if err := json.Unmarshal(statsOf(n), &s); err != nil {
			t.Fatal(err)
		}
		if s.Version != 1 || s.Headline == nil || *s.Headline != want[i] || len(s.Temps) != 1 || s.Temps[0].Celsius != want[i] ||
			s.Memory.Total != n.MemTotalMiB<<20 || s.Memory.Avail != n.MemAvailableMiB<<20 || s.GPUs == nil {
			t.Fatalf("%s: %s", n.Name, statsOf(n))
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"--from-status", file, "--print"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"headline_temp_c": 61.5`) {
		t.Fatalf("--print keeps the mirrored temperature: %d %s %s", code, out.String(), errb.String())
	}
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

// --mem-file: a node listed by name reports the figure from the next
// heartbeat on (a scripted memory drop for a context fill's stop line).
func TestMemFileDropsANodesMemory(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	base := startScheduler(t, agentPort, workerPort)
	nodes := []Node{{ID: "02:00:00:00:00:01", Name: "ps5-a"}, {ID: "02:00:00:00:00:02", Name: "ps5-b"}}
	for i := range nodes {
		fillDefaults(&nodes[i])
	}
	assignAddrs(nodes, "127.0.0.2")
	file := filepath.Join(t.TempDir(), "mem.json")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f, err := Start(ctx, Options{Scheduler: base, Release: "auto", AgentPort: agentPort, WorkerPort: workerPort,
		Heartbeat: 100 * time.Millisecond, MemFile: file}, nodes, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	avail := func(name string) any {
		for _, x := range getMap(t, base+"/api/status")["agents"].([]any) {
			if a := x.(map[string]any); a["name"] == name {
				return a["mem_available_mib"]
			}
		}
		return nil
	}
	eventually(t, "both at the start figure", func() bool { return avail("ps5-a") == float64(15000) && avail("ps5-b") == float64(15000) })
	os.WriteFile(file, []byte(`{"ps5-b": 1500}`), 0o644)
	eventually(t, "ps5-b dropped", func() bool { return avail("ps5-b") == float64(1500) })
	if avail("ps5-a") != float64(15000) {
		t.Fatalf("ps5-a moved: %v", avail("ps5-a"))
	}
}

// --mem-file "silent:<name>": N withholds that agent's reports for N s,
// taking effect within the file's 100 ms poll (not at the next heartbeat),
// and the agent reports at once when the silence ends; the other agent
// keeps reporting.
func TestMemFileSilentWithholdsReports(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	base := startScheduler(t, agentPort, workerPort)
	nodes := []Node{{ID: "02:00:00:00:00:01", Name: "ps5-a"}, {ID: "02:00:00:00:00:02", Name: "ps5-b"}}
	for i := range nodes {
		fillDefaults(&nodes[i])
	}
	assignAddrs(nodes, "127.0.0.2")
	file := filepath.Join(t.TempDir(), "mem.json")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f, err := Start(ctx, Options{Scheduler: base, Release: "auto", AgentPort: agentPort, WorkerPort: workerPort,
		Heartbeat: time.Second, MemFile: file}, nodes, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	ago := func(name string) float64 {
		for _, x := range getMap(t, base+"/api/status")["agents"].([]any) {
			if a := x.(map[string]any); a["name"] == name {
				v, _ := a["last_seen_ago_s"].(float64)
				return v
			}
		}
		return -1
	}
	// Write the silence just after a report of ps5-b: its next one is due about 1 s later.
	eventually(t, "ps5-b just reported", func() bool { a := ago("ps5-b"); return a >= 0 && a < 0.1 })
	wrote := time.Now()
	os.WriteFile(file, []byte(`{"silent:ps5-b": 2}`), 0o644)
	time.Sleep(time.Until(wrote.Add(1600 * time.Millisecond)))
	if a := ago("ps5-b"); a < 1.3 { // a report at the 1 s tick would make this about 0.6
		t.Fatalf("ps5-b reported during its silence: last seen %.2f s ago, 1.6 s after the write", a)
	}
	if a := ago("ps5-a"); a > 1.2 {
		t.Fatalf("ps5-a went silent too: %v", a)
	}
	// The silence ends about 2.1 s after the write: a report goes out at once.
	time.Sleep(time.Until(wrote.Add(2500 * time.Millisecond)))
	if a := ago("ps5-b"); a > 0.45 {
		t.Fatalf("ps5-b did not report at once after its silence: last seen %.2f s ago, 2.5 s after the write", a)
	}
}
