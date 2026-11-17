package scheduler

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
)

// Random tests (design 11): generated event sequences over a small random
// cluster and profile set, with every invariant checked after every step,
// then drained to quiescence.
//
//	go test ./internal/scheduler -run Random -sched.seeds=200 -sched.base=1000 -sched.steps=5000

var (
	flagSeeds = flag.Int("sched.seeds", 12, "number of random-test seeds")
	flagBase  = flag.Int64("sched.base", 1, "first random-test seed")
	flagSteps = flag.Int("sched.steps", 3000, "actions per random-test seed")
)

type actKind int

const (
	aRequest actKind = iota
	aLoad
	aCancel
	aRelease
	aProbe
	aStart
	aHealthy
	aCrash
	aStopExit
	aFetch
	aToggleNode
	aMemory
	aReconnect
	aRestore
	aStale
	aAdvance
	aUnload
	aUnloadIdle
	aHealthErr   // WaitHealthy returned an error (the core must wait for the exit or the load timeout)
	aLoadTimeout // time passes the earliest load deadline: failure by health timeout
	aDropNode    // a node (the local one included) leaves the view entirely
	aReaddNode   // a dropped node comes back under the same ID
	nActKinds
)

var actWeights = [nActKinds]int{
	aRequest: 14, aLoad: 2, aCancel: 4, aRelease: 10, aProbe: 8, aStart: 6, aHealthy: 8,
	aCrash: 2, aStopExit: 8, aFetch: 6, aToggleNode: 2, aMemory: 3, aReconnect: 1, aRestore: 2,
	aStale: 1, aAdvance: 12, aUnload: 2, aUnloadIdle: 1,
	aHealthErr: 2, aLoadTimeout: 2, aDropNode: 2, aReaddNode: 3,
}

type action struct {
	kind    actKind
	a, b, c int
}

func genActions(r *rand.Rand, n int) []action {
	total := 0
	for _, w := range actWeights {
		total += w
	}
	out := make([]action, n)
	for i := range out {
		x := r.Intn(total)
		k := actKind(0)
		for ; x >= actWeights[k]; k++ {
			x -= actWeights[k]
		}
		out[i] = action{kind: k, a: r.Intn(1 << 16), b: r.Intn(1 << 16), c: r.Intn(1 << 16)}
	}
	return out
}

// panicT turns harness failures into panics for replay.
type panicT struct{}

type harnessFailure struct{ msg string }

func (panicT) Helper() {}
func (panicT) Fatalf(format string, args ...any) {
	panic(harnessFailure{fmt.Sprintf(format, args...)})
}

// randWorld is a harness plus the bookkeeping the generator needs.
type randWorld struct {
	h        *harness
	profiles []string
	nodeIDs  []core.NodeID
	removed  []core.Node
	dropped  []core.Node // nodes removed by aDropNode, to be re-added later
	waiters  []*waiter
	leases   []*grant
	leased   map[*waiter]bool
	nStep    int
	log      []string
}

func newRandWorld(seed int64) *randWorld {
	r := rand.New(rand.NewSource(seed))
	h := newHarness(panicT{})
	h.autoProbe, h.autoStart, h.autoExit, h.autoFetch = false, false, false, false
	w := &randWorld{h: h, leased: map[*waiter]bool{}}

	nodes := []core.Node{omen(6000 + 500*r.Intn(5))}
	for i := 0; i < 1+r.Intn(3); i++ {
		nodes = append(nodes, ps5(fmt.Sprintf("ps5-%d", i), 10+i, 9000+1000*r.Intn(7)))
	}
	if r.Intn(2) == 0 {
		nodes = append(nodes, alien("alienware", 6000+500*r.Intn(4)))
	}
	h.nodes(nodes...)
	for _, n := range nodes {
		w.nodeIDs = append(w.nodeIDs, n.ID)
	}
	sizes := []int{3000, 5000, 7000, 9000, 12000, 14000, 20000, 28000, 60000}
	for i := 0; i < 3+r.Intn(3); i++ {
		name := fmt.Sprintf("p%d", i)
		need := sizes[r.Intn(len(sizes))]
		p := h.llama(name, need)
		switch r.Intn(8) {
		case 0:
			k := 1 + r.Intn(2)
			for j := 0; j < k; j++ {
				id := w.nodeIDs[r.Intn(len(w.nodeIDs))]
				if !contains(p.ForceNodes, string(id)) {
					p.ForceNodes = append(p.ForceNodes, string(id))
				}
			}
			if r.Intn(4) == 0 {
				p.ForceNodes = append(p.ForceNodes, "ghost")
			}
		case 1:
			p.AllowTiers = []int{1, 2}
		case 2:
			p.TTL = ptrDur(0)
		case 3:
			h.needs[name] = NeedInfo{Need: placer.Estimated(need / 2), FileBytes: int64(need) << 20}
		case 4:
			if r.Intn(3) == 0 {
				h.needErr[name] = errors.New("model file unreadable")
			}
		}
		w.profiles = append(w.profiles, name)
	}
	if r.Intn(2) == 0 {
		h.command("cmd", 3000+1000*r.Intn(5))
		w.profiles = append(w.profiles, "cmd")
	}
	h.step(evTick{})
	return w
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func (w *randWorld) note(format string, a ...any) {
	w.log = append(w.log, fmt.Sprintf("%5d %8s ", w.nStep, w.h.clk.Now().Sub(t0))+fmt.Sprintf(format, a...))
}

func (w *randWorld) liveWaiters() []*waiter {
	var out []*waiter
	for _, wt := range w.waiters {
		if !wt.answered && !wt.synthetic {
			out = append(out, wt)
		}
	}
	return out
}

func (w *randWorld) harvestLeases() {
	for _, wt := range w.waiters {
		if wt.reply == nil || !wt.answered {
			continue
		}
		if len(wt.reply) > 1 {
			panic(harnessFailure{fmt.Sprintf("waiter %d answered %d times", wt.id, len(wt.reply))})
		}
	}
}

func (w *randWorld) apply(act action) {
	h := w.h
	w.nStep++
	pick := func(n int) int { return act.a % n }
	switch act.kind {
	case aRequest, aLoad:
		p := w.profiles[pick(len(w.profiles))]
		var wt *waiter
		if act.kind == aLoad {
			wt = &waiter{profile: p, synthetic: true}
		} else {
			wt = &waiter{profile: p, reply: make(chan answer, 2), alias: act.b%5 == 0}
		}
		w.waiters = append(w.waiters, wt)
		w.note("%s %s", map[bool]string{true: "load", false: "request"}[act.kind == aLoad], p)
		h.step(evAcquire{w: wt})
		w.takeGrant(wt)
	case aCancel:
		live := w.liveWaiters()
		if len(live) == 0 {
			return
		}
		wt := live[pick(len(live))]
		w.note("cancel waiter %d (%s)", wt.id, wt.profile)
		h.step(evCancel{w: wt})
	case aRelease:
		if len(w.leases) == 0 {
			return
		}
		i := pick(len(w.leases))
		g := w.leases[i]
		w.leases = append(w.leases[:i], w.leases[i+1:]...)
		w.note("release lease %d (%s)", g.ID, g.Profile)
		h.step(evRelease{id: g.ID})
	case aProbe:
		if len(h.probes) == 0 {
			return
		}
		i := pick(len(h.probes))
		ef := h.probes[i]
		h.probes = append(h.probes[:i], h.probes[i+1:]...)
		var rs []probeOutcome
		bad := act.c % len(ef.targets)
		for j, tg := range ef.targets {
			n, _ := h.view.get(tg.id)
			o := probeOutcome{id: tg.id, addr: tg.addr, result: instance.ProbeResult{TotalMiB: n.TotalMiB, FreeMiB: n.FreeMiB}}
			switch {
			case act.b%10 == 0 && j == bad:
				o.err = probeFail(tg.addr)
			case act.b%10 == 1 && j == bad:
				o.err = &instance.ProbeError{Kind: instance.ProbeMultipleRPCLines, Addr: tg.addr}
			case act.b%10 == 2 && j == bad:
				o.result.FreeMiB = n.FreeMiB / 3 // much less than the fleet says
			case act.b%10 == 3 && j == bad:
				o.err = &instance.ProbeError{Kind: instance.ProbeCanceled, Addr: tg.addr, Err: context.Canceled}
			}
			rs = append(rs, o)
		}
		w.note("preflight iid=%d mode=%d", ef.iid, act.b%10)
		h.step(evPreflight{iid: ef.iid, results: rs})
	case aStart:
		if len(h.starts) == 0 {
			return
		}
		i := pick(len(h.starts))
		ef := h.starts[i]
		h.starts = append(h.starts[:i], h.starts[i+1:]...)
		if act.b%8 == 0 {
			h.launcher.failNext = errors.New("exec failed")
		}
		w.note("start iid=%d fail=%v", ef.iid, act.b%8 == 0)
		h.step(h.runStart(ef))
	case aHealthy:
		var cands []*inst
		for _, in := range h.c.sortedInsts() {
			if in.state == stStarting && in.phase == phLoading {
				cands = append(cands, in)
			}
		}
		if len(cands) == 0 {
			return
		}
		in := cands[pick(len(cands))]
		w.note("healthy %s", in.prof.Name)
		h.step(evHealthy{iid: in.iid})
		w.harvestAll()
	case aCrash, aStopExit:
		iids := w.childIIDs(act.kind == aStopExit)
		if len(iids) == 0 {
			return
		}
		iid := iids[pick(len(iids))]
		w.note("%v child iid=%d", map[bool]string{true: "stop-exit", false: "crash"}[act.kind == aStopExit], iid)
		h.exitChild(iid, instance.ExitStatus{Code: 1})
		h.step(evTick{})
	case aFetch:
		if len(h.fetches) == 0 {
			return
		}
		i := pick(len(h.fetches))
		ef := h.fetches[i]
		h.fetches = append(h.fetches[:i], h.fetches[i+1:]...)
		ev := h.runFetch(ef).(evFetched)
		if act.b%6 == 0 {
			ev.err = errors.New("fetch failed")
		}
		w.note("fetch %s err=%v", ef.id, ev.err)
		h.step(ev)
	case aToggleNode:
		id := w.nodeIDs[pick(len(w.nodeIDs))]
		if _, ok := h.view.get(id); !ok {
			return
		}
		h.view.modify(id, func(n *core.Node) { n.Online = !n.Online })
		w.note("toggle %s", id)
		h.step(evViewChanged{})
	case aMemory:
		id := w.nodeIDs[pick(len(w.nodeIDs))]
		if _, ok := h.view.get(id); !ok {
			return
		}
		h.view.modify(id, func(n *core.Node) { n.FreeMiB = act.b % (n.TotalMiB + 1) })
		w.note("memory %s", id)
		h.step(evViewChanged{})
	case aReconnect:
		if act.b%2 == 0 && len(w.removed) == 0 {
			for _, n := range h.view.Nodes() {
				if !n.Local {
					w.removed = append(w.removed, n)
					h.view.remove(n.ID)
				}
			}
		}
		h.view.reconnect()
		w.note("reconnect empty=%v", len(w.removed) > 0)
		h.step(evViewChanged{})
	case aRestore:
		if len(w.removed) == 0 {
			return
		}
		h.view.set(w.removed...)
		w.removed = nil
		w.note("restore nodes")
		h.step(evViewChanged{})
	case aStale:
		if act.b%2 == 0 {
			h.view.stale()
			w.note("feed stops")
		} else {
			h.view.fresh()
			w.note("feed resumes")
		}
		h.step(evViewChanged{})
	case aHealthErr:
		var cands []*inst
		for _, in := range h.c.sortedInsts() {
			if in.state == stStarting && in.phase == phLoading {
				cands = append(cands, in)
			}
		}
		if len(cands) == 0 {
			return
		}
		in := cands[pick(len(cands))]
		w.note("health error %s", in.prof.Name)
		h.step(evHealthy{iid: in.iid, err: errors.New("health check failed")})
	case aLoadTimeout:
		var dl time.Time
		for _, in := range h.c.insts {
			if in.state == stStarting && in.phase == phLoading && (dl.IsZero() || in.loadDL.Before(dl)) {
				dl = in.loadDL
			}
		}
		if dl.IsZero() {
			return
		}
		w.note("advance to the load deadline")
		h.advance(dl.Sub(h.clk.Now()) + time.Second)
	case aDropNode:
		var live []core.Node
		for _, n := range h.view.Nodes() {
			live = append(live, n)
		}
		if len(live) == 0 {
			return
		}
		n := live[pick(len(live))]
		w.dropped = append(w.dropped, n)
		h.view.remove(n.ID)
		w.note("drop node %s from the view", n.ID)
		h.step(evViewChanged{})
	case aReaddNode:
		if len(w.dropped) == 0 {
			return
		}
		i := pick(len(w.dropped))
		n := w.dropped[i]
		w.dropped = append(w.dropped[:i], w.dropped[i+1:]...)
		h.view.set(n)
		w.note("re-add node %s", n.ID)
		h.step(evViewChanged{})
	case aAdvance:
		d := []time.Duration{time.Second, 3 * time.Second, 10 * time.Second, 31 * time.Second, 2 * time.Minute, 7 * time.Minute}[act.b%6]
		w.note("advance %v", d)
		h.advance(d)
	case aUnload:
		p := w.profiles[pick(len(w.profiles))]
		w.note("unload %s force=%v", p, act.b%3 == 0)
		h.step(evUnload{profile: p, force: act.b%3 == 0})
	case aUnloadIdle:
		w.note("unload idle")
		h.step(evUnloadIdle{})
	}
	w.harvestAll()
}

func (w *randWorld) childIIDs(stopped bool) []uint64 {
	var out []uint64
	for iid, ch := range w.h.children {
		if !stopped || len(ch.stopModes()) > 0 {
			out = append(out, iid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// takeGrant records a lease from a waiter's answer, once.
func (w *randWorld) takeGrant(wt *waiter) {
	if wt.reply == nil || len(wt.reply) != 1 || w.leased[wt] {
		return
	}
	a := <-wt.reply
	wt.reply <- a
	if a.grant != nil {
		w.leases = append(w.leases, a.grant)
		w.leased[wt] = true
	}
}

func (w *randWorld) harvestAll() {
	for _, wt := range w.waiters {
		w.takeGrant(wt)
	}
	w.harvestLeases()
}

// quiesce stops generating requests and lets everything complete.
func (w *randWorld) quiesce() {
	h := w.h
	if len(w.removed) > 0 {
		h.view.set(w.removed...)
		w.removed = nil
	}
	if len(w.dropped) > 0 {
		h.view.set(w.dropped...)
		w.dropped = nil
	}
	h.view.fresh()
	for _, id := range w.nodeIDs {
		if _, ok := h.view.get(id); ok {
			h.view.modify(id, func(n *core.Node) { n.Online = true })
		}
	}
	h.step(evViewChanged{})
	for round := 0; round < 400; round++ {
		w.note("quiesce round %d", round)
		for len(w.leases) > 0 {
			g := w.leases[0]
			w.leases = w.leases[1:]
			h.step(evRelease{id: g.ID})
			w.harvestAll()
		}
		for len(h.probes) > 0 {
			ef := h.probes[0]
			h.probes = h.probes[1:]
			h.step(h.runProbe(ef))
		}
		for len(h.starts) > 0 {
			ef := h.starts[0]
			h.starts = h.starts[1:]
			h.step(h.runStart(ef))
		}
		for _, in := range h.c.sortedInsts() {
			if in.state == stStarting && in.phase == phLoading {
				h.step(evHealthy{iid: in.iid})
			}
		}
		for _, iid := range w.childIIDs(true) {
			h.exitChild(iid, instance.ExitStatus{Code: 0, Requested: true})
			h.step(evTick{})
		}
		for len(h.fetches) > 0 {
			ef := h.fetches[0]
			h.fetches = h.fetches[1:]
			h.step(h.runFetch(ef))
		}
		w.harvestAll()
		if round > 20 {
			h.step(evUnloadIdle{})
		}
		h.advance(time.Minute)
		if len(w.leases) == 0 && len(h.probes) == 0 && len(h.starts) == 0 && len(h.fetches) == 0 &&
			len(h.c.waiters) == 0 && len(h.c.insts) == 0 && len(h.c.entries) == 0 && len(h.children) == 0 {
			break
		}
	}
}

func (w *randWorld) checkQuiescent() {
	h := w.h
	if n := len(h.c.insts) + len(h.c.entries) + len(h.c.waiters) + len(h.c.leases); n != 0 {
		panic(harnessFailure{fmt.Sprintf("not quiescent: %d instances, %d entries, %d waiters, %d leases",
			len(h.c.insts), len(h.c.entries), len(h.c.waiters), len(h.c.leases))})
	}
	if len(h.probes)+len(h.starts)+len(h.fetches)+len(h.children) != 0 {
		panic(harnessFailure{fmt.Sprintf("work left: %d probes %d starts %d fetches %d children",
			len(h.probes), len(h.starts), len(h.fetches), len(h.children))})
	}
	for _, wt := range w.waiters {
		if !wt.answered {
			panic(harnessFailure{fmt.Sprintf("waiter %d (%s) never answered", wt.id, wt.profile)})
		}
		if wt.reply != nil && len(wt.reply) != 1 {
			panic(harnessFailure{fmt.Sprintf("waiter %d (%s) has %d answers", wt.id, wt.profile, len(wt.reply))})
		}
	}
	if dl, ok := h.c.nextDeadline(); ok {
		panic(harnessFailure{fmt.Sprintf("timer left at quiescence: %v", dl.Sub(h.clk.Now()))})
	}
	h.step(evShutdown{})
	if !h.c.finished {
		panic(harnessFailure{"shutdown did not finish at quiescence"})
	}
}

// runSeq replays actions on the world built from seed; it returns the
// failure message (empty when all is well) and the event log.
func runSeq(seed int64, acts []action) (fail string, log []string) {
	var w *randWorld
	defer func() {
		if r := recover(); r != nil {
			if hf, ok := r.(harnessFailure); ok {
				fail = hf.msg
			} else {
				fail = fmt.Sprint(r)
			}
			if w != nil {
				log = w.log
			}
		}
	}()
	w = newRandWorld(seed)
	// Shutdown placement varies by seed: none, late (90%), part-way through
	// at a seed-derived point, and part-way followed by a forced shutdown.
	shutdownAt, forceAt := -1, -1
	sr := rand.New(rand.NewSource(seed*31 + 7))
	switch seed % 4 {
	case 1:
		shutdownAt = len(acts) * 9 / 10
	case 2:
		shutdownAt = sr.Intn(max(len(acts), 1))
	case 3:
		shutdownAt = sr.Intn(max(len(acts), 1))
		forceAt = shutdownAt + sr.Intn(max(len(acts)-shutdownAt, 1))
	}
	for i, a := range acts {
		if i == shutdownAt {
			w.note("shutdown")
			w.h.step(evShutdown{})
		}
		if i == forceAt {
			w.note("forced shutdown")
			w.h.step(evShutdown{force: true})
		}
		w.apply(a)
	}
	w.quiesce()
	w.checkQuiescent()
	return "", w.log
}

// shrink removes chunks of actions while the sequence still fails.
func shrink(seed int64, acts []action, budget int) []action {
	cur := acts
	for chunk := len(cur) / 2; chunk >= 1 && budget > 0; chunk /= 2 {
		for i := 0; i+chunk <= len(cur) && budget > 0; {
			cand := append(append([]action(nil), cur[:i]...), cur[i+chunk:]...)
			budget--
			if f, _ := runSeq(seed, cand); f != "" {
				cur = cand
			} else {
				i += chunk
			}
		}
	}
	return cur
}

func TestRandom_InvariantsAndQuiescence(t *testing.T) {
	steps := *flagSteps
	if testing.Short() {
		steps /= 5
	}
	for s := int64(0); s < int64(*flagSeeds); s++ {
		seed := *flagBase + s
		acts := genActions(rand.New(rand.NewSource(seed*7919+1)), steps)
		fail, _ := runSeq(seed, acts)
		if fail == "" {
			continue
		}
		min := shrink(seed, acts, 600)
		f2, log := runSeq(seed, min)
		if f2 == "" {
			f2, log = fail, nil
		}
		if len(log) > 300 {
			log = append([]string{"..."}, log[len(log)-300:]...)
		}
		t.Fatalf("seed %d failed after shrinking %d → %d actions:\n%s\nminimal trace:\n%s", seed, len(acts), len(min), f2, strings.Join(log, "\n"))
	}
}
