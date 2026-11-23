package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// A host: node profile with no measurement of its own is planned from the
// same shape measured hosted on the scheduler's machine (plus the host
// overhead on the host node, which the shares add), not from the generic
// estimate; the basis says so. Once the host shape is measured, that
// measurement is used. (Hardware: a 30B load measured server-hosted was
// refused 425 MiB short the first time as a host profile.)
func TestHost_PlannedFromTheServerHostedMeasurement(t *testing.T) {
	h := hostHarness(t)
	h.viewChanged()
	p := h.c.cfg.Profiles["m"]
	h.needs["m"] = NeedInfo{Need: placer.Estimated(12000), FileBytes: 1 << 30} // the generic estimate, too high
	h.c.llamaBuild, h.c.servedBuild = "aaaaaaa1", "aaaaaaa1"
	server := *p
	server.Host = ""
	h.c.putLearned(&learnedNeed{TotalMiB: 8000, key: needKeyOf(&server), profile: "s", devices: 2, at: h.clk.Now(),
		placement: []core.NodeID{"a", "b"}, split: []float64{0.5, 0.5},
		Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: 4000}, {ID: "b", Fraction: 0.5, MiB: 4000}}})
	h.c.needs_ = map[string]NeedInfo{}

	r := h.c.dryPlan("m")
	if r.NeedMiB != 8000 || !r.Measured || !strings.Contains(r.NeedBasis, "hosted on the scheduler's machine") {
		t.Fatalf("need %d measured %v basis %q", r.NeedMiB, r.Measured, r.NeedBasis)
	}
	// The per-node figures: the measured share, plus the overhead on the host node.
	exp := h.c.expectShares(p, core.Placement{Nodes: []core.PlacedNode{{ID: "a"}, {ID: "b"}}, Host: "a"})
	if len(exp) != 2 || exp[0] != 4000+h.cfg.Defaults.HostOverheadMiB || exp[1] != 4000 {
		t.Fatalf("expect shares %v", exp)
	}
	// The server-hosted profile itself is not touched by host measurements.
	h.c.putLearned(&learnedNeed{TotalMiB: 8600, key: needKeyOf(p), profile: "m", devices: 2, at: h.clk.Now()})
	r = h.c.dryPlan("m")
	if r.NeedMiB != 8600 || strings.Contains(r.NeedBasis, "scheduler's machine") {
		t.Fatalf("after its own measurement: need %d basis %q", r.NeedMiB, r.NeedBasis)
	}
	// A server-hosted profile never takes a host measurement.
	sp := h.llama("s", 0, func(q *core.Profile) {
		q.ForceNodes, q.TensorSplit = []string{"a", "b"}, []int{1, 1}
		q.Model = p.Model
		q.Ctx = p.Ctx
	})
	h.needs["s"] = NeedInfo{Need: placer.Estimated(12000), FileBytes: 1 << 30}
	if info, _ := h.c.need(sp); info.Need.MiB != 8000 {
		t.Fatalf("server profile need %d, want its own 8000", info.Need.MiB)
	}
}
