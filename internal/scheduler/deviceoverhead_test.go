package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	h.llama("e1", 0, func(p *core.Profile) {
		p.ForceNodes, p.TTL, p.Model, p.LoadTimeout = []string{"a"}, ptrDur(0), e.Model, time.Hour
	})
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
// with a history line, and a count line is written either way. The fixture
// has the shape of live's events for the cached TinyLlama reload: the
// reading was taken by a generated benchmark profile that is no longer in
// the configuration; the events name the consoles by hostname while the
// rows are keyed by node id; the reading's learnedNeed comes one average
// window after the loadFinished; the events are on disk in the history
// directory and read back by a fresh history store, as after a restart. A
// row from a cold load, and a row with no events, are kept.
func TestDeviceOverhead_RowsFromCachedLoadsDroppedAtStart(t *testing.T) {
	h := devHarness(t)
	for _, id := range []core.NodeID{"a", "b"} {
		h.view.modify(id, func(n *core.Node) { n.Hostname = "console-" + string(id) })
	}
	h.viewChanged()
	st := openNeedStore(t)
	h.c.restoreNeeds(st)
	e := h.c.cfg.Profiles["e"]
	ready := h.clk.Now().Add(-time.Hour).Truncate(time.Millisecond)
	readAt := ready.Add(36 * time.Second)
	coldAt := ready.Add(-time.Hour)
	for _, r := range []struct {
		node core.NodeID
		mib  int
	}{{"a", 82}, {"b", 39}} {
		row := devMeasured(shapeOf(e), r.node, 2, r.mib, readAt, h.c.llamaBuild)
		row.Profile = "bench-gone"
		h.c.putDeviceOverhead(row)
	}
	h.c.putDeviceOverhead(devMeasured(shapeOf(e), "a", 1, 300, coldAt, h.c.llamaBuild))                 // a cold load (e1)
	h.c.putDeviceOverhead(devMeasured(shapeOf(e), "b", 1, 250, coldAt.Add(-time.Hour), h.c.llamaBuild)) // no events
	flushNeeds(h, st)

	dir := t.TempDir()
	hs := history.Open(history.Options{Dir: dir})
	both := []string{"console-a", "console-b"}
	detail := func(a, b float64) []map[string]any {
		return []map[string]any{{"node": "console-a", "bytes": a, "memory_mib": 430.0}, {"node": "console-b", "bytes": b, "memory_mib": 389.0}}
	}
	for _, ev := range []history.Event{
		{Ts: coldAt.Add(-40 * time.Second).UnixMilli(), Type: history.TLoadFinished, Profile: "e1", Nodes: []string{"console-a"},
			Data: map[string]any{"outcome": "ready", "bytes_measured": true, "bytes": 3600 << 20, "nodes_detail": []map[string]any{{"node": "console-a", "bytes": 3600 << 20, "memory_mib": 4350.0}}}},
		{Ts: coldAt.UnixMilli(), Type: history.TLearnedNeed, Profile: "e1", Nodes: []string{"console-a"},
			Data: map[string]any{"node": "console-a", "device_overhead_mib": 300, "devices": 1}},
		// an earlier cold load of the benchmark profile, then the cached reload that measured
		{Ts: ready.Add(-30 * time.Minute).UnixMilli(), Type: history.TLoadFinished, Profile: "bench-gone", Nodes: both,
			Data: map[string]any{"outcome": "ready", "bytes_measured": true, "bytes": 2 * 3600 << 20, "nodes_detail": detail(3600<<20, 3600<<20)}},
		{Ts: ready.UnixMilli(), Type: history.TLoadFinished, Profile: "bench-gone", Nodes: both,
			Data: map[string]any{"outcome": "ready", "bytes_measured": true, "bytes": 63504, "load_s": 4.0, "nodes_detail": detail(34062, 29442)}},
		{Ts: readAt.UnixMilli(), Type: history.TLearnedNeed, Profile: "bench-gone", Nodes: both,
			Data: map[string]any{"node": "console-a", "device_overhead_mib": 82, "devices": 2}},
		{Ts: readAt.UnixMilli(), Type: history.TLearnedNeed, Profile: "bench-gone", Nodes: both,
			Data: map[string]any{"node": "console-b", "device_overhead_mib": 39, "devices": 2}},
	} {
		ev.Summary = ev.Profile + " finished loading in 4.0 s."
		if ev.Type == history.TLearnedNeed {
			ev.Summary = "Measured the per-device overhead of " + ev.Profile + " (as the image writes it)."
		}
		hs.Record(ev)
	}
	hs.Close()
	hs = history.Open(history.Options{Dir: dir}) // a restart: read back from disk
	t.Cleanup(hs.Close)

	h = devHarness(t)
	for _, id := range []core.NodeID{"a", "b"} {
		h.view.modify(id, func(n *core.Node) { n.Hostname = "console-" + string(id) })
	}
	h.viewChanged()
	h.c.restoreNeeds(st)
	h.c.devEventsFn = hs.Query
	h.step(evTick{})
	h.step(evTick{})
	if _, ok := devRow(h, shapeOf(e), "a", 2); ok {
		t.Fatal("the cached row on a is kept")
	}
	if _, ok := devRow(h, shapeOf(e), "b", 2); ok {
		t.Fatal("the cached row on b is kept")
	}
	if _, ok := devRow(h, shapeOf(e), "a", 1); !ok {
		t.Fatal("the cold row is dropped")
	}
	if _, ok := devRow(h, shapeOf(e), "b", 1); !ok {
		t.Fatal("the row with no events is dropped")
	}
	lines, counts := 0, 0
	for _, x := range h.histTypes(history.TLearnedNeed) {
		if _, ok := x.Data["dropped_device_overhead_mib"]; ok {
			lines++
		}
		if x.Data["device_rows_checked"] == 4 && x.Data["device_rows_dropped"] == 2 && x.Data["device_rows_cold"] == 1 && x.Data["device_rows_older_than_history"] == 1 && x.Data["device_rows_not_found"] == 0 {
			counts++
		}
	}
	if lines != 2 || counts != 1 {
		t.Fatalf("drop lines %d, count lines %d", lines, counts)
	}
	flushNeeds(h, st)
	if rows, _ := st.AllDevice(); len(rows) != 2 {
		t.Fatalf("stored %+v", rows)
	}
}

// At start the fleet view fills node by node. Events name a console by its
// hostname, which is known only once that console is in the view; the check
// waits for every restored row's node (at most two minutes, then it goes on
// with what it knows) instead of running on the first node seen.
func TestDeviceOverhead_DropWaitsForTheRowsNodes(t *testing.T) {
	h := devHarness(t)
	h.view.modify("b", func(n *core.Node) { n.Hostname = "console-b" })
	h.viewChanged()
	st := openNeedStore(t)
	h.c.restoreNeeds(st)
	e := h.c.cfg.Profiles["e"]
	readAt := h.clk.Now().Add(-time.Hour).Truncate(time.Millisecond)
	row := devMeasured(shapeOf(e), "b", 2, 39, readAt, h.c.llamaBuild)
	h.c.putDeviceOverhead(row)
	flushNeeds(h, st)
	evs := []history.Event{
		{Ts: readAt.Add(-36 * time.Second).UnixMilli(), Type: history.TLoadFinished, Profile: "e", Nodes: []string{"a", "console-b"},
			Data: map[string]any{"bytes_measured": true, "nodes_detail": []any{map[string]any{"node": "console-b", "bytes": 29442.0, "memory_mib": 389.0}}}},
		{Ts: readAt.UnixMilli(), Type: history.TLearnedNeed, Profile: "e", Nodes: []string{"a", "console-b"},
			Data: map[string]any{"node": "console-b", "device_overhead_mib": 39.0, "devices": 2.0}},
	}
	h = devHarness(t)
	h.view.modify("b", func(n *core.Node) { n.Hostname = "console-b" })
	h.viewChanged()
	h.c.restoreNeeds(st)
	h.c.devEventsFn = func(q history.Query) history.Result {
		var out []history.Event
		for i := len(evs) - 1; i >= 0; i-- {
			if q.Match(&evs[i]) && (q.Until == 0 || evs[i].Ts <= q.Until) && (q.Since == 0 || evs[i].Ts >= q.Since) {
				out = append(out, evs[i])
			}
		}
		return history.Result{Events: out, OldestTs: evs[0].Ts}
	}
	b := h.c.raw["b"]
	delete(h.c.raw, "b") // b's first heartbeat has not come yet
	h.c.dropCachedDevice()
	if _, ok := devRow(h, shapeOf(e), "b", 2); !ok || h.c.devChecked {
		t.Fatal("checked before b was known")
	}
	h.c.raw["b"] = b
	h.c.dropCachedDevice()
	if _, ok := devRow(h, shapeOf(e), "b", 2); ok {
		t.Fatal("the cached row on b is kept once b is known")
	}
}

// With nothing stored, the count line still says so (a proof reads it).
func TestDeviceOverhead_CountLineWhenNothingIsStored(t *testing.T) {
	h := devHarness(t)
	h.c.devEventsFn = func(history.Query) history.Result { return history.Result{} }
	h.step(evTick{})
	for _, x := range h.histTypes(history.TLearnedNeed) {
		if x.Data["device_rows_checked"] == 0 && x.Data["device_rows_dropped"] == 0 {
			return
		}
	}
	t.Fatal("no count line")
}

// The same settled-base rule per worker: b had an instance leave it just
// before; a had not. Only a gives a reading.
func TestDeviceOverhead_NoReadingFromAnUnsettledBase(t *testing.T) {
	h := devHarness(t)
	h.c.ns("b").releasedAt = h.clk.Now().Add(-5 * time.Second)
	twoConsoleLoad(h, 4600, 4300)
	h.advance(h.cfg.Defaults.FreeAverageWindow + time.Second)
	e := h.c.cfg.Profiles["e"]
	if _, ok := devRow(h, shapeOf(e), "a", 2); !ok {
		t.Fatal("no reading on a")
	}
	if _, ok := devRow(h, shapeOf(e), "b", 2); ok {
		t.Fatal("a reading on b from an unsettled base")
	}
}

// liveShapeHistory writes, as the history file holds them (one JSON object
// per line: ts in epoch milliseconds, seq, type, severity, profile,
// model_file, nodes, session_id, summary, data), the ten events of a cached
// two-console reload of a generated benchmark profile, from loadStarted to
// instanceUnloaded, with generic ids. ready is the loadFinished time; the
// readings come one average window (30 s) and 1 ms later... exactly as live
// stamps them: loadFinished at T, the readings at T + 30.001 s.
func liveShapeHistory(t *testing.T, dir string, ready time.Time) {
	t.Helper()
	start := ready.Add(-4013 * time.Millisecond).UnixMilli()
	fin := ready.UnixMilli()
	read := ready.Add(30001 * time.Millisecond).UnixMilli()
	both := []string{"console-a", "console-b"}
	sess := fmt.Sprintf("ld-%d-13", start)
	mem := func(ts int64, seq int, node, why string) map[string]any {
		return map[string]any{"ts": ts, "seq": seq, "type": "nodeMemory", "severity": "info", "profile": "bench-gen-auto-2048", "model_file": "m.gguf",
			"nodes": []string{node}, "summary": "Memory on " + node + ".", "data": map[string]any{"available_mib": 12449, "reason": why}}
	}
	lines := []map[string]any{
		{"ts": start, "seq": 23, "type": "loadStarted", "severity": "info", "profile": "bench-gen-auto-2048", "model_file": "m.gguf", "nodes": both, "session_id": sess,
			"summary": "Loading bench-gen-auto-2048 onto console-a, console-b.", "data": map[string]any{"bytes_total": 668788096, "need_mib": 682,
				"cache_verdict": []map[string]any{{"node": "console-a", "id": "a", "verdict": "full"}, {"node": "console-b", "id": "b", "verdict": "full"}}}},
		mem(start, 24, "console-a", "load_start"), mem(start, 25, "console-b", "load_start"),
		{"ts": fin, "seq": 26, "type": "loadFinished", "severity": "info", "profile": "bench-gen-auto-2048", "model_file": "m.gguf", "nodes": both, "session_id": sess,
			"summary": "bench-gen-auto-2048 finished loading in 4.0 s (0.0 MB/s).", "data": map[string]any{"avg_mb_s": 0, "bytes": 63504, "bytes_measured": true, "load_s": 4,
				"nodes_detail": []map[string]any{{"bytes": 34062, "memory_mib": 430.5, "node": "console-a"}, {"bytes": 29442, "memory_mib": 388.6, "node": "console-b"}},
				"outcome":      "ready", "reason": ""}},
		mem(fin, 27, "console-a", "load_end"), mem(fin, 28, "console-b", "load_end"),
		{"ts": fin, "seq": 29, "type": "learnedNeed", "severity": "info", "profile": "bench-gen-auto-2048", "model_file": "m.gguf", "nodes": both, "session_id": sess,
			"summary": "Measured the memory need of bench-gen-auto-2048 at 820 MiB.", "data": map[string]any{"highest_mib": 820, "measured_mib": 820}},
		{"ts": read, "seq": 30, "type": "learnedNeed", "severity": "info", "profile": "bench-gen-auto-2048", "model_file": "m.gguf", "nodes": both, "session_id": sess,
			"summary": "Measured the per-device overhead of bench-gen-auto-2048 on console-a at 82 MiB (2 devices); the figure in force. device_overhead_mib is 600.",
			"data":    map[string]any{"device_overhead_mib": 82, "devices": 2, "in_force_mib": 82, "log_part_mib": 82, "node": "console-a", "rule": "first", "setting_mib": 600}},
		{"ts": read, "seq": 31, "type": "learnedNeed", "severity": "info", "profile": "bench-gen-auto-2048", "model_file": "m.gguf", "nodes": both, "session_id": sess,
			"summary": "Measured the per-device overhead of bench-gen-auto-2048 on console-b at 39 MiB (2 devices); the figure in force. device_overhead_mib is 600.",
			"data":    map[string]any{"device_overhead_mib": 39, "devices": 2, "in_force_mib": 39, "log_part_mib": 39, "node": "console-b", "rule": "first", "setting_mib": 600}},
		{"ts": ready.Add(38 * time.Second).UnixMilli(), "seq": 32, "type": "instanceUnloaded", "severity": "info", "profile": "bench-gen-auto-2048", "model_file": "m.gguf",
			"nodes": both, "session_id": sess, "summary": "bench-gen-auto-2048 was unloaded (api).", "data": map[string]any{"reason": "api"}},
	}
	var buf bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	name := "events-" + ready.Add(-time.Hour).UTC().Format("20060102T150405Z") + ".jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The cached reload exactly as live's history file holds it, read by a
// history store opened on that file (as at start), against the two rows as
// live's store holds them (measured_by the generated profile, measured_at
// the reading's instant). Both rows are dropped, whether the generated
// profile is configured (live: yes, with no force_nodes) or not.
//
// "unnamed": the view knows the consoles by id only (the two-minute wait
// ran out): the single event of that figure, profile and device count in
// the window is the reading, and its own node name finds the load's bytes.
// "consoles late": at start the fleet view holds only the scheduler's own
// machine (it is in the view at once; the consoles come with their first
// heartbeats). A check run then knows the consoles by id only, while the
// events name them by hostname: this is how live kept both rows (the count
// line said "2 kept (load not found in the history)", at every start).
func TestDeviceOverhead_LiveShapedCachedReloadDropped(t *testing.T) {
	for _, tc := range []struct{ configured, late, unnamed bool }{{true, false, false}, {false, false, false}, {true, true, false}, {true, false, true}} {
		configured := tc.configured
		t.Run(fmt.Sprintf("configured %v, consoles late %v, unnamed %v", tc.configured, tc.late, tc.unnamed), func(t *testing.T) {
			h := devHarness(t)
			for _, id := range []core.NodeID{"a", "b"} {
				h.view.modify(id, func(n *core.Node) { n.Hostname = "console-" + string(id) })
			}
			e := h.c.cfg.Profiles["e"]
			if configured {
				h.llama("bench-gen-auto-2048", 0, func(p *core.Profile) { p.Model, p.TTL = e.Model, ptrDur(0) })
			}
			h.viewChanged()
			st := openNeedStore(t)
			h.c.restoreNeeds(st)
			ready := h.clk.Now().Add(-3 * time.Hour).Truncate(time.Millisecond)
			readAt := time.UnixMilli(ready.Add(30001 * time.Millisecond).UnixMilli())
			for _, r := range []struct {
				node core.NodeID
				mib  int
			}{{"a", 82}, {"b", 39}} {
				row := devMeasured(shapeOf(e), r.node, 2, r.mib, readAt, h.c.llamaBuild)
				row.Profile = "bench-gen-auto-2048"
				h.c.putDeviceOverhead(row)
			}
			flushNeeds(h, st)
			dir := t.TempDir()
			liveShapeHistory(t, dir, ready)
			hs := history.Open(history.Options{Dir: dir, MemoryEvents: 3})
			t.Cleanup(hs.Close)

			h2 := devHarness(t)
			for _, id := range []core.NodeID{"a", "b"} {
				h2.view.modify(id, func(n *core.Node) {
					n.Hostname = "console-" + string(id)
					if tc.unnamed {
						n.Hostname = "" // the view knows the console by id only
					}
				})
			}
			if configured {
				h2.llama("bench-gen-auto-2048", 0, func(p *core.Profile) { p.Model, p.TTL = e.Model, ptrDur(0) })
			}
			h2.viewChanged()
			h2.c.restoreNeeds(st)
			h2.c.devEventsFn = hs.Query
			if tc.late {
				ra, rb := h2.c.raw["a"], h2.c.raw["b"]
				delete(h2.c.raw, "a")
				delete(h2.c.raw, "b")
				h2.c.raw["sched"] = core.Node{ID: "sched", Hostname: "scheduler", Local: true, Online: true}
				h2.c.dropCachedDevice() // only the scheduler's machine is in the view
				h2.c.raw["a"], h2.c.raw["b"] = ra, rb
				delete(h2.c.raw, "sched")
			}
			h2.c.dropCachedDevice()
			for _, x := range h2.histTypes(history.TLearnedNeed) {
				if _, ok := x.Data["device_rows_checked"]; ok {
					t.Log(x.Summary)
				}
			}
			if _, ok := devRow(h2, shapeOf(e), "a", 2); ok {
				t.Fatal("the cached row on a is kept")
			}
			if _, ok := devRow(h2, shapeOf(e), "b", 2); ok {
				t.Fatal("the cached row on b is kept")
			}
		})
	}
}
