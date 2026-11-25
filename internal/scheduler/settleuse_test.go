package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// settle_use_mib (B8 F2): what each node gave up one free_average_window
// after ready, taken once and only when the free figure is not moving; null
// with the reason otherwise. The event also says the profile's kind, ctx and
// parallel. Nothing reads it for placement: every plan row is the same with
// and without the reading.
func TestSettleUse_TakenOnceAfterTheWindowWhenNotMoving(t *testing.T) {
	type res struct {
		cp    map[string]any
		ev    map[string]any
		plans map[string]PlanReport
	}
	run := func(t *testing.T, mode string) res {
		h := newHarness(t).nodes(psNode("a", 2, 12400))
		h.llama("m", 8000, func(p *core.Profile) { p.Ctx = 8192 })
		h.llama("other", 3000)
		h.request("m")
		setLoad(h, "m", rpcDev(addrA, 7000, 700, 300))
		h.healthy("m")
		h.view.modify("a", func(n *core.Node) {
			n.FreeMiB -= 8000
			n.FreeAvgMiB, n.FreeAvgKnown = n.FreeMiB, true
			if mode == "moving" {
				n.FreeAvgMiB += 500
			}
		})
		h.viewChanged()
		if mode != "ended" {
			h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
			h.viewChanged()
		}
		if mode == "skip" { // as if no reading had been taken
			in := h.inst("m")
			in.settleUse, in.settleWhy = nil, nil
		}
		h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 600; n.FreeAvgMiB = n.FreeMiB }) // the context grows
		h.viewChanged()
		plans := map[string]PlanReport{}
		for name := range h.cfg.Profiles {
			plans[name] = h.c.dryPlan(name)
		}
		h.step(evUnload{profile: "m", force: true})
		evs := unloadEvents(h)
		if len(evs) != 1 {
			t.Fatalf("%s: %d events", mode, len(evs))
		}
		return res{cyclePeaksOf(t, evs[0])[0], evs[0].Data, plans}
	}
	ok := run(t, "steady")
	if ok.cp["settle_use_mib"] != 8000 || ok.cp["peak_mib"] != 8600 || ok.cp["no_settle"] != nil {
		t.Fatalf("steady: %+v", ok.cp)
	}
	if ok.ev["profile_kind"] != "llama" || ok.ev["profile_ctx"] != 8192 || ok.ev["profile_parallel"] != 1 {
		t.Fatalf("event %+v", ok.ev)
	}
	for mode, why := range map[string]string{"moving": noSettleMoving, "ended": noSettleEnded} {
		r := run(t, mode)
		if v, has := r.cp["settle_use_mib"]; !has || v != nil || r.cp["no_settle"] != why {
			t.Errorf("%s: %+v", mode, r.cp)
		}
	}
	skip := run(t, "skip")
	for name, want := range skip.plans {
		if g := ok.plans[name]; !reflect.DeepEqual(g, want) {
			t.Errorf("%s: plan differs with the settle reading:\n%+v\n%+v", name, g, want)
		}
	}
}
