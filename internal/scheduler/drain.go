package scheduler

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Drain then unload (Zones-Confirm "Wait for the runs, then unload"): the
// instance admits nothing new, its runs in flight finish, then it stops as
// an idle unload does. A request arriving meanwhile queues (as for a
// draining victim) and loads the model again once it has stopped; with
// its zones paused it is placed elsewhere. Undrain takes it back.

// ErrNotDraining is returned by undrain for an instance not drained on request.
var ErrNotDraining = errors.New("scheduler: model not draining")

// runAvgWeight is the weight of the newest run in the average run length.
const runAvgWeight = 0.3

func (c *schedCore) noteRunLength(l *lease) {
	if l.admitted.IsZero() {
		return
	}
	d := c.now.Sub(l.admitted)
	if old, ok := c.runAvg[l.profile]; ok {
		d = time.Duration(float64(old)*(1-runAvgWeight) + float64(d)*runAvgWeight)
	}
	c.runAvg[l.profile] = d
}

// drainETA is the seconds until the last run in flight should end: the
// average run length less the longest-running one's age, at least 0; nil
// when no run of the profile has finished yet.
func (c *schedCore) drainETA(in *inst) *float64 {
	avg, ok := c.runAvg[in.prof.Name]
	if !ok {
		return nil
	}
	eta := 0.0
	for _, l := range c.leases {
		if l.iid != in.iid {
			continue
		}
		if left := (avg - c.now.Sub(l.admitted)).Seconds(); left > eta {
			eta = left
		}
	}
	eta = math.Round(eta*10) / 10
	return &eta
}

// drain asks profile's instance to stop once idle. Idle: it stops now
// (nil ETA). Loading: ErrBusy (a load is cancelled by a force unload).
func (c *schedCore) drain(profile string) (*float64, error) {
	in := c.insts[profile]
	if in == nil {
		return nil, ErrNotLoaded
	}
	switch in.state {
	case stStopping:
		return nil, nil
	case stStarting:
		return nil, fmt.Errorf("%w: %s is loading (force unload cancels a load)", ErrBusy, profile)
	}
	if in.inFlight == 0 {
		c.stopFor(in, stopGraceful, "admin")
		return nil, nil
	}
	if !in.drain {
		in.drain = true
		if in.state == stReady {
			in.state = stDraining
			in.drainInFlight = in.inFlight
		}
		c.changed = true
		c.log.Info("draining on request", "profile", profile, "in_flight", in.inFlight)
	}
	return c.drainETA(in), nil
}

// undrain takes back a drain: the instance admits again, with the
// requests that queued meanwhile, unless it is also a victim of a claim.
func (c *schedCore) undrain(profile string) error {
	in := c.insts[profile]
	if in == nil {
		return ErrNotLoaded
	}
	if !in.drain || in.state != stDraining {
		return ErrNotDraining
	}
	in.drain = false
	c.changed = true
	if in.victimOf == "" {
		in.state = stReady
		c.admitEntry(in)
	}
	c.log.Info("drain taken back", "profile", profile)
	return nil
}
