package scheduler

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The scenarios the design reviewers walked through.

func TestScenario_TwoProfilesCannotCoexist_RequestedAlternately(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("A", 12000)
	h.llama("B", 12000)
	h.autoProbe = false
	const loadTime = 40 * time.Second
	served := map[string]int{}
	var readyAt time.Time
	for round := 0; round < 6; round++ {
		p := []string{"A", "B"}[round%2]
		w := h.request(p)
		// Wait until p is starting (the other one must hold for its load time first).
		for i := 0; h.state(p) != "starting"; i++ {
			if i > 200 {
				t.Fatalf("round %d: %s never started", round, p)
			}
			h.advance(time.Second)
		}
		if round > 0 && h.clk.Now().Sub(readyAt) < loadTime {
			t.Fatalf("round %d: previous model evicted after %v, before its hold %v", round, h.clk.Now().Sub(readyAt), loadTime)
		}
		h.completeProbes()
		h.advance(loadTime)
		h.healthy(p)
		readyAt = h.clk.Now()
		h.release(h.granted(w))
		served[p]++
	}
	if served["A"] != 3 || served["B"] != 3 {
		t.Fatalf("served %v", served)
	}
}

func TestScenario_ClaimedEntryVictimDrainsLongGeneration_SmallRequestsUseOnlyUnclaimedNodes(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 5, 15000))
	h.llama("big", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("huge", 25000, func(p *core.Profile) { p.ForceNodes = []string{"a", "b"} })
	h.llama("s1", 4000)
	h.llama("s2", 4000)
	gen := h.ready("big") // a very long generation
	wh := h.request("huge")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	h.wantClaim("huge", "a", "b")
	w1 := h.request("s1")
	h.wantNodes("s1", "c") // b is free but claimed
	h.healthy("s1")
	h.granted(w1)
	w2 := h.request("s2") // c is taken; a and b are claimed
	h.pending(w2)
	h.wantState("s2", "absent")
	for i := 0; i < 30; i++ {
		h.advance(time.Minute) // the generation goes on; nothing cuts it off
		h.wantState("big", "draining")
		if h.inst("big").inFlight != 1 {
			t.Fatalf("generation cut off")
		}
	}
	// s2 may only plan against the unclaimed node c (s1, busy past drain_after).
	h.wantClaim("s2", "c")
	h.wantState("s1", "draining")
	h.release(h.granted(w1))
	h.advance(3 * time.Second)
	h.wantState("s2", "starting")
	h.wantNodes("s2", "c")
	h.release(gen)
	h.advance(3 * time.Second)
	h.wantState("huge", "starting")
	h.wantNodes("huge", "a", "b")
	h.healthy("huge")
	h.granted(wh)
}

func TestScenario_ClaimedNodeGoesOfflineMidPlan(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 9000))
	h.llama("big", 12000)
	h.llama("huge", 18000)
	g := h.ready("big") // a
	wh := h.request("huge")
	h.advance(61 * time.Second)
	h.wantClaim("huge", "a", "b")
	h.wantState("big", "draining")
	h.view.modify("b", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.wantState("big", "ready") // returned from draining
	h.noClaim("huge")
	wb := h.request("big")
	h.granted(wb) // big serves again
	h.view.modify("b", func(n *core.Node) { n.Online = true })
	h.viewChanged()
	h.wantClaim("huge", "a", "b") // the entry has waited past drain_after: plan again
	h.wantState("big", "draining")
	h.release(g)
	h.release(h.granted(wb))
	h.advance(3 * time.Second)
	h.wantState("huge", "starting")
	_ = wh
}

func TestScenario_LoadFailsWhileOtherEntriesHoldClaims(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("big", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("x", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("y", 12000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
	g := h.ready("big")
	wx := h.request("x")
	h.advance(61 * time.Second)
	h.wantClaim("x", "a")
	wy := h.request("y")
	h.wantState("y", "starting")
	h.exit("y", 1)
	h.failed(wy, http.StatusBadGateway, ReasonLoadFailed)
	h.wantClaim("x", "a")
	h.wantState("big", "draining")
	h.pending(wx)
	h.release(g)
	h.advance(3 * time.Second)
	h.healthy("x")
	h.granted(wx)
}

func TestScenario_ForcedNodesProfileWhileThoseNodesRunBusyModel(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 9000), ps5("b", 4, 9000), laptop("al", 7800))
	h.llama("m", 13500) // a + b
	h.llama("bench", 1, func(p *core.Profile) {
		p.ForceNodes = []string{"b", "a"}
		p.TensorSplit = []int{60, 40}
	})
	g := h.ready("m")
	h.wantNodes("m", "a", "b")
	wb := h.request("bench")
	h.advance(59 * time.Second)
	h.wantState("m", "ready")
	h.advance(time.Second)
	h.wantState("m", "draining")
	h.wantClaim("bench", "a", "b")
	h.release(g)
	h.advance(3 * time.Second)
	in := h.inst("bench")
	if in.placement.Nodes[0].ID != "b" || in.placement.Nodes[1].ID != "a" {
		t.Fatalf("forced order lost: %+v", in.placement.Nodes)
	}
	if got := argValue(h.child("bench").Argv(), "--tensor-split"); got != "60,40" {
		t.Fatalf("split %q", got)
	}
	_ = wb
}

func TestScenario_FeedReconnectsWithEmptyView_WhileLoadedAndQueued(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("big", 12000)
	h.llama("m", 8000)
	h.llama("pair", 25000)
	gb := h.ready("big")
	gm := h.ready("m")
	wp := h.request("pair")
	h.advance(61 * time.Second)
	h.wantClaim("pair", "a", "b")
	// The fleet console restarts: the stream reconnects with nobody in it.
	saved := h.view.Nodes()
	for _, n := range saved {
		h.view.remove(n.ID)
	}
	h.view.reconnect()
	h.viewChanged()
	h.noClaim("pair") // claimed nodes are not in the view: not eligible
	h.wantState("big", "ready")
	h.wantState("m", "ready")
	h.granted(h.request("big")) // loaded instances keep serving
	h.advance(20 * time.Second)
	h.view.set(saved...) // nodes report again within 30 s
	h.viewChanged()
	h.advance(time.Minute)
	h.wantState("big", "draining")
	h.wantState("m", "draining")
	h.pending(wp)
	_, _ = gb, gm
}

func TestScenario_FeedReconnectsEmpty_NodesNeverReturn_InstancesKilled(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.ready("m")
	h.view.remove("a")
	h.view.reconnect()
	h.viewChanged()
	h.advance(29 * time.Second)
	h.wantState("m", "ready")
	h.advance(time.Second)
	h.wantState("m", "absent")
}

func TestScenario_CommandProfileOnLocalEvictsIdleLLMIncludingLocal(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 9000), hostNode(8000))
	h.llama("llm", 13000) // a pool of the PS5 and the host machine
	h.command("whisper", 5400)
	h.release(h.ready("llm"))
	h.wantNodes("llm", "a", "host-1")
	ww := h.request("whisper")
	h.advance(30 * time.Second) // hold passes; llm is idle: stopped
	h.wantState("llm", "absent")
	h.wantClaim("whisper", "host-1")
	h.advance(2 * time.Second) // the host machine settles (2 s)
	h.wantState("whisper", "starting")
	h.wantNodes("whisper", "host-1")
	h.healthy("whisper")
	h.granted(ww)
	// The PS5 was released but not claimed.
	if ns := h.c.nodes["a"]; ns.claim != "" || ns.holder != "" {
		t.Fatalf("a: %+v", ns)
	}
}

func TestScenario_QueueTimeout_UnclaimedWhileAnotherStartingThenAfter(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("big", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("x", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("slow", 12000, func(p *core.Profile) { p.ForceNodes = []string{"b"}; p.LoadTimeout = time.Hour })
	h.cfg.Defaults.DrainAfter = time.Hour
	g := h.ready("big")
	wx := h.request("x")
	h.advance(500 * time.Second)
	h.request("slow") // starts on b and loads for 20 minutes
	h.advance(20 * time.Minute)
	h.pending(wx) // clock paused at 500 s
	if got := h.entry("x").clock; got != 500*time.Second {
		t.Fatalf("clock %v", got)
	}
	h.healthy("slow")
	h.advance(99 * time.Second)
	h.pending(wx)
	h.advance(time.Second)
	h.failed(wx, http.StatusServiceUnavailable, ReasonClusterBusy)
	_ = g
}

func TestScenario_NeedThatCanNeverFit(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 3000), ps5("b", 4, 3000))
	h.llama("huge", 31000) // two PS5s at plan budget 15000 each
	w := h.request("huge")
	e := h.failed(w, http.StatusServiceUnavailable, ReasonDoesNotFit)
	if e.CapacityMiB != 30000 || e.NeedMiB != 31000 {
		t.Fatalf("figures %+v", e)
	}
	h.noEntry("huge")
	// Fits the capacity, but the nodes are full of something else: it queues.
	h.llama("fits", 26000)
	w2 := h.request("fits")
	h.pending(w2)
	h.advance(600 * time.Second)
	h.failed(w2, http.StatusServiceUnavailable, ReasonClusterBusy)
}

// A waiter for P arrives while P drains for another entry Q.
func waiterWhileDraining(t *testing.T) (*harness, *grant, *waiter, *waiter) {
	h := twoBig(t) // big, other on one PS5
	g := h.ready("big")
	wo := h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	wb := h.request("big")
	h.pending(wb)
	return h, g, wo, wb
}

func TestScenario_WaiterForPWhileDrainingForOther_PStops(t *testing.T) {
	h, g, wo, wb := waiterWhileDraining(t)
	h.release(g) // big stops; other loads
	h.wantState("big", "absent")
	h.advance(3 * time.Second)
	h.wantState("other", "starting")
	h.healthy("other")
	g2 := h.granted(wo)
	h.pending(wb) // big's entry now waits for other
	if e := h.entry("big"); e.claimed() {
		t.Fatalf("big entry claimed early")
	}
	h.release(g2)
	h.advance(30 * time.Second) // other's hold
	h.advance(3 * time.Second)
	h.wantState("big", "starting")
	h.healthy("big")
	h.granted(wb)
}

func TestScenario_WaiterForPWhileDrainingForOther_PReturnsToReady(t *testing.T) {
	h, g, wo, wb := waiterWhileDraining(t)
	h.cancel(wo) // other's entry has no waiters, nothing stopped yet: dropped
	h.wantState("big", "ready")
	h.granted(wb)
	if h.inst("big").inFlight != 2 {
		t.Fatalf("in_flight %d", h.inst("big").inFlight)
	}
	h.noEntry("big")
	h.noEntry("other")
	_ = g
}

func TestScenario_ReturnedEntryKeepsPlaceInQueue(t *testing.T) {
	// m's pre-flight fails and it is returned; n, created later, must not
	// overtake m for the node both want.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 12000)
	h.llama("n", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.prober.fail["192.0.2.2:50052"] = probeFail("192.0.2.2:50052")
	h.prober.fail["192.0.2.4:50052"] = probeFail("192.0.2.4:50052")
	h.autoProbe = false
	wm := h.request("m")
	h.advance(time.Second)
	wn := h.request("n")
	h.completeProbes() // m returned; a/b quarantined
	h.completeProbes()
	order := h.c.orderedEntries()
	names := []string{}
	for _, e := range order {
		names = append(names, e.prof.Name)
	}
	if !slices.Equal(names, []string{"m", "n"}) {
		t.Fatalf("queue order %v", names)
	}
	_, _ = wm, wn
}
