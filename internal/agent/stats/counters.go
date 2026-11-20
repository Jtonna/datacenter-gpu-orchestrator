package stats

import "time"

// Rates are the values derived from node_exporter counters (contract 3.1).
// A Rates value is never mutated after it is built, so it can be shared.
type Rates struct {
	CPUUtil *float64           // percent, 0-100; nil until known
	Net     map[string]NetRate // by device
}

// NetRate holds the rx/tx rates of one interface in decimal Mb/s.
type NetRate struct {
	Rx, Tx *float64
}

// Bounds on dt between two successful scrapes (contract 3.1 rule 3).
const (
	minRateDT = 500 * time.Millisecond
	maxRateDT = 30 * time.Second
)

type cpuKey struct{ cpu, mode string }

// counterState turns successive node scrapes into rates.
type counterState struct {
	have  bool
	t     time.Duration // monotonic start of the scrape that gave the baseline
	cpu   map[cpuKey]float64
	rx    map[string]float64
	tx    map[string]float64
	rates Rates
}

func readCPU(m Metrics) map[cpuKey]float64 {
	out := map[cpuKey]float64{}
	for _, s := range m["node_cpu_seconds_total"] {
		out[cpuKey{s.Labels["cpu"], s.Labels["mode"]}] = s.Value
	}
	return out
}

func readByDevice(m Metrics, name string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range m[name] {
		if d, ok := s.Labels["device"]; ok {
			out[d] = s.Value
		}
	}
	return out
}

// update takes the counters of a new successful scrape that started at t
// (agent monotonic clock) and recomputes the rates. The new values always
// become the baseline ("start again from cur").
func (c *counterState) update(m Metrics, t time.Duration) {
	cpu := readCPU(m)
	rx := readByDevice(m, "node_network_receive_bytes_total")
	tx := readByDevice(m, "node_network_transmit_bytes_total")
	defer func() { c.have, c.t, c.cpu, c.rx, c.tx = true, t, cpu, rx, tx }()

	if !c.have {
		// Rule 1: the first scrape produces no rate.
		c.rates = Rates{}
		return
	}
	dt := t - c.t
	if dt < minRateDT || dt > maxRateDT {
		// CONTRACT-QUESTION: rule 3 says "discard the sample" without saying
		// whether the previous rates stay. Conservative reading: the sample
		// gives no rate (all rate fields omitted, the CPU figure included,
		// although CPU needs no clock) and the next sample is measured from
		// this one.
		c.rates = Rates{}
		return
	}
	secs := dt.Seconds()
	next := Rates{Net: map[string]NetRate{}}

	// CPU (3.1). The cpu counters are one group: if any of them went
	// backwards, or the set of (cpu, mode) series changed, the CPU sample is
	// discarded.
	// CONTRACT-QUESTION: "discard that sample" on cur < prev: the agent
	// omits util_percent for that report rather than repeating the previous
	// figure (same as after an exporter restart, rule 1). "keep the previous
	// value" is applied only to the d_total == 0 case, as written.
	if len(cpu) > 0 {
		var dTotal, dIdle float64
		valid := len(cpu) == len(c.cpu)
		for k, cur := range cpu {
			prev, ok := c.cpu[k]
			if !ok || cur < prev {
				valid = false
				break
			}
			d := cur - prev
			dTotal += d
			if k.mode == "idle" || k.mode == "iowait" {
				dIdle += d
			}
		}
		switch {
		case !valid:
		case dTotal == 0:
			next.CPUUtil = c.rates.CPUUtil
		default:
			u := clamp(100*(1-dIdle/dTotal), 0, 100)
			next.CPUUtil = &u
		}
	}

	// Network (3.1).
	// CONTRACT-QUESTION: rule 2 says "if any cur < prev ... discard that
	// sample". Applying it to the whole scrape would let one re-created
	// container interface (veth*, lxc*: denylisted, re-created often) blank
	// every rate on the machine. The agent applies it per counter: one
	// interface direction going backwards gives no rate for that
	// interface direction only; a device missing from the previous scrape is
	// a first sample for that device.
	rate := func(cur float64, prevMap map[string]float64, dev string) *float64 {
		prev, ok := prevMap[dev]
		if !ok || cur < prev {
			return nil
		}
		v := (cur - prev) * 8 / secs / 1e6
		return &v
	}
	for dev, cur := range rx {
		nr := next.Net[dev]
		nr.Rx = rate(cur, c.rx, dev)
		next.Net[dev] = nr
	}
	for dev, cur := range tx {
		nr := next.Net[dev]
		nr.Tx = rate(cur, c.tx, dev)
		next.Net[dev] = nr
	}
	c.rates = next
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
