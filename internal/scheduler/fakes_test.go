package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/fleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
)

func init() { checkInvariantsEachStep = true }

var t0 = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

// ---- fake clock ----

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at   time.Time
	f    func()
	done bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: t0} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	if d <= 0 {
		t.done = true
		go f()
	}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.done
		t.done = true
		return was
	}
}

// Set moves the clock to at (never backwards) and fires due timers.
func (c *fakeClock) Set(at time.Time) {
	c.mu.Lock()
	if at.After(c.now) {
		c.now = at
	}
	var due []func()
	for _, t := range c.timers {
		if !t.done && !t.at.After(c.now) {
			t.done = true
			due = append(due, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		go f()
	}
}

func (c *fakeClock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// ---- fake fleet view ----

type fakeView struct {
	mu            sync.Mutex
	clk           *fakeClock
	nodes         map[core.NodeID]core.Node
	lastByte      time.Time
	never         bool
	frozen        bool // Age grows (no bytes arrive)
	connected     bool
	lastReconnect time.Time
	fetchErr      map[core.NodeID]error
	fetchCalls    map[core.NodeID]int
	changed       chan struct{}
}

func newFakeView(clk *fakeClock) *fakeView {
	return &fakeView{clk: clk, nodes: map[core.NodeID]core.Node{}, connected: true,
		lastReconnect: t0, fetchErr: map[core.NodeID]error{}, fetchCalls: map[core.NodeID]int{}, changed: make(chan struct{}, 1), lastByte: t0}
}

func (v *fakeView) Nodes() []core.Node {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]core.Node, 0, len(v.nodes))
	for _, n := range v.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (v *fakeView) Age() time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.never {
		return fleet.NeverReceived
	}
	if !v.frozen {
		v.lastByte = v.clk.Now()
	}
	return v.clk.Now().Sub(v.lastByte)
}

func (v *fakeView) Connected() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.connected }
func (v *fakeView) LastReconnect() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lastReconnect
}
func (v *fakeView) Changed() <-chan struct{} { return v.changed }

func (v *fakeView) FetchNode(_ context.Context, id core.NodeID) (core.Node, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fetchCalls[id]++
	if err := v.fetchErr[id]; err != nil {
		return core.Node{}, err
	}
	n, ok := v.nodes[id]
	if !ok {
		return core.Node{}, fleet.ErrNotFound
	}
	return n, nil
}

func (v *fakeView) Resolve(name string) (core.NodeID, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.nodes[core.NodeID(name)]; ok {
		return core.NodeID(name), nil
	}
	var online []core.NodeID
	var any []core.NodeID
	for id, n := range v.nodes {
		if strings.EqualFold(n.Hostname, name) {
			any = append(any, id)
			if n.Online {
				online = append(online, id)
			}
		}
	}
	switch {
	case len(online) >= 2:
		return "", &fleet.AmbiguousError{Name: name, IDs: online}
	case len(online) == 1:
		return online[0], nil
	case len(any) >= 1:
		return any[0], nil
	}
	return "", fleet.ErrNotFound
}

func (v *fakeView) wake() {
	select {
	case v.changed <- struct{}{}:
	default:
	}
}

func (v *fakeView) set(ns ...core.Node) {
	v.mu.Lock()
	for _, n := range ns {
		v.nodes[n.ID] = n
	}
	v.mu.Unlock()
	v.wake()
}

func (v *fakeView) modify(id core.NodeID, f func(*core.Node)) {
	v.mu.Lock()
	n := v.nodes[id]
	f(&n)
	v.nodes[id] = n
	v.mu.Unlock()
	v.wake()
}

func (v *fakeView) remove(id core.NodeID) {
	v.mu.Lock()
	delete(v.nodes, id)
	v.mu.Unlock()
	v.wake()
}

func (v *fakeView) get(id core.NodeID) (core.Node, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	n, ok := v.nodes[id]
	return n, ok
}

// stale stops bytes arriving; fresh resumes them now.
func (v *fakeView) stale() {
	v.mu.Lock()
	if !v.frozen {
		v.lastByte = v.clk.Now()
	}
	v.frozen = true
	v.mu.Unlock()
}

func (v *fakeView) fresh() {
	v.mu.Lock()
	v.frozen, v.never = false, false
	v.lastByte = v.clk.Now()
	v.mu.Unlock()
	v.wake()
}

func (v *fakeView) reconnect() {
	v.mu.Lock()
	v.lastReconnect = v.clk.Now()
	v.frozen, v.never = false, false
	v.lastByte = v.clk.Now()
	v.connected = true
	v.mu.Unlock()
	v.wake()
}

// ---- fake prober ----

type fakeProber struct {
	mu    sync.Mutex
	view  *fakeView
	free  map[string]int   // addr → free MiB to report (else the view's figure)
	fail  map[string]error // addr → failure
	calls []string
	block chan struct{} // runner tests: Probe waits on this when set
}

func newFakeProber(v *fakeView) *fakeProber {
	return &fakeProber{view: v, free: map[string]int{}, fail: map[string]error{}}
}

func (p *fakeProber) Probe(ctx context.Context, addr string) (instance.ProbeResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, addr)
	block := p.block
	p.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return instance.ProbeResult{}, &instance.ProbeError{Kind: instance.ProbeCanceled, Addr: addr, Err: ctx.Err()}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail[addr]; err != nil {
		return instance.ProbeResult{}, err
	}
	for _, n := range p.view.Nodes() {
		if n.Addr == addr {
			free := n.FreeMiB
			if f, ok := p.free[addr]; ok {
				free = f
			}
			return instance.ProbeResult{TotalMiB: n.TotalMiB, FreeMiB: free}, nil
		}
	}
	return instance.ProbeResult{}, &instance.ProbeError{Kind: instance.ProbeExit, Addr: addr, ExitCode: -1, Detail: "closed port"}
}

func probeFail(addr string) error {
	return &instance.ProbeError{Kind: instance.ProbeTimeout, Addr: addr, Detail: "busy worker"}
}

// ---- fake launcher and child ----

type fakeLauncher struct {
	mu          sync.Mutex
	nextPort    int
	failNext    error
	children    []*fakeChild
	autoHealthy bool // runner tests: children are healthy at once
	autoExit    bool // runner tests: Stop makes the child exit
}

func newFakeLauncher() *fakeLauncher { return &fakeLauncher{nextPort: 5800} }

func (l *fakeLauncher) Start(spec instance.StartSpec) (instance.Child, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.failNext; err != nil {
		l.failNext = nil
		return nil, err
	}
	l.nextPort++
	argv, err := spec.Build(l.nextPort)
	if err != nil {
		return nil, err
	}
	ch := &fakeChild{port: l.nextPort, argv: argv, health: spec.HealthPath,
		exitCh: make(chan instance.ExitStatus, 1), healthyCh: make(chan struct{}), gone: make(chan struct{}),
		tail: []string{"loading model", "error: out of memory"}}
	ch.autoExit = l.autoExit
	if l.autoHealthy {
		ch.becomeHealthy()
	}
	l.children = append(l.children, ch)
	return ch, nil
}

func (l *fakeLauncher) all() []*fakeChild {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*fakeChild(nil), l.children...)
}

type fakeChild struct {
	mu        sync.Mutex
	port      int
	argv      []string
	health    string
	stops     []instance.StopMode
	exited    bool
	exitCh    chan instance.ExitStatus
	healthyCh chan struct{}
	healthy   bool
	gone      chan struct{}
	tail      []string
	load      instance.LoadStats
	autoExit  bool // runner tests: Stop makes the child exit
}

func (c *fakeChild) Port() int      { return c.port }
func (c *fakeChild) PID() int       { return 1000 + c.port }
func (c *fakeChild) Argv() []string { return append([]string(nil), c.argv...) }
func (c *fakeChild) Tail(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.tail) > n {
		return append([]string(nil), c.tail[len(c.tail)-n:]...)
	}
	return append([]string(nil), c.tail...)
}
func (c *fakeChild) Load() instance.LoadStats           { c.mu.Lock(); defer c.mu.Unlock(); return c.load }
func (c *fakeChild) Exited() <-chan instance.ExitStatus { return c.exitCh }

func (c *fakeChild) WaitHealthy(ctx context.Context) error {
	select {
	case <-c.healthyCh:
		return nil
	case <-c.gone:
		return fmt.Errorf("%w", instance.ErrChildExited)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeChild) Stop(mode instance.StopMode) {
	c.mu.Lock()
	c.stops = append(c.stops, mode)
	auto := c.autoExit
	c.mu.Unlock()
	if auto {
		c.exit(instance.ExitStatus{Code: -1, Signal: "killed", Requested: true})
	}
}

// exit makes the child end (once) and reports whether it was still running.
func (c *fakeChild) exit(st instance.ExitStatus) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exited {
		return false
	}
	c.exited = true
	if st.Tail == nil {
		st.Tail = append([]string(nil), c.tail...)
	}
	c.exitCh <- st
	close(c.exitCh)
	close(c.gone)
	return true
}

func (c *fakeChild) becomeHealthy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.healthy {
		c.healthy = true
		close(c.healthyCh)
	}
}

func (c *fakeChild) stopModes() []instance.StopMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]instance.StopMode(nil), c.stops...)
}

var errBoom = errors.New("boom")
