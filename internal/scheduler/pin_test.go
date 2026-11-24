package scheduler

import (
	"net/http"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"testing"
)

func (h *harness) loadOn(profile string, nodes ...string) *waiter {
	h.t.Helper()
	w := &waiter{profile: profile, synthetic: true, pin: nodes, reply: make(chan answer, 1)}
	h.step(evAcquire{w: w})
	return w
}

// A load on chosen zones: placed there as force_nodes would, for this
// load only; the profile is unchanged and the instance is not stale.
func TestPin_LoadOnChosenZones(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	h.viewChanged()
	if r := h.c.dryPlanOf("m", pinCopy(h.c.cfg.Profiles["m"], []string{"b"})); r.Action != PlanStart || len(r.Placement) != 1 || r.Placement[0].ID != "b" {
		t.Fatalf("plan %+v", r)
	}
	h.loadOn("m", "b")
	h.healthy("m")
	h.wantNodes("m", "b")
	st := h.c.instStatus(h.inst("m"))
	if st.StaleProfile || len(st.PinnedNodes) != 1 || st.PinnedNodes[0] != "b" {
		t.Fatalf("status %+v", st)
	}
	if len(h.c.cfg.Profiles["m"].ForceNodes) != 0 {
		t.Fatal("the profile was changed")
	}
	// Loaded already: a pinned load is refused (409), a plain request served.
	w := h.loadOn("m", "a")
	if a, ok := h.peek(w); !ok || a.err == nil || a.err.Status != http.StatusConflict {
		t.Fatalf("second pin %+v %v", a, ok)
	}
	h.granted(h.request("m"))
}

// A pin while a plain load of it waits in the queue is refused.
func TestPin_RefusedWhileQueued(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 14000)
	h.llama("m", 8000)
	h.autoExit = false
	h.ready("big")
	h.request("m") // queued behind big
	if h.entry("m") == nil {
		t.Fatal("m not queued")
	}
	w := h.loadOn("m", "a")
	if a, ok := h.peek(w); !ok || a.err == nil || a.err.Status != http.StatusConflict {
		t.Fatalf("pin while queued %+v %v", a, ok)
	}
}

// Once the pinned instance has gone, the copy is forgotten and the next
// load follows the profile again.
func TestPin_ForgottenAfterUnload(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.loadOn("m", "b")
	h.healthy("m")
	h.wantNodes("m", "b")
	h.step(evUnload{profile: "m"})
	h.drain()
	h.step(evTick{})
	if h.c.insts["m"] != nil || len(h.c.pins) != 0 {
		t.Fatalf("inst %v pins %v", h.c.insts["m"], h.c.pins)
	}
	h.load("m")
	h.healthy("m")
	h.wantNodes("m", "a")
}
