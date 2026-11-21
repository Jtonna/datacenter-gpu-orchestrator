package e2e

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// TestAgentMembershipSplitLoad is agent contract 13 item 2's "worker launches
// around a load", end to end: the real llm-scheduler binary in membership
// agents, with its program release verified at start; two fake agents
// (internal/testutil/fakeagent: built on agentproto, not the real agent code)
// that register over HTTP and run the fake rpc-server as a real child on their
// worker port; and fakellama as the host child. A split load becomes ready,
// answers a request, and its unload stops both worker launches.
func TestAgentMembershipSplitLoad(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	programs := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0",
		Llama: "a01665d30de2885879a64eae999067ef3ef5fd4b", // fakellama --version reports a01665d30
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("agent")},
			"linux-amd64-vulkan": {"llama-server": []byte("llama-server"), "rpc-server": []byte("rpc-server")},
		}})
	c := startCluster(t, options{
		defaults: "  membership: agents\n",
		args:     []string{"--programs-dir", programs},
		env: map[string]string{"LLM_SCHEDULER_TEST_AGENT_PORT": strconv.Itoa(agentPort),
			"LLM_SCHEDULER_TEST_WORKER_PORT": strconv.Itoa(workerPort)},
		profiles: `  split:
    model: split.gguf
    ctx: 4096
    needs_mib: 4000
    force_nodes: [ps5-a, ps5-b]
    tensor_split: [1, 1]
`,
	})
	if sys := c.jsonGet("/api/system"); sys["programs"].(map[string]any)["served"] != true {
		t.Fatalf("programs %+v", sys["programs"])
	}
	rpc := agenttest.Install(t, t.TempDir(), "rpc-server")
	var agents []*fakeagent.Agent
	for i, name := range []string{"ps5-a", "ps5-b"} {
		a := &fakeagent.Agent{ID: "02:00:00:00:00:0" + strconv.Itoa(i+1), Name: name, Addr: "127.0.0." + strconv.Itoa(i+2),
			AgentPort: agentPort, WorkerPort: workerPort, Potluck: c.url, RPCServer: rpc, MemAvail: 14000,
			GPUs:    []agentproto.GPU{{Vendor: "amd", Integrated: true}},
			Devices: []agentproto.Device{{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14336, FreeMiB: 14000, SharesSystemMemory: true}}}
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		agents = append(agents, a)
	}
	c.waitFor("both agents in the pool", 10*time.Second, func() bool {
		st := c.jsonGet("/api/status")
		n := 0
		for _, nd := range st["view"].(map[string]any)["nodes"].([]any) {
			if ag, ok := nd.(map[string]any)["agent"].(map[string]any); ok && ag["state"] == "active" && nd.(map[string]any)["free_mib"].(float64) > 0 {
				n++
			}
		}
		return n == 2
	})

	status, body := c.chat("split", false)
	if status != 200 {
		t.Fatalf("chat: %d %s", status, body)
	}
	var workers []string
	for _, a := range agents {
		st := a.LaunchState()
		if st.State != agentproto.StateReady || !a.Has("POST /launch") {
			t.Fatalf("%s: %+v %v", a.Name, st, a.Calls())
		}
		workers = append(workers, net.JoinHostPort(a.Addr, strconv.Itoa(workerPort)))
	}
	var hostArgv string
	for _, s := range c.starts() {
		if s.Mode == "serve" {
			hostArgv = strings.Join(s.Argv, " ")
		}
	}
	if !strings.Contains(hostArgv, "--rpc "+strings.Join(workers, ",")) && !strings.Contains(hostArgv, "--rpc "+workers[1]+","+workers[0]) {
		t.Fatalf("host child argv %q, want --rpc with %v", hostArgv, workers)
	}
	for _, s := range c.starts() {
		// worker_model_dir is off: the host is told to send the weights itself.
		if got, ok := s.Env["LLAMA_RPC_FILE_LOAD"]; s.Mode == "serve" && (!ok || got != "0") {
			t.Fatalf("host child LLAMA_RPC_FILE_LOAD=%q (set %v), want 0", got, ok)
		}
	}
	if n := c.countStarts("list-devices"); n != 0 {
		t.Fatalf("%d --list-devices probes ran; with agents nothing may hold a worker's client slot", n)
	}

	if status, body := c.post("/api/unload", `{"model":"split"}`); status != 200 {
		t.Fatalf("unload: %d %s", status, body)
	}
	c.waitFor("both worker launches stopped", 15*time.Second, func() bool {
		for _, a := range agents {
			st := a.LaunchState()
			if st.State != agentproto.StateExited || st.Exit == nil || st.Exit.Reason != agentproto.ExitStopped {
				return false
			}
		}
		return true
	})
	for _, a := range agents {
		for _, call := range a.Calls() {
			if strings.HasPrefix(call, "DELETE") && strings.Contains(call, "force") {
				t.Fatalf("%s: a normal unload used force: %v", a.Name, a.Calls())
			}
		}
	}
}

// freeLoopbackPort is a port free on 127.0.0.2 (the agents listen on
// 127.0.0.2 and 127.0.0.3 with the same ports).
func freeLoopbackPort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("no second loopback address on this machine: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestAgentNodeWithoutATierIsListed: an agent node that matches no tier is
// never silent. It is in the node list of GET /api/status, not placeable, with
// the reason no_tier_match (agent contract 11, CR-5).
func TestAgentNodeWithoutATierIsListed(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	programs := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0",
		Llama: "a01665d30de2885879a64eae999067ef3ef5fd4b",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("agent")},
			"linux-amd64-vulkan": {"llama-server": []byte("llama-server"), "rpc-server": []byte("rpc-server")},
		}})
	c := startCluster(t, options{
		defaults: "  membership: agents\n",
		args:     []string{"--programs-dir", programs},
		env: map[string]string{"LLM_SCHEDULER_TEST_AGENT_PORT": strconv.Itoa(agentPort),
			"LLM_SCHEDULER_TEST_WORKER_PORT": strconv.Itoa(workerPort)},
	})
	a := &fakeagent.Agent{ID: "02:00:00:00:00:09", Name: "stray-1", Addr: "127.0.0.2",
		AgentPort: agentPort, WorkerPort: workerPort, Potluck: c.url, MemAvail: 14000,
		GPUs:    []agentproto.GPU{{Vendor: "amd", Integrated: true}},
		Devices: []agentproto.Device{{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14336, FreeMiB: 14000, SharesSystemMemory: true}}}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	var node map[string]any
	c.waitFor("the node without a tier in the node list", 10*time.Second, func() bool {
		for _, nd := range c.jsonGet("/api/status")["view"].(map[string]any)["nodes"].([]any) {
			if m := nd.(map[string]any); m["id"] == a.ID {
				node = m
				return true
			}
		}
		return false
	})
	ag, _ := node["agent"].(map[string]any)
	if node["unplaced"] != "no_tier_match" || node["eligible"] != false || ag["unplaced"] != "no_tier_match" || ag["placeable"] != false {
		t.Fatalf("node %+v", node)
	}
}

// TestAgentWorkerKilledForMemoryIsTheFailureReason: the watchdog test on the
// real machine. The host program dies first (it aborts within milliseconds of
// losing its worker); the agent reports the launch as exited with reason
// watchdog a moment later. The request's 502 and history must name the
// watchdog, not the host program's exit, and the learned need is raised.
// The host child gets LLAMA_RPC_FILE_LOAD=required (worker_model_dir is on).
func TestAgentWorkerKilledForMemoryIsTheFailureReason(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	programs := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0",
		Llama: "a01665d30de2885879a64eae999067ef3ef5fd4b",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("agent")},
			"linux-amd64-vulkan": {"llama-server": []byte("llama-server"), "rpc-server": []byte("rpc-server")},
		}})
	c := startCluster(t, options{
		defaults: "  membership: agents\n  worker_model_dir: true\n",
		topLevel: "history_dir: \"" + filepath.ToSlash(filepath.Join(t.TempDir(), "history")) + "\"\n",
		args:     []string{"--programs-dir", programs},
		env: map[string]string{"LLM_SCHEDULER_TEST_AGENT_PORT": strconv.Itoa(agentPort),
			"LLM_SCHEDULER_TEST_WORKER_PORT": strconv.Itoa(workerPort),
			// The host program never becomes ready and dies 1 s after its start.
			"FAKELLAMA_LOAD_DELAY": "60s", "FAKELLAMA_EXIT_CODE": "134", "FAKELLAMA_EXIT_AFTER": "1s"},
		profiles: `  one:
    model: one.gguf
    ctx: 4096
    needs_mib: 4000
    force_nodes: [ps5-a]
`,
	})
	rpc := agenttest.Install(t, t.TempDir(), "rpc-server")
	a := &fakeagent.Agent{ID: "02:00:00:00:00:01", Name: "ps5-a", Addr: "127.0.0.2",
		AgentPort: agentPort, WorkerPort: workerPort, Potluck: c.url, RPCServer: rpc, MemAvail: 14000,
		GPUs:    []agentproto.GPU{{Vendor: "amd", Integrated: true}},
		Devices: []agentproto.Device{{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14336, FreeMiB: 14000, SharesSystemMemory: true}}}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	c.waitFor("the agent in the pool", 10*time.Second, func() bool {
		for _, nd := range c.jsonGet("/api/status")["view"].(map[string]any)["nodes"].([]any) {
			if ag, ok := nd.(map[string]any)["agent"].(map[string]any); ok && ag["state"] == "active" && ag["placeable"] == true {
				return true
			}
		}
		return false
	})

	type answer struct {
		status int
		body   string
	}
	done := make(chan answer, 1)
	go func() { s, b := c.chat("one", false); done <- answer{s, b} }()
	var host startEntry
	c.waitFor("the host program started", 15*time.Second, func() bool {
		for _, s := range c.starts() {
			if s.Mode == "serve" {
				host = s
				return true
			}
		}
		return false
	})
	if got := host.Env["LLAMA_RPC_FILE_LOAD"]; got != "required" {
		t.Fatalf("host child LLAMA_RPC_FILE_LOAD=%q, want required (worker_model_dir is on)", got)
	}
	// 1.5 s after the host program's start: it has been dead for half a
	// second, and the scheduler is still reading the launch.
	time.Sleep(time.Until(host.Time.Add(1500 * time.Millisecond)))
	a.ExitLaunch(agentproto.ExitWatchdog, "MemAvailable 900 MiB fell below the watchdog floor of 1000 MiB")

	var ans answer
	select {
	case ans = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("no answer")
	}
	if ans.status != 502 || !strings.Contains(ans.body, "exited: watchdog") || !strings.Contains(ans.body, "watchdog floor") {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	c.waitFor("workerExited and learnedNeed in history", 10*time.Second, func() bool {
		w := c.jsonGet("/api/history?type=workerExited&profile=one")["events"].([]any)
		n := c.jsonGet("/api/history?type=learnedNeed&profile=one")["events"].([]any)
		return len(w) == 1 && len(n) == 1
	})
}
