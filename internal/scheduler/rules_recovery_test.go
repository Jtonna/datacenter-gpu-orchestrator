package scheduler

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Recovery requests (design 8.4b): fake clock, fake HTTP poster.

const testFleetURL = "http://fleet.test:8090"

var nonceRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// recoveryCfg is the default recovery block, switched on, with mods applied.
func recoveryCfg(mod ...func(*config.Recovery)) func(*harness) {
	return func(h *harness) {
		r := config.DefaultRecovery()
		r.Enabled = true
		for _, m := range mod {
			m(&r)
		}
		h.cfg.Recovery = r
	}
}

// hungHarness builds "m" ready on workers idA and "b", makes the heartbeat
// unknown (stale view) and the probe of idA fail until idA is hung. A hung
// worker with a stale view is how the tests reach hung without node loss
// ending the instance. The view stays stale; call goLive to make it live.
func hungHarness(t *testing.T, idA string, setup func(*harness)) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5(idA, 2, 15000), ps5("b", 4, 15000))
	h.cfg.Fleet.URL = testFleetURL
	if setup != nil {
		setup(h)
	}
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{idA, "b"}, []int{1, 1}, ptrDur(0)
	})
	h.release(h.ready("m"))
	h.wantState("m", "ready")
	h.view.stale()
	h.wprober.setDown(addrA, true)
	h.advance(130 * time.Second)
	h.wantHealth(core.NodeID(idA), HealthHung)
	return h
}

// goLive makes the view fresh and steps once; a hung node may be sent to now.
func (h *harness) goLive() {
	h.t.Helper()
	h.view.fresh()
	h.advance(time.Second)
}

func (h *harness) posts() []recPost { return h.rposter.all() }

func (h *harness) wantPosts(n int) []recPost {
	h.t.Helper()
	ps := h.posts()
	if len(ps) != n {
		h.t.Fatalf("%d recovery posts, want %d: %v", len(ps), n, h.logs.find("recovery request sent"))
	}
	return ps
}

func (h *harness) recoveryOf(id core.NodeID) *RecoveryStatus {
	h.t.Helper()
	for _, n := range h.c.statusDoc().View.Nodes {
		if n.ID == id {
			return n.Recovery
		}
	}
	h.t.Fatalf("no node %s in the status", id)
	return nil
}

func (h *harness) recoveryEvents() []*RecoveryEvent {
	var out []*RecoveryEvent
	for _, ev := range h.feedLog {
		if ev.Kind == FeedRecovery {
			out = append(out, ev.Recovery)
		}
	}
	return out
}

func TestRecovery_DisabledByDefaultSendsNothing(t *testing.T) {
	if config.DefaultRecovery().Enabled {
		t.Fatal("recovery must be disabled by default")
	}
	h := hungHarness(t, "a", nil) // no recovery block at all
	h.goLive()
	h.advance(20 * time.Minute)
	h.wantPosts(0)
	if h.recoveryOf("a") != nil {
		t.Fatal("status.recovery set while the feature is off")
	}
	// Also with the default block (enabled false).
	h = hungHarness(t, "a", func(h *harness) { h.cfg.Recovery = config.DefaultRecovery() })
	h.goLive()
	h.advance(20 * time.Minute)
	h.wantPosts(0)
}

func TestRecovery_HungSendsOneWellFormedRequest(t *testing.T) {
	id := "02:00:00:00:00:01"
	h := hungHarness(t, id, recoveryCfg())
	h.wantPosts(0) // the view is stale: nothing yet
	h.goLive()
	ps := h.wantPosts(1)
	p := ps[0]
	if want := testFleetURL + "/api/recovery/02%3A00%3A00%3A00%3A00%3A01"; p.URL != want {
		t.Fatalf("url = %s, want %s", p.URL, want)
	}
	if p.Timeout != 3*time.Second {
		t.Fatalf("http timeout %v, want 3s", p.Timeout)
	}
	rec := p.record(t)
	if rec.V != 1 || rec.TargetNodeID != id || rec.Action != "restart_rpc_server" {
		t.Fatalf("record %+v", rec)
	}
	if !nonceRe.MatchString(rec.Nonce) {
		t.Fatalf("nonce %q is not 32 lowercase hex chars", rec.Nonce)
	}
	if rec.IssuedAt != p.At.UnixMilli() || rec.ExpiresAt-rec.IssuedAt != 45000 {
		t.Fatalf("issued %d expires %d (now %d), want a 45 s ttl", rec.IssuedAt, rec.ExpiresAt, p.At.UnixMilli())
	}
	if !strings.HasPrefix(rec.Reason, "hung: ") || !strings.Contains(rec.Reason, "probe silent ") {
		t.Fatalf("reason %q", rec.Reason)
	}
	t.Logf("request: POST %s\n%s", p.URL, p.Body)

	// Logs: the request (INFO) and the answer (INFO for 200).
	sent := h.logs.find("recovery request sent", "node", id, "action", "restart_rpc_server", "nonce", rec.Nonce)
	if len(sent) != 1 || sent[0].Level.String() != "INFO" || sent[0].Attrs["reason"] != rec.Reason {
		t.Fatalf("sent log %+v", sent)
	}
	if st := h.logs.find("recovery request stored", "nonce", rec.Nonce, "status", 200); len(st) != 1 || st[0].Level.String() != "INFO" {
		t.Fatalf("stored log %+v", st)
	}
	// Status and events.
	rs := h.recoveryOf(core.NodeID(id))
	if rs == nil || rs.LastRequest == nil {
		t.Fatalf("status %+v", rs)
	}
	if rs.LastRequest.Status != RecoveryStored || rs.LastRequest.HTTPStatus != 200 ||
		rs.LastRequest.Nonce != rec.Nonce || rs.LastRequest.Action != "restart_rpc_server" || rs.LastRequest.SentAt != rec.IssuedAt ||
		rs.Step != 1 || rs.EpisodeStarted == nil || rs.LastOutcome != nil {
		t.Fatalf("status %+v / %+v", rs, rs.LastRequest)
	}
	evs := h.recoveryEvents()
	if len(evs) != 1 || evs[0].Node != core.NodeID(id) || evs[0].Phase != "sent" || evs[0].Recovery.LastRequest.Nonce != rec.Nonce {
		t.Fatalf("events %+v", evs)
	}
	// One request per episode with one action, however long it stays hung.
	h.advance(30 * time.Minute)
	h.wantPosts(1)
}

func TestRecovery_StaleViewSendsNothing(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg())
	h.advance(10 * time.Minute) // hung the whole time, view stale
	h.wantPosts(0)
	h.goLive()
	h.wantPosts(1)
}

func TestRecovery_ViewGoingStaleAgainBlocksTheNextStep(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(func(r *config.Recovery) {
		r.Actions = []string{config.RecoveryRestartRPC, config.RecoveryRestartDocker}
		r.StepWait = 100 * time.Second
	}))
	h.goLive()
	h.wantPosts(1)
	h.view.stale()
	h.advance(5 * time.Minute)
	h.wantPosts(1)
	h.goLive()
	h.wantPosts(2)
}

func TestRecovery_MinIntervalAcrossEpisodes(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(func(r *config.Recovery) { r.MinInterval = 600 * time.Second }))
	h.goLive()
	first := h.wantPosts(1)[0].At
	// The worker answers again: the episode ends. Then it hangs again soon.
	h.wprober.setDown(addrA, false)
	h.advance(15 * time.Second)
	h.wantHealth("a", HealthOK)
	if rs := h.recoveryOf("a"); rs.EpisodeStarted != nil || rs.Step != 0 {
		t.Fatalf("not reset: %+v", rs)
	}
	h.view.stale()
	h.wprober.setDown(addrA, true)
	h.advance(130 * time.Second)
	h.wantHealth("a", HealthHung)
	h.goLive()
	h.wantPosts(1) // hung again, but min_interval (600 s) has not passed
	h.advance(first.Add(599 * time.Second).Sub(h.clk.Now()))
	h.wantPosts(1)
	h.advance(2 * time.Second)
	ps := h.wantPosts(2)
	if got := ps[1].At.Sub(first); got < 600*time.Second || got > 601*time.Second {
		t.Fatalf("second request %v after the first, want min_interval 600s", got)
	}
}

func TestRecovery_RefusedStatusesAreNotRetried(t *testing.T) {
	for _, code := range []int{400, 403, 404, 409, 410, 422, 429} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := hungHarness(t, "a", recoveryCfg())
			h.rposter.script = []recResp{{status: code, body: `{"error":"no"}`}}
			h.goLive()
			h.advance(5 * time.Minute)
			h.wantPosts(1)
			rs := h.recoveryOf("a")
			if rs.LastRequest.Status != RecoveryRefused || rs.LastRequest.HTTPStatus != code {
				t.Fatalf("last request %+v", rs.LastRequest)
			}
			w := h.logs.find("recovery request refused", "status", code)
			if len(w) != 1 || w[0].Level.String() != "WARN" || w[0].Attrs["body"] != `{"error":"no"}` {
				t.Fatalf("refused log %+v", w)
			}
		})
	}
}

func TestRecovery_ServerErrorRetriedOnceAfter10sWithANewNonce(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg())
	h.rposter.script = []recResp{{status: 503, body: "busy"}, {status: 502, body: "bad gateway"}}
	h.goLive()
	first := h.wantPosts(1)[0]
	h.advance(9 * time.Second)
	h.wantPosts(1)
	h.advance(time.Second)
	ps := h.wantPosts(2)
	if got := ps[1].At.Sub(first.At); got != 10*time.Second {
		t.Fatalf("retry %v after the first, want 10s", got)
	}
	a, b := first.record(t), ps[1].record(t)
	if a.Nonce == b.Nonce || !nonceRe.MatchString(b.Nonce) {
		t.Fatalf("nonces %q then %q: want a new one", a.Nonce, b.Nonce)
	}
	if b.IssuedAt != ps[1].At.UnixMilli() || b.IssuedAt-a.IssuedAt != 10000 || b.ExpiresAt-b.IssuedAt != 45000 {
		t.Fatalf("retry times issued %d expires %d, first issued %d", b.IssuedAt, b.ExpiresAt, a.IssuedAt)
	}
	if b.Action != a.Action {
		t.Fatalf("retry changed the action: %s", b.Action)
	}
	h.advance(10 * time.Minute) // the retry failed too: exactly once
	h.wantPosts(2)
	if rs := h.recoveryOf("a"); rs.LastRequest.Status != RecoveryError || rs.LastRequest.HTTPStatus != 502 || rs.LastRequest.Nonce != b.Nonce {
		t.Fatalf("last request %+v", rs.LastRequest)
	}
	if w := h.logs.find("recovery request failed", "status", 503); len(w) != 1 || w[0].Level.String() != "WARN" {
		t.Fatalf("failure log %+v", w)
	}
}

func TestRecovery_TimeoutRetriedOnceThenStored(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg())
	h.rposter.script = []recResp{{err: errBoom}}
	h.goLive()
	h.wantPosts(1)
	h.advance(10 * time.Second)
	h.wantPosts(2)
	rs := h.recoveryOf("a")
	if rs.LastRequest.Status != RecoveryStored || rs.LastRequest.HTTPStatus != 200 {
		t.Fatalf("last request %+v", rs.LastRequest)
	}
	h.advance(10 * time.Minute)
	h.wantPosts(2)
}

func threeSteps(r *config.Recovery) {
	r.Actions = []string{config.RecoveryRestartRPC, config.RecoveryRestartDocker, config.RecoveryReboot}
}

func TestRecovery_EscalatesThroughThreeActionsWithStepWait(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(threeSteps, func(r *config.Recovery) { r.StepWait = 200 * time.Second; r.MinInterval = 60 * time.Second }))
	h.goLive()
	ps := h.wantPosts(1)
	start := ps[0].At
	h.advance(199 * time.Second)
	h.wantPosts(1)
	h.advance(2 * time.Second)
	h.wantPosts(2)
	h.advance(198 * time.Second)
	h.wantPosts(2)
	h.advance(3 * time.Second)
	h.wantPosts(3)
	h.advance(30 * time.Minute) // never more than len(actions) requests per episode
	ps = h.wantPosts(3)
	var got []string
	for i, p := range ps {
		r := p.record(t)
		got = append(got, r.Action)
		if want := time.Duration(i) * 200 * time.Second; p.At.Sub(start) < want || p.At.Sub(start) > want+2*time.Second {
			t.Errorf("step %d sent %v after the first, want about %v", i+1, p.At.Sub(start), want)
		}
		if i > 0 && !strings.Contains(r.Reason, "escalating after "+got[i-1]) {
			t.Errorf("step %d reason %q lacks the escalation note", i+1, r.Reason)
		}
	}
	if strings.Join(got, ",") != "restart_rpc_server,restart_docker,reboot" {
		t.Fatalf("actions %v", got)
	}
	if rs := h.recoveryOf("a"); rs.Step != 3 || rs.LastRequest.Action != "reboot" {
		t.Fatalf("status %+v", rs)
	}
	if len(h.logs.find("recovery sequence finished and worker still hung")) != 1 {
		t.Fatal("no single warning that the sequence is exhausted")
	}
}

func TestRecovery_MinIntervalGatesEscalation(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(threeSteps, func(r *config.Recovery) { r.StepWait = 10 * time.Second; r.MinInterval = 120 * time.Second }))
	h.goLive()
	first := h.wantPosts(1)[0].At
	h.advance(119 * time.Second)
	h.wantPosts(1)
	h.advance(2 * time.Second)
	ps := h.wantPosts(2)
	if d := ps[1].At.Sub(first); d < 120*time.Second || d > 121*time.Second {
		t.Fatalf("second step %v after the first, want min_interval 120s (step_wait is only 10s)", d)
	}
}

func TestRecovery_HealthReturningToOKResetsTheSequence(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(threeSteps, func(r *config.Recovery) { r.StepWait = 100 * time.Second; r.MinInterval = 60 * time.Second }))
	h.goLive()
	h.wantPosts(1)
	h.advance(100 * time.Second)
	h.wantPosts(2) // step 2 sent
	h.wprober.setDown(addrA, false)
	h.advance(15 * time.Second)
	h.wantHealth("a", HealthOK)
	h.advance(20 * time.Minute) // ok: nothing more, however long
	h.wantPosts(2)
	rs := h.recoveryOf("a")
	if rs.Step != 0 || rs.EpisodeStarted != nil || rs.LastRequest.Action != "restart_docker" {
		t.Fatalf("after reset: %+v", rs)
	}
	if len(h.logs.find("recovery sequence reset", "node", "a")) != 1 {
		t.Fatalf("reset log %+v", h.logs.find("recovery sequence reset"))
	}
	// A new episode starts over at step 1.
	h.view.stale()
	h.wprober.setDown(addrA, true)
	h.advance(130 * time.Second)
	h.wantHealth("a", HealthHung)
	h.goLive()
	ps := h.wantPosts(3)
	if r := ps[2].record(t); r.Action != "restart_rpc_server" {
		t.Fatalf("new episode began with %s, want restart_rpc_server", r.Action)
	}
	if rs := h.recoveryOf("a"); rs.Step != 1 || rs.EpisodeStarted == nil {
		t.Fatalf("new episode status %+v", rs)
	}
}

func TestRecovery_OutcomeIsLoggedReportedAndSentAsAnEvent(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg())
	h.goLive()
	rec := h.wantPosts(1)[0].record(t)
	out := core.RecoveryOutcome{Present: true, Action: rec.Action, At: rec.IssuedAt + 4000, Outcome: "ok", Reason: rec.Reason,
		Nonce: rec.Nonce, DurationMs: 1800}
	h.view.modify("a", func(n *core.Node) { n.Recovery = out })
	h.viewChanged()
	if l := h.logs.find("recovery outcome", "node", "a", "outcome", "ok", "nonce", rec.Nonce, "ours", true); len(l) != 1 || l[0].Level.String() != "INFO" {
		t.Fatalf("outcome log %+v", h.logs.find("recovery outcome"))
	}
	rs := h.recoveryOf("a")
	if rs.LastOutcome == nil || rs.LastOutcome.Outcome != "ok" || rs.LastOutcome.DurationMs != 1800 || rs.LastOutcome.Nonce != rec.Nonce {
		t.Fatalf("last outcome %+v", rs.LastOutcome)
	}
	evs := h.recoveryEvents()
	if len(evs) != 2 || evs[1].Phase != "outcome" || evs[1].Recovery.LastOutcome.Nonce != rec.Nonce {
		t.Fatalf("events %+v", evs)
	}
	h.viewChanged() // the same outcome again is not news
	h.advance(time.Minute)
	if n := len(h.recoveryEvents()); n != 2 {
		t.Fatalf("%d events after the same outcome again", n)
	}
	if n := len(h.logs.find("recovery outcome")); n != 1 {
		t.Fatalf("%d outcome logs", n)
	}
}

func TestRecovery_FailedOutcomeAdvancesOnceMinIntervalAllows(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(threeSteps, func(r *config.Recovery) { r.StepWait = 900 * time.Second; r.MinInterval = 60 * time.Second }))
	h.goLive()
	first := h.wantPosts(1)[0]
	rec := first.record(t)
	failed := core.RecoveryOutcome{Present: true, Action: rec.Action, At: rec.IssuedAt + 2000, Outcome: "failed", Nonce: rec.Nonce}
	h.advance(10 * time.Second)
	h.view.modify("a", func(n *core.Node) { n.Recovery = failed })
	h.viewChanged()
	h.wantPosts(1) // min_interval (60 s) has not passed yet
	h.advance(48 * time.Second)
	h.wantPosts(1)
	h.advance(3 * time.Second)
	ps := h.wantPosts(2)
	if d := ps[1].At.Sub(first.At); d < 60*time.Second || d > 62*time.Second {
		t.Fatalf("next step %v after the first, want min_interval 60s (step_wait is 900s)", d)
	}
	if r := ps[1].record(t); r.Action != "restart_docker" {
		t.Fatalf("action %s", r.Action)
	}
}

func TestRecovery_OKOutcomeDoesNotSkipStepWait(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(threeSteps, func(r *config.Recovery) { r.StepWait = 300 * time.Second; r.MinInterval = 60 * time.Second }))
	h.goLive()
	rec := h.wantPosts(1)[0].record(t)
	h.view.modify("a", func(n *core.Node) {
		n.Recovery = core.RecoveryOutcome{Present: true, Action: rec.Action, At: rec.IssuedAt + 2000, Outcome: "ok", Nonce: rec.Nonce}
	})
	h.viewChanged()
	h.advance(280 * time.Second)
	h.wantPosts(1) // the step did not fix it, but it gets its full step_wait
	h.advance(30 * time.Second)
	h.wantPosts(2)
}

func TestRecovery_NeverForAnExcludedNode(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg())
	h.cfg.ExcludeNodes = []string{"a"} // excluded after it became hung
	h.goLive()
	h.advance(10 * time.Minute)
	h.wantPosts(0)
}

func TestRecovery_NeverForTheLocalNode(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg())
	h.view.set(ps5("a", 2, 15000), ps5("b", 4, 15000), omen(8000))
	h.goLive()
	for _, p := range h.wantPosts(1) {
		if r := p.record(t); r.TargetNodeID == "host-1" || strings.HasSuffix(p.URL, "/host-1") {
			t.Fatalf("request for the local node: %s", p.Body)
		}
	}
	if h.c.recoveryAllowed("host-1") {
		t.Fatal("recoveryAllowed(local) is true")
	}
	// A local-only profile gives the local node no worker health, so no request.
	h2 := newHarness(t).nodes(omen(8000))
	recoveryCfg()(h2)
	h2.llama("l", 3000, func(p *core.Profile) { p.TTL = ptrDur(0) })
	h2.release(h2.ready("l"))
	h2.view.stale()
	h2.advance(30 * time.Minute)
	h2.goLive()
	h2.advance(30 * time.Minute)
	h2.wantPosts(0)
}

func TestRecovery_NoRequestForANodeNoInstanceUses(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg())
	h.step(evUnload{profile: "m", force: true}) // nothing uses the node now
	h.goLive()
	h.advance(10 * time.Minute)
	h.wantPosts(0)
	if h.nodeHealth("a") != nil {
		t.Fatal("health still tracked")
	}
}

func TestRecovery_StatusOmittedUntilSomethingWasSent(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg())
	if h.recoveryOf("a") != nil || h.recoveryOf("b") != nil {
		t.Fatal("recovery in the status before anything was sent")
	}
	h.goLive()
	if h.recoveryOf("a") == nil || h.recoveryOf("b") != nil {
		t.Fatal("recovery must appear on node a only")
	}
}

func TestRecovery_RequestsAreSentOffTheLoop(t *testing.T) {
	// The core only emits an effect; the POST never runs inside handle.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Fleet.URL = testFleetURL
	recoveryCfg()(h)
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	h.release(h.ready("m"))
	h.view.stale()
	h.wprober.setDown(addrA, true)
	h.advance(130 * time.Second)
	h.view.fresh()
	h.c.handle(evViewChanged{})
	var found bool
	for _, ef := range h.c.takeEffects() {
		if _, ok := ef.(effRecoveryPost); ok {
			found = true
		}
	}
	if !found || len(h.posts()) != 0 {
		t.Fatalf("effect emitted %v, posts made inside the step %d", found, len(h.posts()))
	}
}
