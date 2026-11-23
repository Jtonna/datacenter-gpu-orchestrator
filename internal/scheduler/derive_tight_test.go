package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// deriveOnConsole: one console "a" (live rules: fill reserve, measured
// margin 300), a profile "q" forced on it at ctx 32768 with no measurement
// of its own, and the same model measured at 2048 and 8192 (its own pairs
// check the derivation, so the figure carries the measured margin). The
// node's free figure is set so it keeps spare MiB beyond what q will take.
func deriveOnConsole(t *testing.T, spare int) (*harness, PlanReport) {
	t.Helper()
	h := deriveHarness(t)
	h.nodes(psNode("a", 2, 30000))
	liveTight(h)
	h.llama("q", 0, func(p *core.Profile) {
		p.Model, p.Ctx, p.ForceNodes, p.LoadTimeout = "qwen.gguf", 32768, []string{"a"}, time.Hour
	})
	h.measure("qwen.gguf", 2048, 1, 5000)
	h.measure("qwen.gguf", 8192, 1, derivedFrom(5000, 2048, 8192, 1))
	h.viewChanged()
	row := h.c.dryPlan("q").Memory.Nodes[0]
	h.view.modify("a", func(n *core.Node) {
		n.FreeMiB = row.WillTakeMiB + spare + n.HeadroomMiB
		n.TotalMiB = n.FreeMiB
	})
	h.viewChanged()
	return h, h.c.dryPlan("q")
}

// A derived figure counts as measured: the measured margin (300) applies,
// a node that holds the load but not the margin is a tight fit and starts,
// one that holds the margin too is a plain fit, and one short even without
// the margin is refused; the basis says "derived" in every case.
func TestDerivedNeed_WithTheTightFitRule(t *testing.T) {
	want := derivedFrom(derivedFrom(5000, 2048, 8192, 1), 8192, 32768, 1)
	for _, c := range []struct {
		name        string
		spare       int
		action      PlanAction
		tight, fits bool
	}{
		{"tight", 100, PlanStart, true, true},
		{"meets the margin", 300, PlanStart, false, true},
		{"short without the margin", -1, PlanReject, false, false},
	} {
		h, r := deriveOnConsole(t, c.spare)
		_ = h
		if r.NeedMiB != want || r.DerivedFromCtx != 8192 || !strings.HasPrefix(r.NeedBasis, "derived from the measurement at ctx 8192") {
			t.Fatalf("%s: need %d from %d basis %q", c.name, r.NeedMiB, r.DerivedFromCtx, r.NeedBasis)
		}
		row := r.Memory.Nodes[0]
		if row.Basis != BasisMeasured || row.MarginMiB != 300 || row.MarginRule != "margin_measured_mib" {
			t.Fatalf("%s: row basis %s margin %d (%s)", c.name, row.Basis, row.MarginMiB, row.MarginRule)
		}
		if r.Action != c.action || r.TightFit != c.tight || r.Memory.TightFit != c.tight || row.Tight != c.tight || r.Memory.Fits != c.fits {
			t.Fatalf("%s: action %s (%s) tight %v/%v fits %v", c.name, r.Action, r.Reason, r.TightFit, row.Tight, r.Memory.Fits)
		}
		if c.tight && (row.SpareMiB != 100 || row.MarginShortMiB != 200) {
			t.Fatalf("%s: spare %d short %d", c.name, row.SpareMiB, row.MarginShortMiB)
		}
	}
}

// host: node and the order of sources: its own measurement, then the same
// shape hosted on the scheduler's machine, then a derivation from the host
// shape at another context; the basis says which, and the host overhead is
// added on the host node's share in each.
func TestDerivedNeed_HostNodeOrderOfSources(t *testing.T) {
	h := deriveHarness(t)
	h.nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.c.agents = true
	host := func(p *core.Profile) { p.Host = core.HostNode }
	q := h.llama("q", 0, func(p *core.Profile) {
		p.Model, p.Ctx, p.ForceNodes, p.TensorSplit, p.Host = "qwen.gguf", 32768, []string{"a", "b"}, []int{1, 1}, core.HostNode
	})
	h.measure("qwen.gguf", 8192, 2, 9000, host)
	h.measure("qwen.gguf", 32768, 2, 12000) // the server-hosted twin, same context
	h.viewChanged()
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a"}, {ID: "b"}}, Host: "a"}

	r := h.c.dryPlan("q")
	if r.NeedMiB != 12000 || r.DerivedFromCtx != 0 || !strings.Contains(r.NeedBasis, "hosted on the scheduler's machine") {
		t.Fatalf("twin first: need %d from %d basis %q", r.NeedMiB, r.DerivedFromCtx, r.NeedBasis)
	}
	h.c.learned = map[string]map[int]*learnedNeed{}
	h.measure("qwen.gguf", 8192, 2, 9000, host)
	r = h.c.dryPlan("q")
	want := estimateMarginOf(h, derivedFrom(9000, 8192, 32768, 2)) // no pair of its own: +5%
	if r.NeedMiB != want || r.DerivedFromCtx != 8192 || !strings.HasPrefix(r.NeedBasis, "derived from the measurement at ctx 8192") ||
		strings.Contains(r.NeedBasis, "scheduler's machine") {
		t.Fatalf("derived: need %d (want %d) from %d basis %q", r.NeedMiB, want, r.DerivedFromCtx, r.NeedBasis)
	}
	exp := h.c.expectShares(q, pl)
	if len(exp) != 2 || exp[0]-exp[1] < h.cfg.Defaults.HostOverheadMiB-1 {
		t.Fatalf("the host node's share carries the overhead: %v", exp)
	}
	// its own measurement wins
	h.measure("qwen.gguf", 32768, 2, 12500, host)
	if r = h.c.dryPlan("q"); r.NeedMiB != 12500 || r.DerivedFromCtx != 0 || strings.Contains(r.NeedBasis, "derived") {
		t.Fatalf("own: need %d basis %q", r.NeedMiB, r.NeedBasis)
	}
}

func estimateMarginOf(h *harness, v int) int {
	return (v*(1000+permille(h.cfg.Defaults.EstimateMargin)) + 999) / 1000
}
