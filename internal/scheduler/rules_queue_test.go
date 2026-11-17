package scheduler

import (
	"net/http"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// Design 8.3: queue, claims, eviction.

// twoBig: one PS5 that holds either of two 12 GB models, not both.
func twoBig(t *testing.T) *harness {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 12000)
	h.llama("other", 12000)
	return h
}

func TestQueueClock_RunsWhileUnclaimed(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big") // busy: not evictable until drain_after
	_ = g
	h.request("other")
	h.advance(45 * time.Second)
	if got := h.entry("other").clock; got != 45*time.Second {
		t.Fatalf("clock = %v, want 45s", got)
	}
}

func TestQueueClock_PausesWhileAnyInstanceStarting(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("big", 12000)
	h.llama("other", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("third", 12000)
	h.ready("big") // on a or b
	busy := h.inst("big").nodeIDs()[0]
	if busy != "a" {
		t.Fatalf("big placed on %s; fixture expects a", busy)
	}
	w := h.request("other") // forced onto a, held by busy big
	_ = w
	h.advance(10 * time.Second)
	h.request("third") // goes to b: starting
	h.wantState("third", "starting")
	h.advance(100 * time.Second)
	if got := h.entry("other").clock; got != 10*time.Second {
		t.Fatalf("clock = %v while third was starting, want 10s", got)
	}
	h.healthy("third")
	h.advance(5 * time.Second)
	if got := h.entry("other").clock; got != 15*time.Second {
		t.Fatalf("clock = %v after third ready, want 15s", got)
	}
}

func TestQueueClock_StoppedWhileClaimed(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big")
	h.request("other")
	h.advance(61 * time.Second) // drain_after: big drains, other claims a
	h.wantState("big", "draining")
	h.wantClaim("other", "a")
	c0 := h.entry("other").clock
	h.advance(10 * time.Minute)
	if got := h.entry("other").clock; got != c0 {
		t.Fatalf("claimed clock moved %v → %v", c0, got)
	}
	_ = g
}

func TestPass_SkipsEntryWhileProfileHasInstance(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	g := h.ready("m")
	h.release(g)
	h.step(evUnload{profile: "m"})
	h.wantState("m", "stopping")
	w := h.request("m") // b is free and would fit, but the old instance must be gone first
	h.advance(5 * time.Second)
	h.wantState("m", "stopping")
	h.pending(w)
	h.entry("m")
}

func TestPass_AdmitOnReady_DeletesEntry(t *testing.T) {
	// An entry for P exists while P drains for another entry; when that
	// claim is dropped P returns to ready and admits the entry's waiters.
	h := twoBig(t)
	g := h.ready("big")
	wo := h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	wb := h.request("big")
	h.pending(wb)
	h.cancel(wo) // step 6: no waiters, victim still draining → deleted, big → ready
	h.wantState("big", "ready")
	h.noEntry("big")
	h.granted(wb)
	h.release(g)
}

func TestStep1_Place_StartsInstanceAndMovesWaiters(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoProbe = false
	w1 := h.request("m")
	in := h.inst("m")
	if in.state != stStarting || len(in.waiters) != 1 || in.waiters[0] != w1 {
		t.Fatalf("instance %v waiters %d", in.state, len(in.waiters))
	}
	h.noEntry("m")
	if h.c.nodes["a"].holder != "m" {
		t.Fatalf("node a not held")
	}
}

func TestStep2_Plan_ClaimsNodesIdleVictimStops(t *testing.T) {
	h := twoBig(t)
	h.autoExit = false
	g := h.ready("big")
	h.release(g)
	w := h.request("other") // big idle but within hold (30 s)
	h.wantState("big", "ready")
	h.noClaim("other")
	h.advance(30 * time.Second)
	h.wantState("big", "stopping")
	h.wantClaim("other", "a")
	e := h.entry("other")
	if len(e.victims) != 1 || e.victims[0] != "big" || !e.stoppedAny {
		t.Fatalf("victims %v stoppedAny %v", e.victims, e.stoppedAny)
	}
	h.pending(w)
}

func TestStep2_Plan_BusyVictimDrains(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big")
	w := h.request("other")
	h.advance(59 * time.Second)
	h.wantState("big", "ready") // busy and entry not yet unclaimed for drain_after
	h.advance(2 * time.Second)
	h.wantState("big", "draining")
	h.wantClaim("other", "a")
	if h.entry("other").stoppedAny {
		t.Fatalf("draining victim counted as stopped")
	}
	h.release(g) // idle now: the draining victim stops
	h.wantState("big", "absent")
	h.wantState("other", "absent") // a settles first
	h.advance(3 * time.Second)
	h.wantState("other", "starting")
	h.healthy("other")
	h.granted(w)
}

func TestStep2_Plan_TieBreakShortestLoadDuration(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("slow", 12000)
	h.llama("quick", 12000)
	h.llama("new", 12000)
	h.autoProbe = false
	ws := h.request("slow")
	h.completeProbes()
	h.advance(40 * time.Second) // slow loads for 40 s
	h.healthy("slow")
	h.release(h.granted(ws))
	wq := h.request("quick")
	h.completeProbes()
	h.healthy("quick") // loads at once
	h.release(h.granted(wq))
	h.autoProbe = true
	h.advance(41 * time.Second) // both past hold, both idle
	h.request("new")
	h.wantState("quick", "absent")
	h.wantState("slow", "ready")
}

func TestStep3_Skip_EntryStaysUnclaimed(t *testing.T) {
	h := twoBig(t)
	h.ready("big")
	w := h.request("other")
	h.advance(20 * time.Second)
	h.pending(w)
	h.noClaim("other")
	h.wantState("big", "ready")
}

func TestEvictable_NotBeforeHold_HoldIsLoadDuration(t *testing.T) {
	h := twoBig(t)
	h.autoProbe = false
	w := h.request("big")
	h.completeProbes()
	h.advance(100 * time.Second) // load_duration 100 s > min_hold
	h.healthy("big")
	h.release(h.granted(w))
	h.request("other")
	h.advance(99 * time.Second)
	h.wantState("big", "ready")
	h.advance(1 * time.Second)
	h.wantState("big", "absent") // stopped (auto exit) for other
}

func TestEvictable_HoldClampedToMaxHold(t *testing.T) {
	h := twoBig(t)
	h.cfg.Defaults.MaxHold = 50 * time.Second
	h.autoProbe = false
	w := h.request("big")
	h.completeProbes()
	h.advance(100 * time.Second)
	h.healthy("big")
	h.release(h.granted(w))
	h.request("other")
	h.advance(49 * time.Second)
	h.wantState("big", "ready")
	h.advance(1 * time.Second)
	h.wantState("big", "absent")
}

func TestEvictable_BusyOnlyAfterDrainAfter(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big")
	h.advance(10 * time.Minute) // far past hold, still busy
	h.request("other")
	h.advance(59 * time.Second)
	h.wantState("big", "ready")
	h.advance(1 * time.Second)
	h.wantState("big", "draining")
	_ = g
}

func TestStep4_VictimsExitedAndSettled_StartsOnClaim(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 9000))
	h.llama("big", 20000) // a + b
	h.llama("other", 12000)
	h.autoExit = false
	h.autoFetch = false
	g := h.ready("big")
	h.wantNodes("big", "a", "b")
	h.release(g)
	w := h.request("other")
	h.advance(30 * time.Second)
	h.wantClaim("other", "a")
	h.wantState("big", "stopping")
	h.exitChild(h.inst("big").iid, instanceExit(0))
	h.step(evTick{})
	h.pending(w)
	h.advance(3 * time.Second) // settle
	h.wantState("other", "absent")
	h.completeFetches()
	h.wantState("other", "starting")
	h.wantNodes("other", "a")
	h.noEntry("other")
	if ns := h.c.nodes["b"]; ns.claim != "" || ns.holder != "" {
		t.Fatalf("b not released: %+v", ns)
	}
	h.healthy("other")
	h.granted(w)
}

func TestStep4_UnusedClaimedNodesReleased(t *testing.T) {
	// Forced two-node profile victim; the claim keeps only the nodes placed.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("pair", 20000)
	h.llama("one", 12000)
	g := h.ready("pair")
	h.release(g)
	w := h.request("one")
	h.advance(30 * time.Second)
	h.advance(5 * time.Second)
	h.wantState("one", "starting")
	if len(h.nodesOf("one")) != 1 {
		t.Fatalf("one placed on %v", h.nodesOf("one"))
	}
	for _, id := range []core.NodeID{"a", "b"} {
		if h.c.nodes[id].claim != "" {
			t.Fatalf("%s still claimed", id)
		}
	}
	_ = w
}

func TestStep4_RefitFails_DropsClaimAndRecordsEmpty(t *testing.T) {
	h := twoBig(t)
	h.autoExit = false
	g := h.ready("big")
	h.release(g)
	w := h.request("other")
	h.advance(30 * time.Second)
	h.wantClaim("other", "a")
	// Something else takes memory on a while big stops.
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 9000 })
	h.exitChild(h.inst("big").iid, instanceExit(0))
	h.step(evTick{})
	h.advance(3 * time.Second) // settle + fetch: a has 9000 free, budget 8000 < 12000
	h.noClaim("other")
	ns := h.c.nodes["a"]
	if !ns.hasClaim || ns.emptyClaim != 9000 {
		t.Fatalf("empty figure not recorded: %+v", ns)
	}
	if got := h.c.planBudget("a", h.viewNode("a")); got != 8000 {
		t.Fatalf("plan budget = %d, want 9000-1000", got)
	}
	h.pending(w)
	h.wantState("other", "absent")
}

func TestStep5_ClaimedNodeOffline_DropsClaimDrainingVictimsReturnToReady(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 9000))
	h.llama("big", 20000)
	h.llama("other", 20000)
	g := h.ready("big")
	w := h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	h.wantClaim("other", "a", "b")
	h.view.modify("b", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.noClaim("other")
	h.wantState("big", "ready")
	h.pending(w)
	_ = g
}

func TestStep5_FleetViewStale_DropsClaim(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big")
	h.request("other")
	h.advance(61 * time.Second)
	h.wantClaim("other", "a")
	h.view.stale()
	h.advance(61 * time.Second)
	h.noClaim("other")
	h.wantState("big", "ready")
	_ = g
}

func TestStep5_StoppingVictimsStayStopping(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 5, 15000))
	h.llama("x", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("y", 12000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
	h.llama("xy", 20000, func(p *core.Profile) { p.ForceNodes = []string{"a", "b", "c"} })
	h.autoExit = false
	h.release(h.ready("x"))
	gy := h.ready("y") // busy
	w := h.request("xy")
	h.advance(61 * time.Second)
	h.wantState("x", "stopping")
	h.wantState("y", "draining")
	h.view.modify("c", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.noClaim("xy")
	h.wantState("x", "stopping")
	h.wantState("y", "ready")
	h.pending(w)
	_ = gy
}

func TestStep6_NoWaitersAllDraining_DeletedVictimsReturnToReady(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big")
	w := h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	h.cancel(w)
	h.noEntry("other")
	h.wantState("big", "ready")
	if h.c.nodes["a"].claim != "" {
		t.Fatalf("claim left behind")
	}
	_ = g
}

func TestStep6_NoWaitersVictimStopped_StaysAndLoads(t *testing.T) {
	h := twoBig(t)
	h.autoExit = false
	h.release(h.ready("big"))
	w := h.request("other")
	h.advance(30 * time.Second)
	h.wantState("big", "stopping")
	h.cancel(w)
	h.wantClaim("other", "a") // stays: a model was already unloaded for it
	h.exitChild(h.inst("big").iid, instanceExit(0))
	h.step(evTick{})
	h.advance(3 * time.Second)
	h.wantState("other", "starting")
	h.healthy("other")
	h.wantState("other", "ready")
}

func TestStep6_ClaimDroppedWithoutWaiters_Deleted(t *testing.T) {
	h := twoBig(t)
	h.autoExit = false
	h.release(h.ready("big"))
	w := h.request("other")
	h.advance(30 * time.Second)
	h.cancel(w)
	h.wantClaim("other", "a")
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.noEntry("other")
}

func TestRule_ClaimedNodesInvisibleToLaterEntries(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 9000))
	h.llama("big", 20000) // a + b
	h.llama("other", 20000)
	h.llama("small", 7000)
	h.autoExit = false
	h.release(h.ready("big"))
	h.request("other")
	h.advance(30 * time.Second)
	h.wantClaim("other", "a", "b")
	h.exitChild(h.inst("big").iid, instanceExit(0))
	h.step(evTick{})
	h.autoFetch = false
	h.advance(3 * time.Second) // nodes fetching, claimed by other
	ws := h.request("small")   // would fit on a or b, both claimed
	h.pending(ws)
	h.wantState("small", "absent")
}

func TestRule_LaterEntryFittingFreeNodesGoesFirst(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 9000))
	h.llama("big", 12000)
	h.llama("huge", 20000)
	h.llama("small", 7000)
	g := h.ready("big") // on a, busy
	wh := h.request("huge")
	ws := h.request("small") // fits on b now
	h.wantState("small", "starting")
	h.pending(wh)
	_, _ = g, ws
}

func TestRule_QueueTimeout_503ClusterBusy(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big")
	h.cfg.Defaults.DrainAfter = time.Hour
	w := h.request("other")
	h.advance(599 * time.Second)
	h.pending(w)
	h.advance(time.Second)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonClusterBusy)
	if e.RetryAfter != 30*time.Second {
		t.Fatalf("Retry-After = %v", e.RetryAfter)
	}
	h.noEntry("other")
	_ = g
}

func TestRule_QueueTimeout_503ForcedUnavailable(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a", "ghost"} })
	w := h.request("f")
	h.advance(600 * time.Second)
	h.failed(w, http.StatusServiceUnavailable, ReasonForcedUnavailable)
}

func TestRule_QueueTimeout_503FleetStale(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.view.stale()
	h.advance(61 * time.Second)
	w := h.request("m")
	h.advance(600 * time.Second)
	h.failed(w, http.StatusServiceUnavailable, ReasonFleetStale)
}

func TestRule_IdleUnloadAfterTTL(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	g := h.ready("m")
	h.advance(10 * time.Minute) // busy: never idle-unloaded
	h.wantState("m", "ready")
	h.release(g)
	h.advance(299 * time.Second)
	h.wantState("m", "ready")
	h.advance(time.Second)
	h.wantState("m", "absent")
}

func TestRule_IdleUnloadTTLZeroNever(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.TTL = ptrDur(0) })
	h.release(h.ready("m"))
	h.advance(24 * time.Hour)
	h.wantState("m", "ready")
}

func TestRule_IdleUnloadPausedForRemoteWhileStale(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), omen(8000))
	h.llama("remote", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("local", 6000, func(p *core.Profile) { p.ForceNodes = []string{"omen"} })
	h.release(h.ready("remote"))
	h.release(h.ready("local"))
	h.view.stale()
	h.advance(301 * time.Second)
	h.wantState("remote", "ready")
	h.wantState("local", "absent")
	h.view.fresh()
	h.viewChanged()
	h.wantState("remote", "absent")
}

func TestRule_DrainingVictimStopsWhenIdle(t *testing.T) {
	h := twoBig(t)
	h.autoExit = false
	g := h.ready("big")
	g2 := h.request("big")
	h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	h.release(g)
	h.wantState("big", "draining")
	h.release(h.granted(g2))
	h.wantState("big", "stopping")
}

// ---- helpers ----

func (h *harness) noClaim(profile string) {
	h.t.Helper()
	if e := h.c.entries[profile]; e != nil && e.claimed() {
		h.t.Fatalf("%s claimed %v, want unclaimed", profile, e.claim)
	}
}

func (h *harness) viewNode(id core.NodeID) core.Node {
	n, _ := h.c.node(id)
	return n
}
