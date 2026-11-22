package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The second review, SB1 and SS4: measurements by device count.

// oneAndThree has PS5s a, b and c (12400 budgets), the tight settings, and
// two profiles of one shape: "one" forced onto a, "three" forced onto a, b
// and c. Their estimate is 8000 MiB (8400 with the margin) with a 400 MiB
// compute buffer per device: 1000 MiB a device with the 600 overhead.
func oneAndThree(t *testing.T) *harness {
	h := newHarness(t).nodes(psNode("a", 2, 12400), psNode("b", 4, 12400), psNode("c", 6, 12400))
	reserveOnSettings(h)
	for name, nodes := range map[string][]string{"one": {"a"}, "three": {"a", "b", "c"}} {
		h.llama(name, 0, func(p *core.Profile) {
			p.Model, p.Ctx, p.ForceNodes, p.LoadTimeout = "shared.gguf", 8192, nodes, time.Hour
			if len(nodes) == 3 {
				p.TensorSplit = []int{1, 1, 1}
			}
		})
		h.needs[name] = NeedInfo{Need: placer.Estimated(8000), FileBytes: 6 << 30, DeviceMiB: 400}
	}
	h.viewChanged()
	return h
}

// willTake is the plan's per-node "will take" figures of profile.
func willTake(t *testing.T, h *harness, profile string) []int {
	t.Helper()
	r := h.c.dryPlan(profile)
	if r.Memory == nil {
		t.Fatalf("%s: no memory section: %+v", profile, r)
	}
	var out []int
	for _, n := range r.Memory.Nodes {
		out = append(out, n.WillTakeMiB)
	}
	return out
}

// SB1, the reviewer's probe: a 1-node load of the shape measuring 7600 MiB
// does not lower what the 3-node profile is held to on each node.
func TestReview2_SB1_AOneNodeLoadDoesNotLowerThreeNodes(t *testing.T) {
	h := oneAndThree(t)
	before := willTake(t, h, "three")
	h.request("one")
	setLoad(h, "one", rpcDev("192.0.2.2:50052", 6800, 400, 400)) // 7600 on one device
	h.healthy("one")
	h.step(evUnload{profile: "one", force: true})
	h.advance(10 * time.Second)
	after := willTake(t, h, "three")
	if len(after) != 3 || len(before) != 3 {
		t.Fatalf("rows before %v after %v", before, after)
	}
	for i := range after {
		if after[i] < before[i] {
			t.Fatalf("node %d: will take %d after a 1-node load of 7600, %d before (rows %v -> %v)", i, after[i], before[i], before, after)
		}
	}
	// and the 1-node profile itself uses its measurement
	if r := h.c.dryPlan("one"); r.FitNeedMiB != 7600 {
		t.Fatalf("one: need %d, want its measured 7600", r.FitNeedMiB)
	}
}

// SB1: a measurement on fewer devices is raised by the estimator's
// per-device cost for each extra device; never below the estimate.
func TestReview2_SB1_FewerDevicesAreScaledUp(t *testing.T) {
	h := oneAndThree(t)
	p := h.c.cfg.Profiles["three"]
	h.c.putLearned(&learnedNeed{TotalMiB: 13000, key: needKeyOf(p), profile: "one", devices: 1, at: h.clk.Now()})
	info, _ := h.c.needAt(p, 3)
	dev := h.c.cfg.Defaults.DeviceOverheadMiB + 400
	if want := 13000 + 2*dev; info.Need != placer.Measured(want) {
		t.Fatalf("3 devices from a 1-device 13000: %+v, want measured %d", info.Need, want)
	}
	// a low 1-device figure: the estimate for 3 devices stands
	h.c.putLearned(&learnedNeed{TotalMiB: 5000, key: needKeyOf(p), profile: "one", devices: 1, at: h.clk.Now()})
	if info, _ := h.c.needAt(p, 3); info.Need.Measured || info.Source != NeedEstimated {
		t.Fatalf("3 devices from a 1-device 5000: %+v, want the estimate", info)
	}
	// an exact 3-device measurement is used, but never below a 1-device
	// total raised for the two more devices (third review, I4)
	h.c.putLearned(&learnedNeed{TotalMiB: 11000, key: needKeyOf(p), profile: "three", devices: 3, at: h.clk.Now()})
	h.c.putLearned(&learnedNeed{TotalMiB: 11500, key: needKeyOf(p), profile: "one", devices: 1, at: h.clk.Now()})
	if info, _ := h.c.needAt(p, 3); info.Need != placer.Measured(11500+2*dev) {
		t.Fatalf("exact 3-device 11000 with a 1-device 11500: %+v, want %d", info.Need, 11500+2*dev)
	}
	// 2 devices: the 1-device total raised for one more device
	if info, _ := h.c.needAt(p, 2); info.Need != placer.Measured(11500+dev) {
		t.Fatalf("2 devices: %+v", info.Need)
	}
	// only more devices measured: that total, unscaled
	h.c.learned[shapeOf(p)] = nil
	h.c.putLearned(&learnedNeed{TotalMiB: 14000, key: needKeyOf(p), profile: "three", devices: 3, at: h.clk.Now()})
	if info, _ := h.c.needAt(p, 1); info.Need != placer.Measured(14000) {
		t.Fatalf("1 device from a 3-device 14000: %+v, want 14000", info.Need)
	}
}

// SB1: after a restart, a restored total is not lowered by the first load
// of the run until that load ends normally; a load that ends abnormally
// keeps it.
func TestReview2_SB1_ARestoredTotalHoldsUntilANormalCycle(t *testing.T) {
	for _, normal := range []bool{true, false} {
		h := oneAndThree(t)
		p := h.c.cfg.Profiles["one"]
		h.c.putLearned(&learnedNeed{TotalMiB: 9000, key: needKeyOf(p), profile: "one", devices: 1, restored: true, at: h.clk.Now()})
		h.request("one")
		setLoad(h, "one", rpcDev("192.0.2.2:50052", 6800, 400, 400)) // 7600
		h.healthy("one")
		if r := h.c.dryPlan("one"); r.FitNeedMiB != 9000 {
			t.Fatalf("while the first load runs: need %d, want the restored 9000", r.FitNeedMiB)
		}
		if normal {
			h.step(evUnload{profile: "one", force: true})
		} else {
			h.exitChild(h.inst("one").iid, instance.ExitStatus{Code: 1})
			h.step(evViewChanged{})
		}
		h.advance(time.Minute)
		want := map[bool]int{true: 7600, false: 9000}[normal]
		if r := h.c.dryPlan("one"); r.FitNeedMiB != want {
			t.Fatalf("normal end %v: need %d, want %d", normal, r.FitNeedMiB, want)
		}
	}
}

// SS4: a placement on fewer nodes than the peak was measured on is held to
// the whole peak: the rows add up to at least the peak total.
func TestReview2_SS4_FewerNodesKeepTheWholePeak(t *testing.T) {
	h := oneAndThree(t)
	h.cfg.Defaults.FillReserveKiBPerToken = 64 // 512 MiB a node at 8192
	three := h.c.cfg.Profiles["three"]
	h.c.putLearned(&learnedNeed{TotalMiB: 12000, key: needKeyOf(three), profile: "three", placement: []core.NodeID{"a", "b", "c"},
		split: []float64{1. / 3, 1. / 3, 1. / 3}, at: h.clk.Now(), Nodes: []learnedNode{
			{ID: "a", Fraction: 1. / 3, MiB: 4000, PeakMiB: 5200}, {ID: "b", Fraction: 1. / 3, MiB: 4000, PeakMiB: 5200},
			{ID: "c", Fraction: 1. / 3, MiB: 4000, PeakMiB: 5200}}}) // peak 15600 > 12000 + 3 x 512
	h.llama("two", 0, func(p *core.Profile) {
		p.Model, p.Ctx, p.ForceNodes, p.TensorSplit, p.LoadTimeout = "shared.gguf", 8192, []string{"a", "b"}, []int{1, 1}, time.Hour
	})
	h.needs["two"] = h.needs["three"]
	h.viewChanged()
	sum := 0
	for _, w := range willTake(t, h, "two") {
		sum += w
	}
	if sum < 15600 {
		t.Fatalf("2 nodes: rows add up to %d, below the 3-node peak 15600", sum)
	}
	req, _, _ := h.c.request(h.c.cfg.Profiles["two"])
	if r := req.For(2); r.Need.WithMargin()+2*r.PerNodeReserveMiB < 15600 {
		t.Fatalf("2 nodes: the placer asks %d + 2 x %d, below the peak 15600", r.Need.WithMargin(), r.PerNodeReserveMiB)
	}
}
