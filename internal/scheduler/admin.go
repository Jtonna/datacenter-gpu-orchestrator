package scheduler

import (
	"errors"
	"fmt"
)

// ErrNotLoaded is returned by Unload when the profile has no instance.
var ErrNotLoaded = errors.New("scheduler: model not loaded")

// ErrBusy is returned by Unload without force when the instance is serving
// requests or still loading.
var ErrBusy = errors.New("scheduler: model busy")

// unload stops one instance if idle; force stops it whatever it is doing
// (the one way a running generation is cut off, design 9).
func (c *schedCore) unload(profile string, force bool) error { return c.unloadFor(profile, force, "") }

// unloadFor is unload; why says, to the requests a forced unload cuts, why
// (cutAnswer).
func (c *schedCore) unloadFor(profile string, force bool, why string) error {
	in := c.insts[profile]
	if in == nil {
		return ErrNotLoaded
	}
	switch in.state {
	case stStopping:
		if force {
			in.markCut(cutAnswer(profile, why))
			c.stopFor(in, stopKill, "admin")
		}
		return nil
	case stStarting:
		// DESIGN: the design does not say what unloading a loading
		// instance does. Without force it is refused (it is not idle);
		// with force its waiters get 503 "unloaded" and it is killed.
		if !force {
			return fmt.Errorf("%w: %s is loading", ErrBusy, profile)
		}
		ws := in.waiters
		in.waiters = nil
		c.failAll(ws, err503(ReasonUnloaded, fmt.Sprintf("%s was unloaded while loading", profile), queueRetryAfter))
		c.stopFor(in, stopKill, "admin")
		return nil
	default: // ready, draining
		if in.inFlight > 0 && !force {
			return fmt.Errorf("%w: %s has %d requests in flight", ErrBusy, profile, in.inFlight)
		}
		mode := stopGraceful
		if in.inFlight > 0 {
			mode = stopKill
			in.markCut(cutAnswer(profile, why))
		}
		c.stopFor(in, mode, "admin")
		return nil
	}
}

// cutAnswer is what a request in flight cut by a forced unload is told:
// 503 unloaded, "<profile> was unloaded[: why]", the answer a loading or
// queued caller gets.
func cutAnswer(profile, why string) *Error {
	msg := profile + " was unloaded"
	if why != "" {
		msg += ": " + why
	}
	return err503(ReasonUnloaded, msg, queueRetryAfter)
}

// cancelQueued answers every request waiting in the queue for one of
// profiles 503 unloaded, the answer a forced unload gives a loading
// instance's waiters, and removes the entry with its claim. It returns how
// many requests it answered. Only an owner's model delete with interrupt
// calls it (models slice 3); nothing automatic does.
func (c *schedCore) cancelQueued(profiles []string) int {
	n := 0
	for _, name := range profiles {
		e := c.entries[name]
		if e == nil {
			continue
		}
		n += len(e.waiters)
		c.answerEntry(e, err503(ReasonUnloaded, fmt.Sprintf("%s was unloaded: its model file is being deleted", name), queueRetryAfter))
	}
	return n
}

// unloadIdle stops every ready instance with in_flight == 0 and returns
// their profiles.
func (c *schedCore) unloadIdle() []string {
	var out []string
	for _, in := range c.sortedInsts() {
		if in.state == stReady && in.inFlight == 0 {
			c.stopFor(in, stopGraceful, "admin")
			out = append(out, in.prof.Name)
		}
	}
	return out
}

// beginShutdown is design 8.5: stop accepting, answer every queued waiter
// 503, stop starting instances and answer their waiters 503. In-flight
// requests may finish within shutdown_grace (shutdownProgress); force
// stops everything at once.
func (c *schedCore) beginShutdown(force bool) {
	if !c.shutdown {
		c.shutdown = true
		c.shutdownDL = c.now.Add(c.cfg.Defaults.ShutdownGrace)
		queued := 0
		for _, e := range c.entries {
			queued += len(e.waiters)
		}
		c.log.Info("shutdown begun", "grace", c.cfg.Defaults.ShutdownGrace, "force", force, "instances", len(c.insts),
			"queued_waiters", queued)
		// Every queued waiter first, so a claim dropped below cannot admit
		// a queued waiter to a victim returning to ready.
		for _, e := range c.orderedEntries() {
			c.occRefused(e, "refused: shutting down") // occupied.go
			ws := e.waiters
			e.waiters = nil
			c.failAll(ws, err503(ReasonShuttingDown, "the scheduler is shutting down", queueRetryAfter))
		}
		for _, e := range c.orderedEntries() {
			c.dropClaim(e)
			c.deleteEntry(e)
		}
		for _, in := range c.sortedInsts() {
			if in.state == stStarting {
				ws := in.waiters
				in.waiters = nil
				c.failAll(ws, err503(ReasonShuttingDown, "the scheduler is shutting down", queueRetryAfter))
				c.stopFor(in, stopGraceful, "shutdown")
			}
		}
	}
	if force {
		if !c.killAll {
			c.log.Warn("shutdown forced: killing every instance now")
		}
		c.stopAll, c.killAll = true, true
	}
}

// shutdownProgress stops every instance once nothing is in flight or the
// grace has passed, and marks the core finished when none is left.
func (c *schedCore) shutdownProgress() {
	if !c.shutdown {
		return
	}
	inFlight := 0
	for _, in := range c.insts {
		inFlight += in.inFlight
	}
	if inFlight == 0 || !c.now.Before(c.shutdownDL) {
		c.stopAll = true
	}
	if c.stopAll && !c.stopLogged {
		c.stopLogged = true
		c.log.Info("shutdown stopping instances", "in_flight", inFlight, "grace_passed", !c.now.Before(c.shutdownDL),
			"kill", c.killAll, "instances", len(c.insts))
	}
	if c.stopAll {
		mode := stopGraceful
		if c.killAll {
			mode = stopKill
		}
		for _, in := range c.sortedInsts() {
			if in.state != stStopping || (mode == stopKill && in.stopMode != stopKill) {
				ws := in.waiters
				in.waiters = nil
				c.failAll(ws, err503(ReasonShuttingDown, "the scheduler is shutting down", queueRetryAfter))
				c.stopFor(in, mode, "shutdown")
			}
		}
	}
	if len(c.insts) == 0 {
		c.finished = true
		if !c.doneLogged {
			c.doneLogged = true
			c.log.Info("shutdown complete")
		}
	}
}
