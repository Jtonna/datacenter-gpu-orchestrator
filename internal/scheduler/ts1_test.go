package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Tenth review: the evictWaits hold is bounded. Load and unload churn on a
// node of the placement keeps moving heldChanged, so its average stays left
// out; without a bound the request would be held until its load timeout. The
// hold ends one window after it began, and the decision is then made at that
// deadline, not at the next event.
func TestNeverFits_TheEvictHoldEndsOneWindowAfterItBegan(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 2, 12400))
	h.cfg.Defaults.PlacementUsesReserved = true
	w := h.cfg.Defaults.FreeAverageWindow
	if w <= 0 {
		t.Fatalf("window %v", w)
	}
	h.llama("v", 8000)
	h.llama("m", 20000) // over both nodes: v must go from one
	wv := h.request("v")
	h.healthy("v")
	h.release(h.granted(wv))
	h.advance(time.Minute) // past v's hold, well inside its TTL
	vn := h.nodesOf("v")[0]
	other := core.NodeID("a")
	if vn == other {
		other = "b"
	}
	// churn on the other node: what is placed there just changed (a small
	// load came and went), and its average still holds samples from while
	// it ran
	churn := func() { h.c.ns(other).heldChanged = h.c.now }
	churn()
	h.view.modify(vn, func(n *core.Node) { n.FreeMiB -= 8000 })
	h.view.modify(other, func(n *core.Node) { n.FreeAvgMiB, n.FreeAvgKnown = 800, true })
	h.viewChanged()
	r := h.c.dryPlan("m")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "average free figure") {
		t.Fatalf("plan %s %q, want the eviction to wait for the average", r.Action, r.Reason)
	}
	wm := h.request("m")
	began := h.clk.Now()
	if h.c.insts["v"].state != stReady {
		t.Fatalf("v evicted while the decision depended on the average: %v", h.c.insts["v"].state)
	}
	// the churn goes on, well inside each window
	for h.clk.Now().Before(began.Add(w - 10*time.Second)) {
		h.runTo(10 * time.Second)
		churn()
		h.viewChanged()
		if v := h.c.insts["v"]; v == nil || v.state != stReady {
			t.Fatalf("v evicted %v after the hold began, before its bound", h.clk.Now().Sub(began))
		}
	}
	h.runTo(began.Add(w).Sub(h.clk.Now()))
	if v := h.c.insts["v"]; v != nil && v.state == stReady {
		t.Fatalf("still held %v after the hold began (window %v): churn holds the request until its load timeout", h.clk.Now().Sub(began), w)
	}
	if a, ok := h.peek(wm); ok && a.err != nil {
		t.Fatalf("refused at the hold's end: %v", a.err)
	}
}
