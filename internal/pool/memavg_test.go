package pool

import (
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
