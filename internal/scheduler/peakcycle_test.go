package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// Per-cycle peaks, collected only (peakcycle.go).

func unloadEvents(h *harness) []history.Event {
	var out []history.Event
	for _, e := range h.histLog {
		if e.Type == history.TInstanceUnloaded {
			out = append(out, e)
		}
	}
	return out
}

func cyclePeaksOf(t *testing.T, e history.Event) []map[string]any {
	t.Helper()
	ps, ok := e.Data["cycle_peaks"].([]map[string]any)
	if !ok {
		t.Fatalf("no cycle_peaks in %+v", e.Data)
	}
	return ps
}

// Every cycle's peak goes into its instanceUnloaded event, with what the
// plan reserved on the node and whether the cycle ended normally, with the
// reserve on and off alike: the event keeps the spread the stored peak
// loses (a normal cycle replaces it).
func TestPeakCycles_UnloadEventCarriesEveryCyclePeak(t *testing.T) {
	for _, reserve := range []bool{false, true} {
		h := newHarness(t).nodes(psNode("a", 2, 12400))
		if reserve {
			h.cfg.Defaults.FillReserveKiBPerToken = 8
		}
		p := h.llama("m", 8000, func(p *core.Profile) { p.Ctx = 65536 }) // need 8000 + fill 512
		cycle(h, 9500, true)
		cycle(h, 8900, false)
		cycle(h, 8700, true)
		evs := unloadEvents(h)
		if len(evs) != 3 {
			t.Fatalf("reserve %v: %d unload events", reserve, len(evs))
		}
		for i, want := range []struct {
			peak   int
			normal bool
		}{{9500, true}, {8900, false}, {8700, true}} {
			e := evs[i]
			if e.Data["shape"] != shapeOf(p) || e.Data["ended_normally"] != want.normal {
				t.Fatalf("reserve %v cycle %d: %+v", reserve, i, e.Data)
			}
			ps := cyclePeaksOf(t, e)
			if len(ps) != 1 || ps[0]["node_id"] != "a" || ps[0]["node"] != "a" || ps[0]["peak_mib"] != want.peak || ps[0]["no_peak"] != nil {
				t.Fatalf("reserve %v cycle %d: %+v", reserve, i, ps)
			}
		}
		first := cyclePeaksOf(t, evs[0])[0]
		wantShare, wantReserved := 8000, 8000
		if reserve {
			wantReserved = 8512
		}
		if first["share_mib"] != wantShare || first["reserved_mib"] != wantReserved || first["basis"] != BasisMeasured {
			t.Fatalf("reserve %v: plan figures %+v", reserve, first)
		}
		// With the reserve on, the second cycle was planned on the stored
		// peak of the first.
		second := cyclePeaksOf(t, evs[1])[0]
		if reserve && (second["reserved_mib"] != 9500 || second["basis"] != BasisPeak) {
			t.Fatalf("second cycle planned %+v, want the stored peak 9500", second)
		}
		if !reserve && second["reserved_mib"] != 8000 {
			t.Fatalf("second cycle planned %+v, want 8000", second)
		}
	}
}

// A node without a free-memory reading in the cycle says so instead of 0.
func TestPeakCycles_NoReadingSaysSo(t *testing.T) {
	for _, atStart := range []bool{true, false} {
		h := newHarness(t).nodes(psNode("a", 2, 12400))
		h.llama("m", 8000)
		h.request("m")
		if atStart {
			delete(h.inst("m").baseFree, "a")
		} else {
			h.view.modify("a", func(n *core.Node) { n.FreeUnknown = true })
			h.viewChanged()
		}
		setLoad(h, "m", rpcDev(addrA, 7000, 700, 300))
		h.healthy("m")
		h.viewChanged()
		h.step(evUnload{profile: "m", force: true})
		evs := unloadEvents(h)
		if len(evs) != 1 {
			t.Fatalf("%d unload events", len(evs))
		}
		ps := cyclePeaksOf(t, evs[0])
		want := noPeakLoaded
		if atStart {
			want = noPeakAtStart
		}
		if v, ok := ps[0]["peak_mib"]; !ok || v != nil || ps[0]["no_peak"] != want {
			t.Fatalf("at start %v: %+v", atStart, ps)
		}
	}
}

// The flip list: nothing reads the per-cycle figures for placement. The
// same cycles run twice, once with the cycle's peaks recorded and once with
// none (no reading reached the instance); every plan row of every profile
// is identical while the instance is loaded, and with the reserve off (the
// figures are then collected and nothing is stored) after every cycle too.
// With the reserve on a recorded cycle peak becomes the stored peak when the
// cycle ends (reserve.go, unchanged and tested there); the comparison then
// stops at the cycle's end.
func TestPeakCycles_FlipListByEveryRow(t *testing.T) {
	type snap struct {
		loaded []map[string]PlanReport // while loaded, each cycle
		after  map[string]PlanReport
		events []history.Event
	}
	run := func(t *testing.T, reserve, record bool) snap {
		h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400))
		if reserve {
			reserveOnSettings(h)
		}
		h.llama("m", 8000, func(p *core.Profile) { p.Ctx = 65536; p.ForceNodes = []string{"a"} })
		h.llama("other", 0, func(p *core.Profile) { p.ForceNodes, p.TTL = []string{"a", "b"}, ptrDur(0) })
		h.needs["other"] = NeedInfo{Need: placer.Estimated(6000), FileBytes: 1 << 31, DeviceMiB: 100}
		h.llama("free", 0, func(p *core.Profile) { p.TTL = ptrDur(0) })
		h.needs["free"] = NeedInfo{Need: placer.Estimated(5000), FileBytes: 1 << 31, DeviceMiB: 100}
		h.llama("onb", 3000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
		h.viewChanged()
		plans := func() map[string]PlanReport {
			out := map[string]PlanReport{}
			for name := range h.cfg.Profiles {
				out[name] = h.c.dryPlan(name)
			}
			return out
		}
		var s snap
		for i, gave := range []int{9500, 8900, 8700} {
			h.request("m")
			setLoad(h, "m", rpcDev(addrA, 7000, 700, 300))
			h.healthy("m")
			h.view.modify("a", func(n *core.Node) { n.FreeMiB -= gave })
			h.viewChanged()
			if !record {
				h.inst("m").cyclePeak = nil // as if no reading had reached it
			}
			s.loaded = append(s.loaded, plans())
			if !record {
				// and none reaches it before the cycle ends (taken after
				// the plans: the base also feeds the node's measured use)
				delete(h.inst("m").baseFree, "a")
			}
			if i == 1 {
				in := h.inst("m")
				h.exitChild(in.iid, instance.ExitStatus{Code: 1})
				h.step(evViewChanged{})
			} else {
				h.step(evUnload{profile: "m", force: true})
			}
			h.advance(time.Minute)
			h.view.modify("a", func(n *core.Node) { n.FreeMiB += gave })
			h.viewChanged()
		}
		s.after = plans()
		s.events = unloadEvents(h)
		return s
	}
	for _, reserve := range []bool{false, true} {
		with, without := run(t, reserve, true), run(t, reserve, false)
		if len(with.events) != 3 || len(without.events) != 3 {
			t.Fatalf("reserve %v: events %d and %d", reserve, len(with.events), len(without.events))
		}
		for i := range with.events {
			w, wo := cyclePeaksOf(t, with.events[i])[0], cyclePeaksOf(t, without.events[i])[0]
			if w["peak_mib"] == nil || wo["peak_mib"] != nil {
				t.Fatalf("reserve %v cycle %d: recorded %+v, not recorded %+v", reserve, i, w, wo)
			}
		}
		for i := range without.loaded {
			if reserve && i > 0 {
				break // planned on the stored peak of the cycle before (reserve.go)
			}
			for name, want := range without.loaded[i] {
				if g := with.loaded[i][name]; !reflect.DeepEqual(g, want) {
					t.Errorf("reserve %v cycle %d: %s while loaded differs with cycle peaks recorded:\n%+v\n%+v", reserve, i, name, g, want)
				}
			}
		}
		if reserve {
			continue
		}
		for name, want := range without.after {
			if g := with.after[name]; !reflect.DeepEqual(g, want) {
				t.Errorf("%s after the cycles differs with cycle peaks recorded:\n%+v\n%+v", name, g, want)
			}
		}
	}
}
