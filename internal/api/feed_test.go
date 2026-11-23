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
func feedEnv(t *testing.T, mod ...func(*config.Config)) (*env, *pool.Pool, *feedClock) {
	t.Helper()
	return feedEnvOpts(t, nil, mod...)
}

// feedEnvOpts is feedEnv with a change to the API options (apiMod may be nil).
func feedEnvOpts(t *testing.T, apiMod func(*Options), mod ...func(*config.Config)) (*env, *pool.Pool, *feedClock) {
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
	for _, m := range mod {
		m(cfg)
	}
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
	e := newEnv(t, chatOK, func(o *Options) {
		o.Pool = p
		if apiMod != nil {
			apiMod(o)
		}
	})
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

// An excluded node (exclude_nodes) says so in the feed, as in /api/status:
// excluded with the matching entry, not placeable, and the reason. The rest
// of its entry stays (state, release, devices), and the feed's agent entry is
// still the status agent entry byte for byte. The local node too.
func TestNodeFeedExcludedNode(t *testing.T) {
	const id = "02:00:00:00:00:01"
	e, p, _ := feedEnv(t, func(c *config.Config) { c.ExcludeNodes = []string{id, "HOST-*"} })
	register(t, e, id)
	feedHeartbeat(t, p, id, "")
	resp := e.get("/api/status")
	var st struct {
		Agents []json.RawMessage `json:"agents"`
	}
	json.Unmarshal([]byte(body(t, resp)), &st)
	f := readFeed(t, e)
	if len(st.Agents) != 1 || len(f.Nodes) != 2 || string(f.Nodes[0]) != string(st.Agents[0]) {
		t.Fatalf("feed %s, status %s", f.Nodes, st.Agents)
	}
	var a, local map[string]any
	json.Unmarshal(f.Nodes[0], &a)
	json.Unmarshal(f.Nodes[1], &local)
	if a["excluded"] != true || a["excluded_by"] != id || a["placeable"] != false || a["unplaced"] != "excluded" ||
		a["unplaced_message"] != "excluded by exclude_nodes: "+id || a["state"] != "active" || a["release"] != "0.1.0" || a["devices"] == nil {
		t.Fatalf("agent entry %s", f.Nodes[0])
	}
	if local["excluded"] != true || local["excluded_by"] != "HOST-*" || local["placeable"] != false || local["unplaced"] != "excluded" ||
		local["unplaced_message"] != "excluded by exclude_nodes: HOST-*" || local["state"] != "active" {
		t.Fatalf("local entry %s", f.Nodes[1])
	}
}

// A node that is not excluded has no exclusion fields at all.
func TestNodeFeedNotExcludedHasNoExclusionFields(t *testing.T) {
	e, p, _ := feedEnv(t)
	register(t, e, "02:00:00:00:00:01")
	feedHeartbeat(t, p, "02:00:00:00:00:01", "")
	for _, n := range readFeed(t, e).Nodes {
		if strings.Contains(string(n), "excluded") {
			t.Fatalf("%s", n)
		}
	}
}

// The Nodes page shows an excluded machine as excluded, with its entry, and
// not as "Not placeable": the reason word excluded is known, and the machine
// badge elsewhere says Excluded too.
func TestUIExcludedNode(t *testing.T) {
	acts := uiText(t, "core/nodeactions.js")
	if !strings.Contains(acts, "excluded: { short: 'Excluded',") {
		t.Error("core/nodeactions.js does not know the reason excluded")
	}
	nodes := uiText(t, "views/nodes.js")
	for _, want := range []string{
		"const excludedOf = (n, a) => !!(n.excluded || (a && a.excluded));",
		"const exBy = n.excluded_by || (agent && agent.excluded_by) || '';",
		"agent.capabilities.length ? ['Its agent can also: ', h('span.mono', agent.capabilities.join(', ')), '.']",
		"if (excludedOf(n, agent)) return [n.holder || (agent && agent.running_while_excluded) ? 'Excluded, busy' : 'Excluded', 'dash', 9];",
		"const npOther = np && np.code !== 'paused' && np.code !== 'excluded';",
		"label.startsWith('Excluded') && exBy ? 'excluded by exclude_nodes: ' + exBy",
		"[' (exclude_nodes: ', h('span.mono', exBy), ')']",
		"npOther ? badge('Not placeable'",
	} {
		if !strings.Contains(nodes, want) {
			t.Errorf("views/nodes.js lacks %s", want)
		}
	}
	if !strings.Contains(uiText(t, "core/status.js"), "if (np.code === 'excluded') {") {
		t.Error("core/status.js: the machine badge does not say Excluded")
	}
}

// 0.1.7 review, ES1: a node excluded while a model runs on it is shown
// excluded AND busy: running_while_excluded names the instance.
func TestNodeFeedExcludedNodeStillBusy(t *testing.T) {
	var pc *config.Config
	e, _, _ := feedEnv(t, func(c *config.Config) { pc = c })
	if resp := e.post("/v1/chat/completions", chat("alpha")); resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, body(t, resp))
	}
	pc.ExcludeNodes = []string{"host-1"}
	nc := *e.cfg
	nc.ExcludeNodes = []string{"host-1"}
	e.sched.ApplyConfig(&nc)
	var local map[string]any
	for _, n := range readFeed(t, e).Nodes {
		var m map[string]any
		json.Unmarshal(n, &m)
		if m["local"] == true {
			local = m
		}
	}
	r, _ := local["running_while_excluded"].(map[string]any)
	if local["excluded"] != true || r == nil || r["profile"] != "alpha" || r["state"] != "ready" {
		t.Fatalf("local entry %v", local)
	}
	nodes := uiText(t, "views/nodes.js")
	if !strings.Contains(nodes, "keeps running and serving until it is unloaded") {
		t.Error("the Nodes page does not say that the excluded machine still runs a model")
	}
}
