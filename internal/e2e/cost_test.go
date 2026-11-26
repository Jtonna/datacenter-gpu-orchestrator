package e2e

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// TestCostAttribution measures what the scheduler costs with nobody looking
// and with one browser tab on a page, on a fleet shaped as the owner's: seven
// agents heartbeating every 2 s (pool.HeartbeatEvery, the interval the
// registration answer asks for) with a stats object and memory that moves on
// every heartbeat, and 315 profiles. For each case it reads the scheduler's
// loop steps (GET /api/system loop.steps) and the process's CPU time over
// windows of 30 s. Opt-in (it takes minutes):
//
//	POTLUCK_MEASURE=1 MEASURE_CHROME=<path of chrome> go test ./internal/e2e -run TestCostAttribution -v -timeout 9m
//
// Without MEASURE_CHROME only the no-client case runs. MEASURE_CASES picks
// cases (comma-separated: none, overview, models, benchmarks).
func TestCostAttribution(t *testing.T) {
	if os.Getenv("POTLUCK_MEASURE") == "" {
		t.Skip("opt-in: POTLUCK_MEASURE=1")
	}
	const agentsN, profilesN = 7, 315
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	programs := progtree.Write(t, t.TempDir(), progtree.Tree{Release: agentproto.Release,
		Llama: "a01665d30de2885879a64eae999067ef3ef5fd4b",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("agent")},
			"linux-amd64-vulkan": {"llama-server": []byte("llama-server"), "rpc-server": []byte("rpc-server")},
		}})
	var prof strings.Builder
	for i := 0; i < profilesN; i++ {
		fmt.Fprintf(&prof, "  p%03d:\n    model: m%03d.gguf\n    ctx: 4096\n    needs_mib: %d\n", i, i%120, 2000+(i%40)*700)
	}
	c := startCluster(t, options{
		defaults: "  membership: agents\n",
		args:     []string{"--programs-dir", programs},
		env: map[string]string{"LLM_SCHEDULER_TEST_AGENT_PORT": strconv.Itoa(agentPort),
			"LLM_SCHEDULER_TEST_WORKER_PORT": strconv.Itoa(workerPort)},
		profiles: prof.String(),
	})
	stats := func(avail int64) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"stats_version": 1,
			"sources": map[string]any{"node": map[string]any{"status": "ok", "age_ms": 500, "scrape_ms": 20}},
			"cpu":     map[string]any{"util_percent": 2.0, "logical_cpus": 8},
			"memory":  map[string]any{"total_bytes": int64(16000) << 20, "available_bytes": avail << 20},
			"load":    map[string]any{"load1": 0.1}, "net": []any{},
			"temps":           []any{map[string]any{"id": "fake/temp1", "label": "fake", "celsius": 50.0}},
			"headline_temp_c": 50.0, "fans": []any{}, "gpus": []any{}})
		return b
	}
	var agents []*fakeagent.Agent
	for i := 0; i < agentsN; i++ {
		a := &fakeagent.Agent{ID: fmt.Sprintf("02:00:00:00:00:%02d", i+1), Name: fmt.Sprintf("ps5-%c", 'a'+i), Addr: "127.0.0." + strconv.Itoa(i+2),
			AgentPort: agentPort, WorkerPort: workerPort, Potluck: c.url, MemAvail: 14000, HeartbeatEvery: 2 * time.Second, Stats: stats(14000),
			GPUs:    []agentproto.GPU{{Vendor: "amd", Integrated: true}},
			Devices: []agentproto.Device{{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14336, FreeMiB: 14000, SharesSystemMemory: true}}}
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		agents = append(agents, a)
	}
	// memory moves on every heartbeat, as live's MemAvailable does
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		r := rand.New(rand.NewSource(1))
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
			for _, a := range agents {
				m := 14000 - int64(r.Intn(400))
				a.SetMemAvail(m, stats(m))
			}
		}
	}()
	c.waitFor("seven agents in the pool", 30*time.Second, func() bool {
		st := c.jsonGet("/api/status")
		n := 0
		for _, nd := range st["view"].(map[string]any)["nodes"].([]any) {
			if ag, ok := nd.(map[string]any)["agent"].(map[string]any); ok && ag["state"] == "active" {
				n++
			}
		}
		return n == agentsN
	})
	steps := func() float64 {
		return c.jsonGet("/api/system")["loop"].(map[string]any)["steps"].(float64)
	}
	window := func(name string) {
		for w := 1; w <= 2; w++ {
			s0, c0, t0 := steps(), procCPU(c.cmd.Process.Pid), time.Now()
			time.Sleep(30 * time.Second)
			s1, c1, el := steps(), procCPU(c.cmd.Process.Pid), time.Since(t0)
			t.Logf("COST %-10s window %d: %5.0f loop steps (%4.1f/s, %.1f rounds of %d plans), CPU %5.2f s = %.3f core (%3.0f ticks per 30 s)",
				name, w, s1-s0, (s1-s0)/el.Seconds(), (s1-s0)/profilesN, profilesN, (c1 - c0).Seconds(), (c1-c0).Seconds()/el.Seconds(), (c1-c0).Seconds()*100*30/el.Seconds())
		}
	}
	cases := []string{"none", "overview", "models"}
	if v := os.Getenv("MEASURE_CASES"); v != "" {
		cases = strings.Split(v, ",")
	}
	chrome := os.Getenv("MEASURE_CHROME")
	time.Sleep(10 * time.Second) // the start settles (the first catalog, the agents' first reports)
	for _, cs := range cases {
		if cs == "none" {
			window("no client")
			continue
		}
		if chrome == "" {
			t.Logf("COST %s: skipped (no MEASURE_CHROME)", cs)
			continue
		}
		tab := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--user-data-dir="+t.TempDir(),
			"--remote-debugging-port=0", "--window-size=1536,864", c.url+"/ui/#/"+cs)
		if err := tab.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Second) // the open itself is not in the window
		window("tab on " + cs)
		tab.Process.Kill()
		tab.Wait()
		time.Sleep(5 * time.Second)
	}
}
