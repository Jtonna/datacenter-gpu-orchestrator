package scheduler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The owner's rule (tight fit): "if we can load the model and make the
// context window fit on one console we can be looser with the fit. however
// if we cant fit the model and the configured context window than we
// cant". The live case: a single-console DeepSeek profile at ctx 2048 will
// take 11,571 MiB against 11,589 available, with an estimated margin of 578
// MiB: it fits by 18 MiB without the margin and starts as a tight fit; the
// same model at 8192 does not fit without eating the headroom and stays
// refused.

// deepSeek is a harness with one console "a" and the estimated profile
// "ds" at ctx (forced onto a, or automatic with max_pool 1), its node's
// free figure set so that the node has spare MiB beyond what ds will take
// (negative: short by that much even without the margin). It returns the
// will_take and margin figures of a's row.
func deepSeek(t *testing.T, mode string, forced bool, ctx, spare int) (h *harness, willTake, margin int) {
	t.Helper()
	h = inMode(newHarness(t).nodes(psNode("a", 2, 30000)), mode)
	liveTight(h)
	h.cfg.Defaults.MaxPool = 1
	h.llama("ds", 0, func(p *core.Profile) {
		p.Ctx, p.LoadTimeout = ctx, time.Hour
		if forced {
			p.ForceNodes = []string{"a"}
		}
	})
	h.needs["ds"] = NeedInfo{Need: placer.Estimated(10500 + ctx/8), FileBytes: 9885 << 20}
	h.viewChanged()
	r := h.c.dryPlan("ds")
	if r.Memory == nil || len(r.Memory.Nodes) != 1 {
		t.Fatalf("no memory rows: %+v", r)
	}
	row := r.Memory.Nodes[0]
	willTake, margin = row.WillTakeMiB, row.MarginMiB
	if margin < 500 {
		t.Fatalf("margin %d, want the estimated rule's at least 500", margin)
	}
	h.view.modify("a", func(n *core.Node) {
		n.FreeMiB = willTake + spare + n.HeadroomMiB
		n.TotalMiB = n.FreeMiB // what it has free is all it can have: the capacity check sees the same
	})
	h.viewChanged()
	return h, willTake, margin
}

func TestTightFit_OnlyTheMarginShortStartsTight(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, forced := range []bool{true, false} {
			name := fmt.Sprintf("%s, forced %v", mode, forced)
			h, willTake, margin := deepSeek(t, mode, forced, 2048, 18)
			r := h.c.dryPlan("ds")
			if r.Action != PlanStart {
				t.Fatalf("%s: plan %s (%s), want start", name, r.Action, r.Reason)
			}
			if !r.TightFit || r.Memory == nil || !r.Memory.TightFit || !r.Memory.Fits {
				t.Fatalf("%s: plan not marked tight: %+v", name, r.Memory)
			}
			row := r.Memory.Nodes[0]
			if !row.Tight || row.SpareMiB != 18 || row.MarginShortMiB != margin-18 {
				t.Fatalf("%s: row tight %v spare %d margin_short %d, want true 18 %d", name, row.Tight, row.SpareMiB, row.MarginShortMiB, margin-18)
			}
			want := fmt.Sprintf("tight fit: 18 MiB spare on a, margin %d MiB not met", margin)
			if !strings.Contains(r.Reason, want) {
				t.Fatalf("%s: plan text %q lacks %q", name, r.Reason, want)
			}
			w := h.request("ds")
			if _, answered := h.peek(w); answered && h.c.insts["ds"] == nil {
				a, _ := h.peek(w)
				t.Fatalf("%s: refused: %v", name, a.err)
			}
			if h.c.insts["ds"] == nil || h.c.entries["ds"] != nil {
				t.Fatalf("%s: did not start (entry %v)", name, h.c.entries["ds"] != nil)
			}
			if mode == "agents" {
				// the launch asks the agent for the floor plus what the load
				// will take: never the margin it was started without
				ts := targets(h.lastLaunch())
				if ts["a"].ExpectMiB != int64(willTake) || ts["a"].MinMemAvailableMiB != int64(willTake+1000) {
					t.Fatalf("%s: expect_mib %d min_mem_available_mib %d, want %d and %d", name, ts["a"].ExpectMiB, ts["a"].MinMemAvailableMiB, willTake, willTake+1000)
				}
				h.launched(WorkerResult{})
			}
			if mode == "fleet" {
				h.advance(time.Second)
			}
			st := h.of(history.TLoadStarted)
			if len(st) == 0 {
				t.Fatalf("%s: no loadStarted event", name)
			}
			tf, ok := st[len(st)-1].Data["tight_fit"].([]map[string]any)
			if !ok || len(tf) != 1 || tf[0]["spare_mib"] != 18 || tf[0]["margin_mib"] != margin {
				t.Fatalf("%s: loadStarted tight_fit %#v", name, st[len(st)-1].Data["tight_fit"])
			}
		}
	}
}

// A load that meets its margin is not tight and asks the agent for the
// margin as before.
func TestTightFit_MarginMetIsNotTight(t *testing.T) {
	h, willTake, margin := deepSeek(t, "agents", true, 2048, 900)
	r := h.c.dryPlan("ds")
	if r.Action != PlanStart || r.TightFit || r.Memory.TightFit || r.Memory.Nodes[0].Tight || strings.Contains(r.Reason, "tight") {
		t.Fatalf("plan %s tight %v (%s)", r.Action, r.TightFit, r.Reason)
	}
	h.request("ds")
	if ts := targets(h.lastLaunch()); ts["a"].ExpectMiB != int64(willTake+margin) {
		t.Fatalf("expect_mib %d, want %d (will take + margin, as before)", ts["a"].ExpectMiB, willTake+margin)
	}
}

// The headroom stays hard: short without the margin is refused at once,
// with the shortfall against available, not the margin.
func TestTightFit_ShortEvenWithoutTheMarginIsRefused(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, forced := range []bool{true, false} {
			name := fmt.Sprintf("%s, forced %v", mode, forced)
			h, _, _ := deepSeek(t, mode, forced, 8192, -643)
			r := h.c.dryPlan("ds")
			if r.Action != PlanReject {
				t.Fatalf("%s: plan %s (%s), want reject", name, r.Action, r.Reason)
			}
			want := "short by 643 MiB on a even without the margin"
			if !forced {
				want = "short by 643 MiB even without the margin" // the capacity check: the cluster, not a node
			}
			if !strings.Contains(r.Reason, want) || strings.Contains(r.Reason, "margin_estimated") {
				t.Fatalf("%s: the reason does not name the shortfall without the margin: %s", name, r.Reason)
			}
			w := h.request("ds")
			if a, ok := h.peek(w); !ok || a.err == nil {
				t.Fatalf("%s: not refused at once", name)
			}
			if h.c.insts["ds"] != nil || h.c.entries["ds"] != nil {
				t.Fatalf("%s: started or queued", name)
			}
		}
	}
}

// margin_refuses brings the old rule back: the margin refuses again.
func TestTightFit_MarginRefusesRestoresTheOldRule(t *testing.T) {
	for _, forced := range []bool{true, false} {
		h, _, _ := deepSeek(t, "agents", forced, 2048, 18)
		cfg := cloneCfg(h.c.cfg)
		cfg.Defaults.MarginRefuses = true
		h.step(evConfig{cfg})
		r := h.c.dryPlan("ds")
		if r.Action != PlanReject || r.TightFit {
			t.Fatalf("forced %v: plan %s tight %v (%s), want reject", forced, r.Action, r.TightFit, r.Reason)
		}
		w := h.request("ds")
		if a, ok := h.peek(w); !ok || a.err == nil {
			t.Fatalf("forced %v: not refused at once", forced)
		}
	}
}

// Automatic placement prefers a placement that meets the margin (here a
// split over two consoles) to a tight one (one console).
func TestTightFit_AutomaticPrefersMetMargins(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		h := inMode(newHarness(t).nodes(psNode("a", 2, 30000), psNode("b", 4, 9000), psNode("c", 6, 9000)), mode)
		liveTight(h)
		h.cfg.Defaults.MaxPool = 2
		h.llama("m", 0, func(p *core.Profile) { p.LoadTimeout = time.Hour })
		h.needs["m"] = NeedInfo{Need: placer.Measured(10000), FileBytes: 8 << 30}
		h.viewChanged()
		pl, err := placer.Place(func() placer.Request { r, _, _ := h.c.request(h.c.cfg.Profiles["m"]); return r }(), h.c.freeCandidates(h.c.cfg.Profiles["m"]))
		if err != nil || len(pl.Nodes) != 1 {
			t.Fatalf("%s: setup: the loose placer should take one node: %v %v", mode, pl, err)
		}
		rows, _ := h.c.memoryRows(h.c.cfg.Profiles["m"], pl, map[string]bool{"m": true})
		h.view.modify(pl.Nodes[0].ID, func(n *core.Node) { n.FreeMiB = rows[0].WillTakeMiB + 50 + n.HeadroomMiB })
		h.viewChanged()
		r := h.c.dryPlan("m")
		if r.Action != PlanStart || r.TightFit || len(r.Placement) != 2 {
			t.Fatalf("%s: plan %s tight %v on %v (%s), want a margin-met split", mode, r.Action, r.TightFit, r.Placement, r.Reason)
		}
		h.request("m")
		if in := h.c.insts["m"]; in == nil || len(in.placement.Nodes) != 2 {
			t.Fatalf("%s: the request did not start the split", mode)
		}
	}
}

// A tight fit on a free node beats an eviction that would meet the margin.
func TestTightFit_NoEvictionToMeetTheMargin(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		h := inMode(newHarness(t).nodes(psNode("a", 2, 30000), psNode("b", 4, 30000)), mode)
		liveTight(h)
		h.cfg.Defaults.MaxPool = 1
		h.llama("small", 3000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
		h.llama("m", 0, func(p *core.Profile) { p.LoadTimeout = time.Hour })
		h.needs["m"] = NeedInfo{Need: placer.Measured(10000), FileBytes: 8 << 30}
		h.viewChanged()
		loadSmall(h, mode)
		p := h.c.cfg.Profiles["m"]
		pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a"}}}
		rows, _ := h.c.memoryRows(p, pl, map[string]bool{"m": true})
		h.view.modify("a", func(n *core.Node) { n.FreeMiB = rows[0].WillTakeMiB + 50 + n.HeadroomMiB })
		h.view.modify("b", func(n *core.Node) { n.FreeMiB = 16000 })
		h.viewChanged()
		h.advance(time.Minute) // small is past min_hold and idle
		r := h.c.dryPlan("m")
		if r.Action != PlanStart || !r.TightFit || len(r.Victims) != 0 {
			t.Fatalf("%s: plan %s tight %v victims %v (%s), want a tight start on a", mode, r.Action, r.TightFit, r.Victims, r.Reason)
		}
		// with the old rule the same situation evicts small: the eviction
		// was there to be had (fleet membership: in agents membership
		// small is still loading, so nothing is evictable)
		if mode != "fleet" {
			continue
		}
		cfg := cloneCfg(h.c.cfg)
		cfg.Defaults.MarginRefuses = true
		h.step(evConfig{cfg})
		if r := h.c.dryPlan("m"); r.Action != PlanEvict {
			t.Fatalf("%s: with margin_refuses the plan is %s (%s), want evict", mode, r.Action, r.Reason)
		}
	}
}
