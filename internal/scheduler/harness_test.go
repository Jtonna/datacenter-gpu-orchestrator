package scheduler

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
)

// harness drives schedCore.handle directly: no goroutines, no real time.
// Effects are executed synchronously against the fakes; follow-up events
// are queued and handled in order. Auto flags decide which async results
// arrive at once; the rest stay pending until the test completes them.
// fatalT is the part of testing.TB the harness uses; the random tests
// substitute one that panics, so a failing sequence can be replayed.
type fatalT interface {
	Helper()
	Fatalf(format string, args ...any)
}

type harness struct {
	t        fatalT
	clk      *fakeClock
	view     *fakeView
	prober   *fakeProber
	wprober  *fakeWorkerProber
	rposter  *fakeRecoveryPoster
	launcher *fakeLauncher
	cfg      *config.Config
	needs    map[string]NeedInfo
	needErr  map[string]error
	c        *schedCore
	logs     *capLog

	autoProbe, autoStart, autoExit, autoFetch, autoSample bool
	holdWorkerProbes                                      bool // worker connect probes wait in wprobes
	wprobes                                               []effWorkerProbe

	probes   []effProbe
	starts   []effStart
	fetches  []effFetch
	children map[uint64]*fakeChild // iid → child not yet reported exited
	queue    []event
	stepping bool
	trace    []string
	feedLog  []FeedEvent // every feed event taken after a step
}

func ptrDur(d time.Duration) *time.Duration { return &d }

func testConfig() *config.Config {
	d := config.DefaultDefaults()
	// The connect probe is off by default; most health tests exercise it, so
	// the harness turns it on. Tests of the default set WorkerProbeInterval = 0.
	d.WorkerProbeInterval = 10 * time.Second
	return &config.Config{
		Fleet:     config.Fleet{LocalNode: "omen", StaleAfter: 60 * time.Second},
		ModelsDir: "/models",
		Defaults:  d,
		Profiles:  map[string]*core.Profile{},
	}
}

func newHarness(t fatalT) *harness {
	clk := newFakeClock()
	view := newFakeView(clk)
	h := &harness{
		t: t, clk: clk, view: view, prober: newFakeProber(view), wprober: newFakeWorkerProber(), rposter: &fakeRecoveryPoster{clk: clk}, launcher: newFakeLauncher(),
		cfg: testConfig(), needs: map[string]NeedInfo{}, needErr: map[string]error{},
		autoProbe: true, autoStart: true, autoExit: true, autoFetch: true, autoSample: true,
		children: map[uint64]*fakeChild{}, logs: &capLog{},
	}
	needs := NeedFunc(func(p *core.Profile) (NeedInfo, error) {
		if err := h.needErr[p.Name]; err != nil {
			return NeedInfo{}, err
		}
		if n, ok := h.needs[p.Name]; ok {
			return n, nil
		}
		return NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1 << 30}, nil
	})
	h.c = newCore(h.cfg, clk, view, needs, instance.OptionsFromConfig(h.cfg), slog.New(h.logs))
	return h
}

// ---- cluster fixtures ----

func ps5(id string, n int, free int) core.Node {
	return core.Node{ID: core.NodeID(id), Hostname: id, Online: true, Addr: fmt.Sprintf("10.0.0.%d:50052", n),
		TotalMiB: 16000, FreeMiB: free, Priority: 1, HeadroomMiB: 1000, SharedMemory: true, LinkMBs: 11, Settle: 3 * time.Second}
}

func alien(id string, free int) core.Node {
	return core.Node{ID: core.NodeID(id), Hostname: id, Online: true, Addr: "10.0.0.3:50052",
		TotalMiB: 8000, FreeMiB: free, Priority: 2, HeadroomMiB: 200, LinkMBs: 11, Settle: 8 * time.Second}
}

func omen(free int) core.Node {
	return core.Node{ID: "omen", Hostname: "omen", Local: true, Online: true,
		TotalMiB: 8000, FreeMiB: free, Priority: 3, HeadroomMiB: 200, Settle: 2 * time.Second}
}

func (h *harness) nodes(ns ...core.Node) *harness {
	h.view.set(ns...)
	return h
}

func (h *harness) llama(name string, needs int, mod ...func(*core.Profile)) *core.Profile {
	p := &core.Profile{Name: name, Kind: core.KindLlama, Model: name + ".gguf", Ctx: 4096, Parallel: 1, NeedsMiB: needs}
	for _, m := range mod {
		m(p)
	}
	h.cfg.Profiles[name] = p
	return p
}

func (h *harness) command(name string, needs int) *core.Profile {
	p := &core.Profile{Name: name, Kind: core.KindCommand, Cmd: "whisper --port ${PORT}", CheckEndpoint: "/ready", NeedsMiB: needs}
	h.cfg.Profiles[name] = p
	return p
}

// ---- stepping ----

func (h *harness) step(ev event) {
	h.t.Helper()
	h.queue = append(h.queue, ev)
	if h.stepping {
		return
	}
	h.stepping = true
	defer func() { h.stepping = false }()
	for len(h.queue) > 0 {
		e := h.queue[0]
		h.queue = h.queue[1:]
		h.trace = append(h.trace, fmt.Sprintf("%s %s", h.clk.Now().Sub(t0), describe(e)))
		h.c.handle(e)
		if err := h.c.checkInvariants(); err != nil {
			h.t.Fatalf("invariant after %s: %v\ntrace:\n%s", describe(e), err, strings.Join(h.trace, "\n"))
		}
		h.drain()
	}
}

func describe(e event) string {
	switch e := e.(type) {
	case evAcquire:
		return "acquire " + e.w.profile
	case evCancel:
		return "cancel " + e.w.profile
	case evRelease:
		return fmt.Sprintf("release %d", e.id)
	case evPreflight:
		return fmt.Sprintf("preflight iid=%d %d results", e.iid, len(e.results))
	case evChildStarted:
		return fmt.Sprintf("child-started iid=%d err=%v", e.iid, e.err)
	case evHealthy:
		return fmt.Sprintf("healthy iid=%d err=%v", e.iid, e.err)
	case evExited:
		return fmt.Sprintf("exited iid=%d", e.iid)
	case evFetched:
		return fmt.Sprintf("fetched %s err=%v", e.id, e.err)
	case evLoadSample:
		return fmt.Sprintf("load-sample iid=%d bytes=%d sockets=%d err=%v", e.iid, e.bytes, e.sockets, e.err)
	}
	return fmt.Sprintf("%T", e)
}

// drain executes the core's effects against the fakes.
func (h *harness) drain() {
	for _, ef := range h.c.takeEffects() {
		switch ef := ef.(type) {
		case effReply:
			select {
			case ef.w.reply <- ef.a:
			default:
				h.t.Fatalf("waiter %d answered more than once", ef.w.id)
			}
		case effProbe:
			if h.autoProbe {
				h.queue = append(h.queue, h.runProbe(ef))
			} else {
				h.probes = append(h.probes, ef)
			}
		case effStart:
			if h.autoStart {
				h.queue = append(h.queue, h.runStart(ef))
			} else {
				h.starts = append(h.starts, ef)
			}
		case effStop:
			ch := ef.child.(*fakeChild)
			ch.Stop(ef.mode)
			if h.autoExit {
				h.exitChild(ef.iid, instance.ExitStatus{Code: -1, Signal: "terminated", Requested: true})
			}
		case effFetch:
			if h.autoFetch {
				h.queue = append(h.queue, h.runFetch(ef))
			} else {
				h.fetches = append(h.fetches, ef)
			}
		case effWorkerProbe:
			if h.holdWorkerProbes {
				h.wprobes = append(h.wprobes, ef)
			} else {
				h.queue = append(h.queue, evWorkerProbe{id: ef.id, gen: ef.gen, err: h.wprober.Connect(nil, ef.addr, workerProbeTimeout)})
			}
		case effRecoveryPost:
			r, err := h.rposter.Post(nil, ef.url, ef.body, ef.timeout)
			h.queue = append(h.queue, evRecoveryPosted{id: ef.id, epoch: ef.epoch, action: ef.action, nonce: ef.nonce,
				status: r.Status, tail: r.BodyTail, err: err})
		case effCancelHealth:
		case effSample:
			if h.autoSample {
				sc, err := ef.counter.SentBytes(ef.peers)
				h.queue = append(h.queue, evLoadSample{iid: ef.iid, bytes: sc.Bytes, sockets: sc.Sockets, peers: sc.Peers, err: err})
			}
		default:
			h.t.Fatalf("unknown effect %T", ef)
		}
	}
	h.feedLog = append(h.feedLog, h.c.takeFeed()...)
}

func (h *harness) runProbe(ef effProbe) event {
	var rs []probeOutcome
	for _, tg := range ef.targets {
		r, err := h.prober.Probe(nil, tg.addr)
		rs = append(rs, probeOutcome{id: tg.id, addr: tg.addr, result: r, err: err})
	}
	return evPreflight{iid: ef.iid, results: rs}
}

func (h *harness) runStart(ef effStart) event {
	ch, err := h.launcher.Start(ef.spec)
	if err != nil {
		return evChildStarted{iid: ef.iid, err: err}
	}
	h.children[ef.iid] = ch.(*fakeChild)
	return evChildStarted{iid: ef.iid, child: ch}
}

func (h *harness) runFetch(ef effFetch) event {
	n, err := h.view.FetchNode(nil, ef.id)
	return evFetched{id: ef.id, gen: ef.gen, node: n, err: err}
}

func (h *harness) exitChild(iid uint64, st instance.ExitStatus) {
	ch := h.children[iid]
	if ch == nil || !ch.exit(st) {
		return
	}
	delete(h.children, iid)
	got := <-ch.exitCh
	h.queue = append(h.queue, evExited{iid: iid, status: got})
}

// completeProbes finishes held pre-flights (autoProbe off), in order.
func (h *harness) completeProbes() {
	h.t.Helper()
	ps := h.probes
	h.probes = nil
	for _, p := range ps {
		h.step(h.runProbe(p))
	}
}

func (h *harness) completeStarts() {
	h.t.Helper()
	ss := h.starts
	h.starts = nil
	for _, s := range ss {
		h.step(h.runStart(s))
	}
}

func (h *harness) completeFetches() {
	h.t.Helper()
	fs := h.fetches
	h.fetches = nil
	for _, f := range fs {
		h.step(h.runFetch(f))
	}
}

// ---- requests ----

func (h *harness) request(profile string) *waiter {
	h.t.Helper()
	w := &waiter{profile: profile, reply: make(chan answer, 2)}
	h.step(evAcquire{w: w})
	return w
}

func (h *harness) load(profile string) *waiter {
	h.t.Helper()
	w := &waiter{profile: profile, synthetic: true}
	h.step(evAcquire{w: w})
	return w
}

func (h *harness) cancel(w *waiter) { h.t.Helper(); h.step(evCancel{w: w}) }

func (h *harness) peek(w *waiter) (answer, bool) {
	select {
	case a := <-w.reply:
		w.reply <- a
		return a, true
	default:
		return answer{}, false
	}
}

func (h *harness) pending(w *waiter) {
	h.t.Helper()
	if a, ok := h.peek(w); ok {
		h.t.Fatalf("waiter for %s answered %+v (err %v), want pending", w.profile, a.grant, a.err)
	}
}

func (h *harness) granted(w *waiter) *grant {
	h.t.Helper()
	a, ok := h.peek(w)
	if !ok {
		h.t.Fatalf("waiter for %s not answered, want a lease\ntrace:\n%s", w.profile, strings.Join(h.trace, "\n"))
	}
	if a.err != nil {
		h.t.Fatalf("waiter for %s got error %v, want a lease", w.profile, a.err)
	}
	return a.grant
}

func (h *harness) failed(w *waiter, status int, reason Reason) *Error {
	h.t.Helper()
	a, ok := h.peek(w)
	if !ok {
		h.t.Fatalf("waiter for %s not answered, want %d %s\ntrace:\n%s", w.profile, status, reason, strings.Join(h.trace, "\n"))
	}
	if a.err == nil {
		h.t.Fatalf("waiter for %s got a lease, want %d %s", w.profile, status, reason)
	}
	if a.err.Status != status || a.err.Reason != reason {
		h.t.Fatalf("waiter for %s got %d %q (%s), want %d %q", w.profile, a.err.Status, a.err.Reason, a.err.Message, status, reason)
	}
	return a.err
}

func (h *harness) release(g *grant) { h.t.Helper(); h.step(evRelease{id: g.ID}) }

// ---- instance control ----

func (h *harness) inst(profile string) *inst {
	h.t.Helper()
	in := h.c.insts[profile]
	if in == nil {
		h.t.Fatalf("no instance for %s", profile)
	}
	return in
}

func (h *harness) state(profile string) string {
	if in := h.c.insts[profile]; in != nil {
		return in.state.String()
	}
	return "absent"
}

func (h *harness) wantState(profile, want string) {
	h.t.Helper()
	if got := h.state(profile); got != want {
		h.t.Fatalf("%s is %s, want %s\ntrace:\n%s", profile, got, want, strings.Join(h.trace, "\n"))
	}
}

func (h *harness) healthy(profile string) {
	h.t.Helper()
	in := h.inst(profile)
	if ch := h.children[in.iid]; ch != nil {
		ch.becomeHealthy()
	}
	h.step(evHealthy{iid: in.iid})
}

// ready runs a request for profile through to ready and returns its lease.
func (h *harness) ready(profile string) *grant {
	h.t.Helper()
	w := h.request(profile)
	h.healthy(profile)
	return h.granted(w)
}

func (h *harness) exit(profile string, code int) {
	h.t.Helper()
	in := h.inst(profile)
	h.exitChild(in.iid, instance.ExitStatus{Code: code})
	h.step(evTick{})
}

func (h *harness) child(profile string) *fakeChild {
	h.t.Helper()
	in := h.inst(profile)
	ch, ok := in.child.(*fakeChild)
	if !ok || ch == nil {
		h.t.Fatalf("%s has no child", profile)
	}
	return ch
}

func (h *harness) nodesOf(profile string) []core.NodeID {
	h.t.Helper()
	ids := h.inst(profile).nodeIDs()
	slices.Sort(ids)
	return ids
}

func (h *harness) wantNodes(profile string, want ...core.NodeID) {
	h.t.Helper()
	got := h.nodesOf(profile)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		h.t.Fatalf("%s placed on %v, want %v", profile, got, want)
	}
}

// ---- time ----

// advance moves the clock by d, stepping at every core deadline on the way.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	target := h.clk.Now().Add(d)
	for i := 0; ; i++ {
		if i > 100000 {
			h.t.Fatalf("advance: deadline loop")
		}
		dl, ok := h.c.nextDeadline()
		if !ok || dl.After(target) {
			break
		}
		if !dl.After(h.clk.Now()) {
			h.t.Fatalf("advance: deadline %v not after now %v", dl, h.clk.Now())
		}
		h.clk.Set(dl)
		h.step(evTick{})
	}
	h.clk.Set(target)
	h.step(evTick{})
}

func (h *harness) viewChanged() { h.t.Helper(); h.step(evViewChanged{}) }

func (h *harness) entry(profile string) *entry {
	h.t.Helper()
	e := h.c.entries[profile]
	if e == nil {
		h.t.Fatalf("no queue entry for %s", profile)
	}
	return e
}

func (h *harness) noEntry(profile string) {
	h.t.Helper()
	if e := h.c.entries[profile]; e != nil {
		h.t.Fatalf("queue entry for %s exists (claim %v, %d waiters), want none", profile, e.claim, len(e.waiters))
	}
}

func (h *harness) wantClaim(profile string, want ...core.NodeID) {
	h.t.Helper()
	e := h.entry(profile)
	got := slices.Clone(e.claim)
	slices.Sort(got)
	slices.Sort(want)
	if e.claim == nil || !slices.Equal(got, want) {
		h.t.Fatalf("%s claim = %v, want %v", profile, e.claim, want)
	}
}

var _ = http.StatusOK

var _ fatalT = testing.TB(nil)
