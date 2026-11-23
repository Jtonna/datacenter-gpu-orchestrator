package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// The owner's counter: every time a request's only option was a console
// another instance holds, one occupiedOnly event when the case ends, with
// how it ended. Observation only: nothing it does changes a decision.

// occupiedHarness has console a held by "small" (loaded, ready, idle,
// younger than min_hold) and a profile "m" forced onto a (or automatic,
// with b too small for it).
func occupiedHarness(t *testing.T, forced bool) (*harness, *grant) {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 3000))
	h.llama("small", 4000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("m", 9000, func(p *core.Profile) {
		if forced {
			p.ForceNodes = []string{"a"}
		}
	})
	h.viewChanged()
	g := h.ready("small")
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 4000 })
	h.viewChanged()
	return h, g
}

func occupiedEvents(h *harness) []history.Event { return h.of(history.TOccupiedOnly) }

func oneOccupied(t *testing.T, h *harness, outcome string) history.Event {
	t.Helper()
	ev := occupiedEvents(h)
	if len(ev) != 1 {
		t.Fatalf("%d occupiedOnly events, want 1: %+v", len(ev), ev)
	}
	e := ev[0]
	if e.Profile != "m" || e.Severity != history.Info || e.Data["outcome"] != outcome ||
		fmt.Sprint(e.Data["occupied_nodes"]) != "[a]" || fmt.Sprint(e.Data["occupants"]) != "[small]" {
		t.Fatalf("event %+v, want m on a held by small, outcome %s", e, outcome)
	}
	if _, ok := e.Data["began"].(int64); !ok {
		t.Fatalf("began %T %v", e.Data["began"], e.Data["began"])
	}
	return e
}

func TestOccupiedOnly_WaitedForTheOccupantToLeave(t *testing.T) {
	for _, forced := range []bool{true, false} {
		h, g := occupiedHarness(t, forced)
		h.release(g)
		w := h.request("m")
		if h.c.insts["m"] != nil {
			t.Fatalf("forced %v: m started while small holds a", forced)
		}
		for range 3 { // many passes, one case
			h.advance(time.Second)
			h.viewChanged()
		}
		if len(occupiedEvents(h)) != 0 {
			t.Fatalf("forced %v: an event before the case ended", forced)
		}
		h.step(evUnload{profile: "small"}) // the occupant leaves by itself
		h.advance(5 * time.Second)
		h.view.modify("a", func(n *core.Node) { n.FreeMiB = 15000 })
		h.viewChanged()
		h.advance(time.Second)
		if h.c.insts["m"] == nil {
			t.Fatalf("forced %v: m did not start after small left", forced)
		}
		e := oneOccupied(t, h, "waited")
		if ws, _ := e.Data["wait_s"].(float64); ws < 3 {
			t.Fatalf("forced %v: wait_s %v", forced, e.Data["wait_s"])
		}
		if _, ok := e.Data["evicted"]; ok {
			t.Fatalf("forced %v: evicted set without an eviction", forced)
		}
		_ = w
	}
}

func TestOccupiedOnly_StartedAfterAnEviction(t *testing.T) {
	h, g := occupiedHarness(t, true)
	h.release(g)
	h.request("m")
	h.advance(31 * time.Second) // small passes min_hold: it is evicted
	h.viewChanged()
	for range 4 {
		h.advance(2 * time.Second)
		h.view.modify("a", func(n *core.Node) { n.FreeMiB = 15000 })
		h.viewChanged()
	}
	if h.c.insts["m"] == nil {
		t.Fatalf("m did not start after the eviction")
	}
	e := oneOccupied(t, h, "evicted")
	ev, _ := e.Data["evicted"].([]map[string]any)
	if len(ev) != 1 || ev[0]["profile"] != "small" || ev[0]["busy"] != false {
		t.Fatalf("evicted %#v", e.Data["evicted"])
	}
	if idle, _ := ev[0]["idle_s"].(float64); idle < 30 {
		t.Fatalf("idle_s %v", ev[0]["idle_s"])
	}
}

func TestOccupiedOnly_TimedOut(t *testing.T) {
	h, _ := occupiedHarness(t, true)
	h.c.cfg.Defaults.QueueTimeout = 20 * time.Second // before small passes min_hold (30 s): nothing evicts it
	w := h.request("m")
	h.advance(21 * time.Second)
	h.viewChanged()
	if a, ok := h.peek(w); !ok || a.err == nil {
		t.Fatalf("m was not refused at queue_timeout")
	}
	e := oneOccupied(t, h, "timed_out")
	if e.Data["how"] != "queue_timeout" {
		t.Fatalf("how %v", e.Data["how"])
	}
	if ws, _ := e.Data["wait_s"].(float64); ws < 20 {
		t.Fatalf("wait_s %v", e.Data["wait_s"])
	}
}

func TestOccupiedOnly_Cancelled(t *testing.T) {
	h, _ := occupiedHarness(t, true)
	w := h.request("m")
	h.advance(5 * time.Second)
	h.cancel(w)
	oneOccupied(t, h, "cancelled")
}

// Not counted: a request that starts at once, and one that waits for a
// reason other than an occupied console.
func TestOccupiedOnly_NotCountedOtherwise(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 9000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.viewChanged()
	h.release(h.ready("m"))
	if n := len(occupiedEvents(h)); n != 0 {
		t.Fatalf("%d events for a request that started at once", n)
	}
	h = newHarness(t).nodes(ps5("a", 2, 5000))
	h.llama("big", 9000)
	h.viewChanged()
	w := h.request("big") // too big for any node: nothing of ours is in the way
	h.advance(5 * time.Second)
	h.cancel(w)
	if n := len(occupiedEvents(h)); n != 0 {
		t.Fatalf("%d events for a request no occupant was in the way of", n)
	}
}
