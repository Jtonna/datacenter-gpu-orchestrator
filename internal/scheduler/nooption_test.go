package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Loads, plans and statuses that do not use a chosen-zones load are as
// before: no pinned copy is made, the instance runs the configured profile
// (the same pointer), the plan is the configured profile's, and the status
// carries none of the new fields. A configured force_nodes profile too.
func TestPin_NoOptionChangesNothing(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000)
	h.llama("f", 6000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
	h.autoExit = false
	h.viewChanged()
	for _, name := range []string{"m", "f"} {
		if got, want := h.c.dryPlan(name), h.c.dryPlanOf(name, h.c.cfg.Profiles[name]); !reflect.DeepEqual(got, want) {
			t.Fatalf("plan of %s\n got %+v\nwant %+v", name, got, want)
		}
	}
	h.ready("m")
	h.load("f")
	h.healthy("f")
	h.wantNodes("f", "b")
	if len(h.c.pins) != 0 {
		t.Fatalf("pins %v", h.c.pins)
	}
	for _, name := range []string{"m", "f"} {
		in := h.inst(name)
		if in.prof != h.c.cfg.Profiles[name] {
			t.Fatalf("%s runs a copy of its profile", name)
		}
		st := h.c.instStatus(in)
		if st.PinnedNodes != nil || st.Drain || st.DrainEtaS != nil || st.StaleProfile {
			t.Fatalf("%s status %+v", name, st)
		}
	}
}

// Without a drain request the eviction drain is as before: a busy victim
// drains, returns to ready when the claim drops, and a ready model whose
// runs end stays loaded; a victim whose runs end stops for its claimant.
func TestDrain_NoOptionChangesNothing(t *testing.T) {
	h := twoBig(t)
	h.autoExit = false
	g := h.ready("big")
	w := h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	if in := h.inst("big"); in.drain || in.victimOf != "other" {
		t.Fatalf("victim %+v", in)
	}
	h.cancel(w) // the claim drops: the victim admits again
	h.wantState("big", "ready")
	h.release(g)
	h.wantState("big", "ready") // runs ended, nothing asked it to go
	if st := h.c.instStatus(h.inst("big")); st.Drain || st.DrainEtaS != nil {
		t.Fatalf("status %+v", st)
	}
	g = h.granted(h.request("big"))
	h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	h.release(g)
	h.wantState("big", "stopping")
	if r := h.inst("big").stopReason; r != "evicted for other" {
		t.Fatalf("stop reason %q", r)
	}
}
