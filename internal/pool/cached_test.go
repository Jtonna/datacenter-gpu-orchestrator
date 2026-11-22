package pool

import (
	"strings"
	"testing"
)

// The agent's file cache figure (stats memory.cached_bytes) reaches the
// node for the reload-from-cache line; a heartbeat without it clears it.
func TestCachedBytesReachTheNode(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq(upID))
	stats := `,"stats":{"stats_version":1,"sources":{"node":{"status":"ok"}},"memory":{"total_bytes":16000000000,"available_bytes":9000000000,"cached_bytes":7297040384},"gpus":[]}}`
	body := strings.TrimSuffix(strings.TrimSpace(string(hbBody("s1", "", true, "true"))), "}") + stats
	if res := r.p.Heartbeat(upID, []byte(body)); res.Status != 200 {
		t.Fatalf("heartbeat %d %s", res.Status, res.Err)
	}
	if n, _ := r.node(upID); !n.CachedKnown || n.CachedMiB != 6959 {
		t.Fatalf("node %+v", n)
	}
	r.heartbeat(upID) // no stats
	if n, _ := r.node(upID); n.CachedKnown {
		t.Fatalf("node %+v", n)
	}
}
