package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The hosting machine's memory budget (p33; the owner, 26 Nov: "same rule
// as the consoles"). A model's host process keeps its conversation memory
// (restore points x the model's checkpoint cell, per slot, plus the prompt
// cache: HostMemory.TermMiB) on the machine that hosts it. That maximum is
// counted against what the machine has, with count_host_memory on:
//
//   - On a console (host: node) the term joins the console's reservation in
//     its own node fit (hostnode.go hostOverhead, reserve.go): the console's
//     memory is its devices' memory too, so the rule is checked there, with
//     the console's figures: MemAvailable minus watchdog_floor_mib minus its
//     tier's headroom_mib (planfigures.go).
//   - On the scheduler's machine (every profile without host: node) the
//     same formula, checked here: MemAvailable as the scheduler reads it
//     for itself (Options.LocalMemory), minus watchdog_floor_mib, minus the
//     headroom_mib of the tier its local node matches (0 when none). What
//     must fit: the terms of the loaded models hosted there (a
//     reservation, until the instance is gone) plus the one being planned.
//
// When a new load does not fit, the consoles' rule: idle models hosted
// there are unloaded first (longest idle first), then it queues; with
// nothing of ours hosted there it is refused. An unknown reading checks
// nothing ("host memory not checked") and never refuses.

// localMemReading is the scheduler machine's MemAvailable, as the last
// evLocalMem said.
type localMemReading struct {
	availMiB int
	at       time.Time
	known    bool
	err      string
	warned   bool // the "not checked" WARN, once until a reading is known again
}

// localMemStale: a reading older than this does not count.
const localMemStale = 60 * time.Second

func (evLocalMem) isEvent() {}

// evLocalMem is one reading of the scheduler's machine (runner.go
// readLocalMemory).
type evLocalMem struct {
	availMiB int
	at       time.Time
	err      error
}

func (c *schedCore) localMemRead(e evLocalMem) {
	if e.err != nil {
		c.localMem.known, c.localMem.err = false, e.err.Error()
		return
	}
	c.localMem.availMiB, c.localMem.at, c.localMem.known, c.localMem.err, c.localMem.warned = e.availMiB, e.at, true, "", false
	c.changed = true // a queued request may fit now
}

// HostRoom is the hosting machine's budget for one plan (PlanReport).
type HostRoom struct {
	Machine string
	// Known: the machine's available memory was read (fresh). When not,
	// Note says so and nothing is checked.
	Known bool
	// MemAvailableMiB as read; FloorMiB (watchdog_floor_mib) and
	// HeadroomMiB (its tier's) kept free; BudgetMiB what is left for
	// conversation memory.
	MemAvailableMiB int
	ReadAt          time.Time `view:"omitempty"`
	FloorMiB        int
	HeadroomMiB     int
	BudgetMiB       int
	// HeldMiB is the sum of the terms of the loaded models hosted there
	// (Held names them), ThisMiB this profile's.
	HeldMiB int
	Held    []HeldTerm
	ThisMiB int
	Fits    bool
	// Unload: idle models hosted there that would be unloaded first for it.
	Unload []string `view:"omitempty"`
	Note   string
}

// HeldTerm is one loaded model's conversation memory on the machine.
type HeldTerm struct {
	Profile string
	MiB     int
	Idle    bool
}

// serverHosted: in's host process runs on the scheduler's machine.
func serverHosted(p *core.Profile, pl core.Placement) bool {
	return p != nil && p.Kind == core.KindLlama && hostOf(p, pl) == ""
}

// serverHostRoom is the budget of the scheduler's machine for p, with the
// instances in gone left out (victims already chosen); ok is false when the
// rule does not apply (count_host_memory off, a console host, a command
// profile, a model whose term is not known).
func (c *schedCore) serverHostRoom(p *core.Profile, gone []string) (HostRoom, bool) {
	if !c.cfg.Defaults.CountHostMemory || p == nil || p.Kind != core.KindLlama || p.Host == core.HostNode {
		return HostRoom{}, false
	}
	hm, ok := c.hostMemoryOf(p, false)
	if !ok {
		return HostRoom{}, false
	}
	r := HostRoom{Machine: c.serverName(), ThisMiB: hm.TermMiB(), FloorMiB: max(c.cfg.Defaults.WatchdogFloorMiB, 0)}
	for _, n := range c.raw {
		if n.Local {
			r.HeadroomMiB = max(r.HeadroomMiB, n.HeadroomMiB)
		}
	}
	for _, in := range c.sortedInsts() {
		if in.prof.Name == p.Name || slices.Contains(gone, in.prof.Name) || !serverHosted(in.prof, in.placement) {
			continue
		}
		if t, ok := c.hostMemoryOf(in.prof, false); ok && t.TermMiB() > 0 {
			r.Held = append(r.Held, HeldTerm{Profile: in.prof.Name, MiB: t.TermMiB(), Idle: in.state == stReady && in.inFlight == 0})
			r.HeldMiB += t.TermMiB()
		}
	}
	lm := c.localMem
	switch {
	case !lm.known:
		why := "it could not be read"
		if lm.err == "" {
			why = "no reading yet"
		}
		r.Note = fmt.Sprintf("host memory not checked: available memory of %s unknown (%s)", r.Machine, why)
		return r, true
	case c.now.Sub(lm.at) > localMemStale:
		r.Note = fmt.Sprintf("host memory not checked: available memory of %s unknown (the last reading is %s old)", r.Machine, c.now.Sub(lm.at).Round(time.Second))
		return r, true
	}
	r.Known, r.MemAvailableMiB, r.ReadAt = true, lm.availMiB, lm.at
	r.BudgetMiB = lm.availMiB - r.FloorMiB - r.HeadroomMiB
	r.Fits = r.HeldMiB+r.ThisMiB <= r.BudgetMiB
	return r, true
}

// hostVictims are the idle models hosted on the scheduler's machine that
// unloading frees enough for r, longest idle first (the consoles' rule);
// nil when even all of them are not enough.
func (c *schedCore) hostVictims(r HostRoom, gone []string, clock time.Duration) []string {
	var idle []*inst
	for _, in := range c.sortedInsts() {
		if slices.Contains(gone, in.prof.Name) || !serverHosted(in.prof, in.placement) || in.inFlight > 0 || !c.evictable(in, clock) || in.victimOf != "" {
			continue
		}
		idle = append(idle, in)
	}
	slices.SortStableFunc(idle, func(a, b *inst) int { return a.lastUsed.Compare(b.lastUsed) })
	short := r.HeldMiB + r.ThisMiB - r.BudgetMiB
	var out []string
	for _, in := range idle {
		if short <= 0 {
			break
		}
		if t, ok := c.hostMemoryOf(in.prof, false); ok && t.TermMiB() > 0 {
			out, short = append(out, in.prof.Name), short-t.TermMiB()
		}
	}
	if short > 0 {
		return nil
	}
	return out
}

// hostShortWords is the plan's sentence for r when the load does not fit:
// "<machine> has N MiB available (MemAvailable M MiB minus watchdog_floor_mib
// F MiB); the models hosted there hold up to H MiB; this model may hold up
// to T MiB of conversation memory there".
func (r HostRoom) hostShortWords() string {
	kept := fmt.Sprintf("watchdog_floor_mib %s MiB", commasInt(r.FloorMiB))
	if r.HeadroomMiB > 0 {
		kept += fmt.Sprintf(" and the tier's headroom_mib %s MiB", commasInt(r.HeadroomMiB))
	}
	s := fmt.Sprintf("%s has %s MiB available for conversation memory (MemAvailable %s MiB minus %s)", r.Machine, commasInt(max(r.BudgetMiB, 0)), commasInt(r.MemAvailableMiB), kept)
	if r.HeldMiB > 0 {
		var names []string
		for _, h := range r.Held {
			names = append(names, h.Profile)
		}
		s += fmt.Sprintf("; the models hosted there (%s) may hold up to %s MiB", strings.Join(names, ", "), commasInt(r.HeldMiB))
	}
	return s + fmt.Sprintf("; this model may hold up to %s MiB of conversation memory there", commasInt(r.ThisMiB))
}

// smallerWords says what would make p fit: "with --ctx-checkpoints 3
// --cache-ram 0 on this profile it would hold up to 960 MiB"; "" when that
// is no smaller than what it holds now or the size is not known.
func (c *schedCore) smallerWords(p *core.Profile) string {
	hm, ok := c.hostMemoryOf(p, p.Host == core.HostNode)
	if !ok || hm.SizeUnknown || hm.Family == "" {
		return ""
	}
	if hm.Family == "plain" || hm.CellMiB == 0 { // no checkpoints: the prompt cache is all of it
		if hm.PromptCacheMiB == 0 {
			return ""
		}
		return "with --cache-ram 0 on this profile it would hold none"
	}
	n := 3 * max(p.Parallel, 1)
	alt := int(float64(n)*hm.CellMiB + 0.999999)
	if alt >= hm.TermMiB() {
		return ""
	}
	return fmt.Sprintf("with --ctx-checkpoints 3 --cache-ram 0 on this profile it would hold up to %s MiB", commasInt(alt))
}

// noteNotChecked WARNs once that the scheduler machine's memory is not
// known while a plan needs it.
func (c *schedCore) noteNotChecked(r HostRoom) {
	if r.Known || c.localMem.warned {
		return
	}
	c.localMem.warned = true
	c.log.Warn("host memory not checked: the scheduler's machine's available memory is unknown; loads hosted there are not held to it", "machine", r.Machine, "err", c.localMem.err)
}

// commasInt is n with thousands separators.
func commasInt(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// How often Run reads the scheduler's machine, and the change worth
// posting (a smaller move would only churn the plans).
const (
	localMemEvery = 5 * time.Second
	localMemStep  = 64
)

// startLocalMemory reads the scheduler's machine in the background
// (Options.LocalMemory): once now, then every localMemEvery; it posts the
// first reading, every change of localMemStep MiB or more, every error,
// and a reading at least every localMemStale/2 so a fresh one never ages.
func (s *Scheduler) startLocalMemory() {
	read := s.opts.LocalMemory
	if read == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		last, lastAt, posted := 0, time.Time{}, false
		for {
			mib, err := read()
			now := time.Now()
			if err != nil || !posted || abs(mib-last) >= localMemStep || now.Sub(lastAt) >= localMemStale/2 {
				if !s.post(s.bgCtx, evLocalMem{availMiB: mib, at: s.opts.Clock.Now(), err: err}) {
					return
				}
				posted, lastAt = err == nil, now
				if err == nil {
					last = mib
				}
			}
			select {
			case <-s.bgCtx.Done():
				return
			case <-time.After(localMemEvery):
			}
		}
	}()
}

// hostCheck is the hosting machine's rule for a load of p with the
// instances in gone already leaving: victims are the idle models hosted
// there to unload first; wait is true when it does not fit even so (it
// queues). An unknown reading checks nothing (WARN once).
func (c *schedCore) hostCheck(p *core.Profile, gone []string, clock time.Duration) (victims []string, wait bool) {
	r, ok := c.serverHostRoom(p, gone)
	if !ok {
		return nil, false
	}
	if !r.Known {
		c.noteNotChecked(r)
		return nil, false
	}
	if r.Fits {
		return nil, false
	}
	if victims = c.hostVictims(r, gone, clock); victims == nil {
		return nil, true
	}
	c.log.Info("idle models unloaded first for conversation memory", "profile", p.Name, "unload", victims, "detail", r.hostShortWords())
	return victims, false
}

// dryHost is hostCheck for a plan that would start (gone nil) or evict
// (gone its victims): the action and reason change when the machine that
// hosts it has not the room; r.HostRoom shows the figures either way.
func (c *schedCore) dryHost(r *PlanReport, p *core.Profile, gone []string) {
	room, ok := c.serverHostRoom(p, gone)
	if !ok {
		return
	}
	defer func() { r.HostRoom = &room }()
	if !room.Known || room.Fits {
		return
	}
	text := room.hostShortWords()
	if hv := c.hostVictims(room, gone, 0); hv != nil {
		room.Unload = hv
		r.Action = PlanEvict
		for _, v := range hv {
			r.Victims = append(r.Victims, PlanVictim{Profile: v})
		}
		text += "; " + strings.Join(hv, ", ") + ", idle, would be unloaded first"
	} else {
		r.Action = PlanQueue
		text += "; nothing hosted there is idle to unload, so it waits"
	}
	if s := c.smallerWords(p); s != "" {
		text += "; " + s
	}
	if r.Reason == "" {
		r.Reason = text
	} else {
		r.Reason = text + "; " + r.Reason
	}
}
