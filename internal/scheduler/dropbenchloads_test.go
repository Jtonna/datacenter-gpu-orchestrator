package scheduler

import "testing"

// A benchmark run that is cancelled while its POST /api/load still waits in
// the queue (the fleet is busy) must take that load with it: a synthetic
// waiter never disconnects, so without this the model would load later with
// nobody left to unload it. Only the run's own loads go; another caller's
// waiter for the same profile and the entry it keeps stay.
func TestDropBenchLoads_RemovesOnlyTheRunsQueuedLoads(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("busy", 8000)
	h.llama("m", 8000)
	g := h.ready("busy")

	mine := &waiter{profile: "m", synthetic: true, benchRun: "run-1"}
	h.step(evAcquire{w: mine})
	if h.entry("m") == nil {
		t.Fatal("m did not queue")
	}
	if n := h.c.dropBenchLoads("m", "run-1"); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	h.drain()
	h.noEntry("m")
	if n := h.c.dropBenchLoads("m", "run-1"); n != 0 {
		t.Fatalf("twice: %d", n)
	}

	// someone else's load and request for m stay queued with the run's
	other := &waiter{profile: "m", synthetic: true, benchRun: "run-2"}
	h.step(evAcquire{w: other})
	req := h.request("m")
	again := &waiter{profile: "m", synthetic: true, benchRun: "run-1"}
	h.step(evAcquire{w: again})
	if n := h.c.dropBenchLoads("m", "run-1"); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	h.drain()
	e := h.entry("m")
	if e == nil || len(e.waiters) != 2 {
		t.Fatalf("the others' waiters went too: %+v", e)
	}
	h.pending(req)
	// an empty run id never matches (a load without X-Bench-Run is nobody's run)
	if n := h.c.dropBenchLoads("m", ""); n != 0 {
		t.Fatalf("an empty run id dropped %d", n)
	}
	h.release(g)
	h.drain()
}
