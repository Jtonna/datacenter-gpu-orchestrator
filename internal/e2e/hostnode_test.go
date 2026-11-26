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

// TestAgentHostOnNode is phase 6, a console as host, end to end: the real
// llm-scheduler binary, a profile with host: node, and two fake agents. The
// first runs the host launch (a fake llama-server that wants the per-launch
// key), the second a worker launch. A request is answered through the
// proxy with the key; no host child runs on the scheduler's machine; the
// unload stops both launches and closes the host port.
func TestAgentHostOnNode(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	programs := progtree.Write(t, t.TempDir(), progtree.Tree{Release: agentproto.Release,
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
		profiles: `  hosted:
    model: hosted.gguf
    ctx: 4096
    needs_mib: 4000
    host: node
    force_nodes: [ps5-a, ps5-b]
    tensor_split: [1, 1]
`,
	})
	rpc := agenttest.Install(t, t.TempDir(), "rpc-server")
	var agents []*fakeagent.Agent
	for i, name := range []string{"ps5-a", "ps5-b"} {
		a := &fakeagent.Agent{ID: "02:00:00:00:00:0" + strconv.Itoa(i+1), Name: name, Addr: "127.0.0." + strconv.Itoa(i+2),
			AgentPort: agentPort, WorkerPort: workerPort, Potluck: c.url, RPCServer: rpc, MemAvail: 14000,
			GPUs:    []agentproto.GPU{{Vendor: "amd", Integrated: true}},
			Devices: []agentproto.Device{{Name: "Vulkan0", Backend: "vulkan", TotalMiB: 14336, FreeMiB: 14000, SharesSystemMemory: true}}}
		if i == 0 {
			a.Capabilities, a.HostPort = []string{agentproto.CapHostLaunch}, fakeagent.AutoPort // binds port 0 at the launch
		}
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

	status, body := c.chat("hosted", false)
	if status != 200 {
		t.Fatalf("chat: %d %s", status, body)
	}
	host, worker := agents[0], agents[1]
	hl, wl := host.Launches(), worker.Launches()
	if len(hl) != 1 || hl[0].Role != agentproto.RoleHost || hl[0].Host == nil || len(wl) != 1 || wl[0].Role != agentproto.RoleWorker {
		t.Fatalf("host %+v worker %+v", hl, wl)
	}
	if w := hl[0].Host.Workers; len(w) != 1 || w[0] != net.JoinHostPort(worker.Addr, strconv.Itoa(workerPort)) {
		t.Fatalf("workers %v", w)
	}
	key := hl[0].Host.APIKey
	auth := host.HostAuth()
	if len(key) < 32 || len(auth) == 0 || auth[len(auth)-1] != "Bearer "+key {
		t.Fatalf("key %q, host saw %q", key, auth)
	}
	for _, s := range c.starts() {
		if s.Mode == "serve" {
			t.Fatalf("a host child ran on the scheduler's machine: %v", s.Argv)
		}
	}
	// The key is never shown.
	if _, b := c.get("/api/status"); strings.Contains(b, key) {
		t.Fatal("the key is in /api/status")
	}
	if _, b := c.get("/running"); strings.Contains(b, key) {
		t.Fatal("the key is in /running")
	}

	if status, body := c.post("/api/unload", `{"model":"hosted"}`); status != 200 {
		t.Fatalf("unload: %d %s", status, body)
	}
	c.waitFor("both launches stopped", 15*time.Second, func() bool {
		for _, a := range agents {
			st := a.LaunchState()
			if st.State != agentproto.StateExited || st.Exit == nil || st.Exit.Reason != agentproto.ExitStopped {
				return false
			}
		}
		return true
	})
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(host.Addr, strconv.Itoa(host.HostPortNow())), 500*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("the host port is still open after the unload")
	}
}
