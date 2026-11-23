package scheduler

import (
	"fmt"
	"net/http"
	"sort"
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

// The cases on live once both laptops are excluded: the local node (the
// OMEN, by hostname) and the Alienware (by id). Six laptop profiles force
// them alone, together, or with consoles; one profile forces a console that
// is paused; the command profiles (speech, images) run on the excluded local
// node; an automatic profile admits only the laptops' tier. With the
// defaults and the tight settings, fleet and agents, each is listed,
// planned and requested without a panic or an error of its own, is refused
// at once, and nothing starts.
func TestNoNodeLeft_TheLiveLaptopCases(t *testing.T) {
	alien := laptop("02:00:00:00:00:44", 7000)
	alien.Hostname = "alienware"
	nodes := func() []core.Node {
		b := ps5("b", 4, 15000)
		b.Paused = true
		return []core.Node{hostNode(6000), alien, ps5("a", 2, 15000), b, ps5("c", 6, 15000)}
	}
	laptops := map[string][]string{
		"lap-local":          {"host-1"},
		"lap-alien":          {"alienware"},
		"lap-both":           {"host-1", "alienware"},
		"lap-alien-by-id":    {"02:00:00:00:00:44"},
		"lap-local-consoles": {"host-1", "a", "c"},
		"lap-alien-consoles": {"alienware", "a"},
	}
	for _, tight := range []bool{false, true} {
		for _, mode := range []string{"fleet", "agents"} {
			name := fmt.Sprintf("tight %v/%s", tight, mode)
			h := inMode(newHarness(t).nodes(nodes()...), mode)
			for p, force := range laptops {
				h.llama(p, 5000, func(pr *core.Profile) { pr.ForceNodes = force })
			}
			h.llama("on-paused", 5000, func(pr *core.Profile) { pr.ForceNodes = []string{"b"} })
			h.llama("laptop-tier", 5000, func(pr *core.Profile) { pr.AllowTiers = []int{2, 3} })
			h.command("whisper", 5400)
			h.command("images", 6000)
			h.viewChanged()
			nc := cloneCfg(h.c.cfg)
			nc.ExcludeNodes = []string{"host-1", "02:00:00:00:00:44"}
			if tight {
				liveTight(&harness{cfg: nc})
			}
			h.step(evConfig{cfg: nc})

			var all []string
			for p := range h.c.cfg.Profiles {
				all = append(all, p)
			}
			sort.Strings(all)
			for _, p := range all {
				var r PlanReport
				func() {
					defer func() {
						if v := recover(); v != nil {
							t.Fatalf("%s: planning %s panicked: %v", name, p, v)
						}
					}()
					r = h.c.dryPlan(p)
				}()
				if r.Action != PlanReject {
					t.Errorf("%s: %s plan %s %q, want a refusal", name, p, r.Action, r.Reason)
				}
				_, isLaptop := laptops[p]
				if (isLaptop || p == "whisper" || p == "images") && r.ExcludedBy == "" {
					t.Errorf("%s: %s refused for %q, not by the exclusion", name, p, r.Reason)
				}
				if p == "on-paused" && !strings.Contains(r.Reason, "paused by an operator") {
					t.Errorf("%s: on-paused refused for %q", name, r.Reason)
				}
				w := h.request(p)
				a, ok := h.peek(w)
				if !ok || a.err == nil {
					t.Errorf("%s: %s not refused at once", name, p)
				} else if a.err.Status == http.StatusInternalServerError {
					t.Errorf("%s: %s answered with an internal error: %v", name, p, a.err)
				}
				if h.c.insts[p] != nil || h.c.entries[p] != nil {
					t.Errorf("%s: %s started or queued", name, p)
				}
			}
			if _, err := func() (st Status, err error) {
				defer func() {
					if v := recover(); v != nil {
						err = fmt.Errorf("status panicked: %v", v)
					}
				}()
				return h.c.statusDoc(), nil
			}(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
}
