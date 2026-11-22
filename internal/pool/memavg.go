package pool

import "time"

// The short average of a node's free memory (the memory reservation rule):
// the pool keeps the memory figures of the heartbeats received within
// defaults.free_average_window and averages them, so placement with
// placement_uses_reserved can use min(latest, average) and one lucky sample
// cannot admit a load. Nothing is asked of the agent: the figures are the
// heartbeats' own.

// memSample is one heartbeat's memory figures: the node's available memory
// and the free memory of the device its worker uses.
type memSample struct {
	at      time.Time
	avail   int64
	devFree int64
	devOK   bool
}

// sampleMemLocked records the memory figures of the heartbeat just taken
// and drops the ones older than the window. A window of 0 keeps the latest
// only.
func (p *Pool) sampleMemLocked(a *agent, now time.Time) {
	if a.hb == nil {
		return
	}
	s := memSample{at: now, avail: a.hb.MemAvailableMiB}
	if d, _, ok := pickDevice(a); ok {
		s.devFree, s.devOK = d.FreeMiB, true
	}
	a.mem = append(a.mem, s)
	window := p.o.Config().Defaults.FreeAverageWindow
	keep := 0
	for keep < len(a.mem)-1 && now.Sub(a.mem[keep].at) > window {
		keep++
	}
	a.mem = append(a.mem[:0], a.mem[keep:]...)
}

// memAverage is the average available memory and device free memory of the
// samples kept (whole MiB, rounded down: the safe side). ok is false when no
// sample is kept; the device average is the available one's partner only
// over the samples that had a device figure.
func (a *agent) memAverage() (avail, devFree int64, ok bool) {
	if len(a.mem) == 0 {
		return 0, 0, false
	}
	var sa, sd, nd int64
	for _, s := range a.mem {
		sa += s.avail
		if s.devOK {
			sd += s.devFree
			nd++
		}
	}
	avail = sa / int64(len(a.mem))
	if nd > 0 {
		devFree = sd / nd
	}
	return avail, devFree, true
}

// sharedAverage is, for a node whose device shares system memory, the
// average over the samples kept of each sample's usable figure: min(device
// free, available - floor), rounded down (review N7: the smaller of the two
// averages can be above it when the binding figure alternates).
func (a *agent) sharedAverage(floor int64) (int64, bool) {
	if len(a.mem) == 0 {
		return 0, false
	}
	var sum int64
	for _, s := range a.mem {
		u := s.avail - floor
		if s.devOK {
			u = min(u, s.devFree)
		}
		sum += u
	}
	return sum / int64(len(a.mem)), true
}
