package e2e

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// Cancel from the Benchmarks pages, end to end on the real binary with two
// fake agents (v88): a run cancelled while its model loads on the agents, and
// a run cancelled while its load still waits in the scheduler's queue. After
// either: no instance, no reservation or claim, no queue entry, no worker
// launch left running on any agent, the steps not started skipped, the
// results so far kept, the outcome cancelled with who and why.

func agentBenchCluster(t *testing.T, readyAfter func() time.Duration, env map[string]string) (*cluster, []*fakeagent.Agent) {
	t.Helper()
	agentPort, workerPort := freeLoopbackPort(t), freeLoopbackPort(t)
	programs := progtree.Write(t, t.TempDir(), progtree.Tree{Release: agentproto.Release,
		Llama: "a01665d30de2885879a64eae999067ef3ef5fd4b",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("agent")},
			"linux-amd64-vulkan": {"llama-server": []byte("llama-server"), "rpc-server": []byte("rpc-server")},
		}})
	dir := t.TempDir()
	top := "data_dir: \"" + filepath.ToSlash(filepath.Join(dir, "data")) + "\"\nhistory_dir: \"" + filepath.ToSlash(filepath.Join(dir, "history")) + "\"\n"
	c := startCluster(t, options{
		defaults: "  membership: agents\n",
		topLevel: top,
		args:     []string{"--programs-dir", programs},
		env: envWith(env, map[string]string{"LLM_SCHEDULER_TEST_AGENT_PORT": strconv.Itoa(agentPort),
			"LLM_SCHEDULER_TEST_WORKER_PORT": strconv.Itoa(workerPort)}),
		profiles: `  split:
    model: split.gguf
    ctx: 4096
    needs_mib: 4000
    force_nodes: [ps5-a, ps5-b]
    tensor_split: [1, 1]
  wide:
    model: wide.gguf
    ctx: 4096
    needs_mib: 20000
    force_nodes: [ps5-a, ps5-b]
    tensor_split: [1, 1]
  second:
    model: second.gguf
    ctx: 4096
    needs_mib: 2000
    force_nodes: [ps5-a, ps5-b]
    tensor_split: [1, 1]
`,
	})
	rpc := agenttest.Install(t, t.TempDir(), "rpc-server")
	var agents []*fakeagent.Agent
	for i, name := range []string{"ps5-a", "ps5-b"} {
		a := &fakeagent.Agent{ID: "02:00:00:00:00:0" + strconv.Itoa(i+1), Name: name, Addr: "127.0.0." + strconv.Itoa(i+2),
			AgentPort: agentPort, WorkerPort: workerPort, Potluck: c.url, RPCServer: rpc, MemAvail: 14000, ReadyAfterFn: readyAfter,
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
	for _, b := range []string{
		`{"id":"b-split","name":"Split check","profile":"split","requests":[{"prompt":"hi","max_tokens":4}]}`,
		`{"id":"b-second","name":"Second check","profile":"second","requests":[{"prompt":"hi","max_tokens":4}]}`,
	} {
		mustStatus(t, c, "POST", "/api/bench/benchmarks", b, 201)
	}
	mustStatus(t, c, "POST", "/api/bench/groups", `{"id":"g","name":"Two","benchmarks":["b-second","b-split"],"cooldown_s":0,"only_when_idle":false}`, 201)
	return c, agents
}

// nothingLeft: no instance, no queue entry, no reservation or claim on any
// node, every worker launch stopped.
func nothingLeft(t *testing.T, c *cluster, agents []*fakeagent.Agent, keep string) {
	t.Helper()
	c.waitFor("nothing of the run left", 20*time.Second, func() bool {
		st := c.jsonGet("/api/status")
		for _, in := range st["instances"].([]any) {
			if in.(map[string]any)["profile"] != keep {
				return false
			}
		}
		if q, _ := st["queue"].([]any); len(q) != 0 {
			return false
		}
		for _, nd := range st["view"].(map[string]any)["nodes"].([]any) {
			n := nd.(map[string]any)
			if s, _ := n["claimed_by"].(string); s != "" {
				return false
			}
			if h, _ := n["holder"].(string); h != "" && h != keep {
				return false
			}
			if r, _ := n["reserved_mib"].(float64); r != 0 && keep == "" {
				return false
			}
		}
		if keep == "" {
			for _, a := range agents {
				if s := a.LaunchState().State; s != "" && s != agentproto.StateExited {
					return false
				}
			}
		}
		return true
	})
}

func envWith(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func runDoc(t *testing.T, c *cluster, id string) map[string]any {
	t.Helper()
	var r map[string]any
	c.waitFor("the run finished", 20*time.Second, func() bool {
		r = c.jsonGet("/api/bench/runs/" + id)
		s := r["state"]
		return s == "cancelled" || s == "completed" || s == "failed" || s == "skipped"
	})
	return r
}

func TestBenchCancelWhileLoadingOnAgents(t *testing.T) {
	// the first launches (the second benchmark's model) are quick; once its
	// step has its results, the split load stays starting
	var slow atomic.Bool
	c, agents := agentBenchCluster(t, func() time.Duration {
		if slow.Load() {
			return 60 * time.Second
		}
		return 0
	}, nil)
	st, b, _ := c.do("POST", "/api/bench/groups/g/run", `{"comment":"e2e cancel while loading"}`)
	if st != 202 {
		t.Fatalf("start: %d %s", st, b)
	}
	var run map[string]any
	json.Unmarshal([]byte(b), &run)
	id := run["id"].(string)
	c.waitFor("the first step answered", 30*time.Second, func() bool {
		r := c.jsonGet("/api/bench/runs/" + id)
		steps, _ := r["steps"].([]any)
		if len(steps) == 0 {
			return false
		}
		reqs, _ := steps[0].(map[string]any)["requests"].([]any)
		if len(reqs) > 0 {
			slow.Store(true)
			return true
		}
		return false
	})
	c.waitFor("split loading on both agents", 30*time.Second, func() bool {
		for _, a := range agents {
			if a.LaunchState().State != agentproto.StateStarting {
				return false
			}
		}
		st := c.jsonGet("/api/status")
		for _, in := range st["instances"].([]any) {
			if m := in.(map[string]any); m["profile"] == "split" && m["state"] == "starting" {
				return true
			}
		}
		return false
	})
	if st, b, _ := c.do("POST", "/api/bench/runs/"+id+"/cancel", `{"comment":"wrong zones"}`); st != 202 {
		t.Fatalf("cancel: %d %s", st, b)
	}
	r := runDoc(t, c, id)
	if r["state"] != "cancelled" || !strings.HasPrefix(r["error"].(string), "cancelled at step 2 of 2 by ") || !strings.HasSuffix(r["error"].(string), `: "wrong zones"`) {
		t.Fatalf("outcome: %v %q", r["state"], r["error"])
	}
	steps, _ := r["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps: %v", steps)
	}
	s0, s1 := steps[0].(map[string]any), steps[1].(map[string]any)
	if reqs, _ := s0["requests"].([]any); s0["benchmark_id"] != "b-second" || len(reqs) == 0 || s1["outcome"] != "cancelled" {
		b0, _ := json.Marshal(s0)
		b1, _ := json.Marshal(s1)
		t.Fatalf("steps: %s / %s", b0[:min(len(b0), 1500)], b1[:min(len(b1), 800)])
	}
	nothingLeft(t, c, agents, "")
	for _, a := range agents {
		st := a.LaunchState()
		if st.Exit == nil || (st.Exit.Reason != agentproto.ExitStopped && st.Exit.Reason != agentproto.ExitForced) {
			t.Fatalf("%s: the half-started launch did not end as stopped: %+v %v", a.Name, st, a.Calls())
		}
	}
}

// The run's load waits in the scheduler's queue (another caller's model fills
// the agents and answers a long request): a cancel withdraws it, so it never
// loads later, and the other model is not drained for it any more.
func TestBenchCancelWhileItsLoadIsQueued(t *testing.T) {
	c, agents := agentBenchCluster(t, nil, map[string]string{"FAKELLAMA_STREAM_DELAY": "2s"})
	done := make(chan struct{})
	go func() { defer close(done); c.chat("wide", true) }() // about 10 s in flight
	c.waitFor("wide answering", 20*time.Second, func() bool {
		for _, in := range c.jsonGet("/api/status")["instances"].([]any) {
			if m := in.(map[string]any); m["profile"] == "wide" && m["in_flight"].(float64) > 0 {
				return true
			}
		}
		return false
	})
	// the run does not wait until idle, so the scheduler would unload wide
	// for it once its request ends: the start must name it
	st, b, _ := c.do("POST", "/api/bench/benchmarks/b-split/run", `{"comment":"x","only_when_idle":false}`)
	if st != 409 || !strings.Contains(b, "evict_set_changed") || !strings.Contains(b, `"wide"`) {
		t.Fatalf("start without the list: %d %s", st, b)
	}
	st, b, _ = c.do("POST", "/api/bench/benchmarks/b-split/run", `{"comment":"x","only_when_idle":false,"evict":["wide"]}`)
	if st != 202 {
		t.Fatalf("start: %d %s", st, b)
	}
	var run map[string]any
	json.Unmarshal([]byte(b), &run)
	id := run["id"].(string)
	// queued, with a claim on both agents: wide drains for it
	c.waitFor("split queued with its claim", 20*time.Second, func() bool {
		q, _ := c.jsonGet("/api/status")["queue"].([]any)
		return len(q) == 1 && q[0].(map[string]any)["claimed"] == true
	})
	if st, b, _ := c.do("POST", "/api/bench/runs/"+id+"/cancel", `{"comment":"not now"}`); st != 202 {
		t.Fatalf("cancel: %d %s", st, b)
	}
	r := runDoc(t, c, id)
	if r["state"] != "cancelled" || !strings.HasSuffix(r["error"].(string), `: "not now"`) {
		t.Fatalf("outcome: %v %q", r["state"], r["error"])
	}
	nothingLeft(t, c, agents, "wide")
	<-done
	// wide's request is over: the withdrawn load does not come back, and wide
	// is not unloaded for it
	time.Sleep(2 * time.Second)
	got := []string{}
	for _, in := range c.jsonGet("/api/status")["instances"].([]any) {
		got = append(got, in.(map[string]any)["profile"].(string)+":"+in.(map[string]any)["state"].(string))
	}
	if strings.Join(got, ",") != "wide:ready" { // not draining for split any more
		t.Fatalf("after the cancel: %v", got)
	}
}
