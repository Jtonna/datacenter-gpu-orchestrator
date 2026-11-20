package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakefleet"
)

// Recovery requests end to end: the real llm-scheduler binary, the fake fleet
// manager with its recovery endpoint. Node goes hung -> request stored ->
// outcome ok reported -> health recovers -> sequence reset.
func TestRecoveryRequestFlowFromHungToReset(t *testing.T) {
	const id = "aa:bb:cc:00:00:01"
	const encoded = "aa%3Abb%3Acc%3A00%3A00%3A01"
	c := startCluster(t, options{
		members:  []ps5Member{ps5(id, "ps5-062e1d84", "10.0.0.2")},
		profiles: profilesBasic,
		// Worker health: the heartbeat alone, silent for 3 x worker_silence with
		// no other channel known ok, makes a worker hung (8.4a).
		defaults: "  worker_silence: 1s\n",
		topLevel: "recovery: {enabled: true, request_ttl: 30s, http_timeout: 2s}\n",
	})
	ev := c.events()
	if status, body := c.chat("tiny:2k", false); status != 200 {
		t.Fatalf("chat: %d %s", status, body)
	}
	c.waitFor("worker health ok", 10*time.Second, func() bool {
		h, _ := nodeStatus(c, id)["health"].(map[string]any)
		return h != nil && h["state"] == "ok"
	})
	if n := len(c.fleet.RecoveryRequests()); n != 0 {
		t.Fatalf("%d recovery requests while healthy", n)
	}

	// 1. The node stops reporting: hung after about 4 s.
	c.fleet.SetOnline(id, false)
	c.fleet.Push()
	c.waitFor("a recovery request at the fleet manager", 20*time.Second, func() bool {
		return len(c.fleet.RecoveryRequests()) >= 1
	})
	reqs := c.fleet.RecoveryRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.RawPath != "/api/recovery/"+encoded || r.NodeID != id || r.Status != 200 {
		t.Fatalf("request %+v", r)
	}
	rec, err := r.Record()
	if err != nil {
		t.Fatal(err)
	}
	if rec.V != 1 || rec.TargetNodeID != id || rec.Action != "restart_rpc_server" || len(rec.Nonce) != 32 ||
		rec.ExpiresAt-rec.IssuedAt != 30000 || !strings.HasPrefix(rec.Reason, "hung: heartbeat silent ") {
		t.Fatalf("record %+v", rec)
	}
	t.Logf("request: POST %s\n%s", r.RawPath, r.Body)

	// 2. Stored: status says so, and the sent event arrived.
	c.waitFor("request stored in the status", 10*time.Second, func() bool {
		lr := lastRequest(nodeStatus(c, id))
		return lr != nil && lr["status"] == "stored"
	})
	lr := lastRequest(nodeStatus(c, id))
	if lr["nonce"] != rec.Nonce || lr["action"] != "restart_rpc_server" || lr["http_status"] != float64(200) {
		t.Fatalf("last_request %v", lr)
	}
	waitEvent(t, ev, "sent", rec.Nonce)

	// 3. The node reports the outcome in the fleet data.
	c.fleet.SetLastRecovery(id, &fakefleet.RecoveryAction{Action: rec.Action, At: time.Now().UnixMilli(), Outcome: "ok",
		Reason: "rpc-server restarted", Nonce: rec.Nonce, DurationMs: 1700})
	c.fleet.Push()
	waitEvent(t, ev, "outcome", rec.Nonce)
	c.waitFor("outcome in the status", 10*time.Second, func() bool {
		r, _ := nodeStatus(c, id)["recovery"].(map[string]any)
		o, _ := r["last_outcome"].(map[string]any)
		return o != nil && o["outcome"] == "ok"
	})
	rs := nodeStatus(c, id)["recovery"].(map[string]any)
	if rs["step"] != float64(1) || rs["episode_started"] == nil {
		t.Fatalf("during the episode: %v", rs)
	}
	t.Logf("status nodes[].recovery:\n%s", mustJSON(rs))
	if !strings.Contains(c.stderr.String(), "recovery outcome") {
		t.Fatalf("no outcome log line:\n%s", c.stderr.String())
	}

	// 4. The node comes back: health ok, the sequence resets, nothing more is sent.
	c.fleet.SetOnline(id, true)
	c.fleet.Push()
	c.waitFor("sequence reset", 15*time.Second, func() bool {
		r, _ := nodeStatus(c, id)["recovery"].(map[string]any)
		return r != nil && r["step"] == float64(0) && r["episode_started"] == nil
	})
	if h := nodeStatus(c, id)["health"].(map[string]any); h["state"] != "ok" {
		t.Fatalf("health %v", h)
	}
	// The status changes before the log line has travelled through the child's stderr
	// pipe, so wait for the line instead of reading the buffer once.
	c.waitFor("reset log line", 5*time.Second, func() bool {
		return strings.Contains(c.stderr.String(), "recovery sequence reset")
	})
	time.Sleep(2 * time.Second)
	if n := len(c.fleet.RecoveryRequests()); n != 1 {
		t.Fatalf("%d requests in all, want exactly 1", n)
	}
	// Last request and outcome stay visible after the reset.
	rs = nodeStatus(c, id)["recovery"].(map[string]any)
	if rs["last_request"] == nil || rs["last_outcome"] == nil {
		t.Fatalf("after reset: %v", rs)
	}
}

// With the feature at its default (off), a hung worker gets no request.
func TestRecoveryOffByDefaultSendsNothing(t *testing.T) {
	const id = "aa:bb:cc:00:00:01"
	c := startCluster(t, options{
		members:  []ps5Member{ps5(id, "ps5-062e1d84", "10.0.0.2")},
		profiles: profilesBasic,
		defaults: "  worker_silence: 1s\n",
	})
	if status, body := c.chat("tiny:2k", false); status != 200 {
		t.Fatalf("chat: %d %s", status, body)
	}
	c.fleet.SetOnline(id, false)
	c.fleet.Push()
	c.waitFor("worker hung", 15*time.Second, func() bool {
		h, _ := nodeStatus(c, id)["health"].(map[string]any)
		return h != nil && h["state"] == "hung"
	})
	time.Sleep(2 * time.Second)
	if n := len(c.fleet.RecoveryRequests()); n != 0 {
		t.Fatalf("%d recovery requests with the feature off", n)
	}
	if _, has := nodeStatus(c, id)["recovery"]; has {
		t.Fatal("recovery in the status with the feature off")
	}
}

func nodeStatus(c *cluster, id string) map[string]any {
	c.t.Helper()
	view, _ := c.jsonGet("/api/status")["view"].(map[string]any)
	nodes, _ := view["nodes"].([]any)
	for _, n := range nodes {
		if m, _ := n.(map[string]any); m != nil && m["id"] == id {
			return m
		}
	}
	return map[string]any{}
}

func lastRequest(node map[string]any) map[string]any {
	r, _ := node["recovery"].(map[string]any)
	lr, _ := r["last_request"].(map[string]any)
	return lr
}

// waitEvent reads /api/events until a recoveryRequest event with the phase and nonce arrives.
func waitEvent(t *testing.T, s *sseClient, phase, nonce string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		m := s.next(20 * time.Second)
		if m.Type != "recoveryRequest" {
			continue
		}
		d, _ := m.Data.(map[string]any)
		if d["phase"] != phase {
			continue
		}
		rs, _ := d["recovery"].(map[string]any)
		var n any
		if phase == "sent" {
			n = lastRequestOf(rs)
		} else if o, _ := rs["last_outcome"].(map[string]any); o != nil {
			n = o["nonce"]
		}
		if n == nonce {
			return
		}
	}
	t.Fatalf("no recoveryRequest %s event for %s", phase, nonce)
}

func lastRequestOf(rs map[string]any) any {
	lr, _ := rs["last_request"].(map[string]any)
	return lr["nonce"]
}

func mustJSON(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
