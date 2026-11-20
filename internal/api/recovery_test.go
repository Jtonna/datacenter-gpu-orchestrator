package api

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Recovery on GET /api/status and the recoveryRequest event (design 8.4b).

type recPoster struct {
	mu    sync.Mutex
	urls  []string
	posts [][]byte
}

func (p *recPoster) Post(_ context.Context, url string, body []byte, _ time.Duration) (scheduler.RecoveryResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urls = append(p.urls, url)
	p.posts = append(p.posts, append([]byte(nil), body...))
	return scheduler.RecoveryResponse{Status: 200}, nil
}

func (p *recPoster) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.posts) }

func TestRecoveryStatusAndEvent(t *testing.T) {
	poster := &recPoster{}
	e, view, probe := healthEnvWith(t, "recovery: {enabled: true}", poster)
	c := openEvents(t, e)
	if err := e.sched.Load(context.Background(), "big"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both workers ok", func() bool {
		a, _ := statusNode(t, e, "ps5-a")["health"].(map[string]any)
		return a != nil && a["state"] == "ok"
	})
	for _, id := range []string{"ps5-a", "ps5-b"} {
		if _, has := statusNode(t, e, id)["recovery"]; has {
			t.Fatalf("%s has recovery before anything was sent", id)
		}
	}

	probe.setDown(workerA)
	view.setOffline("ps5-a")

	var sent map[string]any
	for {
		typ, d := c.parsed(5 * time.Second)
		if typ == "recoveryRequest" {
			sent = d.(map[string]any)
			break
		}
	}
	if sent["node"] != "ps5-a" || sent["phase"] != "sent" {
		t.Fatalf("event %v", sent)
	}
	t.Logf("recoveryRequest event data (sent):\n%s", pretty(sent))
	lr := sent["recovery"].(map[string]any)["last_request"].(map[string]any)
	if lr["action"] != "restart_rpc_server" || lr["status"] == nil || len(lr["nonce"].(string)) != 32 {
		t.Fatalf("last_request %v", lr)
	}

	eventually(t, "the request stored", func() bool {
		r, _ := statusNode(t, e, "ps5-a")["recovery"].(map[string]any)
		l, _ := r["last_request"].(map[string]any)
		return l != nil && l["status"] == "stored"
	})
	if poster.count() != 1 {
		t.Fatalf("%d posts", poster.count())
	}
	var rec map[string]any
	json.Unmarshal(poster.posts[0], &rec)
	if poster.urls[0] != "http://127.0.0.1:1/api/recovery/ps5-a" || rec["target_node_id"] != "ps5-a" || rec["nonce"] != lr["nonce"] {
		t.Fatalf("post %s %s", poster.urls[0], poster.posts[0])
	}

	// The node reports its outcome.
	view.setOutcome("ps5-a", core.RecoveryOutcome{Present: true, Action: "restart_rpc_server", At: time.Now().UnixMilli(),
		Outcome: "ok", Reason: "restarted", Nonce: lr["nonce"].(string), DurationMs: 2300})
	var out map[string]any
	for {
		typ, d := c.parsed(5 * time.Second)
		if typ == "recoveryRequest" && d.(map[string]any)["phase"] == "outcome" {
			out = d.(map[string]any)
			break
		}
	}
	t.Logf("recoveryRequest event data (outcome):\n%s", pretty(out))

	r := statusNode(t, e, "ps5-a")["recovery"].(map[string]any)
	t.Logf("status nodes[].recovery:\n%s", pretty(r))
	req := r["last_request"].(map[string]any)
	oc := r["last_outcome"].(map[string]any)
	if req["status"] != "stored" || req["http_status"] != float64(200) || req["action"] != "restart_rpc_server" ||
		req["nonce"] != lr["nonce"] || req["sent_at"] == nil {
		t.Fatalf("last_request %v", req)
	}
	if oc["outcome"] != "ok" || oc["duration_ms"] != float64(2300) || oc["nonce"] != lr["nonce"] || oc["action"] != "restart_rpc_server" ||
		oc["reason"] != "restarted" || oc["at"] == nil {
		t.Fatalf("last_outcome %v", oc)
	}
	if r["step"] != float64(1) || r["episode_started"] == nil {
		t.Fatalf("step/episode_started %v", r)
	}
	if _, has := statusNode(t, e, "ps5-b")["recovery"]; has {
		t.Fatal("recovery on the healthy node")
	}
	// Existing fields are untouched.
	for _, k := range []string{"id", "hostname", "online", "addr", "eligible", "quarantined", "missing_since", "health"} {
		if _, ok := statusNode(t, e, "ps5-a")[k]; !ok {
			t.Errorf("node field %s is gone", k)
		}
	}
}
