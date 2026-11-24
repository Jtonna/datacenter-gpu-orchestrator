package scheduler

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// Drain: the instance admits nothing new, its runs finish, then it stops;
// a request arriving meanwhile queues and loads it again afterwards.
func TestDrain_WaitsForRunsThenStops(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	g1 := h.ready("m")
	g2 := h.granted(h.request("m"))
	h.advance(10 * time.Second)
	// No run of m has finished yet: the ETA is unknown.
	if eta, err := h.c.drain("m"); err != nil || eta != nil {
		t.Fatalf("drain: %v %v", eta, err)
	}
	h.wantState("m", "draining")
	if st := h.c.instStatus(h.inst("m")); !st.Drain || st.DrainEtaS != nil {
		t.Fatalf("status %+v", st)
	}
	w := h.request("m")
	h.pending(w) // queues, not admitted
	if h.entry("m") == nil {
		t.Fatal("no queue entry")
	}
	h.release(g1)
	h.wantState("m", "draining")
	h.release(g2)
	h.wantState("m", "stopping")
	if _, err := h.c.drain("nope"); !errors.Is(err, ErrNotLoaded) {
		t.Fatal(err)
	}
}

// The ETA is the longest remaining time of the runs in flight, from the
// average length of the profile's finished runs.
func TestDrain_ETAFromRunLengths(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	g := h.ready("m")
	h.advance(40 * time.Second)
	h.release(g)
	g = h.granted(h.request("m"))
	h.advance(10 * time.Second)
	eta, err := h.c.drain("m")
	if err != nil || eta == nil || *eta < 29 || *eta > 31 {
		t.Fatalf("eta %v %v", eta, err)
	}
	h.advance(50 * time.Second) // past the average: at least 0
	if st := h.c.instStatus(h.inst("m")); st.DrainEtaS == nil || *st.DrainEtaS != 0 {
		t.Fatalf("eta past the average %v", st.DrainEtaS)
	}
	h.release(g)
	h.wantState("m", "stopping")
}

// Idle: it stops at once. Loading: refused (force-unload cancels a load).
// Cancel: it admits again, and the queued request is admitted.
func TestDrain_IdleLoadingAndCancel(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000)
	h.llama("n", 8000)
	h.autoExit = false
	g := h.ready("m")
	h.release(g)
	if _, err := h.c.drain("m"); err != nil {
		t.Fatal(err)
	}
	h.wantState("m", "stopping")

	h.request("n") // loading
	if _, err := h.c.drain("n"); !errors.Is(err, ErrBusy) {
		t.Fatalf("drain loading: %v", err)
	}
	h.healthy("n")
	if _, err := h.c.drain("n"); err != nil {
		t.Fatal(err)
	}
	h.wantState("n", "draining")
	w := h.request("n")
	h.pending(w)
	if err := h.c.undrain("n"); err != nil {
		t.Fatal(err)
	}
	h.step(evTick{})
	h.wantState("n", "ready")
	h.granted(w)
	if err := h.c.undrain("n"); !errors.Is(err, ErrNotDraining) {
		t.Fatalf("undrain twice: %v", err)
	}
}

// A shutdown (Potluck stopping or restarting) mid-drain is an ordinary
// shutdown: the queued request is answered 503, the run in flight may
// finish within shutdown_grace, then everything stops; nothing of the
// drain is kept (after a restart nothing is loaded and nothing drains).
func TestDrain_ShutdownMidDrain(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	g := h.ready("m")
	if _, err := h.c.drain("m"); err != nil {
		t.Fatal(err)
	}
	w := h.request("m")
	h.step(evShutdown{})
	h.failed(w, http.StatusServiceUnavailable, ReasonShuttingDown)
	h.wantState("m", "draining") // its run goes on within the grace
	h.release(g)
	h.wantState("m", "stopping")
}
