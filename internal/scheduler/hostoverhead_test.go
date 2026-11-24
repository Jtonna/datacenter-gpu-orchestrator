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
	if _, ok := h.c.hostOverheadAt(shapeOf(q), "a", 1); ok {
		t.Fatal("measured before a full average window after ready")
	}
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	e, ok := h.c.hostOverheadAt(shapeOf(q), "a", 1)
	if !ok || e.MiB != 600 || e.LogHostMiB != 150 || e.Profile != "q" || e.Devices != 1 {
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
	if e, ok := h.c.hostOverheadAt(shapeOf(h.c.cfg.Profiles["q"]), "a", 1); ok {
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
	if e, ok := h.c.hostOverheadAt(shapeOf(h.c.cfg.Profiles["q"]), "a", 1); !ok || e.MiB != 700 {
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
	e, ok := h.c.hostOverheadAt(shapeOf(h.c.cfg.Profiles["q"]), "a", 1)
	if !ok || e.MiB != 1100 || e.Cause == needstore.CauseLoad {
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
	// A one-device measurement of q's shape on a: q only. Not split (q's
	// shape hosted on a, but on two devices: never across device counts),
	// q2 (hosted on b), other (another shape), srv or auto (not hosted).
	h.c.putHostOverhead(measured(shapeOf(h.c.cfg.Profiles["q"]), "a", 1, 600, h.clk.Now(), h.c.llamaBuild))
	after := plans()
	var changed []string
	for n := range after {
		if !reflect.DeepEqual(before[n], after[n]) {
			changed = append(changed, n)
		}
	}
	slices.Sort(changed)
	want := []string{"q"}
	if shapeOf(h.c.cfg.Profiles["split"]) != shapeOf(h.c.cfg.Profiles["q"]) {
		t.Fatal("split must have q's shape for this test")
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

// measured is a stored one-reading row.
func measured(shape string, node core.NodeID, devices, mib int, at time.Time, build string) *hostOH {
	e := &hostOH{HostOverhead: needstore.HostOverhead{Shape: shape, Node: string(node), Devices: devices, LlamaBuild: build, Profile: "q"}}
	e.Add(needstore.HostReading{MiB: mib, At: at, Cause: needstore.CauseLoad}, false)
	return e
}

// againOnA unloads q, then loads it again on a with the node giving up used.
func againOnA(h *harness, used int) {
	h.t.Helper()
	if h.c.insts["q"] != nil {
		h.step(evUnload{profile: "q", force: true})
	}
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 15000 })
	h.viewChanged()
	h.advance(2 * time.Minute) // the node's release window
	h.viewChanged()
	hostLoad(h, used)
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
}

// Keep the higher: a lower reading does not replace the figure in force;
// a higher one does; three consecutive lower readings bring it down to the
// highest of the three. The plan names the figure in force and its date.
func TestHostOverhead_KeepsTheHigher(t *testing.T) {
	h := ohHarness(t)
	q := h.c.cfg.Profiles["q"]
	hostLoad(h, 4401) // 401
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	e0, _ := h.c.hostOverheadAt(shapeOf(q), "a", 1)
	first := e0.MeasuredAt
	againOnA(h, 4298) // 298: kept 401
	e, _ := h.c.hostOverheadAt(shapeOf(q), "a", 1)
	if e.MiB != 401 || !e.MeasuredAt.Equal(first) || len(e.Readings) != 2 || e.Readings[0].MiB != 298 {
		t.Fatalf("after a lower reading %+v", e.HostOverhead)
	}
	if ev := h.histTypes(history.TLearnedNeed); ev[len(ev)-1].Data["rule"] != needstore.HostKept || ev[len(ev)-1].Data["in_force_mib"] != 401 {
		t.Fatalf("history %+v", ev[len(ev)-1])
	}
	h.step(evUnload{profile: "q", force: true})
	if r := h.c.dryPlan("q"); r.HostOverheadMiB != 401 || !r.HostOverheadMeasuredAt.Equal(first) {
		t.Fatalf("plan %d %v", r.HostOverheadMiB, r.HostOverheadMeasuredAt)
	}
	againOnA(h, 4450) // higher: 450
	if e, _ := h.c.hostOverheadAt(shapeOf(q), "a", 1); e.MiB != 450 {
		t.Fatalf("higher %+v", e.HostOverhead)
	}
	againOnA(h, 4300)
	againOnA(h, 4320)
	if e, _ := h.c.hostOverheadAt(shapeOf(q), "a", 1); e.MiB != 450 {
		t.Fatalf("two lower %+v", e.HostOverhead)
	}
	againOnA(h, 4310) // the third consecutive lower reading
	if e, _ := h.c.hostOverheadAt(shapeOf(q), "a", 1); e.MiB != 320 || len(e.Readings) != 3 {
		t.Fatalf("three lower %+v", e.HostOverhead)
	}
	// A new llama build: the next reading replaces the figure.
	h.c.llamaBuild = "ccccccc3"
	againOnA(h, 4250)
	if e, _ := h.c.hostOverheadAt(shapeOf(q), "a", 1); e.MiB != 250 || len(e.Readings) != 1 {
		t.Fatalf("new build %+v", e.HostOverhead)
	}
}

// Keyed by device count: q2's two-device host on a keeps the setting after
// q's one-device measurement there, and is measured on its own.
func TestHostOverhead_NeverAcrossDeviceCounts(t *testing.T) {
	h := ohHarness(t)
	q := h.c.cfg.Profiles["q"]
	h.llama("split", 6000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.Host, p.Model, p.TTL = []string{"a", "b"}, []int{1, 1}, core.HostNode, q.Model, ptrDur(0)
	})
	h.viewChanged()
	h.c.putHostOverhead(measured(shapeOf(q), "a", 1, 401, h.clk.Now(), h.c.llamaBuild))
	if r := h.c.dryPlan("split"); r.HostOverheadMiB != 1024 || r.HostOverheadSource != HostOverheadFromSetting {
		t.Fatalf("split %d %q", r.HostOverheadMiB, r.HostOverheadSource)
	}
	if r := h.c.dryPlan("q"); r.HostOverheadMiB != 401 {
		t.Fatalf("q %d", r.HostOverheadMiB)
	}
	h.c.putHostOverhead(measured(shapeOf(q), "a", 2, 700, h.clk.Now(), h.c.llamaBuild))
	if r := h.c.dryPlan("split"); r.HostOverheadMiB != 700 || r.HostOverheadSource != HostOverheadMeasured {
		t.Fatalf("split own %d %q", r.HostOverheadMiB, r.HostOverheadSource)
	}
	if r := h.c.dryPlan("q"); r.HostOverheadMiB != 401 {
		t.Fatalf("q %d", r.HostOverheadMiB)
	}
}

// The correction of a row copied from the release before device counts:
// it held 298 (its latest reading) after 401. At start its readings are
// read back from the history: only q's shape, on a, on one device. The
// figure in force becomes 401 and a history line says why. Every plan row
// then equals the setting-only plan's, except q's host row, 1024 - 401
// lower: the split host on a (two devices) is back on the setting, which
// the release before had planned on 298.
func TestHostOverhead_CorrectsTheCopiedRowFromTheHistory(t *testing.T) {
	h := ohHarness(t)
	reserveOnSettings(h)
	q := h.c.cfg.Profiles["q"]
	h.llama("split", 6000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.Host, p.Model, p.TTL = []string{"a", "b"}, []int{1, 1}, core.HostNode, q.Model, ptrDur(0)
	})
	h.llama("srv", 4000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.viewChanged()
	t1, t2 := h.clk.Now().Add(-2*time.Hour), h.clk.Now().Add(-time.Hour)
	copied := &hostOH{HostOverhead: needstore.HostOverhead{Shape: shapeOf(q), Node: "a", Devices: 1, MiB: 298, MeasuredAt: t2,
		LlamaBuild: h.c.llamaBuild, Profile: "q", Cause: "load"}, restored: true}
	reads := 0
	h.c.hostEventsFn = func() []history.Event {
		reads++
		ev := func(ts time.Time, profile string, nodes []string, mib any) history.Event {
			return history.Event{Ts: ts.UnixMilli(), Type: history.TLearnedNeed, Profile: profile, Nodes: nodes,
				Data: map[string]any{"node": nodes[0], "host_overhead_mib": mib, "cause": "load"}}
		}
		return []history.Event{
			ev(t2, "q", []string{"a"}, float64(298)),
			ev(t1, "q", []string{"a"}, float64(401)),
			ev(t1, "split", []string{"a", "b"}, float64(900)), // two devices: another row
			ev(t1, "q2", []string{"b"}, float64(999)),         // another machine
			ev(t1, "srv", []string{"a"}, float64(950)),        // another shape (hosted on the scheduler)
			{Ts: t1.UnixMilli(), Type: history.TLearnedNeed, Profile: "q", Nodes: []string{"a"}, Data: map[string]any{"measured_mib": 4000}},
		}
	}
	plans := func() map[string]PlanReport {
		out := map[string]PlanReport{}
		for name := range h.c.cfg.Profiles {
			out[name] = h.c.dryPlan(name)
		}
		return out
	}
	h.cfg.Defaults.HostOverheadSettingOnly = true
	setting := plans()
	h.cfg.Defaults.HostOverheadSettingOnly = false
	h.c.putHostOverhead(copied)
	h.viewChanged() // a step: the node is known, the row is corrected
	e, _ := h.c.hostOverheadAt(shapeOf(q), "a", 1)
	if e.MiB != 401 || !e.MeasuredAt.Equal(t1.Truncate(time.Millisecond)) || len(e.Readings) != 2 || e.Readings[0].MiB != 298 || e.Pending() {
		t.Fatalf("corrected %+v", e.HostOverhead)
	}
	line := h.histTypes(history.TLearnedNeed)
	if len(line) == 0 || line[len(line)-1].Data["kept_mib"] != 401 || line[len(line)-1].Data["was_mib"] != 298 {
		t.Fatalf("history %+v", line)
	}
	h.viewChanged()
	if reads != 1 || len(h.histTypes(history.TLearnedNeed)) != len(line) {
		t.Fatalf("corrected once: %d reads, %d lines", reads, len(h.histTypes(history.TLearnedNeed)))
	}
	after := plans()
	for name, a := range after {
		b := setting[name]
		if a.Memory == nil || b.Memory == nil || len(a.Memory.Nodes) != len(b.Memory.Nodes) {
			t.Fatalf("%s: memory %+v %+v", name, a.Memory, b.Memory)
		}
		for i := range a.Memory.Nodes {
			want := b.Memory.Nodes[i].WillTakeMiB
			if name == "q" {
				want -= 1024 - 401
			}
			if got := a.Memory.Nodes[i].WillTakeMiB; got != want {
				t.Errorf("%s row %s: will take %d, want %d", name, a.Memory.Nodes[i].Node, got, want)
			}
		}
		if name != "q" && !reflect.DeepEqual(a, b) {
			t.Errorf("%s changed:\n%+v\n%+v", name, b, a)
		}
	}
	if after["split"].HostOverheadMiB != 1024 || after["split"].HostOverheadSource != HostOverheadFromSetting || after["q"].HostOverheadMiB != 401 {
		t.Fatalf("split %d %q q %d", after["split"].HostOverheadMiB, after["split"].HostOverheadSource, after["q"].HostOverheadMiB)
	}
}

// A row lists only the profiles that use it: a forced profile when its host
// node (the first forced node) and its device count are the row's; a
// profile with no forced nodes on any row of its shape (its placement
// decides the node and the count).
func TestHostOverhead_RowListsOnlyTheProfilesThatUseIt(t *testing.T) {
	h := ohHarness(t)
	q := h.c.cfg.Profiles["q"]
	h.llama("split", 6000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.Host, p.Model, p.TTL = []string{"a", "b"}, []int{1, 1}, core.HostNode, q.Model, ptrDur(0)
	})
	h.llama("free", 4000, func(p *core.Profile) { p.Host, p.Model, p.TTL = core.HostNode, q.Model, ptrDur(0) })
	h.viewChanged()
	h.c.putHostOverhead(measured(shapeOf(q), "a", 1, 401, h.clk.Now(), h.c.llamaBuild))
	h.c.putHostOverhead(measured(shapeOf(q), "a", 2, 251, h.clk.Now(), h.c.llamaBuild))
	got := map[int][]string{}
	for _, r := range h.c.hostOverheadRows() {
		got[r.Devices] = r.Profiles
	}
	want := map[int][]string{1: {"free", "q"}, 2: {"free", "split"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles %v, want %v", got, want)
	}
}

// A reading needs a settled base: an instance that ran left the host within
// one free_average_window before this one was created, so the free figure
// taken as the base may still show it (the memory is still being returned),
// and base - free would come out low. No reading then, and no warning; a
// release longer ago than the window gives the reading.
func TestHostOverhead_NoReadingFromAnUnsettledBase(t *testing.T) {
	for _, tc := range []struct {
		name string
		ago  time.Duration
		want bool
	}{{"just released", 10 * time.Second, false}, {"settled", 31 * time.Second, true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := ohHarness(t)
			h.c.ns("a").releasedAt = h.clk.Now().Add(-tc.ago)
			hostLoad(h, 4600)
			h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
			if _, ok := h.c.hostOverheadAt(shapeOf(h.c.cfg.Profiles["q"]), "a", 1); ok != tc.want {
				t.Fatalf("reading %v, want %v", ok, tc.want)
			}
			for _, ev := range h.histTypes(history.TLearnedNeed) {
				if ev.Severity != history.Info {
					t.Fatalf("warned: %+v", ev)
				}
			}
		})
	}
}
