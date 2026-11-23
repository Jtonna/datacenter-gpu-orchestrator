package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The 0.1.7 readiness crash: exclude_nodes written, then the tight
// settings, then every profile planned (GET /api/profiles) with no node
// left to place on. CheckCapacity divided by the number of candidate nodes
// in its configured-margin branch. Now every kind of profile is planned and
// requested without a panic, and none starts:
//   - an automatic profile (measured and estimated need), and one whose
//     allow_tiers admit only excluded nodes: refused, capacity 0 or the
//     tiers' exclusion;
//   - a profile forcing an excluded node, and a command profile on the
//     excluded local node: refused by the exclusion;
//   - a profile forcing a node not in the view: never started.
func TestNoNodeLeft_EveryProfileKindIsPlannedWithoutAPanic(t *testing.T) {
	views := map[string]func() []core.Node{
		"only the local node": func() []core.Node { return []core.Node{hostNode(6000)} },
		"local and consoles": func() []core.Node {
			return []core.Node{hostNode(6000), ps5("a", 2, 15000), ps5("b", 4, 15000)}
		},
	}
	for vn, view := range views {
		for _, mode := range []string{"fleet", "agents"} {
			name := vn + "/" + mode
			h := inMode(newHarness(t).nodes(view()...), mode)
			h.llama("auto", 7000)
			h.llama("est", 0)
			h.needs["est"] = NeedInfo{Need: placer.Estimated(7000), FileBytes: 4 << 30}
			h.llama("tier", 7000, func(p *core.Profile) { p.AllowTiers = []int{1} })
			h.llama("fx", 7000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
			h.llama("fm", 7000, func(p *core.Profile) { p.ForceNodes = []string{"ghost"} })
			h.command("cmd", 2000)
			h.viewChanged()

			nc := cloneCfg(h.c.cfg)
			nc.ExcludeNodes = []string{"host-1", "a", "b"}
			h.step(evConfig{cfg: nc})
			plans := func(stage string) map[string]PlanReport {
				out := map[string]PlanReport{}
				for _, p := range []string{"auto", "est", "tier", "fx", "fm", "cmd"} {
					func() {
						defer func() {
							if r := recover(); r != nil {
								t.Fatalf("%s, %s: planning %s panicked: %v", name, stage, p, r)
							}
						}()
						out[p] = h.c.dryPlan(p)
					}()
				}
				return out
			}
			plans("excluded")
			tight := cloneCfg(h.c.cfg)
			liveTight(&harness{cfg: tight})
			h.step(evConfig{cfg: tight})
			ps := plans("excluded, then tight")
			for _, p := range []string{"auto", "est", "tier", "fm"} {
				t.Logf("%s: %s: %s %q", name, p, ps[p].Action, ps[p].Reason)
			}
			for _, p := range []string{"auto", "est", "tier", "fx", "cmd"} {
				if ps[p].Action != PlanReject {
					t.Errorf("%s: %s plan %s %q, want a refusal", name, p, ps[p].Action, ps[p].Reason)
				}
			}
			for _, p := range []string{"fx", "cmd"} {
				if ps[p].ExcludedBy == "" || !strings.Contains(ps[p].Reason, "excluded by config") {
					t.Errorf("%s: %s refused for %q, not by the exclusion", name, p, ps[p].Reason)
				}
			}
			if a := ps["fm"].Action; a == PlanStart || a == PlanEvict {
				t.Errorf("%s: fm plan %s for a node not in the view", name, a)
			}
			for _, p := range []string{"auto", "est", "tier", "fx", "fm", "cmd"} {
				w := h.request(p)
				if h.c.insts[p] != nil {
					t.Errorf("%s: %s started with every node excluded", name, p)
				}
				if p != "fm" {
					if a, ok := h.peek(w); !ok || a.err == nil {
						t.Errorf("%s: %s not refused at once: %+v %v", name, p, a, ok)
					}
				}
			}
		}
	}
}
