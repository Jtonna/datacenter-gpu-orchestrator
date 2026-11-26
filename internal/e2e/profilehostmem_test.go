package e2e

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// p33: a profile's own restore points and prompt cache (ctx_checkpoints,
// cache_ram_mib) are what the launch passes on the scheduler's machine, in
// place of the cluster values; a profile without them keeps those (2048,
// 32: TestChildVerbosityFeedsMemoryAndStderrStaysQuiet).
func TestProfileHostMemoryOnTheSchedulerLaunch(t *testing.T) {
	o := options{profiles: `  own:
    model: tiny.gguf
    ctx: 2048
    needs_mib: 1000
    allow_tiers: [1]
    ctx_checkpoints: 3
    cache_ram_mib: 512
`}
	o.members = append(o.members, ps5(psA, "ps5-a", "192.0.2.2"))
	c := startCluster(t, o)
	if status, body := c.chat("own", false); status != 200 {
		t.Fatalf("chat: %d %s", status, body)
	}
	run := c.running()
	if len(run) != 1 || !strings.Contains(run[0].Cmd, "--cache-ram 512 --ctx-checkpoints 3 -lv 4 --rpc 192.0.2.2:50052") ||
		strings.Count(run[0].Cmd, "--ctx-checkpoints") != 1 || strings.Count(run[0].Cmd, "--cache-ram") != 1 {
		t.Fatalf("cmd = %v", run)
	}
}

// p33, the host launch on a node: the agent is given the profile's own
// values (HostSpec.ExtraArgs), not node_host_cache_ram_mib (0) and
// ctx_checkpoints (32).
func TestProfileHostMemoryOnTheNodeHostLaunch(t *testing.T) {
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
    ctx_checkpoints: 1
    cache_ram_mib: 256
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
			a.Capabilities, a.HostPort = []string{agentproto.CapHostLaunch}, fakeagent.AutoPort
		}
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		agents = append(agents, a)
	}
	c.waitFor("both agents in the pool", 10*time.Second, func() bool {
		n := 0
		for _, nd := range c.jsonGet("/api/status")["view"].(map[string]any)["nodes"].([]any) {
			if ag, ok := nd.(map[string]any)["agent"].(map[string]any); ok && ag["state"] == "active" && nd.(map[string]any)["free_mib"].(float64) > 0 {
				n++
			}
		}
		return n == 2
	})
	if status, body := c.chat("hosted", false); status != 200 {
		t.Fatalf("chat: %d %s", status, body)
	}
	hl := agents[0].Launches()
	if len(hl) != 1 || hl[0].Host == nil {
		t.Fatalf("host launches %+v", hl)
	}
	args := strings.Join(hl[0].Host.ExtraArgs, " ")
	if !strings.Contains(args, "--jinja --cache-ram 256 --ctx-checkpoints 1 -lv 4") || strings.Count(args, "--ctx-checkpoints") != 1 || strings.Count(args, "--cache-ram") != 1 {
		t.Fatalf("host launch args %q", args)
	}
}
