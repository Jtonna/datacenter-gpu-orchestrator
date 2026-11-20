package stats

import (
	"fmt"
	"testing"
	"time"
)

func cpuNet(t *testing.T, idle, iowait, user, rx, tx float64) Metrics {
	t.Helper()
	return parseText(t, fmt.Sprintf(`node_cpu_seconds_total{cpu="0",mode="idle"} %g
node_cpu_seconds_total{cpu="0",mode="iowait"} %g
node_cpu_seconds_total{cpu="0",mode="user"} %g
node_cpu_seconds_total{cpu="1",mode="idle"} %g
node_cpu_seconds_total{cpu="1",mode="iowait"} 0
node_cpu_seconds_total{cpu="1",mode="user"} 0
node_network_receive_bytes_total{device="eth0"} %g
node_network_transmit_bytes_total{device="eth0"} %g
`, idle, iowait, user, idle, rx, tx), keepNode)
}

func TestRatesFirstScrapeThenRate(t *testing.T) {
	var c counterState
	c.update(cpuNet(t, 100, 0, 100, 1e6, 2e6), 0)
	r := c.rates
	wantNil(t, "first cpu", r.CPUUtil)
	if len(r.Net) != 0 {
		t.Fatalf("first net = %+v", r.Net)
	}

	// 2 s later: cpu0 idle +30, iowait +10, user +40; cpu1 idle +30
	// d_total = 110, d_idle+d_iowait = 70 => 100*(1-70/110) %.
	c.update(cpuNet(t, 130, 10, 140, 1e6+500000, 2e6+250000), 2*time.Second)
	r = c.rates
	wantF(t, "cpu", r.CPUUtil, wantUtil)
	// 500000 bytes * 8 / 2 s / 1e6 = 2 Mb/s
	wantF(t, "rx", r.Net["eth0"].Rx, 2)
	wantF(t, "tx", r.Net["eth0"].Tx, 1)

	// d_total 0 keeps previous util; net rate becomes 0.
	c.update(cpuNet(t, 130, 10, 140, 1e6+500000, 2e6+250000), 4*time.Second)
	wantF(t, "cpu kept", c.rates.CPUUtil, wantUtil)
	wantF(t, "rx zero", c.rates.Net["eth0"].Rx, 0)
}

func TestRatesCounterResetDiscards(t *testing.T) {
	var c counterState
	c.update(cpuNet(t, 100, 0, 100, 1e6, 2e6), 0)
	c.update(cpuNet(t, 150, 0, 150, 2e6, 3e6), 2*time.Second)
	wantF(t, "cpu", c.rates.CPUUtil, 100*(1-(50.0+50)/(50+50+50)))

	// CPU counter and rx go backwards: no rate, restart from cur.
	c.update(cpuNet(t, 10, 0, 10, 100, 4e6), 4*time.Second)
	wantNil(t, "cpu after reset", c.rates.CPUUtil)
	wantNil(t, "rx after reset", c.rates.Net["eth0"].Rx)
	wantF(t, "tx unaffected", c.rates.Net["eth0"].Tx, 4) // 1e6*8/2/1e6

	// Next sample is measured from the reset values.
	c.update(cpuNet(t, 20, 0, 20, 100+250000, 4e6), 6*time.Second)
	wantF(t, "cpu after restart", c.rates.CPUUtil, 100*(1-20.0/30))
	wantF(t, "rx after restart", c.rates.Net["eth0"].Rx, 1)
}

func TestRatesDtBounds(t *testing.T) {
	var c counterState
	c.update(cpuNet(t, 100, 0, 100, 1e6, 2e6), 0)
	c.update(cpuNet(t, 110, 0, 110, 2e6, 3e6), 400*time.Millisecond) // < 0.5 s
	wantNil(t, "cpu dt too short", c.rates.CPUUtil)
	wantNil(t, "rx dt too short", c.rates.Net["eth0"].Rx)

	c.update(cpuNet(t, 120, 0, 120, 3e6, 4e6), 400*time.Millisecond+31*time.Second) // > 30 s
	wantNil(t, "cpu dt too long", c.rates.CPUUtil)

	// Baseline restarted at the last sample: a 1 s step yields a rate.
	c.update(cpuNet(t, 130, 0, 120, 3e6+125000, 4e6), 400*time.Millisecond+32*time.Second)
	wantF(t, "cpu ok", c.rates.CPUUtil, 0)
	wantF(t, "rx ok", c.rates.Net["eth0"].Rx, 1)

	// Exactly 0.5 s and 30 s are accepted.
	var d counterState
	d.update(cpuNet(t, 100, 0, 100, 0, 0), 0)
	d.update(cpuNet(t, 100, 0, 200, 0, 0), 500*time.Millisecond)
	wantF(t, "dt 0.5", d.rates.CPUUtil, 100)
	d.update(cpuNet(t, 100, 0, 300, 0, 0), 30500*time.Millisecond)
	wantF(t, "dt 30", d.rates.CPUUtil, 100)
}

func TestRatesUtilClamped(t *testing.T) {
	// idle grows more than total cannot happen with real counters, but a
	// per-mode reset in one cpu and growth elsewhere is caught as a reset;
	// here only check the clamp helper.
	if clamp(-3, 0, 100) != 0 || clamp(103, 0, 100) != 100 || clamp(50, 0, 100) != 50 {
		t.Fatal("clamp")
	}
}

func TestRatesSurfaceThroughNormalize(t *testing.T) {
	var c counterState
	c.update(cpuNet(t, 100, 0, 100, 1e6, 2e6), 0)
	m := cpuNet(t, 130, 10, 140, 1e6+500000, 2e6+250000)
	m["node_network_up"] = []Sample{{Labels: map[string]string{"device": "eth0"}, Value: 1}}
	c.update(m, 2*time.Second)
	s := Normalize(Input{Sources: okSources("node"), Node: m, Rates: c.rates, Primary: "eth0"})
	wantF(t, "cpu", s.CPU.UtilPercent, wantUtil)
	wantF(t, "rx", s.Net[0].RxMbps, 2)
	wantF(t, "tx", s.Net[0].TxMbps, 1)
	if *s.CPU.LogicalCPUs != 2 {
		t.Fatalf("logical = %d", *s.CPU.LogicalCPUs)
	}
}

const wantUtil = 100 * (1 - 70.0/110)
