package scheduler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Part (a): a recovery request the fleet manager refused stops the escalation
// sequence of that hung episode.

func TestRecovery_RefusedRequestStopsTheSequenceForTheEpisode(t *testing.T) {
	for _, code := range []int{400, 403, 404, 409, 410, 422, 429} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := hungHarness(t, "a", recoveryCfg(threeSteps, func(r *config.Recovery) {
				r.StepWait = 100 * time.Second
				r.MinInterval = 60 * time.Second
			}))
			h.rposter.script = []recResp{{status: code, body: `{"error":"no"}`}}
			h.goLive()
			h.wantPosts(1)
			h.advance(30 * time.Minute) // far past every step_wait: a refused restart must not be followed by a reboot
			h.wantPosts(1)
			w := h.logs.find("recovery sequence stopped", "node", "a")
			if len(w) != 1 || w[0].Level.String() != "WARN" || fmt.Sprint(w[0].Attrs["status"]) != fmt.Sprint(code) ||
				w[0].Attrs["action"] != "restart_rpc_server" {
				t.Fatalf("stopped log %+v", w)
			}
			// The stop is for this episode only: a new episode starts the sequence again.
			h.wprober.setDown(addrA, false)
			h.advance(15 * time.Second)
			h.wantHealth("a", HealthOK)
			h.view.stale()
			h.wprober.setDown(addrA, true)
			h.advance(130 * time.Second)
			h.wantHealth("a", HealthHung)
			h.goLive()
			h.advance(70 * time.Second)
			ps := h.wantPosts(2)
			if got := ps[1].record(t).Action; got != "restart_rpc_server" {
				t.Fatalf("the new episode starts the sequence again, but sent %s", got)
			}
		})
	}
}

func TestRecovery_ServerErrorStillRetriesAndEscalates(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(threeSteps, func(r *config.Recovery) { r.StepWait = 100 * time.Second; r.MinInterval = 60 * time.Second }))
	h.rposter.script = []recResp{{status: 503}, {status: 200}}
	h.goLive()
	h.advance(10 * time.Second)
	h.wantPosts(2) // the first send and its one retry
	if h.logs.find("recovery sequence stopped") != nil {
		t.Fatal("a 5xx must not stop the sequence")
	}
	h.advance(5 * time.Minute)
	h.wantPosts(4) // the retry, then restart_docker and reboot
}

// Part (b): a node that hangs is killed off by node loss at 30 s, and worker
// health must keep tracking it as hung until it is seen online again.

// lostHarness has "m" ready on workers a and b with the default settings
// (connect probe off), recovery as configured, and a live view.
func lostHarness(t *testing.T, rec func(*harness)) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Defaults.WorkerProbeInterval = 0 // the real default
	h.cfg.Fleet.URL = testFleetURL
	if rec != nil {
		rec(h)
	}
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	h.release(h.ready("m"))
	h.wantState("m", "ready")
	return h
}

func TestNodeLoss_KeepsTrackingTheNodeAsHungUntilItReturns(t *testing.T) {
	h := lostHarness(t, recoveryCfg())
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(29 * time.Second)
	h.wantState("m", "ready")
	if got := h.nodeHealth("a"); got != nil && got.State == HealthHung {
		t.Fatalf("hung before node loss: %+v", got)
	}
	h.advance(2 * time.Second) // 31 s: node loss kills the instance
	h.wantState("m", "absent")
	got := h.wantHealth("a", HealthHung)
	if got.Channels.Heartbeat != ChanSilent || got.Channels.Upload != ChanUnknown || got.Channels.Probe != ChanUnknown {
		t.Fatalf("channels %+v", got.Channels)
	}
	if got.SilentForS["heartbeat"] < 29 {
		t.Fatalf("silent_for_s %+v: the episode starts when the node went missing", got.SilentForS)
	}
	if !h.c.quarantined("a") || h.c.quarantined("b") {
		t.Fatal("a must be quarantined, b not")
	}
	if h.nodeHealth("b") != nil && h.nodeHealth("b").State != HealthOK {
		t.Fatalf("b: %+v", h.nodeHealth("b"))
	}
	// CR-9 review, B8: node loss is logged as node loss, not as a hung worker.
	if w := h.logs.find("worker hung", "node", "a"); len(w) != 0 {
		t.Fatalf("node loss logged as worker hung %d times", len(w))
	}
	if w := h.logs.find("worker node lost; kept as hung until it is back", "node", "a"); len(w) != 1 {
		t.Fatalf("node loss logged %d times", len(w))
	}
	if q := h.logs.find("node quarantined", "node", "a"); len(q) != 1 || !strings.Contains(fmt.Sprint(q[0].Attrs["err"]), "node lost") ||
		strings.Contains(fmt.Sprint(q[0].Attrs["err"]), "worker hung") {
		t.Fatalf("quarantine records %+v", q)
	}
	// Still tracked, with no instance using it, for as long as it stays gone.
	h.advance(10 * time.Minute)
	h.wantHealth("a", HealthHung)
	if h.logs.find("worker health no longer tracked") != nil {
		t.Fatal("tracking dropped while the node is still gone")
	}
	// Exactly one recovery request, sent on the live view.
	ps := h.wantPosts(1)
	if rec := ps[0].record(t); rec.TargetNodeID != "a" || rec.Action != "restart_rpc_server" {
		t.Fatalf("record %+v", rec)
	}
	if rs := h.recoveryOf("a"); rs == nil || rs.EpisodeStarted == nil || rs.Step != 1 {
		t.Fatalf("recovery status %+v", rs)
	}
	// The node comes back: recovered is logged with the duration, state clears.
	h.view.modify("a", func(n *core.Node) { n.Online = true })
	h.viewChanged()
	w := h.logs.find("worker recovered", "node", "a")
	if len(w) != 1 || w[0].Level.String() != "INFO" || w[0].Attrs["hung_for"] == nil {
		t.Fatalf("recovered log %+v", w)
	}
	h.advance(time.Second)
	if got := h.nodeHealth("a"); got != nil {
		t.Fatalf("health %+v, want cleared", got)
	}
	if rs := h.recoveryOf("a"); rs.EpisodeStarted != nil || rs.Step != 0 {
		t.Fatalf("recovery episode not reset: %+v", rs)
	}
	h.advance(10 * time.Minute)
	h.wantPosts(1)
}

func TestNodeLoss_WithRecoveryOffSendsNothingButStillShowsHung(t *testing.T) {
	h := lostHarness(t, nil)
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(40 * time.Second)
	h.wantState("m", "absent")
	h.wantHealth("a", HealthHung)
	h.advance(20 * time.Minute)
	h.wantPosts(0)
}

func TestNodeLoss_NodeAbsentFromTheViewIsShownHungToo(t *testing.T) {
	h := lostHarness(t, recoveryCfg())
	h.view.remove("a")
	h.viewChanged()
	h.advance(40 * time.Second)
	h.wantState("m", "absent")
	h.wantHealth("a", HealthHung)
	h.wantPosts(1)
	h.view.set(ps5("a", 2, 15000))
	h.viewChanged()
	h.advance(time.Second)
	if got := h.nodeHealth("a"); got != nil && got.State == HealthHung {
		t.Fatalf("still hung: %+v", got)
	}
}

func TestNodeLoss_StaleViewDoesNotDecideTheNodeIsBack(t *testing.T) {
	h := lostHarness(t, recoveryCfg())
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(40 * time.Second)
	h.wantHealth("a", HealthHung)
	h.view.stale() // no bytes arrive: after stale_after (60 s) the view is not live
	h.advance(65 * time.Second)
	h.wantHealth("a", HealthHung)
	h.view.modify("a", func(n *core.Node) { n.Online = true })
	h.viewChanged()
	h.advance(5 * time.Second)
	h.wantHealth("a", HealthHung) // the return is not believed on a stale view
	h.view.fresh()
	h.advance(time.Second)
	if got := h.nodeHealth("a"); got != nil && got.State == HealthHung {
		t.Fatalf("still hung: %+v", got)
	}
}

// The node-loss rule that fires for a node already hung (heartbeat and probe
// both silent) leaves it hung with its original episode.
func TestNodeLoss_AlreadyHungNodeKeepsItsEpisodeStart(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Defaults.WorkerProbeInterval = 5 * time.Second
	h.cfg.Defaults.WorkerSilence = 5 * time.Second
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	h.release(h.ready("m"))
	h.wprober.setDown(addrA, true)
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(25 * time.Second)
	got := h.wantHealth("a", HealthHung)
	since := got.Since
	h.advance(10 * time.Second) // node loss at 30 s
	h.wantState("m", "absent")
	got = h.wantHealth("a", HealthHung)
	if got.Since != since {
		t.Fatalf("episode start changed: %d -> %d", since, got.Since)
	}
	if n := len(h.logs.find("worker hung", "node", "a")); n != 1 {
		t.Fatalf("worker hung logged %d times", n)
	}
}
