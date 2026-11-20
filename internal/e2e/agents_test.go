package e2e

import (
	"net"
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
