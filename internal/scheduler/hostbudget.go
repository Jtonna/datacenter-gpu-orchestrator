package scheduler

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The hosting machine's memory budget (p33; the owner, 26 Nov: "same rule
// as the consoles"). A model's host process keeps its conversation memory
// (restore points x the model's checkpoint cell, per slot, plus the prompt
// cache: HostMemory.TermMiB, its maximum) on the machine that hosts it.
// With count_host_memory on, that maximum is counted against what the
// machine has:
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
//     must fit: what the loaded models hosted there may still take (their
//     maximum minus what their host process has already taken, which is
//     gone from MemAvailable already: Options.ProcMemory; the full maximum,
//     "conservative", when that cannot be read) plus this one's maximum.
//
// When a new load does not fit, the consoles' rule: idle models hosted
// there are unloaded first (longest idle first), then it waits, and the
// plan says plainly it will not start until memory frees or the profile's
// limits are lowered. A model whose maximum is more than the machine's
// total memory minus the same margin can never fit: refused. An unknown
// reading checks nothing ("host memory not checked") and never refuses.

// LocalMem is one reading of the scheduler's machine: MemAvailable and
// MemTotal, MiB (Options.LocalMemory).
type LocalMem struct {
	AvailableMiB int
	TotalMiB     int
}

// localMemReading is the scheduler machine's memory, as the last
// evLocalMem said.
type localMemReading struct {
	availMiB int
	totalMiB int // kept from the last good reading: it does not move
	at       time.Time
	known    bool
	err      string
	warned   bool // the "not checked" WARN, once until a reading is known again
}

// localMemStale: a reading older than this does not count.
const localMemStale = 60 * time.Second

// evLocalMem is one reading of the scheduler's machine and of the host
// processes of the models it hosts (startLocalMemory).
type evLocalMem struct {
	mem LocalMem
	at  time.Time
	err error
	// procs: instance iid -> its host process's anonymous resident memory
	// (MiB), for the ones read this time.
	procs map[uint64]int
}

func (evLocalMem) isEvent() {}

func (c *schedCore) localMemRead(e evLocalMem) {
	for iid, mib := range e.procs {
		in := c.instByIID(iid)
		if in == nil || !serverHosted(in.prof, in.placement) {
			continue
		}
		if !in.anonBaseSet { // the first reading after ready: what it took to load
			in.anonBase, in.anonBaseSet = mib, true
		}
		in.anonNow, in.anonAt = mib, e.at
	}
	c.changed = true // a queued request may fit now
	if e.err != nil {
		c.localMem.known, c.localMem.err = false, e.err.Error()
		return
	}
	c.localMem.availMiB, c.localMem.at, c.localMem.known, c.localMem.err, c.localMem.warned = e.mem.AvailableMiB, e.at, true, "", false
	if e.mem.TotalMiB > 0 {
		c.localMem.totalMiB = e.mem.TotalMiB
	}
}

// takenOf is what in's host process has taken for conversation memory
// since it was ready (its anonymous memory now minus its first reading);
// ok is false when that is not read or the reading is stale.
func (c *schedCore) takenOf(in *inst) (int, bool) {
	if !in.anonBaseSet || c.now.Sub(in.anonAt) > localMemStale {
		return 0, false
	}
	return max(in.anonNow-in.anonBase, 0), true
}

// HostRoom is the hosting machine's budget for one plan (PlanReport).
type HostRoom struct {
	Machine string
	// Known: the machine's available memory was read (fresh). When not,
	// Note says so and nothing but Never is checked.
	Known bool
	// MemAvailableMiB as read; FloorMiB (watchdog_floor_mib) and
	// HeadroomMiB (its tier's) kept free; BudgetMiB what is left for
	// conversation memory.
	MemAvailableMiB int
	ReadAt          time.Time `view:"omitempty"`
	FloorMiB        int
	HeadroomMiB     int
	BudgetMiB       int
	// MemTotalMiB and CapacityMiB (MemTotal minus the same margin): the
	// most conversation memory the machine could ever give one model;
	// Never is set when this one's maximum is more (0 when not read).
	MemTotalMiB int
	CapacityMiB int
	Never       bool
	// HeldMiB is what the loaded models hosted there may still take (Held
	// names them), ThisMiB this profile's maximum.
	HeldMiB int
	Held    []HeldTerm
	ThisMiB int
	Fits    bool
	// Conservative names the loaded models whose growth could not be read:
	// they count at their full maximum.
	Conservative []string `view:"omitempty"`
	// Unload: idle models hosted there that would be unloaded first for it.
	Unload []string `view:"omitempty"`
	Note   string
}

// HeldTerm is one loaded model's conversation memory on the machine: its
// maximum, what its host process has already taken (when Measured), and
// MiB, what it may still take (counted).
type HeldTerm struct {
	Profile  string
	MaxMiB   int
	TakenMiB int
	Measured bool
	MiB      int
	Idle     bool
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
		t, ok := c.hostMemoryOf(in.prof, false)
		if !ok || t.TermMiB() <= 0 {
			continue
		}
		h := HeldTerm{Profile: in.prof.Name, MaxMiB: t.TermMiB(), MiB: t.TermMiB(), Idle: in.state == stReady && in.inFlight == 0}
		if tk, ok := c.takenOf(in); ok {
			h.Measured, h.TakenMiB = true, min(tk, h.MaxMiB)
			h.MiB = h.MaxMiB - h.TakenMiB
		} else {
			r.Conservative = append(r.Conservative, in.prof.Name)
		}
		r.Held = append(r.Held, h)
		r.HeldMiB += h.MiB
	}
	lm := c.localMem
	if lm.totalMiB > 0 {
		r.MemTotalMiB = lm.totalMiB
		r.CapacityMiB = lm.totalMiB - r.FloorMiB - r.HeadroomMiB
		r.Never = r.ThisMiB > r.CapacityMiB
	}
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
	if len(r.Conservative) > 0 {
		r.Note = fmt.Sprintf("conservative: what the host process of %s has already taken could not be read, so it counts at its full maximum", strings.Join(r.Conservative, ", "))
	}
	return r, true
}

// hostVictims are the idle models hosted on the scheduler's machine that
// unloading frees enough for r, longest idle first (the consoles' rule);
// nil when even all of them are not enough. Unloading one frees its whole
// maximum: what it may still take leaves the sum, what it took comes back
// to MemAvailable.
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
		for _, h := range r.Held {
			if h.Profile == in.prof.Name {
				out, short = append(out, h.Profile), short-h.MaxMiB
			}
		}
	}
	if short > 0 {
		return nil
	}
	return out
}

// margin says what is kept free: "watchdog_floor_mib 1,000 MiB and the
// tier's headroom_mib 200 MiB".
func (r HostRoom) margin() string {
	kept := fmt.Sprintf("watchdog_floor_mib %s MiB", commasInt(r.FloorMiB))
	if r.HeadroomMiB > 0 {
		kept += fmt.Sprintf(" and the tier's headroom_mib %s MiB", commasInt(r.HeadroomMiB))
	}
	return kept
}

// hostShortWords is the plan's sentence for r when the load does not fit:
// what the machine has, what the models hosted there may still take, and
// this model's maximum.
func (r HostRoom) hostShortWords() string {
	s := fmt.Sprintf("%s has %s MiB available for conversation memory (MemAvailable %s MiB minus %s)", r.Machine, commasInt(max(r.BudgetMiB, 0)), commasInt(r.MemAvailableMiB), r.margin())
	if len(r.Held) > 0 {
		var each []string
		for _, h := range r.Held {
			w := fmt.Sprintf("%s up to %s MiB", h.Profile, commasInt(h.MaxMiB))
			switch {
			case !h.Measured:
				w += " (conservative: what it has taken could not be read)"
			case h.TakenMiB > 0:
				w += fmt.Sprintf(", %s MiB already taken", commasInt(h.TakenMiB))
			}
			each = append(each, w)
		}
		s += fmt.Sprintf("; the models hosted there may still take up to %s MiB more (%s)", commasInt(r.HeldMiB), strings.Join(each, "; "))
	}
	return s + fmt.Sprintf("; this model may hold up to %s MiB of conversation memory there", commasInt(r.ThisMiB))
}

// neverWords is the refusal of a model whose maximum is more than the
// machine could ever give it.
func (r HostRoom) neverWords() string {
	return fmt.Sprintf("%s has at most %s MiB for conversation memory (MemTotal %s MiB minus %s); this model may hold up to %s MiB of conversation memory there, so it can never start there",
		r.Machine, commasInt(max(r.CapacityMiB, 0)), commasInt(r.MemTotalMiB), r.margin(), commasInt(r.ThisMiB))
}

// fitWords says which extra arguments on p would make it fit room MiB:
// the most restore points per slot that fit with the prompt cache off
// ("with --ctx-checkpoints 5 --cache-ram 0 on this profile it would hold up
// to 1,600 MiB and fit"); for a family without checkpoints, the prompt
// cache that fits. "" when the size is not known or nothing would help.
func (c *schedCore) fitWords(p *core.Profile, room int) string {
	hm, ok := c.hostMemoryOf(p, p.Host == core.HostNode)
	if !ok || hm.SizeUnknown || hm.Family == "" {
		return ""
	}
	room = max(room, 0)
	if hm.Family == "plain" || hm.CellMiB == 0 { // the prompt cache is all of it
		if hm.PromptCacheMiB <= room {
			return ""
		}
		if room == 0 {
			return "with --cache-ram 0 on this profile it would hold none"
		}
		return fmt.Sprintf("with --cache-ram %d on this profile it would fit", room)
	}
	slots := max(p.Parallel, 1)
	n := int(math.Floor(float64(room) / (hm.CellMiB * float64(slots))))
	n = min(n, hm.PerSlot)
	alt := int(math.Ceil(float64(n*slots) * hm.CellMiB))
	if alt >= hm.TermMiB() {
		return ""
	}
	w := fmt.Sprintf("with --ctx-checkpoints %d --cache-ram 0 on this profile it would hold up to %s MiB", n, commasInt(alt))
	if n == hm.PerSlot { // the prompt cache alone is in the way
		w = fmt.Sprintf("with --cache-ram 0 on this profile it would hold up to %s MiB", commasInt(alt))
	}
	if alt <= room {
		w += " and fit"
	}
	return w
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
// (Options.LocalMemory) and the host processes of the models it hosts
// (Options.ProcMemory, by the pids the last snapshot lists): once now,
// then every localMemEvery. It posts the first reading, every change of
// localMemStep MiB or more (of the machine or of one process), a changed
// set of processes, every error, and a reading at least every
// localMemStale/2 so a fresh one never ages.
func (s *Scheduler) startLocalMemory() {
	read, proc := s.opts.LocalMemory, s.opts.ProcMemory
	if read == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var last LocalMem
		var lastProcs map[uint64]int
		lastAt, posted := time.Time{}, false
		for {
			m, err := read()
			procs := map[uint64]int{}
			if snap := s.snap.Load(); snap != nil && proc != nil {
				for iid, pid := range snap.hostPIDs {
					if mib, perr := proc(pid); perr == nil {
						procs[iid] = mib
					}
				}
			}
			moved := len(procs) != len(lastProcs)
			for iid, mib := range procs {
				if old, ok := lastProcs[iid]; !ok || abs(mib-old) >= localMemStep {
					moved = true
				}
			}
			now := time.Now()
			if err != nil || !posted || moved || abs(m.AvailableMiB-last.AvailableMiB) >= localMemStep || now.Sub(lastAt) >= localMemStale/2 {
				if !s.post(s.bgCtx, evLocalMem{mem: m, at: s.opts.Clock.Now(), err: err, procs: procs}) {
					return
				}
				posted, lastAt, lastProcs = err == nil, now, procs
				if err == nil {
					last = m
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
// queues); refuse is set when it can never fit there. An unknown reading
// checks nothing else (WARN once).
func (c *schedCore) hostCheck(p *core.Profile, gone []string, clock time.Duration) (victims []string, wait bool, refuse *Error) {
	r, ok := c.serverHostRoom(p, gone)
	if !ok {
		return nil, false, nil
	}
	if r.Never {
		msg := "profile " + p.Name + ": " + r.neverWords()
		if s := c.fitWords(p, r.CapacityMiB); s != "" {
			msg += "; " + s
		}
		e := err503(ReasonDoesNotFit, msg, 0)
		e.RetryAfter = 0 // permanent, as every does-not-fit
		e.NeedMiB, e.CapacityMiB = r.ThisMiB, max(r.CapacityMiB, 0)
		return nil, false, e
	}
	if !r.Known {
		c.noteNotChecked(r)
		return nil, false, nil
	}
	if r.Fits {
		return nil, false, nil
	}
	if victims = c.hostVictims(r, gone, clock); victims == nil {
		return nil, true, nil
	}
	c.log.Info("idle models unloaded first for conversation memory", "profile", p.Name, "unload", victims, "detail", r.hostShortWords())
	return victims, false, nil
}

// waitWords is what a plan or a queue reason says when p waits for the
// machine that hosts it: the figures, that it will not start until memory
// frees or its limits are lowered, and the extra arguments that would make
// it fit now.
func (c *schedCore) waitWords(p *core.Profile, r HostRoom) string {
	text := r.hostShortWords() + fmt.Sprintf("; nothing hosted there is idle to unload: it will NOT start until memory frees on %s or this profile's limits are lowered", r.Machine)
	if s := c.fitWords(p, r.BudgetMiB-r.HeldMiB); s != "" {
		text += "; " + s
	}
	return text
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
	var text string
	switch {
	case room.Never:
		r.Action, r.Victims = PlanReject, nil
		r.RequiredMiB, r.CapacityMiB = room.ThisMiB, max(room.CapacityMiB, 0)
		text = room.neverWords()
		if s := c.fitWords(p, room.CapacityMiB); s != "" {
			text += "; " + s
		}
	case !room.Known || room.Fits:
		return
	default:
		if hv := c.hostVictims(room, gone, 0); hv != nil {
			room.Unload = hv
			r.Action = PlanEvict
			for _, v := range hv {
				r.Victims = append(r.Victims, PlanVictim{Profile: v})
			}
			text = room.hostShortWords() + "; " + strings.Join(hv, ", ") + ", idle, would be unloaded first"
			if s := c.fitWords(p, room.BudgetMiB-room.HeldMiB); s != "" {
				text += "; to start beside them instead: " + s
			}
		} else {
			r.Action = PlanQueue
			text = c.waitWords(p, room)
		}
	}
	if r.Reason == "" {
		r.Reason = text
	} else {
		r.Reason = text + "; " + r.Reason
	}
}
