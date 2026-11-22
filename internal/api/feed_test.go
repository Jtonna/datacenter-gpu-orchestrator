package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// feedClock is a hand-moved clock for the pool.
type feedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *feedClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *feedClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// feedEnv is an agents-mode API whose pool runs on a fake clock and has a
// local node (a local probe that always answers).
func feedEnv(t *testing.T) (*env, *pool.Pool, *feedClock) {
	t.Helper()
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0", Llama: "a01665d30",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("AGENT-BINARY")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("RPC-SERVER")},
		}})
	rel, err := release.Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	cfg := &config.Config{Defaults: d, Fleet: config.Fleet{LocalNode: "host-1"}, LocalProbeCmd: "probe",
		Tiers: []config.TierRule{{Match: config.Match{Hostname: "*"}, Tier: core.Tier{Priority: 1}}}}
	clk := &feedClock{t: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)}
	p, err := pool.New(pool.Options{Config: func() *config.Config { return cfg }, Programs: pool.Programs{Release: rel}, PotluckRelease: "0.1.0",
		Clock: clk.now, RunProbe: func(context.Context, string) ([]byte, error) { return []byte("24576, 1024\n"), nil },
		HostInfo: func() (float64, string, bool) { return 5000, "fedcba9876543210fedcba9876543210", true }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx) // the local probe runs at once
	e := newEnv(t, chatOK, func(o *Options) { o.Pool = p })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := p.Local(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no local node")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return e, p, clk
}

type feedAnswer struct {
	Time  string            `json:"time"`
	Seq   int64             `json:"seq"`
	Nodes []json.RawMessage `json:"nodes"`
}

func readFeed(t *testing.T, e *env) feedAnswer {
	t.Helper()
	resp := e.get("/api/nodes")
	b := body(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/nodes: %d %s", resp.StatusCode, b)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("the feed sends CORS headers")
	}
	var a feedAnswer
	if err := json.Unmarshal([]byte(b), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func feedHeartbeat(t *testing.T, p *pool.Pool, id, extra string) {
	t.Helper()
	b := `{"agent_start_id":"s1","seq":1,"uptime_s":3,"role":"idle","launch_id":null,"programs_ready":true,"programs_error":null,
		"model_source":{"kind":"share","reachable":true,"checked_age_s":1.5},"mem_available_mib":9000,` + extra + `
		"devices":[{"name":"Vulkan0","backend":"vulkan","total_mib":14336,"free_mib":14000,"shares_system_memory":true,"read_age_s":4}]}`
	if res := p.Heartbeat(id, []byte(b)); res.Status != 200 {
		t.Fatalf("heartbeat %d %s", res.Status, res.Err)
	}
}

// The feed's agent entries are exactly the agents of /api/status, byte for
// byte; the local machine is one more entry, marked local, with only the
// fields that apply to it.
func TestNodeFeedEntriesAreTheStatusAgents(t *testing.T) {
	e, p, _ := feedEnv(t)
	register(t, e, "02:00:00:00:00:01")
	feedHeartbeat(t, p, "02:00:00:00:00:01", `"host_uptime_s":812.5,"boot_id":"0123456789abcdef0123456789abcdef",`)

	resp := e.get("/api/status")
	var st struct {
		Agents []json.RawMessage `json:"agents"`
	}
	json.Unmarshal([]byte(body(t, resp)), &st)
	f := readFeed(t, e)
	if len(st.Agents) != 1 || len(f.Nodes) != 2 {
		t.Fatalf("status has %d agents, the feed %d nodes", len(st.Agents), len(f.Nodes))
	}
	if string(f.Nodes[0]) != string(st.Agents[0]) {
		t.Fatalf("the feed's agent entry differs from status:\nfeed   %s\nstatus %s", f.Nodes[0], st.Agents[0])
	}
	for _, k := range []string{`"host_uptime_s":812.5`, `"boot_id":"0123456789abcdef0123456789abcdef"`} {
		if !strings.Contains(string(f.Nodes[0]), k) {
			t.Errorf("agent entry lacks %s: %s", k, f.Nodes[0])
		}
	}
	var local map[string]any
	json.Unmarshal(f.Nodes[1], &local)
	if local["local"] != true || local["id"] != "host-1" || local["state"] != "active" || local["host_uptime_s"] != 5000.0 ||
		local["boot_id"] != "fedcba9876543210fedcba9876543210" {
		t.Fatalf("local entry %s", f.Nodes[1])
	}
	for _, k := range []string{"programs_ready", "programs_error", "launch_id", "paused", "devices", "stats"} {
		if _, ok := local[k]; ok {
			t.Errorf("the local entry has %s, which does not apply to it", k)
		}
	}
	if f.Time != "2026-11-17T10:00:00.000Z" {
		t.Fatalf("time %q", f.Time)
	}
}

// seq stays while only ages change, and rises when anything else does.
func TestNodeFeedSeq(t *testing.T) {
	e, p, clk := feedEnv(t)
	register(t, e, "02:00:00:00:00:01")
	feedHeartbeat(t, p, "02:00:00:00:00:01", `"host_uptime_s":812.5,`)
	a := readFeed(t, e)
	if b := readFeed(t, e); b.Seq != a.Seq {
		t.Fatalf("nothing changed, seq %d -> %d", a.Seq, b.Seq)
	}
	// Only time passes and the agent reports the same with older ages and a
	// longer uptime: the same seq.
	clk.advance(3 * time.Second)
	feedHeartbeat(t, p, "02:00:00:00:00:01", `"host_uptime_s":815.5,`)
	if b := readFeed(t, e); b.Seq != a.Seq {
		t.Fatalf("only ages changed, seq %d -> %d", a.Seq, b.Seq)
	}
	// A real change: the node is paused.
	if _, err := p.SetPaused("02:00:00:00:00:01", true); err != nil {
		t.Fatal(err)
	}
	b := readFeed(t, e)
	if b.Seq <= a.Seq {
		t.Fatalf("a pause did not raise seq: %d -> %d", a.Seq, b.Seq)
	}
	// A new node.
	register(t, e, "02:00:00:00:00:02")
	if c := readFeed(t, e); c.Seq <= b.Seq {
		t.Fatalf("a new node did not raise seq: %d -> %d", b.Seq, c.Seq)
	}
}

// An agent that sends neither host_uptime_s nor boot_id is listed all the
// same, without them.
func TestNodeFeedOlderAgent(t *testing.T) {
	e, p, _ := feedEnv(t)
	register(t, e, "02:00:00:00:00:01")
	feedHeartbeat(t, p, "02:00:00:00:00:01", "")
	f := readFeed(t, e)
	if len(f.Nodes) != 2 || strings.Contains(string(f.Nodes[0]), "host_uptime_s") || strings.Contains(string(f.Nodes[0]), "boot_id") {
		t.Fatalf("%s", f.Nodes)
	}
}

// Without agent membership there is no feed.
func TestNodeFeedOffInFleetMembership(t *testing.T) {
	e := newEnv(t, chatOK)
	if resp := e.get("/api/nodes"); resp.StatusCode != 503 {
		t.Fatalf("GET /api/nodes in fleet membership: %d", resp.StatusCode)
	}
}
