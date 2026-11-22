package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// FS1 (fourth review): with the default settings the per-node figures
// (expect_mib, the forced check, the cache prediction) use a stored
// per-node share only when its row is trusted, and scaled up to the need
// for that device count when the shares add up to less. The TB1 rows: a
// trusted 1-device 11800 and an untrusted 3-device 8000 with per-node
// figures of 2700/2650/2650 on a, b, c; the need on 3 devices is 13800
// (1000 MiB a device).
func defaultsThree(t *testing.T) (*harness, *core.Profile) {
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, 12400))
	p := h.llama("three", 0, func(p *core.Profile) {
		p.Model, p.Ctx, p.ForceNodes, p.TensorSplit, p.LoadTimeout = "shared.gguf", 8192, []string{"a", "b", "c"}, []int{1, 1, 1}, time.Hour
	})
	h.needs["three"] = NeedInfo{Need: placer.Estimated(8000), FileBytes: 6 << 30, DeviceMiB: 400}
	h.c.llamaBuild, h.c.servedBuild = "aaaaaaa1", "aaaaaaa1"
	h.viewChanged()
	return h, p
}

func threeRow(p *core.Profile, total int, a, b, c int) *learnedNeed {
	f := 1.0 / 3
	return &learnedNeed{TotalMiB: total, key: needKeyOf(p), profile: "three", devices: 3,
		placement: []core.NodeID{"a", "b", "c"}, split: []float64{f, f, f},
		Nodes: []learnedNode{{ID: "a", Fraction: f, MiB: a}, {ID: "b", Fraction: f, MiB: b}, {ID: "c", Fraction: f, MiB: c}}}
}

func TestReview4_FS1_SharesFollowTheNeed(t *testing.T) {
	cases := []struct {
		name    string
		trusted bool
		minSum  int // the shares must add up to at least this
	}{
		{"an untrusted 3-device row is not used per node", false, 13800},
		{"a trusted row below the need is scaled up", true, 13800},
	}
	for _, tc := range cases {
		h, p := defaultsThree(t)
		h.c.putLearned(&learnedNeed{TotalMiB: 11800, key: needKeyOf(p), profile: "one", devices: 1, at: h.clk.Now()})
		l := threeRow(p, 8000, 2700, 2650, 2650)
		l.at = h.clk.Now()
		if !tc.trusted {
			l.restored, l.build, l.release = true, "ccccccc3", "0.1.5"
		}
		h.c.putLearned(l)
		if info, _ := h.c.needAt(p, 3); info.Need != placer.Measured(13800) {
			t.Fatalf("%s: need on 3 %+v", tc.name, info.Need)
		}
		r := h.c.dryPlan("three")
		if len(r.Placement) != 3 {
			t.Fatalf("%s: plan %+v", tc.name, r)
		}
		pl := core.Placement{}
		for _, n := range r.Placement {
			pl.Nodes = append(pl.Nodes, core.PlacedNode{ID: n.ID, Addr: n.Addr})
		}
		shares := h.c.expectShares(p, pl)
		sum := 0
		for _, s := range shares {
			sum += s
		}
		if sum < tc.minSum {
			t.Errorf("%s: expect_mib %v adds up to %d, below the need %d", tc.name, shares, sum, tc.minSum)
		}
		// the forced check: node c with 4000 MiB usable is short for a third
		// of 13800, not for the stored 2650
		h.view.modify("c", func(n *core.Node) { n.FreeMiB, n.TotalMiB = 5000, 5000 })
		h.viewChanged()
		short, _ := h.c.sharedShortfalls(p, mustNeedAt(t, h, p, 3), pl, false)
		if len(short) == 0 {
			t.Errorf("%s: the forced check finds node c not short: it used the stored 2650", tc.name)
		}
	}
	// a trusted row whose shares cover the need is used as measured
	h, p := defaultsThree(t)
	l := threeRow(p, 9000, 3100, 2950, 2950)
	l.at = h.clk.Now()
	h.c.putLearned(l)
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	if got := h.c.expectShares(p, pl); len(got) != 3 || got[0] != 3100 || got[1] != 2950 {
		t.Fatalf("a trusted row covering the need: %v, want its own figures", got)
	}
}

func mustNeedAt(t *testing.T, h *harness, p *core.Profile, k int) NeedInfo {
	t.Helper()
	info, err := h.c.needAt(p, k)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
