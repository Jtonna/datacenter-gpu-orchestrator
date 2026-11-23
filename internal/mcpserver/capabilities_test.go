package mcpserver

import (
	"encoding/json"
	"testing"
)

// 0.1.7: the status view's nodes carry the agent's capabilities; a node
// without an agent entry (the local node) has an empty list.
func TestStatusNodesCarryCapabilities(t *testing.T) {
	var doc map[string]any
	json.Unmarshal([]byte(`{"view":{"nodes":[{"id":"02:00:00:00:00:01"},{"id":"host-1","local":true}]},
		"agents":[{"id":"02:00:00:00:00:01","capabilities":["file_cache_evict"]}]}`), &doc)
	out := deriveStatus(doc, map[string]any{}).(map[string]any)
	b, _ := json.Marshal(out["nodes"])
	if string(b) != `[{"capabilities":["file_cache_evict"],"id":"02:00:00:00:00:01"},{"capabilities":[],"id":"host-1","local":true}]` {
		t.Fatalf("%s", b)
	}
}
