package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The per-device overhead measured per shape, node and device count
// (deviceoverhead.go).

// devHarness: two consoles a and b (15000 free, headroom 1000); profile e,
// an estimate of 8000 MiB with a 100 MiB compute buffer per device, forced
// on both and hosted on the scheduler's machine (a and b are workers);
// profile e1 has e's shape forced on a alone.
func devHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Defaults.Membership = "agents"
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.c.llamaBuild, h.c.servedBuild = "aaaaaaa1", "aaaaaaa1"
	e := h.llama("e", 0, func(p *core.Profile) { p.ForceNodes, p.TTL, p.LoadTimeout = []string{"a", "b"}, ptrDur(0), time.Hour })
	h.llama("e1", 0, func(p *core.Profile) { p.ForceNodes, p.TTL, p.Model, p.LoadTimeout = []string{"a"}, ptrDur(0), e.Model, time.Hour })
	est := NeedInfo{Need: placer.Estimated(8000), FileBytes: 1 << 32, DeviceMiB: 100}
	h.needs["e"], h.needs["e1"] = est, est
	h.viewChanged()
	return h
}

// twoConsoleLoad loads e on a and b; the log puts 4350 MiB on each, and the
// nodes give up usedA and usedB (their averages follow at once).
func twoConsoleLoad(h *harness, usedA, usedB int) {
	h.t.Helper()
	loadOnTwo(h, usedA, usedB, 3600<<20, 3600<<20) // a cold load: the weights crossed the wire
}

// loadOnTwo is twoConsoleLoad with the bytes the host sent to a and b
// (measured on the wire).
func loadOnTwo(h *harness, usedA, usedB int, sentA, sentB int64) {
	h.t.Helper()
	w := h.request("e")
	h.step(evWorkersLaunched{iid: h.lastLaunch().spec.IID, res: WorkerResult{LaunchIDs: map[core.NodeID]string{}}})
	in := h.inst("e") // its load has begun: the bytes the log progress would report
	in.peerSent = map[string]int64{addrA: sentA, addrB: sentB}
	in.sentBytes = sentA + sentB
	setLoad(h, "e", rpcDev(addrA, 3600, 400, 350), rpcDev(addrB, 3600, 400, 350))
	h.healthy("e")
	h.release(h.granted(w))
	for id, used := range map[core.NodeID]int{"a": usedA, "b": usedB} {
		h.view.modify(id, func(n *core.Node) { n.FreeMiB -= used; n.FreeAvgMiB, n.FreeAvgKnown = n.FreeMiB, true })
	}
	h.viewChanged()
}

func devRow(h *harness, shape string, id core.NodeID, k int) (*hostOH, bool) {
	return h.c.deviceOverheadAt(shape, id, k)
}

// A two-console load gives a reading on each console for two devices, one
// average window after ready: (given up) - share - compute buffer, never
// below the log's part.
func TestDeviceOverhead_ReadOnEveryWorkerForItsDeviceCount(t *testing.T) {
	h := devHarness(t)
	st := openNeedStore(t)
	h.c.restoreNeeds(st)
	e := h.c.cfg.Profiles["e"]
	twoConsoleLoad(h, 4600, 4300)
	if _, ok := devRow(h, shapeOf(e), "a", 2); ok {
		t.Fatal("read before a full average window after ready")
	}
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	// share 4000 (half of 8000), compute 100, log part 4350 - 4100 = 250.
	a, okA := devRow(h, shapeOf(e), "a", 2)
	b, okB := devRow(h, shapeOf(e), "b", 2)
	if !okA || !okB || a.MiB != 500 || a.LogHostMiB != 250 || b.MiB != 250 || b.LogHostMiB != 250 || a.Profile != "e" {
		t.Fatalf("a %+v %v | b %+v %v", a, okA, b, okB)
	}
	if _, ok := devRow(h, shapeOf(e), "a", 1); ok {
		t.Fatal("a one-device row from a two-device load")
	}
	ev := h.histTypes(history.TLearnedNeed)
	n := 0
	for _, x := range ev {
		if _, ok := x.Data["device_overhead_mib"]; ok {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("history %+v", ev)
	}
	// Stored by shape, node and device count.
	flushNeeds(h, st)
	rows, err := st.AllDevice()
	if err != nil || len(rows) != 2 || rows[0].Devices != 2 || rows[0].MiB != 500 {
		t.Fatalf("stored %+v %v", rows, err)
	}
	// Once per cycle.
	h.advance(2 * time.Minute)
	if a, _ := devRow(h, shapeOf(e), "a", 2); len(a.Readings) != 1 {
		t.Fatalf("read twice %+v", a.Readings)
	}
}

// No reading while a node is moving (latest and average more than 128 MiB
// apart), from a configured need, or from a command profile.
func TestDeviceOverhead_NoReadingWhenItCannotBeTrusted(t *testing.T) {
	h := devHarness(t)
	e := h.c.cfg.Profiles["e"]
	twoConsoleLoad(h, 4600, 4300)
	h.view.modify("b", func(n *core.Node) { n.FreeAvgMiB = n.FreeMiB + 400 })
	h.viewChanged()
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	if _, ok := devRow(h, shapeOf(e), "a", 2); ok {
		t.Fatal("read while b was moving")
	}
	h.view.modify("b", func(n *core.Node) { n.FreeAvgMiB = n.FreeMiB })
	h.viewChanged()
	h.advance(time.Second)
	if _, ok := devRow(h, shapeOf(e), "b", 2); !ok {
		t.Fatal("no reading once b settled")
	}

	h = devHarness(t)
	h.needs["e"] = NeedInfo{Need: placer.Measured(8000), FileBytes: 1 << 32}
	delete(h.c.needs_, "e")
	twoConsoleLoad(h, 4600, 4300)
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	if len(h.c.devOHs) != 0 {
		t.Fatalf("read from a configured need %+v", h.c.devOHs)
	}
}

func devMeasured(shape string, node core.NodeID, devices, mib int, at time.Time, build string) *hostOH {
	e := measured(shape, node, devices, mib, at, build)
	e.Profile = "e"
	return e
}

// The figure is used where the shape's need is an estimate: on that node,
// for that device count only. Plans of other nodes, of other device counts
// and of measured needs are unchanged; device_overhead_setting_only plans
// with the setting again.
func TestDeviceOverhead_UsedOnThatNodeAndCountOnly(t *testing.T) {
	h := devHarness(t)
	e := h.c.cfg.Profiles["e"]
	row := func(name string, id core.NodeID) PlanMemoryNode {
		t.Helper()
		r := h.c.dryPlan(name)
		for _, m := range r.Memory.Nodes {
			if m.ID == id {
				return m
			}
		}
		t.Fatalf("%s: no row for %s in %+v", name, id, r.Memory)
		return PlanMemoryNode{}
	}
	a2, b2, a1 := row("e", "a"), row("e", "b"), row("e1", "a")
	if a2.DeviceOverheadMiB != 600 || a2.DeviceOverheadSource != DeviceOverheadFromSetting || !a2.DeviceOverheadMeasuredAt.IsZero() {
		t.Fatalf("before %+v", a2)
	}
	at := h.clk.Now()
	h.c.putDeviceOverhead(devMeasured(shapeOf(e), "a", 2, 250, at, h.c.llamaBuild))
	na2, nb2, na1 := row("e", "a"), row("e", "b"), row("e1", "a")
	if na2.DeviceOverheadMiB != 250 || na2.DeviceOverheadSource != DeviceOverheadMeasured || !na2.DeviceOverheadMeasuredAt.Equal(at) ||
		na2.WillTakeMiB != a2.WillTakeMiB-350 {
		t.Fatalf("a on two devices %+v (was %d)", na2, a2.WillTakeMiB)
	}
	if !reflect.DeepEqual(nb2, b2) || !reflect.DeepEqual(na1, a1) {
		t.Fatalf("b or one device changed:\n%+v\n%+v\n%+v\n%+v", nb2, b2, na1, a1)
	}
	if got := h.c.expectShares(e, core.Placement{Nodes: []core.PlacedNode{{ID: "a"}, {ID: "b"}}}); len(got) != 2 {
		t.Fatalf("shares %v", got)
	}
	h.cfg.Defaults.DeviceOverheadSettingOnly = true
	h.viewChanged()
	if r := row("e", "a"); !reflect.DeepEqual(r, a2) {
		t.Fatalf("setting only %+v", r)
	}
	h.cfg.Defaults.DeviceOverheadSettingOnly = false
	// A measured need takes no per-device overhead.
	h.needs["e"] = NeedInfo{Need: placer.Measured(8000), FileBytes: 1 << 32}
	delete(h.c.needs_, "e")
	h.viewChanged()
	if r := row("e", "a"); r.DeviceOverheadMiB != 0 || r.DeviceOverheadSource != "" {
		t.Fatalf("measured need %+v", r)
	}
}

// The flip list: with a figure on (shape of e, a, 2 devices), every plan
// row of every profile equals the setting-only plan's except e's row on a,
// lower by 600 - 250 in what it will take. And with no figure, the plans
// are the setting-only plans exactly.
func TestDeviceOverhead_FlipListByEveryRow(t *testing.T) {
	h := devHarness(t)
	reserveOnSettings(h)
	h.llama("other", 0, func(p *core.Profile) { p.ForceNodes, p.TTL = []string{"a", "b"}, ptrDur(0) })
	h.needs["other"] = NeedInfo{Need: placer.Estimated(6000), FileBytes: 1 << 31, DeviceMiB: 100}
	h.llama("conf", 5000, func(p *core.Profile) { p.ForceNodes, p.TTL = []string{"a"}, ptrDur(0) })
	h.llama("free", 0, func(p *core.Profile) { p.TTL = ptrDur(0) })
	h.needs["free"] = NeedInfo{Need: placer.Estimated(5000), FileBytes: 1 << 31, DeviceMiB: 100}
	h.viewChanged()
	plans := func() map[string]PlanReport {
		out := map[string]PlanReport{}
		for name := range h.cfg.Profiles {
			out[name] = h.c.dryPlan(name)
		}
		return out
	}
	h.cfg.Defaults.DeviceOverheadSettingOnly = true
	setting := plans()
	h.cfg.Defaults.DeviceOverheadSettingOnly = false
	if got := plans(); !reflect.DeepEqual(got, setting) {
		t.Fatal("plans differ with no figure measured")
	}
	e := h.c.cfg.Profiles["e"]
	h.c.putDeviceOverhead(devMeasured(shapeOf(e), "a", 2, 250, h.clk.Now(), h.c.llamaBuild))
	got := plans()
	for name, want := range setting {
		g := got[name]
		if name != "e" {
			if !reflect.DeepEqual(g, want) {
				t.Errorf("%s changed:\n%+v\n%+v", name, g, want)
			}
			continue
		}
		if g.Action != want.Action || len(g.Memory.Nodes) != len(want.Memory.Nodes) {
			t.Fatalf("e: %s vs %s", g.Action, want.Action)
		}
		for i, r := range g.Memory.Nodes {
			w := want.Memory.Nodes[i]
			if r.ID == "a" {
				if r.WillTakeMiB != w.WillTakeMiB-350 || r.DeviceOverheadSource != DeviceOverheadMeasured {
					t.Errorf("e on a: will_take %d, setting %d", r.WillTakeMiB, w.WillTakeMiB)
				}
				continue
			}
			if !reflect.DeepEqual(r, w) {
				t.Errorf("e on %s changed:\n%+v\n%+v", r.ID, r, w)
			}
		}
	}
}

// Keep the higher, and the row's profiles and used flag: used is false once
// the shape plans from its own measured need.
func TestDeviceOverhead_KeepsTheHigherAndSaysWhenItIsUnused(t *testing.T) {
	h := devHarness(t)
	e := h.c.cfg.Profiles["e"]
	twoConsoleLoad(h, 4600, 4300)
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	again := func(usedA int) {
		h.step(evUnload{profile: "e", force: true})
		h.advance(10 * time.Second)
		h.view.modify("a", func(n *core.Node) { n.FreeMiB, n.FreeAvgMiB = 15000, 15000 })
		h.view.modify("b", func(n *core.Node) { n.FreeMiB, n.FreeAvgMiB = 15000, 15000 })
		h.viewChanged()
		h.advance(2 * time.Minute)
		twoConsoleLoad(h, usedA, 4300)
		h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	}
	again(4400) // 300: kept 500
	if a, _ := devRow(h, shapeOf(e), "a", 2); a.MiB != 500 || len(a.Readings) != 2 {
		t.Fatalf("kept %+v", a.HostOverhead)
	}
	again(4800) // 700: raised
	if a, _ := devRow(h, shapeOf(e), "a", 2); a.MiB != 700 {
		t.Fatalf("raised %+v", a.HostOverhead)
	}
	var rows []HostOverheadRow
	for _, r := range h.c.deviceOverheadRows() {
		if r.Node == "a" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 || !reflect.DeepEqual(rows[0].Profiles, []string{"e"}) || rows[0].Used {
		t.Fatalf("rows %+v", rows)
	}
	if len(h.c.learned[shapeOf(e)]) == 0 {
		t.Fatal("the load stored no measured need")
	}
}

// A cached reload (the workers already hold the tensors: kilobytes cross
// the wire) gives no reading; nor does a node that received less than half
// of what the log puts on it, or a load whose bytes were not measured. A
// cold load does.
func TestDeviceOverhead_OnlyFromAColdLoad(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sentA, sentB int64
		wantA, wantB bool
	}{
		{"cached reload", 34062, 29442, false, false},
		{"b partly cached", 3600 << 20, 1000 << 20, true, false},
		{"half of the log on each", 2175 << 20, 2175 << 20, true, true},
		{"not measured", 0, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := devHarness(t)
			e := h.c.cfg.Profiles["e"]
			loadOnTwo(h, 4600, 4300, tc.sentA, tc.sentB)
			h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
			_, okA := devRow(h, shapeOf(e), "a", 2)
			_, okB := devRow(h, shapeOf(e), "b", 2)
			if okA != tc.wantA || okB != tc.wantB {
				t.Fatalf("a %v b %v, want %v %v", okA, okB, tc.wantA, tc.wantB)
			}
			for _, ev := range h.histTypes(history.TLearnedNeed) {
				if ev.Severity != history.Info {
					t.Fatalf("a cached load warned: %+v", ev)
				}
			}
		})
	}
}

// At start, a stored row whose reading came from a cached load is dropped,
// with a history line saying why: its load is found in the event history
// (the learnedNeed event that recorded the reading, then the loadFinished
// event of that load before it: its bytes to that node). A row from a cold
// load, and a row with no events, are kept.
func TestDeviceOverhead_RowsFromCachedLoadsDroppedAtStart(t *testing.T) {
	h := devHarness(t)
	st := openNeedStore(t)
	h.c.restoreNeeds(st)
	e := h.c.cfg.Profiles["e"]
	t1, t2 := h.clk.Now().Add(-2*time.Hour), h.clk.Now().Add(-time.Hour)
	for _, r := range []struct {
		node core.NodeID
		mib  int
		at   time.Time
	}{{"a", 82, t2}, {"b", 39, t2}} {
		row := devMeasured(shapeOf(e), r.node, 2, r.mib, r.at, h.c.llamaBuild)
		row.Readings[0].At, row.MeasuredAt = r.at, r.at
		h.c.putDeviceOverhead(row)
	}
	cold := devMeasured(shapeOf(h.c.cfg.Profiles["e1"]), "a", 1, 300, t1, h.c.llamaBuild)
	h.c.putDeviceOverhead(cold) // e1 has e's shape on one device: a cold load
	flushNeeds(h, st)
	h = devHarness(t)
	h.c.restoreNeeds(st)
	ev := func(ts time.Time, typ, profile string, nodes []string, data map[string]any) history.Event {
		return history.Event{Ts: ts.UnixMilli(), Type: typ, Profile: profile, Nodes: nodes, Data: data}
	}
	detail := func(a, b float64) []any {
		return []any{map[string]any{"node": "a", "bytes": a, "memory_mib": 430.0}, map[string]any{"node": "b", "bytes": b, "memory_mib": 389.0}}
	}
	reads := 0
	h.c.devEventsFn = func() []history.Event {
		reads++
		return []history.Event{
			ev(t1.Add(-time.Minute), history.TLoadFinished, "e1", []string{"a"},
				map[string]any{"bytes_measured": true, "nodes_detail": []any{map[string]any{"node": "a", "bytes": float64(3600 << 20), "memory_mib": 4350.0}}}),
			ev(t1, history.TLearnedNeed, "e1", []string{"a"}, map[string]any{"node": "a", "device_overhead_mib": 300.0, "devices": 1.0}),
			// an older cold load of e on both, then the cached reload that measured
			ev(t2.Add(-time.Hour), history.TLoadFinished, "e", []string{"a", "b"},
				map[string]any{"bytes_measured": true, "nodes_detail": detail(float64(3600<<20), float64(3600<<20))}),
			ev(t2.Add(-time.Minute), history.TLoadFinished, "e", []string{"a", "b"},
				map[string]any{"bytes_measured": true, "nodes_detail": detail(34062, 29442)}),
			ev(t2, history.TLearnedNeed, "e", []string{"a", "b"}, map[string]any{"node": "a", "device_overhead_mib": 82.0, "devices": 2.0}),
			ev(t2, history.TLearnedNeed, "e", []string{"a", "b"}, map[string]any{"node": "b", "device_overhead_mib": 39.0, "devices": 2.0}),
		}
	}
	h.c.servedBuild = h.c.llamaBuild
	h.step(evTick{})
	h.step(evTick{})
	if reads != 1 {
		t.Fatalf("history read %d times", reads)
	}
	if _, ok := devRow(h, shapeOf(e), "a", 2); ok {
		t.Fatal("the cached row on a is kept")
	}
	if _, ok := devRow(h, shapeOf(e), "b", 2); ok {
		t.Fatal("the cached row on b is kept")
	}
	if _, ok := devRow(h, shapeOf(e), "a", 1); !ok {
		t.Fatal("the cold row is dropped")
	}
	n := 0
	for _, x := range h.histTypes(history.TLearnedNeed) {
		if _, ok := x.Data["dropped_device_overhead_mib"]; ok {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("history lines %d", n)
	}
	flushNeeds(h, st)
	if rows, _ := st.AllDevice(); len(rows) != 1 || rows[0].Devices != 1 {
		t.Fatalf("stored %+v", rows)
	}
}
