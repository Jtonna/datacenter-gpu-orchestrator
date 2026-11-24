package scheduler

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
)

// The host overhead measured per node and shape (hostoverhead.go).

// ohHarness: agents membership, two consoles a and b (15000 free, headroom
// 1000), and profile q hosted on a alone (needs_mib 4000). q2 has q's shape
// and is hosted on b.
func ohHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.c.llamaBuild, h.c.servedBuild = "aaaaaaa1", "aaaaaaa1"
	q := h.llama("q", 4000, func(p *core.Profile) {
		p.ForceNodes, p.TTL, p.Host = []string{"a"}, ptrDur(0), core.HostNode
	})
	h.llama("q2", 4000, func(p *core.Profile) {
		p.ForceNodes, p.TTL, p.Host, p.Model = []string{"b"}, ptrDur(0), core.HostNode, q.Model
	})
	h.viewChanged()
	return h
}

func onNode(id core.NodeID) core.Placement {
	return core.Placement{Nodes: []core.PlacedNode{{ID: id}}, Host: id}
}

// hostLoad loads q on a, with the log putting 4000 MiB on a's own device
// (and 150 MiB of host buffers), then the node gives up used MiB.
func hostLoad(h *harness, used int) {
	h.t.Helper()
	w := h.request("q")
	h.step(evWorkersLaunched{iid: h.lastLaunch().spec.IID, res: WorkerResult{LaunchIDs: map[core.NodeID]string{}}})
	setLoad(h, "q", instance.DeviceMem{Name: "Vulkan0", ModelMiB: 3000, KVMiB: 800, ComputeMiB: 200},
		instance.DeviceMem{Name: "Vulkan_Host", Host: true, ComputeMiB: 150})
	h.healthy("q")
	h.release(h.granted(w))
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= used })
	h.viewChanged()
}

func TestHostOverhead_SettingFirstThenTheMeasurementOnThatNodeOnly(t *testing.T) {
	h := ohHarness(t)
	st := openNeedStore(t)
	h.c.restoreNeeds(st)
	q, q2 := h.c.cfg.Profiles["q"], h.c.cfg.Profiles["q2"]
	r := h.c.dryPlan("q")
	if r.HostOverheadMiB != 1024 || r.HostOverheadSource != HostOverheadFromSetting || !r.HostOverheadMeasuredAt.IsZero() {
		t.Fatalf("first plan: %d %q %v", r.HostOverheadMiB, r.HostOverheadSource, r.HostOverheadMeasuredAt)
	}
	if got := h.c.expectShares(q, onNode("a")); !slices.Equal(got, []int{4000 + 1024}) {
		t.Fatalf("before: %v", got)
	}

	hostLoad(h, 4600) // 4000 on the device, 600 for the host process
	if _, ok := h.c.hostOverheadAt(shapeOf(q), "a"); ok {
		t.Fatal("measured before a full average window after ready")
	}
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	e, ok := h.c.hostOverheadAt(shapeOf(q), "a")
	if !ok || e.mib != 600 || e.logHost != 150 || e.profile != "q" {
		t.Fatalf("measured %+v %v", e, ok)
	}
	if ev := h.histTypes(history.TLearnedNeed); len(ev) == 0 || ev[len(ev)-1].Data["host_overhead_mib"] != 600 {
		t.Fatalf("history %+v", ev)
	}
	// q on a plans with 600; q2 (same shape) on b keeps the setting.
	if got := h.c.expectShares(q, onNode("a")); !slices.Equal(got, []int{4000 + 600}) {
		t.Fatalf("after on a: %v", got)
	}
	if got := h.c.expectShares(q2, onNode("b")); !slices.Equal(got, []int{4000 + 1024}) {
		t.Fatalf("q2 on b: %v", got)
	}
	if r := h.c.dryPlan("q2"); r.HostOverheadMiB != 1024 || r.HostOverheadSource != HostOverheadFromSetting {
		t.Fatalf("q2 plan %d %q", r.HostOverheadMiB, r.HostOverheadSource)
	}
	h.step(evUnload{profile: "q", force: true})
	r = h.c.dryPlan("q")
	if r.HostOverheadMiB != 600 || r.HostOverheadSource != HostOverheadMeasured || r.HostOverheadMeasuredAt.IsZero() {
		t.Fatalf("plan after: %d %q %v", r.HostOverheadMiB, r.HostOverheadSource, r.HostOverheadMeasuredAt)
	}

	// Stored and restored; trusted on the same llama build.
	flushNeeds(h, st)
	rows, err := st.AllHost()
	if err != nil || len(rows) != 1 || rows[0].MiB != 600 || rows[0].Node != "a" || rows[0].Shape != shapeOf(q) {
		t.Fatalf("stored %+v %v", rows, err)
	}
	h2 := ohHarness(t)
	h2.c.restoreNeeds(st)
	h2.c.servedBuild = "aaaaaaa1" // as the served release's release.json gives it
	if got := h2.c.expectShares(h2.c.cfg.Profiles["q"], onNode("a")); !slices.Equal(got, []int{4600}) {
		t.Fatalf("restored: %v", got)
	}
	// Restored on another llama build: it may only raise the setting.
	h3 := ohHarness(t)
	h3.c.llamaBuild, h3.c.servedBuild = "bbbbbbb2", "bbbbbbb2"
	h3.c.restoreNeeds(st)
	h3.c.servedBuild = "bbbbbbb2"
	if r := h3.c.dryPlan("q"); r.HostOverheadMiB != 1024 || r.HostOverheadSource != HostOverheadFromSetting {
		t.Fatalf("other build: %d %q", r.HostOverheadMiB, r.HostOverheadSource)
	}

	// The way back without a rollback: host_overhead_setting_only.
	h2.cfg.Defaults.HostOverheadSettingOnly = true
	if r := h2.c.dryPlan("q"); r.HostOverheadMiB != 1024 || r.HostOverheadSource != HostOverheadFromSetting {
		t.Fatalf("setting only: %d %q", r.HostOverheadMiB, r.HostOverheadSource)
	}
	h2.cfg.Defaults.HostOverheadSettingOnly = false

	// Forgetting the shape forgets its host overheads, here and stored.
	if !h2.c.forgetShape(shapeOf(q)) {
		t.Fatal("forget reports nothing")
	}
	flushNeeds(h2, st)
	if r := h2.c.dryPlan("q"); r.HostOverheadMiB != 1024 {
		t.Fatalf("after forget %d", r.HostOverheadMiB)
	}
	if rows, _ := st.AllHost(); len(rows) != 0 {
		t.Fatalf("stored after forget %+v", rows)
	}
}

// The plan's memory line names which figure was used on the host row.
func TestHostOverhead_MemoryLineNamesTheSource(t *testing.T) {
	h := ohHarness(t)
	reserveOnSettings(h)
	h.viewChanged()
	row := func() PlanMemoryNode {
		r := h.c.dryPlan("q")
		if r.Memory == nil || len(r.Memory.Nodes) != 1 {
			t.Fatalf("memory %+v", r.Memory)
		}
		return r.Memory.Nodes[0]
	}
	if m := row(); m.HostOverheadMiB != 1024 || m.HostOverheadSource != HostOverheadFromSetting {
		t.Fatalf("%+v", m)
	}
	hostLoad(h, 4600)
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	h.step(evUnload{profile: "q", force: true})
	if m := row(); m.HostOverheadMiB != 600 || m.HostOverheadSource != HostOverheadMeasured || m.HostOverheadMeasuredAt.IsZero() {
		t.Fatalf("%+v", m)
	}
}

// A reading that cannot be right (below 0: the node gave up less than the
// log put on its device) is not stored; the setting stays.
func TestHostOverhead_ANegativeReadingIsNotStored(t *testing.T) {
	h := ohHarness(t)
	hostLoad(h, 3500)
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	if e, ok := h.c.hostOverheadAt(shapeOf(h.c.cfg.Profiles["q"]), "a"); ok {
		t.Fatalf("stored %+v", e)
	}
}

// The average free figure is used when known, once its window holds only
// readings taken after ready.
func TestHostOverhead_UsesTheAverage(t *testing.T) {
	h := ohHarness(t)
	hostLoad(h, 4600)
	h.view.modify("a", func(n *core.Node) { n.FreeAvgMiB, n.FreeAvgKnown = 15000-4700, true })
	h.viewChanged()
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	if e, ok := h.c.hostOverheadAt(shapeOf(h.c.cfg.Profiles["q"]), "a"); !ok || e.mib != 700 {
		t.Fatalf("%+v %v", e, ok)
	}
}

// A cycle that ends abnormally (here the host exits; the watchdog, an OOM
// kill) raises the figure to the most the
// host was seen to take in the cycle; a normal one never lowers it below
// its own settled reading.
func TestHostOverhead_AWatchdogEndRaisesIt(t *testing.T) {
	h := ohHarness(t)
	hostLoad(h, 4600)
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	h.view.modify("a", func(n *core.Node) { n.FreeMiB -= 500 }) // the host grew by 500
	h.viewChanged()
	h.exit("q", 137)
	e, ok := h.c.hostOverheadAt(shapeOf(h.c.cfg.Profiles["q"]), "a")
	if !ok || e.mib != 1100 || e.cause == needstore.CauseLoad {
		t.Fatalf("%+v %v", e, ok)
	}
}

// Before any measurement exists every plan is the one the setting gives
// (the release before this one): with host_overhead_setting_only on and
// off, every profile's plan is the same, the host ones on
// host_overhead_mib. Once a shape is measured on a node, only the profiles
// of that shape hosted on that node change, and only by setting - measured
// on the host row (the flip list).
func TestHostOverhead_NoMeasurementChangesNoPlan(t *testing.T) {
	h := ohHarness(t)
	h.llama("srv", 4000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })                          // hosted on the scheduler's machine
	h.llama("other", 3000, func(p *core.Profile) { p.ForceNodes, p.Host = []string{"a"}, core.HostNode }) // another shape on a
	h.llama("split", 6000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.Host, p.Model = []string{"a", "b"}, []int{1, 1}, core.HostNode, h.c.cfg.Profiles["q"].Model
	})
	h.llama("auto", 2000)
	h.viewChanged()
	plans := func() map[string]PlanReport {
		out := map[string]PlanReport{}
		for name := range h.c.cfg.Profiles {
			out[name] = h.c.dryPlan(name)
		}
		return out
	}
	h.cfg.Defaults.HostOverheadSettingOnly = true
	before := plans()
	h.cfg.Defaults.HostOverheadSettingOnly = false
	if now := plans(); !reflect.DeepEqual(before, now) {
		for n := range now {
			if !reflect.DeepEqual(before[n], now[n]) {
				t.Errorf("%s changed with no measurement:\n%+v\n%+v", n, before[n], now[n])
			}
		}
	}
	for _, n := range []string{"q", "q2", "other", "split"} {
		if before[n].HostOverheadMiB != 1024 || before[n].HostOverheadSource != HostOverheadFromSetting {
			t.Fatalf("%s: %d %q", n, before[n].HostOverheadMiB, before[n].HostOverheadSource)
		}
	}
	// A measurement of q's shape on a: q, and split when it has q's shape
	// (the shape does not depend on the device count) since a hosts it; not
	// q2 (hosted on b), other (another shape), srv or auto (not hosted).
	h.c.putHostOverhead(shapeOf(h.c.cfg.Profiles["q"]), "a", &hostOH{mib: 600, at: h.clk.Now(), build: h.c.llamaBuild, profile: "q"})
	after := plans()
	var changed []string
	for n := range after {
		if !reflect.DeepEqual(before[n], after[n]) {
			changed = append(changed, n)
		}
	}
	slices.Sort(changed)
	want := []string{"q"}
	if shapeOf(h.c.cfg.Profiles["split"]) == shapeOf(h.c.cfg.Profiles["q"]) {
		want = []string{"q", "split"} // same shape, hosted on a
	}
	if !slices.Equal(changed, want) {
		t.Fatalf("changed %v, want %v", changed, want)
	}
	if after["q"].HostOverheadMiB != 600 || after["q"].HostOverheadSource != HostOverheadMeasured {
		t.Fatalf("q %+v", after["q"])
	}
	q := h.c.cfg.Profiles["q"]
	if got := h.c.expectShares(q, onNode("a")); got[0] != 4000+1024-(1024-600) {
		t.Fatalf("q's host row %v", got)
	}
}
