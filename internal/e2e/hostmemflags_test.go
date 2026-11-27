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

// p33: a profile's --ctx-checkpoints and --cache-ram in extra_args are
// passed in place of the cluster values, once, on the scheduler's machine.
// The exact launch line for: no flag, restore points only, cache only,
// both; the "=" form is refused on write (the server does not read it).
func TestProfileHostMemoryFlagsOnTheSchedulersMachine(t *testing.T) {
	c := withPS5(t, nil)
	cases := []struct{ id, args, want string }{
		{"f-none", `["--temp","0.7"]`, "--cache-ram 2048 --ctx-checkpoints 32 -lv 4 --rpc 192.0.2.2:50052 --device RPC0 --temp 0.7"},
		{"f-ctx", `["--ctx-checkpoints","3","--temp","0.7"]`, "--cache-ram 2048 --ctx-checkpoints 3 -lv 4 --rpc 192.0.2.2:50052 --device RPC0 --temp 0.7"},
		{"f-cache", `["-cram","0","--temp","0.7"]`, "--cache-ram 0 --ctx-checkpoints 32 -lv 4 --rpc 192.0.2.2:50052 --device RPC0 --temp 0.7"},
		{"f-both", `["--ctx-checkpoints","3","--cache-ram","0","--temp","0.7"]`, "--cache-ram 0 --ctx-checkpoints 3 -lv 4 --rpc 192.0.2.2:50052 --device RPC0 --temp 0.7"},
	}
	for _, k := range cases {
		code, b, _ := c.do("POST", "/api/config/profiles", `{"id":"`+k.id+`","model":"tiny.gguf","ctx":2048,"needs_mib":1000,"allow_tiers":[1],"extra_args":`+k.args+`}`)
		if code != 201 {
			t.Fatalf("%s: create %d %s", k.id, code, b)
		}
		if status, body := c.chat(k.id, false); status != 200 {
			t.Fatalf("%s: chat %d %s", k.id, status, body)
		}
		run := c.running()
		if len(run) != 1 {
			t.Fatalf("%s: running %v", k.id, run)
		}
		_, tail, _ := strings.Cut(run[0].Cmd, " --jinja ")
		if tail != k.want {
			t.Errorf("%s:\n got %s\nwant %s\n(full: %s)", k.id, tail, k.want, run[0].Cmd)
		}
		if status, body := c.post("/api/unload", `{"model":"`+k.id+`"}`); status != 200 {
			t.Fatalf("%s: unload %d %s", k.id, status, body)
		}
		c.waitFor(k.id+" unloaded", 15*time.Second, func() bool { return len(c.running()) == 0 })
	}
	code, b, _ := c.do("POST", "/api/config/profiles", `{"id":"f-eq","model":"tiny.gguf","ctx":2048,"needs_mib":1000,"allow_tiers":[1],"extra_args":["--ctx-checkpoints=3"]}`)
	if code < 400 || !strings.Contains(b, "the model server does not read --ctx-checkpoints=value and would not start") {
		t.Fatalf("the = form: %d %s", code, b)
	}
}

// The same on a console host (host: node): the host launch's arguments.
func TestProfileHostMemoryFlagsOnAConsoleHost(t *testing.T) {
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	programs := progtree.Write(t, t.TempDir(), progtree.Tree{Release: agentproto.Release,
		Llama: "a01665d30de2885879a64eae999067ef3ef5fd4b",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("agent")},
			"linux-amd64-vulkan": {"llama-server": []byte("llama-server"), "rpc-server": []byte("rpc-server")},
		}})
	prof := func(id, args string) string {
		return "  " + id + ":\n    model: hosted.gguf\n    ctx: 4096\n    needs_mib: 4000\n    host: node\n    force_nodes: [ps5-a, ps5-b]\n    tensor_split: [1, 1]\n    extra_args: " + args + "\n"
	}
	cases := []struct{ id, args, want string }{
		{"h-none", `["--temp", "0.7"]`, "--n-gpu-layers 999 --fit off --jinja --cache-ram 0 --ctx-checkpoints 32 -lv 4 --temp 0.7"},
		{"h-ctx", `["--ctx-checkpoints", "3", "--temp", "0.7"]`, "--n-gpu-layers 999 --fit off --jinja --cache-ram 0 --ctx-checkpoints 3 -lv 4 --temp 0.7"},
		{"h-cache", `["--cache-ram", "512", "--temp", "0.7"]`, "--n-gpu-layers 999 --fit off --jinja --cache-ram 512 --ctx-checkpoints 32 -lv 4 --temp 0.7"},
		{"h-both", `["-ctxcp", "1", "-cram", "256", "--temp", "0.7"]`, "--n-gpu-layers 999 --fit off --jinja --cache-ram 256 --ctx-checkpoints 1 -lv 4 --temp 0.7"},
	}
	var profiles string
	for _, k := range cases {
		profiles += prof(k.id, k.args)
	}
	c := startCluster(t, options{
		defaults: "  membership: agents\n",
		args:     []string{"--programs-dir", programs},
		env: map[string]string{"LLM_SCHEDULER_TEST_AGENT_PORT": strconv.Itoa(agentPort),
			"LLM_SCHEDULER_TEST_WORKER_PORT": strconv.Itoa(workerPort)},
		profiles: profiles,
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
	for i, k := range cases {
		if status, body := c.chat(k.id, false); status != 200 {
			t.Fatalf("%s: chat %d %s", k.id, status, body)
		}
		hl := agents[0].Launches()
		if len(hl) != i+1 || hl[i].Host == nil {
			t.Fatalf("%s: host launches %+v", k.id, hl)
		}
		if got := strings.Join(hl[i].Host.ExtraArgs, " "); got != k.want {
			t.Errorf("%s:\n got %s\nwant %s", k.id, got, k.want)
		}
		if status, body := c.post("/api/unload", `{"model":"`+k.id+`"}`); status != 200 {
			t.Fatalf("%s: unload %d %s", k.id, status, body)
		}
		c.waitFor(k.id+" stopped", 15*time.Second, func() bool {
			st := agents[0].LaunchState()
			return st.State == agentproto.StateExited
		})
	}
}
