package scheduler

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// Loop health (B9): how long the core loop's steps take, for production.
// A slow loop holds every request, status read and heartbeat behind it, and
// it fails nothing, so nothing else shows it. The loop writes a few atomics
// per step (its start time, and at the end a compare against the current
// slot's longest); readers only load them, so a stalled loop is still
// reportable: busy_now grows while the step that stalls it runs.

const (
	loopSlot      = 10 * time.Second
	loopSlots     = 360 // one hour of slots
	slowStepEvery = time.Minute
)

type loopSlotMax struct {
	slot atomic.Int64 // slot number (unix time / loopSlot) these figures are of
	ns   atomic.Int64
	ev   atomic.Pointer[string]
}

type loopHealth struct {
	cur   atomic.Int64 // unix ns the running step started; 0 while idle
	curEv atomic.Pointer[stepRef]
	slots [loopSlots]loopSlotMax
	steps atomic.Int64
	slow  atomic.Int64 // steps over loop_step_warn_ms since the start
	// lastSlowEv is when the last loopSlowStep event was recorded (unix ns).
	lastSlowEv int64 // loop goroutine only
}

// LoopStats is the core loop's health as GET /api/system and the health
// summary report it. Durations are in milliseconds.
type LoopStats struct {
	// BusyNowMs is how long the step in progress has run (0 when the loop
	// waits for work), CurrentEvent what it is.
	BusyNowMs    int64  `json:"busy_now_ms"`
	CurrentEvent string `json:"current_event,omitempty"`
	// The longest step in the last minute, ten minutes and hour, and since
	// the start, each with its event.
	MaxLastMinuteMs int64  `json:"max_last_minute_ms"`
	MaxLastMinuteEv string `json:"max_last_minute_event,omitempty"`
	Max10mMs        int64  `json:"max_10m_ms"`
	Max10mEv        string `json:"max_10m_event,omitempty"`
	MaxLastHourMs   int64  `json:"max_last_hour_ms"`
	MaxLastHourEv   string `json:"max_last_hour_event,omitempty"`
	MaxMs           int64  `json:"max_ms"`
	MaxEv           string `json:"max_event,omitempty"`
	// Steps is how many steps ran; SlowSteps how many took longer than
	// WarnMs (loop_step_warn_ms).
	Steps     int64 `json:"steps"`
	SlowSteps int64 `json:"slow_steps"`
	WarnMs    int64 `json:"warn_ms"`
}

// stepRef is the event of the step in progress; its name is made only
// when someone reads it.
type stepRef struct{ ev event }

// begin is called by the loop as a step starts.
func (l *loopHealth) begin(t0 time.Time, ev event) {
	l.curEv.Store(&stepRef{ev})
	l.cur.Store(t0.UnixNano())
}

// end is called by the loop as a step ends; it returns whether the step was
// slow and a loopSlowStep event is due (at most one a minute).
func (l *loopHealth) end(t1 time.Time, d time.Duration, name func() string, warn time.Duration) (slowEvent bool) {
	l.cur.Store(0)
	l.steps.Add(1)
	slot := t1.UnixNano() / int64(loopSlot)
	b := &l.slots[slot%loopSlots]
	if b.slot.Load() != slot {
		n := name()
		b.ns.Store(int64(d))
		b.ev.Store(&n)
		b.slot.Store(slot)
	} else if int64(d) > b.ns.Load() {
		n := name()
		b.ns.Store(int64(d))
		b.ev.Store(&n)
	}
	if warn <= 0 || d <= warn {
		return false
	}
	l.slow.Add(1)
	if t1.UnixNano()-l.lastSlowEv < int64(slowStepEvery) {
		return false
	}
	l.lastSlowEv = t1.UnixNano()
	return true
}

// windowMax is the longest step in the slots of the last span before now.
func (l *loopHealth) windowMax(now time.Time, span time.Duration) (time.Duration, string) {
	cur := now.UnixNano() / int64(loopSlot)
	first := cur - int64(span/loopSlot) + 1
	var best int64
	ev := ""
	for i := range l.slots {
		b := &l.slots[i]
		s := b.slot.Load()
		if s < first || s > cur {
			continue
		}
		if ns := b.ns.Load(); ns > best {
			best = ns
			if p := b.ev.Load(); p != nil {
				ev = *p
			}
		}
	}
	return time.Duration(best), ev
}

// LoopStats reads the core loop's health without asking the loop: it
// answers while the loop is stalled.
func (s *Scheduler) LoopStats() LoopStats {
	now := time.Now()
	l := &s.loop
	st := LoopStats{Steps: l.steps.Load(), SlowSteps: l.slow.Load(), WarnMs: int64(s.loopWarn() / time.Millisecond)}
	if t := l.cur.Load(); t != 0 {
		if d := now.UnixNano() - t; d > 0 {
			st.BusyNowMs = d / int64(time.Millisecond)
		}
		if p := l.curEv.Load(); p != nil {
			st.CurrentEvent = eventName(p.ev)
		}
	}
	ms := func(d time.Duration) int64 { return int64(d / time.Millisecond) }
	d, ev := l.windowMax(now, time.Minute)
	st.MaxLastMinuteMs, st.MaxLastMinuteEv = ms(d), ev
	d, ev = l.windowMax(now, 10*time.Minute)
	st.Max10mMs, st.Max10mEv = ms(d), ev
	d, ev = l.windowMax(now, time.Hour)
	st.MaxLastHourMs, st.MaxLastHourEv = ms(d), ev
	d, ev = s.LongestStep()
	st.MaxMs, st.MaxEv = ms(d), ev
	return st
}

// loopWarn is loop_step_warn_ms of the configuration in force.
func (s *Scheduler) loopWarn() time.Duration {
	ms := config.DefaultLoopStepWarnMs
	if c := s.Config(); c != nil && c.LoopStepWarnMs > 0 {
		ms = c.LoopStepWarnMs
	}
	return time.Duration(ms) * time.Millisecond
}

// slowStepEvent is the loopSlowStep history event.
func slowStepEvent(at time.Time, d, warn time.Duration, name string) history.Event {
	return history.Event{Ts: at.UnixMilli(), Type: history.TLoopSlowStep, Severity: history.Warn, Nodes: []string{},
		Summary: fmt.Sprintf("The scheduler's core loop spent %s on one step (%s), more than loop_step_warn_ms (%s); requests and status reads waited meanwhile.",
			d.Round(time.Millisecond), name, warn),
		Data: map[string]any{"event": name, "ms": d.Milliseconds(), "warn_ms": warn.Milliseconds()}}
}
