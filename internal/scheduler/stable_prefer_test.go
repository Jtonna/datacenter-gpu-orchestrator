package scheduler

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// storedOn puts a trusted measurement of p's shape with per-node figures on
// the set.
func storedOn(h *harness, p *core.Profile, need int, set ...core.NodeID) {
	k := len(set)
	l := &learnedNeed{TotalMiB: need, key: needKeyOf(p), profile: p.Name, devices: k}
	for _, id := range set {
		l.placement = append(l.placement, id)
		l.split = append(l.split, 1/float64(k))
		l.Nodes = append(l.Nodes, learnedNode{ID: id, Fraction: 1 / float64(k), MiB: need / k, PeakMiB: need/k + 150})
	}
	h.c.putLearned(l)
}

func placedOn(r PlanReport) []core.NodeID {
	var out []core.NodeID
	for _, pn := range r.Placement {
		out = append(out, pn.ID)
	}
	return out
}

// The flip test on a fixture fleet of six equal consoles: with the
// preference on, against the same fleet without it, exactly the profiles
// whose stored per-node figures are on consoles the name order did not
// take move to those consoles; a warm console still comes first; a profile
// measured on the very consoles the name order takes, one with no stored
// figures and a forced one do not move; no action changes.
func TestStable_PreferStoredFixtureFlip(t *testing.T) {
	build := func(noPrefer bool) *harness {
		h := newHarness(t)
		h.cfg.Defaults.PlacementToleranceMiB = stableTol
		h.c.noPreferStored = noPrefer
		h.nodes(liveShapeFleet(nil, 0, 0)...)
		storedOn(h, h.llama("moves", 8000), 8000, "console-3")
		h.llama("plain", 8000)
		storedOn(h, h.llama("already", 8000), 8000, "console-1")
		storedOn(h, h.llama("pair", 19000), 19000, "console-4", "console-5")
		storedOn(h, h.llama("warm", 8000), 8000, "console-3")
		storedOn(h, h.llama("forced", 8000, func(p *core.Profile) { p.ForceNodes = []string{"console-6"} }), 8000, "console-3")
		h.c.ns("console-2").lastFile = "warm.gguf"
		h.c.needs_ = map[string]NeedInfo{}
		h.viewChanged()
		return h
	}
	on, off := build(false), build(true)
	want := map[string][2][]core.NodeID{ // with the preference, without it
		"moves":   {{"console-3"}, {"console-1"}},
		"plain":   {{"console-1"}, {"console-1"}},
		"already": {{"console-1"}, {"console-1"}},
		"pair":    {{"console-4", "console-5"}, {"console-1", "console-2"}},
		"warm":    {{"console-2"}, {"console-2"}},
		"forced":  {{"console-6"}, {"console-6"}},
	}
	var flipped []string
	for name, w := range want {
		a, b := on.c.dryPlan(name), off.c.dryPlan(name)
		if a.Action != PlanStart || b.Action != PlanStart {
			t.Fatalf("%s: actions %s and %s, want start both", name, a.Action, b.Action)
		}
		ga, gb := placedOn(a), placedOn(b)
		slices.Sort(ga)
		slices.Sort(gb)
		if !slices.Equal(ga, w[0]) || !slices.Equal(gb, w[1]) {
			t.Errorf("%s: on %v with the preference, %v without; want %v and %v", name, ga, gb, w[0], w[1])
		}
		if stableRows(a) != stableRows(b) {
			flipped = append(flipped, name)
		}
		if a.NeedMiB != b.NeedMiB || a.Measured != b.Measured || len(a.Placement) != len(b.Placement) {
			t.Errorf("%s: need %d/%d measured %v/%v devices %d/%d: only the consoles may change", name, a.NeedMiB, b.NeedMiB, a.Measured, b.Measured, len(a.Placement), len(b.Placement))
		}
	}
	slices.Sort(flipped)
	if got := strings.Join(flipped, ","); got != "moves,pair" {
		t.Fatalf("plans that changed: %s, want moves,pair", got)
	}
}

// No stored figures anywhere: the plans with the preference are identical,
// byte for byte, to the plans without it (today's order), on live's shape
// with 120 profiles (single and split loads, host: node ones, refused ones)
// over 20 jittered reads.
func TestStable_NoStoredFiguresIdenticalToToday(t *testing.T) {
	build := func(noPrefer bool) (*harness, []string) {
		h := newHarness(t)
		h.cfg.Defaults.PlacementToleranceMiB = stableTol
		h.c.noPreferStored = noPrefer
		h.nodes(liveShapeFleet(nil, 0, 0)...)
		r := rand.New(rand.NewSource(55))
		var names []string
		for i := 0; i < 120; i++ {
			name := fmt.Sprintf("p%03d", i)
			names = append(names, name)
			switch {
			case i%7 == 0:
				h.llama(name, 60000+r.Intn(60000))
			case i%5 == 0:
				h.llama(name, 500+r.Intn(44000), func(p *core.Profile) { p.Host = core.HostNode })
			default:
				h.llama(name, 500+r.Intn(44000))
			}
		}
		h.viewChanged()
		return h, names
	}
	on, names := build(false)
	off, _ := build(true)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 20; i++ {
		fleet := liveShapeFleet(rng, 25, 0)
		on.nodes(fleet...)
		off.nodes(fleet...)
		on.viewChanged()
		off.viewChanged()
		for _, n := range names {
			a, _ := json.Marshal(on.c.dryPlan(n))
			b, _ := json.Marshal(off.c.dryPlan(n))
			if string(a) != string(b) {
				t.Fatalf("read %d %s: the plan differs without stored figures:\n%s\n%s", i, n, a, b)
			}
		}
	}
}
