package scheduler

import (
	"reflect"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The flip list of slice B1: stage and display_name change no plan. Every
// profile's plan (automatic and on each chosen node) is identical, field by
// field, with every profile marked and with none, at each point of the same
// run; a loaded instance whose marks change is not stale.
func TestStage_FlipListMarksChangeNoPlan(t *testing.T) {
	run := func(t *testing.T, marked bool) []map[string]PlanReport {
		h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 5, 15000), laptop("l", 7000))
		mark := func(stage, name string) func(*core.Profile) {
			return func(p *core.Profile) {
				if marked {
					p.Stage, p.DisplayName = stage, name
				}
			}
		}
		h.llama("m", 8000, mark("production", "Model M"))
		h.llama("big", 0, func(p *core.Profile) { p.Ctx = 32768 }, mark("test", "Big"))
		h.needs["big"] = NeedInfo{Need: placer.Estimated(26000), FileBytes: 1 << 34, DeviceMiB: 300}
		h.llama("onb", 6000, func(p *core.Profile) { p.ForceNodes = []string{"b"} }, mark("production", "On B"))
		h.llama("bench-m-auto-4k", 7000, mark("test", ""))
		h.llama("never", 90000)
		h.viewChanged()
		plans := func() map[string]PlanReport {
			out := map[string]PlanReport{}
			for name, p := range h.c.cfg.Profiles {
				out[name] = h.c.dryPlan(name)
				for _, n := range []string{"a", "b", "c", "l"} {
					if h.c.pinConflict(p) == nil {
						out[name+" on "+n] = h.c.dryPlanOf(name, pinCopy(p, []string{n}))
					}
				}
			}
			return out
		}
		var out []map[string]PlanReport
		out = append(out, plans())
		g := h.ready("m")
		out = append(out, plans())
		// the marks change while m is loaded: not stale, nothing replans
		nc := cloneCfg(h.cfg)
		nc.Profiles["m"].Stage, nc.Profiles["m"].DisplayName = "test", "Renamed"
		nc.Revision = 2
		h.step(evConfig{cfg: nc})
		h.cfg = nc
		if h.c.instStatus(h.inst("m")).StaleProfile {
			t.Fatal("a loaded instance is stale after only its marks changed")
		}
		out = append(out, plans())
		h.release(g)
		h.request("onb")
		h.healthy("onb")
		out = append(out, plans())
		return out
	}
	without, with := run(t, false), run(t, true)
	rows := 0
	for i := range without {
		if len(with[i]) != len(without[i]) {
			t.Fatalf("point %d: %d and %d rows", i, len(without[i]), len(with[i]))
		}
		for name, a := range without[i] {
			if b := with[i][name]; !reflect.DeepEqual(a, b) {
				t.Fatalf("point %d, %s:\nunmarked %+v\nmarked   %+v", i, name, a, b)
			}
			rows++
		}
	}
	if rows < 4*5 {
		t.Fatalf("only %d rows", rows)
	}
}
