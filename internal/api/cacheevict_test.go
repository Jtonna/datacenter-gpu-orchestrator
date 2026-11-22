package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// pageCache pretends a fixed share of every file is cached until dropped.
type pageCache struct {
	mu      sync.Mutex
	dropped map[string]bool
	rest    int64
}

func (c *pageCache) Cached(f *os.File, size int64) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped[f.Name()] {
		return c.rest, nil
	}
	return size, nil
}

func (c *pageCache) Drop(f *os.File, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == nil {
		c.dropped = map[string]bool{}
	}
	c.dropped[f.Name()] = true
	return nil
}

func TestCacheEvictEndpoint(t *testing.T) {
	// one agent API on loopback that answers the eviction
	var mu sync.Mutex
	var asked []string
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == agentproto.PathLaunch {
			w.Write([]byte(`{"state":"none"}`))
			return
		}
		var req agentproto.CacheEvictRequest
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+req.File)
		mu.Unlock()
		json.NewEncoder(w).Encode(agentproto.CacheEvictAnswer{File: req.File, FileBytes: 5000, CachedBefore: 5000, CachedAfter: 10})
	}))
	t.Cleanup(agentSrv.Close)
	_, portText, _ := net.SplitHostPort(agentSrv.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0", Llama: "a01665d30",
		Folders: map[string]map[string][]byte{"agent": {"potluck-agent-linux-amd64": []byte("A")}, "linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("R")}}})
	rel, err := release.Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	pcfg := &config.Config{Defaults: d}
	p, err := pool.New(pool.Options{Config: func() *config.Config { return pcfg }, Programs: pool.Programs{Release: rel}, PotluckRelease: "0.1.0", AgentPort: port})
	if err != nil {
		t.Fatal(err)
	}
	cache := &pageCache{rest: 0}
	e := newEnv(t, chatOK, func(o *Options) { o.Pool, o.fileCache = p, cache })
	models := t.TempDir()
	os.WriteFile(filepath.Join(models, "a.gguf"), make([]byte, 2048), 0o644)
	e.sched.Config().ModelsDir = models

	for _, reg := range []struct {
		id   string
		caps []string
	}{{"02:00:00:00:00:01", []string{agentproto.CapFileCacheEvict}}, {"02:00:00:00:00:02", nil}} {
		req := agentproto.RegisterRequest{ID: reg.id, Name: "console-" + reg.id[len(reg.id)-1:], AgentRelease: "0.1.0", AgentStartID: "s1", OS: "linux", Arch: "amd64",
			Install: "bare", Addr: "127.0.0.1", Backends: []string{"vulkan"}, Capabilities: reg.caps}
		req.FillEmpty()
		if a := p.Register(req); !a.Accepted {
			t.Fatalf("%+v", a)
		}
	}

	resp := e.post("/api/cache/evict", `{"model":"alpha-old","nodes":["02:00:00:00:00:01","02:00:00:00:00:02","02:00:00:00:00:09"],"local":true}`)
	b := body(t, resp)
	t.Logf("POST /api/cache/evict -> %s", b)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	var ans bench.CacheEvictAnswer
	if err := json.Unmarshal([]byte(b), &ans); err != nil {
		t.Fatal(err)
	}
	if ans.Model != "alpha" || ans.File != "a.gguf" || ans.Local == nil || ans.Local.EvictFigures == nil ||
		ans.Local.FileBytes != 2048 || ans.Local.CachedBefore != 2048 || ans.Local.CachedAfter != 0 {
		t.Fatalf("%+v local %+v", ans, ans.Local)
	}
	if len(ans.Nodes) != 3 {
		t.Fatalf("%+v", ans.Nodes)
	}
	n1, n2, n9 := ans.Nodes[0], ans.Nodes[1], ans.Nodes[2]
	if n1.Status != 200 || !n1.Capable || n1.EvictFigures == nil || n1.CachedAfter != 10 || n1.Node != "console-1" {
		t.Fatalf("node 1 %+v", n1)
	}
	if n2.Capable || n2.EvictFigures != nil || n2.Reason != agentproto.ReasonUnsupported {
		t.Fatalf("node 2 %+v", n2)
	}
	if n9.Reason != "unknown_node" || n9.EvictFigures != nil {
		t.Fatalf("node 9 %+v", n9)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0] != "POST /cache/evict a.gguf" {
		t.Fatalf("asked %v", asked)
	}
	mu.Unlock()

	// without local the scheduler's machine is left alone; bad bodies and models are refused
	cache.dropped = nil
	resp = e.post("/api/cache/evict", `{"model":"alpha"}`)
	ans = bench.CacheEvictAnswer{}
	if b := body(t, resp); resp.StatusCode != 200 || json.Unmarshal([]byte(b), &ans) != nil || ans.Local != nil || len(ans.Nodes) != 0 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	for in, want := range map[string]int{`{}`: 400, `{"model":"nope"}`: 404, `not json`: 400} {
		if resp := e.post("/api/cache/evict", in); resp.StatusCode != want {
			t.Errorf("%s: %d %s", in, resp.StatusCode, body(t, resp))
		}
	}
}

func TestCacheEvictLocalFailureIsReported(t *testing.T) {
	e := newEnv(t, chatOK) // no pool, no page cache calls on this system unless Linux
	e.api.opts.fileCache = nil
	e.sched.Config().ModelsDir = t.TempDir() // a.gguf is missing
	resp := e.post("/api/cache/evict", `{"model":"alpha","nodes":["n1"],"local":true}`)
	b := body(t, resp)
	var ans bench.CacheEvictAnswer
	if resp.StatusCode != 200 || json.Unmarshal([]byte(b), &ans) != nil {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if ans.Local == nil || ans.Local.EvictFigures != nil || ans.Local.Error == "" {
		t.Fatalf("local %+v", ans.Local)
	}
	if len(ans.Nodes) != 1 || ans.Nodes[0].Reason != "no_agents" {
		t.Fatalf("%+v", ans.Nodes)
	}
}
