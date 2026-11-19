package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// ---- fake worker connect prober ----

// fakeWorkerProber answers the worker health probe: it connects unless the
// address is in down.
type fakeWorkerProber struct {
	mu    sync.Mutex
	down  map[string]bool
	calls []string
}

func newFakeWorkerProber() *fakeWorkerProber { return &fakeWorkerProber{down: map[string]bool{}} }

func (p *fakeWorkerProber) Connect(_ context.Context, addr string, _ time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, addr)
	if p.down[addr] {
		return errors.New("connect: i/o timeout")
	}
	return nil
}

func (p *fakeWorkerProber) setDown(addr string, down bool) {
	p.mu.Lock()
	p.down[addr] = down
	p.mu.Unlock()
}

func (p *fakeWorkerProber) callsTo(addr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.calls {
		if a == addr {
			n++
		}
	}
	return n
}

// ---- fake launcher and child ----

type fakeLauncher struct {
	mu          sync.Mutex
	nextPort    int
	failNext    error
	children    []*fakeChild
	autoHealthy bool  // runner tests: children are healthy at once
	autoExit    bool  // runner tests: Stop makes the child exit
	sent        int64 // runner tests: children count this many bytes sent (0: they cannot count)
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
		logFile: fmt.Sprintf("/logs/%s-%d.log", spec.Profile, spec.InstanceID),
		exitCh:  make(chan instance.ExitStatus, 1), healthyCh: make(chan struct{}), gone: make(chan struct{}),
		tail: []string{"loading model", "error: out of memory"}}
	ch.autoExit = l.autoExit
	if l.sent > 0 {
		ch.counting, ch.sent, ch.socks = true, l.sent, 1
	}
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
	logFile   string

	// Upload counting (instance.ByteCounter): off unless counting is set,
	// when SentBytes answers sent/socks/sentErr and counts its calls.
	counting  bool
	sent      int64
	socks     int
	sentErr   error
	sentPeers map[string]int64
	sentCall  int
}

func (c *fakeChild) SentBytes(peers []string) (instance.SentCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sentCall++
	if !c.counting {
		return instance.SentCount{}, instance.ErrSentBytesUnsupported
	}
	var per map[string]int64
	if c.sentPeers != nil {
		per = map[string]int64{}
		for k, v := range c.sentPeers {
			per[k] = v
		}
	}
	return instance.SentCount{Bytes: c.sent, Sockets: c.socks, Peers: per}, c.sentErr
}

// setSentPeers is setSent with per-worker figures (the sum is their total).
func (c *fakeChild) setSentPeers(per map[string]int64) {
	var sum int64
	for _, v := range per {
		sum += v
	}
	c.mu.Lock()
	c.counting, c.sent, c.socks, c.sentErr, c.sentPeers = true, sum, len(per), nil, per
	c.mu.Unlock()
}

func (c *fakeChild) setSent(bytes int64, socks int, err error) {
	c.mu.Lock()
	c.counting, c.sent, c.socks, c.sentErr = true, bytes, socks, err
	c.mu.Unlock()
}

func (c *fakeChild) sentCalls() int { c.mu.Lock(); defer c.mu.Unlock(); return c.sentCall }

func (c *fakeChild) LogFile() string { return c.logFile }

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

// ---- capturing logger ----

type logRec struct {
	Level slog.Level
	Msg   string
	Attrs map[string]any
}

// capLog is a slog.Handler that records every record, so tests assert on
// messages and attribute keys, never on formatting.
type capLog struct {
	mu   sync.Mutex
	recs []logRec
}

func (l *capLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *capLog) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *capLog) WithGroup(string) slog.Handler            { return l }
func (l *capLog) Handle(_ context.Context, r slog.Record) error {
	rec := logRec{Level: r.Level, Msg: r.Message, Attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool { rec.Attrs[a.Key] = a.Value.Any(); return true })
	l.mu.Lock()
	l.recs = append(l.recs, rec)
	l.mu.Unlock()
	return nil
}

func (l *capLog) all() []logRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]logRec(nil), l.recs...)
}

// find returns the records with message msg whose attrs include every
// key/value pair in match (given as alternating key, value).
func (l *capLog) find(msg string, match ...any) []logRec {
	var out []logRec
	for _, r := range l.all() {
		if r.Msg != msg {
			continue
		}
		ok := true
		for i := 0; i+1 < len(match); i += 2 {
			if fmt.Sprint(r.Attrs[match[i].(string)]) != fmt.Sprint(match[i+1]) {
				ok = false
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// ---- fake recovery poster ----

// recPost is one recovery POST the scheduler made.
type recPost struct {
	At      time.Time
	URL     string
	Body    []byte
	Timeout time.Duration
}

// record decodes the body.
func (p recPost) record(t fatalT) recoveryRecord {
	t.Helper()
	var r recoveryRecord
	if err := json.Unmarshal(p.Body, &r); err != nil {
		t.Fatalf("recovery body %s: %v", p.Body, err)
	}
	return r
}

// fakeRecoveryPoster records every POST and answers from a script (one entry
// per call, in order), then with 200.
type fakeRecoveryPoster struct {
	mu     sync.Mutex
	clk    *fakeClock
	posts  []recPost
	script []recResp
}

type recResp struct {
	status int
	body   string
	err    error
}

func (p *fakeRecoveryPoster) Post(_ context.Context, url string, body []byte, timeout time.Duration) (RecoveryResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.posts = append(p.posts, recPost{At: p.clk.Now(), URL: url, Body: append([]byte(nil), body...), Timeout: timeout})
	if len(p.script) == 0 {
		return RecoveryResponse{Status: 200, BodyTail: `{"ok":true}`}, nil
	}
	r := p.script[0]
	p.script = p.script[1:]
	return RecoveryResponse{Status: r.status, BodyTail: r.body}, r.err
}

func (p *fakeRecoveryPoster) all() []recPost {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recPost(nil), p.posts...)
}
