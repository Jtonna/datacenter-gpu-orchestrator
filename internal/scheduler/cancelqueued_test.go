package scheduler

import (
	"net/http"
	"testing"
)

// cancelQueued (models slice 3, delete with interrupt): every request
// waiting in the queue for one of the named profiles is answered 503
// unloaded, as a forced unload answers a loading instance's waiters, and its
// entry goes with its claim. Other profiles' entries and instances stay.
func TestCancelQueued_AnswersWaitersUnloadedAndDropsTheEntry(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("busy", 8000)
	h.llama("m", 8000)
	h.llama("other", 100)
	g := h.ready("busy")
	w1, w2 := h.request("m"), h.request("m")
	h.pending(w1)
	h.pending(w2)
	if h.entry("m") == nil {
		t.Fatal("m did not queue")
	}
	if n := h.c.cancelQueued([]string{"m", "nope"}); n != 2 {
		t.Fatalf("answered %d, want 2", n)
	}
	h.drain()
	for _, w := range []*waiter{w1, w2} {
		if e := h.failed(w, http.StatusServiceUnavailable, ReasonUnloaded); e.Message == "" {
			t.Fatal("no message")
		}
	}
	h.noEntry("m")
	if n := h.c.cancelQueued([]string{"m"}); n != 0 {
		t.Fatalf("twice: %d", n)
	}
	// busy is untouched: it serves on, and is not stopped for m any more
	h.release(g)
	h.drain()
	if st := h.state("busy"); st != "ready" {
		t.Fatalf("busy is %s", st)
	}
}
