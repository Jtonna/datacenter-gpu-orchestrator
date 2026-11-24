package scheduler

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The never-fits table for a load on chosen zones: the live example of
// TestNeverFits_ForcedIsRefusedAtOnce with the zones chosen for the load
// instead of configured, in both membership modes, single node and split:
// refused at once with the plan's own text (the reservation gate, the
// agents' fit against available memory less the watchdog floor).
func TestNeverFits_ChosenZonesRefusedAtOnce(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, split := range []bool{false, true} {
			h := inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
			liveTight(h)
			nodes, need := []string{"a"}, 19266
			if split {
				nodes, need = []string{"a", "b"}, 2*19266
			}
			h.llama("big", 0, func(p *core.Profile) { p.LoadTimeout = time.Hour })
			h.needs["big"] = NeedInfo{Need: placer.Measured(need), FileBytes: 10 << 30}
			h.viewChanged()
			r := h.c.dryPlanOf("big", pinCopy(h.c.cfg.Profiles["big"], nodes))
			if r.Action != PlanReject {
				t.Fatalf("%s split %v: plan %s (%s)", mode, split, r.Action, r.Reason)
			}
			w := h.loadOn("big", nodes...)
			a, ok := h.peek(w)
			if !ok || a.err == nil || a.err.Reason != ReasonForcedDoesNotFit || a.err.Message != r.Reason {
				t.Fatalf("%s split %v: %+v (plan %q)", mode, split, a.err, r.Reason)
			}
			if h.c.entries["big"] != nil || len(h.c.insts) != 0 {
				t.Fatalf("%s split %v: left an entry or instance", mode, split)
			}
			for _, s := range []string{"would take", "short by"} {
				if !strings.Contains(a.err.Message, s) {
					t.Errorf("%s split %v: message lacks %q: %s", mode, split, s, a.err.Message)
				}
			}
		}
	}
}

// A chosen zone that is lost (offline), excluded, paused (also while its
// unpause is pending), set aside or outside the pool is refused at once,
// plan and request alike; nothing is queued or placed.
func TestPin_UnusableZonesRefusedAtOnce(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, kind := range []string{"lost", "excluded", "paused", "unpause-pending", "set-aside", "outside-pool", "unknown"} {
			name := mode + " " + kind
			h := inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
			h.llama("m", 6000)
			zone := "a"
			switch kind {
			case "lost":
				h.view.modify("a", func(n *core.Node) { n.Online = false })
			case "excluded":
				h.cfg.ExcludeNodes = []string{"a"}
			case "paused":
				h.view.modify("a", func(n *core.Node) { n.Paused = true })
			case "unpause-pending":
				h.view.modify("a", func(n *core.Node) { n.Paused, n.UnpausePending = true, true })
				h.view.setUnplaced("a", UnplacedPaused)
			case "outside-pool":
				h.view.setUnplaced("a", UnplacedSilent)
			case "unknown":
				zone = "zz"
			}
			h.viewChanged()
			if kind == "set-aside" {
				h.c.quarantine("a", setAsideErr(SetAsidePreflight, "x", errors.New("preflight failed")))
			}
			r := h.c.dryPlanOf("m", pinCopy(h.c.cfg.Profiles["m"], []string{zone}))
			e := h.c.pinUnusable(h.c.cfg.Profiles["m"], []string{zone})
			if e == nil {
				t.Fatalf("%s: not refused (plan %s %s)", name, r.Action, r.Reason)
			}
			w := h.loadOn("m", zone)
			a, ok := h.peek(w)
			if !ok || a.err == nil || a.err.Status != http.StatusConflict || a.err.Reason != ReasonPinUnusable {
				t.Fatalf("%s: %+v %v", name, a.err, ok)
			}
			if h.c.entries["m"] != nil || h.c.insts["m"] != nil || len(h.c.pins) != 0 {
				t.Fatalf("%s: something was queued or started", name)
			}
		}
	}
}

// One model per console, as for forced profiles: a chosen zone that holds
// another (configured forced) model is never shared; the plan says evict
// (or the load waits), the holder stops first, then the chosen load starts.
func TestPin_NeverSharesAConsole(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("f", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("m", 6000)
	h.autoExit = false
	h.release(h.ready("f"))
	h.advance(61 * time.Second) // past min_hold: f is evictable
	if r := h.c.dryPlanOf("m", pinCopy(h.c.cfg.Profiles["m"], []string{"a"})); r.Action == PlanStart {
		t.Fatalf("plan start on a held console: %+v", r)
	}
	h.loadOn("m", "a")
	if h.c.insts["m"] != nil {
		t.Fatal("m started while f holds a")
	}
	h.wantState("f", "stopping")
	h.exit("f", 0)
	h.advance(5 * time.Second) // a settles
	h.completeFetches()
	h.completeProbes()
	h.completeStarts()
	h.step(evTick{})
	h.wantNodes("m", "a")
	if h.c.insts["f"] != nil {
		t.Fatal("f and m both on a")
	}
}

// Point 10: without the option every plan is the configured profile's,
// over a fleet in several states (loaded, busy, queued, paused, excluded,
// lost, set aside, forced, split), before and after a chosen-zones load of
// another profile has come and gone.
func TestPin_PlansUnchangedWithoutOption(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 5, 15000), ps5("d", 6, 15000),
		ps5("e", 7, 15000), ps5("f", 8, 15000))
	h.llama("small", 3000)
	h.llama("mid", 9000)
	h.llama("big", 20000)
	h.llama("huge", 200000)
	h.llama("forced", 6000, func(p *core.Profile) { p.ForceNodes = []string{"d"} })
	h.llama("pair", 20000, func(p *core.Profile) { p.ForceNodes, p.TensorSplit = []string{"e", "f"}, []int{1, 1} })
	h.llama("other", 6000)
	h.autoExit = false
	h.cfg.ExcludeNodes = []string{"c"}
	h.view.modify("b", func(n *core.Node) { n.Paused = true })
	h.viewChanged()
	h.ready("small") // busy
	h.release(h.ready("forced"))
	all := func() map[string]PlanReport {
		out := map[string]PlanReport{}
		for name := range h.c.cfg.Profiles {
			out[name] = h.c.dryPlan(name)
			if want := h.c.dryPlanOf(name, h.c.cfg.Profiles[name]); !reflect.DeepEqual(out[name], want) {
				t.Fatalf("plan of %s differs from the configured profile's", name)
			}
		}
		return out
	}
	before := all()
	// a chosen-zones load of other comes and goes
	h.loadOn("other", "f")
	h.healthy("other")
	h.step(evUnload{profile: "other"})
	h.exit("other", 0)
	h.advance(5 * time.Second) // f settles
	h.completeFetches()
	h.step(evTick{})
	if len(h.c.pins) != 0 || h.c.insts["other"] != nil {
		t.Fatal("the chosen-zones load left something behind")
	}
	after := all()
	for name, b := range before {
		a := after[name]
		if name == "other" {
			continue // its own load came and went (load times learned)
		}
		if a.Action != b.Action || a.Reason != b.Reason || !reflect.DeepEqual(a.Placement, b.Placement) || !reflect.DeepEqual(a.Victims, b.Victims) {
			t.Errorf("plan of %s changed: %s %q %v -> %s %q %v", name, b.Action, b.Reason, b.Placement, a.Action, a.Reason, a.Placement)
		}
	}
}
