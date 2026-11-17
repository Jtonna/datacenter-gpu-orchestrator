package scheduler

import (
	"net/http"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
)

// Design 8.2: the request path.

func TestStep2_Backoff_FailsFastWith503(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	w := h.request("m")
	h.exit("m", 1) // load fails → backoff 60 s
	h.failed(w, http.StatusBadGateway, ReasonLoadFailed)

	w2 := h.request("m")
	e := h.failed(w2, http.StatusServiceUnavailable, ReasonBackoff)
	if e.RetryAfter <= 0 || e.RetryAfter > 60*time.Second {
		t.Fatalf("Retry-After = %v, want (0, 60s]", e.RetryAfter)
	}
	h.noEntry("m")
	h.wantState("m", "absent")
}

func TestStep3_Ready_AdmitsAndCountsInFlight(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	g1 := h.ready("m")
	w := h.request("m")
	g2 := h.granted(w)
	if g1.Addr != "127.0.0.1:5801" || g2.Addr != g1.Addr || g2.Profile != "m" {
		t.Fatalf("grants %+v %+v", g1, g2)
	}
	if got := h.inst("m").inFlight; got != 2 {
		t.Fatalf("in_flight = %d, want 2", got)
	}
	h.release(g1)
	h.release(g2)
	if got := h.inst("m").inFlight; got != 0 {
		t.Fatalf("in_flight = %d, want 0", got)
	}
}

func TestStep3_Starting_WaitsThenAdmittedOnReady(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	w1 := h.request("m")
	h.wantState("m", "starting")
	w2 := h.request("m")
	h.pending(w1)
	h.pending(w2)
	h.noEntry("m")
	h.healthy("m")
	h.granted(w1)
	h.granted(w2)
	if got := h.inst("m").inFlight; got != 2 {
		t.Fatalf("in_flight = %d, want 2", got)
	}
}

func TestStep3_Draining_JoinsQueueEntry(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 12000)
	h.llama("other", 12000)
	g := h.ready("big")
	h.advance(61 * time.Second) // past hold; entry for other must wait drain_after while big is busy
	wo := h.request("other")
	h.advance(61 * time.Second) // drain_after passes: big drains
	h.wantState("big", "draining")
	w := h.request("big")
	h.pending(w)
	e := h.entry("big")
	if e.claimed() || len(e.waiters) != 1 {
		t.Fatalf("big entry claimed=%v waiters=%d", e.claimed(), len(e.waiters))
	}
	if h.inst("big").inFlight != 1 {
		t.Fatalf("draining instance admitted a request")
	}
	_ = g
	_ = wo
}

func TestStep3_Stopping_JoinsQueueEntry(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	g := h.ready("m")
	h.release(g)
	reply := make(chan error, 1)
	h.step(evUnload{profile: "m", reply: reply})
	if err := <-reply; err != nil {
		t.Fatal(err)
	}
	h.wantState("m", "stopping")
	w := h.request("m")
	h.pending(w)
	h.entry("m")
	// The entry is skipped until the old instance is gone, then placed.
	h.advance(time.Minute)
	h.wantState("m", "stopping")
	h.exitChild(h.inst("m").iid, instanceExit(0))
	h.step(evTick{})
	h.advance(5 * time.Second) // settle
	h.wantState("m", "starting")
	h.noEntry("m")
}

func TestStep3_NoInstance_CreatesEntryWithCurrentTime(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 12000)
	h.llama("m", 12000)
	h.ready("big") // holds a; m cannot fit and big is within hold
	h.advance(10 * time.Second)
	w := h.request("m")
	h.pending(w)
	e := h.entry("m")
	if !e.created.Equal(h.clk.Now()) || e.claimed() {
		t.Fatalf("entry created %v claimed %v, want %v unclaimed", e.created, e.claimed(), h.clk.Now())
	}
}

func TestStep4_OneEntryPerProfile_SharedByWaiters(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 12000)
	h.llama("m", 12000)
	h.ready("big")
	w1 := h.request("m")
	h.advance(time.Second)
	w2 := h.request("m")
	e := h.entry("m")
	if len(h.c.entries) != 1 || len(e.waiters) != 2 || !e.created.Equal(t0) {
		t.Fatalf("entries=%d waiters=%d created=%v", len(h.c.entries), len(e.waiters), e.created)
	}
	h.pending(w1)
	h.pending(w2)
}

func TestStep5_ClientGoneFromEntry_RemovedAndEntryDeleted(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 12000)
	h.llama("m", 12000)
	h.ready("big")
	w1 := h.request("m")
	w2 := h.request("m")
	h.cancel(w1)
	h.failed(w1, StatusClientClosed, ReasonClientGone)
	if len(h.entry("m").waiters) != 1 {
		t.Fatalf("waiter not removed")
	}
	h.cancel(w2)
	h.noEntry("m")
}

func TestStep5_ClientGoneFromStarting_InstanceCarriesOnAndUnloadsByTTL(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	w := h.request("m")
	h.cancel(w)
	h.failed(w, StatusClientClosed, ReasonClientGone)
	h.wantState("m", "starting")
	h.healthy("m")
	h.wantState("m", "ready")
	if h.inst("m").inFlight != 0 {
		t.Fatalf("in_flight != 0")
	}
	h.advance(299 * time.Second)
	h.wantState("m", "ready")
	h.autoExit = false
	h.advance(2 * time.Second)
	h.wantState("m", "stopping")
}

func TestStep6_LoadWaiter_RemovedAtReady(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.load("m")
	h.wantState("m", "starting")
	if n := len(h.inst("m").waiters); n != 1 {
		t.Fatalf("waiters = %d, want the synthetic one", n)
	}
	h.healthy("m")
	if h.inst("m").inFlight != 0 || len(h.c.waiters) != 0 {
		t.Fatalf("synthetic waiter was admitted or kept: in_flight %d waiters %d", h.inst("m").inFlight, len(h.c.waiters))
	}
}

func TestStep6_LoadWaiter_RemovedWhenEntryAnsweredWithError(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.ForceNodes = []string{"ghost"} })
	h.load("m")
	h.entry("m")
	h.advance(601 * time.Second) // queue_timeout: forced nodes unavailable
	h.noEntry("m")
	if len(h.c.waiters) != 0 {
		t.Fatalf("synthetic waiter still pending: %d", len(h.c.waiters))
	}
}

func instanceExit(code int) instance.ExitStatus { return instance.ExitStatus{Code: code} }
