package pool

import (
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"strconv"
	"testing"
	"time"
)

// The short average of free memory: the heartbeats within
// free_average_window are averaged; older ones drop out; the node gets
// FreeAvgMiB (the placement figure, floor taken off on a shared-memory node)
// and its facts mem_available_avg_mib.
func TestFreeMemoryAverageWindow(t *testing.T) {
	r := newRig(t)
	r.cfg.Defaults.FreeAverageWindow = 30 * time.Second
	const id = "02:00:00:00:00:72"
	r.p.Register(regReq(id))
	hb := func(avail int) {
		t.Helper()
		b := `{"agent_start_id":"s1","seq":1,"programs_ready":true,"model_source":{"kind":"share","reachable":true},` +
			`"mem_available_mib":` + strconv.Itoa(avail) + `,"devices":[{"name":"Vulkan0","backend":"vulkan","total_mib":14336,"free_mib":14000,"shares_system_memory":true}]}`
		if res := r.p.Heartbeat(id, []byte(b)); res.Status != 200 {
			t.Fatalf("heartbeat %+v", res)
		}
	}
	check := func(free, avg int, availAvg int64) {
		t.Helper()
		n, _ := r.node(id)
		if n.FreeMiB != free || !n.FreeAvgKnown || n.FreeAvgMiB != avg {
			t.Fatalf("free %d avg %d known %v, want %d %d", n.FreeMiB, n.FreeAvgMiB, n.FreeAvgKnown, free, avg)
		}
		f := r.p.Facts()[0]
		if f.MemAvailAvgMiB == nil || *f.MemAvailAvgMiB != availAvg {
			t.Fatalf("mem_available_avg_mib %v, want %d", f.MemAvailAvgMiB, availAvg)
		}
	}
	hb(9000)
	check(8000, 8000, 9000) // one sample: the average is the sample
	r.clk.advance(10 * time.Second)
	hb(12000) // a lucky sample: the latest is high, the average is not
	check(11000, 9500, 10500)
	r.clk.advance(10 * time.Second)
	hb(6000)
	check(5000, 8000, 9000)
	// 25 s later the first two samples are older than the window.
	r.clk.advance(25 * time.Second)
	hb(9000)
	check(8000, 6500, 7500)
	// A window of 0 keeps the latest sample only.
	r.cfg.Defaults.FreeAverageWindow = 0
	r.clk.advance(time.Second)
	hb(10000)
	check(9000, 9000, 10000)
}

// N7: on a shared-memory node the average is the average of each sample's
// usable figure, min(device free, available - floor), not the smaller of
// the two averages: when the binding figure alternates, the latter is too
// high. Floor 1000: samples (dev 14000, avail 6000) and (dev 5000, avail
// 14000) are each 5000 usable; the averages would give min(9500, 9000).
func TestFreeMemoryAverageOfSampleMinimums(t *testing.T) {
	r := newRig(t)
	r.cfg.Defaults.FreeAverageWindow = 30 * time.Second
	const id = "02:00:00:00:00:73"
	r.p.Register(regReq(id))
	hb := func(avail, dev int) {
		t.Helper()
		b := `{"agent_start_id":"s1","seq":1,"programs_ready":true,"model_source":{"kind":"share","reachable":true},` +
			`"mem_available_mib":` + strconv.Itoa(avail) + `,"devices":[{"name":"Vulkan0","backend":"vulkan","total_mib":14336,"free_mib":` +
			strconv.Itoa(dev) + `,"shares_system_memory":true}]}`
		if res := r.p.Heartbeat(id, []byte(b)); res.Status != 200 {
			t.Fatalf("heartbeat %+v", res)
		}
	}
	hb(6000, 14000)
	r.clk.advance(5 * time.Second)
	hb(14000, 5000)
	n, _ := r.node(id)
	if !n.FreeAvgKnown || n.FreeAvgMiB != 5000 {
		t.Fatalf("average %d (known %v), want 5000", n.FreeAvgMiB, n.FreeAvgKnown)
	}
}

// N2: with placement_uses_reserved off (the defaults), a change of the
// average alone does not notify the scheduler (no decision reads it); with
// it on, it does.
func TestAverageAloneNotifiesOnlyWhenUsed(t *testing.T) {
	for _, on := range []bool{false, true} {
		r := newRig(t)
		r.cfg.Defaults.FreeAverageWindow = 30 * time.Second
		r.cfg.Defaults.PlacementUsesReserved = on
		const id = "02:00:00:00:00:74"
		r.p.Register(regReq(id))
		hb := func(dev int) {
			t.Helper()
			b := `{"agent_start_id":"s1","seq":1,"programs_ready":true,"model_source":{"kind":"share","reachable":true},` +
				`"mem_available_mib":30000,"devices":[{"name":"CUDA0","backend":"cuda","total_mib":16000,"free_mib":` + strconv.Itoa(dev) + `}]}`
			if res := r.p.Heartbeat(id, []byte(b)); res.Status != 200 {
				t.Fatalf("heartbeat %+v", res)
			}
		}
		hb(14000)
		r.clk.advance(time.Second)
		hb(12000) // the latest changes: notified either way
		drain(r.p.Changed())
		r.clk.advance(time.Second)
		hb(12000) // only the average moves (13000 to 12666)
		if got := drain(r.p.Changed()); got != on {
			t.Fatalf("setting %v: notified %v", on, got)
		}
	}
}

// ES2 (eighth review): a node carries the time of its memory reading. A
// heartbeat that changes nothing but that time does not notify, unless the
// scheduler waits for a reading of that node after a given time
// (WantFigures); then the first such reading notifies, once.
func TestFigureAtAndWantFigures(t *testing.T) {
	r := newRig(t)
	const id = "02:00:00:00:00:75"
	r.p.Register(regReq(id))
	hb := func() {
		t.Helper()
		b := `{"agent_start_id":"s1","seq":1,"programs_ready":true,"model_source":{"kind":"share","reachable":true},` +
			`"mem_available_mib":30000,"devices":[{"name":"CUDA0","backend":"cuda","total_mib":16000,"free_mib":12000}]}`
		if res := r.p.Heartbeat(id, []byte(b)); res.Status != 200 {
			t.Fatalf("heartbeat %+v", res)
		}
	}
	hb()
	drain(r.p.Changed())
	n, _ := r.node(id)
	if n.FigureAt.IsZero() || r.clk.now().Sub(n.FigureAt) > time.Second {
		t.Fatalf("FigureAt %v, want the heartbeat's time", n.FigureAt)
	}
	r.clk.advance(time.Second)
	hb()
	if drain(r.p.Changed()) {
		t.Fatal("a reading that changes nothing but its time notified")
	}
	r.p.WantFigures(map[core.NodeID]time.Time{id: r.clk.now()})
	r.clk.advance(time.Second)
	hb()
	if !drain(r.p.Changed()) {
		t.Fatal("the reading the scheduler waits for did not notify")
	}
	// each further reading notifies while the scheduler still waits (it
	// decides whether one is fresh enough), but the same reading only once
	r.clk.advance(time.Second)
	hb()
	if !drain(r.p.Changed()) {
		t.Fatal("a further reading of a waited-on node did not notify")
	}
	r.p.rebuildLocked2()
	if drain(r.p.Changed()) {
		t.Fatal("the same reading notified twice")
	}
	// NB1: a worker launch end on the node is seen, and the readings after
	// it are counted
	r.p.mu.Lock()
	r.p.launches["l-1"] = &launch{id: "l-1", node: id}
	r.p.endLaunchLocked(r.p.launches["l-1"])
	r.p.unlock()
	if !drain(r.p.Changed()) {
		t.Fatal("the launch end of a waited-on node did not notify")
	}
	if n, _ := r.node(id); n.StopSeenAt.IsZero() || n.FiguresSinceStop != 0 {
		t.Fatalf("after the stop: seen %v, readings %d", n.StopSeenAt, n.FiguresSinceStop)
	}
	r.clk.advance(time.Second)
	hb()
	r.clk.advance(time.Second)
	hb()
	if n, _ := r.node(id); n.FiguresSinceStop != 2 {
		t.Fatalf("readings since the stop %d, want 2", n.FiguresSinceStop)
	}
	r.p.WantFigures(nil)
	drain(r.p.Changed())
	r.clk.advance(time.Second)
	hb()
	if drain(r.p.Changed()) {
		t.Fatal("a reading notified after the scheduler stopped waiting")
	}
}

// rebuildLocked2 rebuilds the view under the lock (a test helper).
func (p *Pool) rebuildLocked2() {
	p.mu.Lock()
	defer p.unlock()
	p.rebuildLocked()
}
