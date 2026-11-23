package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// 0.1.7: every node entry of /api/nodes and every agent of /api/status
// carries capabilities, the words the agent advertised at registration,
// sorted; an empty list when there are none (an older agent, the local node).
func TestNodeCapabilities(t *testing.T) {
	e, p, _ := feedEnv(t)
	req := agentproto.RegisterRequest{ID: "02:00:00:00:00:01", Name: "console-1", AgentRelease: "0.1.0", AgentStartID: "s1", OS: "linux", Arch: "amd64",
		Install: "bare", Addr: "192.0.2.101", Backends: []string{"vulkan"}, Capabilities: []string{"zeta_word", agentproto.CapFileCacheEvict}}
	req.FillEmpty()
	b, _ := json.Marshal(req)
	if resp := e.post(agentproto.PathRegister, string(b)); resp.StatusCode != 200 {
		t.Fatalf("register: %d %s", resp.StatusCode, body(t, resp))
	}
	register(t, e, "02:00:00:00:00:02")
	feedHeartbeat(t, p, "02:00:00:00:00:01", "")
	feedHeartbeat(t, p, "02:00:00:00:00:02", "")
	var st struct {
		Agents []json.RawMessage `json:"agents"`
	}
	json.Unmarshal([]byte(body(t, e.get("/api/status"))), &st)
	f := readFeed(t, e)
	if len(f.Nodes) != 3 || len(st.Agents) != 2 {
		t.Fatalf("feed %d, status %d", len(f.Nodes), len(st.Agents))
	}
	want := []string{`"capabilities":["file_cache_evict","zeta_word"]`, `"capabilities":[]`, `"capabilities":[]`}
	for i, n := range f.Nodes {
		if !strings.Contains(string(n), want[i]) {
			t.Errorf("feed node %d lacks %s: %s", i, want[i], n)
		}
		if i < 2 && string(n) != string(st.Agents[i]) {
			t.Errorf("feed and status differ:\n%s\n%s", n, st.Agents[i])
		}
	}
}
