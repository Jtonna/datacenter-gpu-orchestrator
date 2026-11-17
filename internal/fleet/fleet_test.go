package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/fakefleet"
)

// ---- helpers ----

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type logSink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), sub)
}
func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type fakeProbe struct {
	mu    sync.Mutex
	out   string
	err   error
	calls int
}

func (p *fakeProbe) set(out string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out, p.err = out, err
}
func (p *fakeProbe) run(ctx context.Context, cmd string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return []byte(p.out), p.err
}
func (p *fakeProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func testConfig() *config.Config {
	local := true
	return &config.Config{
		Fleet:         config.Fleet{LocalNode: "omen-runner", StaleAfter: time.Minute},
		LocalProbeCmd: "probe-cmd",
		Tiers: []config.TierRule{
			{Match: config.Match{Hostname: "ps5-*"}, Tier: core.Tier{Priority: 1, HeadroomMiB: 1024, SharedMemory: true, LinkMBs: 11, Settle: 3 * time.Second}},
			{Match: config.Match{Hostname: "alienware*"}, Tier: core.Tier{Priority: 2, HeadroomMiB: 256, LinkMBs: 11, Settle: 8 * time.Second}},
			{Match: config.Match{Local: &local}, Tier: core.Tier{Priority: 3, HeadroomMiB: 256, Settle: 2 * time.Second}},
		},
		NodeOverrides: map[string]config.NodeOverride{},
	}
}

func toMember(fm fakefleet.Member) Member {
	b, _ := json.Marshal(fm)
	var m Member
	if err := json.Unmarshal(b, &m); err != nil {
		panic(err)
	}
	return m
}

func omenMember() fakefleet.Member {
	m := fakefleet.Relay("3c:52:82:11", "omen-runner", "10.0.0.1", 8, 2)
	m.Role, m.RPCPort = "host", nil
	return m
}

type env struct {
	t     *testing.T
	f     *fakefleet.Fleet
	c     *Client
	clock *fakeClock
	log   *logSink
	probe *fakeProbe
	cfg   *config.Config
	opts  Options
	done  chan struct{}
	stop  context.CancelFunc
}

// newEnv prepares a client against a fresh fake console. Call start to run it.
// A leak check is registered first so that it runs after everything else.
func newEnv(t *testing.T) *env {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		http.DefaultClient.CloseIdleConnections()
		eventually(t, func() bool { return runtime.NumGoroutine() <= before }, "goroutines to return to baseline")
	})
	e := &env{t: t, f: fakefleet.New(t), clock: newClock(), log: &logSink{}, probe: &fakeProbe{}, cfg: testConfig()}
	e.probe.set("", errors.New("nvidia-smi: not found"))
	e.cfg.Fleet.URL = e.f.URL()
	e.opts = Options{
		HTTPClient:     &http.Client{Transport: &http.Transport{}},
		Clock:          e.clock.Now,
		PollInterval:   5 * time.Millisecond,
		ProbeInterval:  2 * time.Millisecond,
		Backoff:        []time.Duration{2 * time.Millisecond},
		IdleTimeout:    5 * time.Second,
		RequestTimeout: 500 * time.Millisecond,
		RunProbe:       e.probe.run,
		Logger:         slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return e
}

func (e *env) start() *Client {
	e.t.Helper()
	e.opts.HTTPClient.Transport.(*http.Transport).DisableKeepAlives = false
	e.c = New(e.cfg, e.opts)
	ctx, cancel := context.WithCancel(context.Background())
	e.stop = cancel
	e.done = make(chan struct{})
	go func() { defer close(e.done); e.c.Run(ctx) }()
	e.t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(3 * time.Second):
			e.t.Error("Run did not return after cancel")
		}
		e.opts.HTTPClient.CloseIdleConnections()
	})
	return e.c
}

func (e *env) nodeByID(id string) (core.Node, bool) {
	for _, n := range e.c.Nodes() {
		if string(n.ID) == id {
			return n, true
		}
	}
	return core.Node{}, false
}

// ---- conversion ----

func TestNodeFromMember(t *testing.T) {
	cfg := testConfig()
	type tc struct {
		name   string
		m      fakefleet.Member
		local  bool
		ok     bool
		total  int
		free   int
		addr   string
		prio   int
		shared bool
	}
	nullVRAM := fakefleet.Relay("alienware-llm", "alienware-llm", "10.0.0.12", 8, 2)
	nullVRAM.VRAMTotal, nullVRAM.VRAMUsed = nil, nil
	nullRAM := fakefleet.PS5("p", "ps5-1", "10.0.0.2", 12, 1, 14, 1)
	nullRAM.RAMTotalGB = nil
	noIP := fakefleet.PS5("p", "ps5-1", "10.0.0.2", 12, 1, 14, 1)
	noIP.IP = nil
	noPort := fakefleet.PS5("p", "ps5-1", "10.0.0.2", 12, 1, 14, 1)
	noPort.RPCPort = nil
	cases := []tc{
		{name: "ps5 vram binding", m: fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2), ok: true, total: 12288, free: 7680, addr: "10.0.0.2:50052", prio: 1, shared: true},
		{name: "ps5 ram binding", m: fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 1, 14, 13), ok: true, total: 12288, free: 1024, addr: "10.0.0.2:50052", prio: 1, shared: true},
		{name: "dedicated vram ignores ram", m: fakefleet.Relay("alienware-llm", "alienware-llm", "10.0.0.12", 8, 2), ok: true, total: 8192, free: 6144, addr: "10.0.0.12:50052", prio: 2},
		{name: "never negative", m: fakefleet.Relay("alienware-llm", "alienware-llm", "10.0.0.12", 8, 9), ok: true, total: 8192, free: 0, addr: "10.0.0.12:50052", prio: 2},
		{name: "null vram is no memory", m: nullVRAM, ok: true, total: 0, free: 0, addr: "10.0.0.12:50052", prio: 2},
		{name: "shared with unknown ram is not free", m: nullRAM, ok: true, total: 12288, free: 0, addr: "10.0.0.2:50052", prio: 1, shared: true},
		{name: "null ip gives empty addr", m: noIP, ok: true, total: 12288, free: 11264, addr: "", prio: 1, shared: true},
		{name: "null port gives empty addr", m: noPort, ok: true, total: 12288, free: 11264, addr: "", prio: 1, shared: true},
		{name: "no tier", m: fakefleet.Relay("x", "mystery", "10.0.0.9", 8, 2), ok: false},
		{name: "local flag selects local tier", m: omenMember(), local: true, ok: true, total: 8192, free: 6144, addr: "", prio: 3},
		{name: "remote omen has no tier", m: omenMember(), ok: false},
	}
	for _, c := range cases {
		n, ok := nodeFromMember(cfg, toMember(c.m), c.local)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if n.TotalMiB != c.total || n.FreeMiB != c.free || n.Addr != c.addr || n.Priority != c.prio || n.SharedMemory != c.shared || n.Local != c.local || !n.Online {
			t.Errorf("%s: got %+v", c.name, n)
		}
		if string(n.ID) != c.m.ID || n.Hostname != c.m.Hostname {
			t.Errorf("%s: identity %+v", c.name, n)
		}
	}
	// tier settings are copied through
	n, _ := nodeFromMember(cfg, toMember(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2)), false)
	if n.HeadroomMiB != 1024 || n.LinkMBs != 11 || n.Settle != 3*time.Second {
		t.Errorf("tier settings: %+v", n)
	}
	// offline stays offline
	off := fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2)
	off.Online = false
	if n, _ := nodeFromMember(cfg, toMember(off), false); n.Online {
		t.Error("offline member converted as online")
	}
}

func TestNodeOverrideWinsByIDThenHostname(t *testing.T) {
	cfg := testConfig()
	cfg.NodeOverrides["alienware-llm"] = config.NodeOverride{RPC: "10.9.9.9:50052"}
	m := fakefleet.Relay("mac-1", "alienware-llm", "10.0.0.12", 8, 2)
	if n, _ := nodeFromMember(cfg, toMember(m), false); n.Addr != "10.9.9.9:50052" {
		t.Errorf("hostname override: %q", n.Addr)
	}
	cfg.NodeOverrides["mac-1"] = config.NodeOverride{RPC: "10.8.8.8:50052"}
	if n, _ := nodeFromMember(cfg, toMember(m), false); n.Addr != "10.8.8.8:50052" {
		t.Errorf("id override should win: %q", n.Addr)
	}
	// override works even when the fleet has no ip
	m.IP = nil
	if n, _ := nodeFromMember(cfg, toMember(m), false); n.Addr != "10.8.8.8:50052" {
		t.Errorf("override with null ip: %q", n.Addr)
	}
}

func TestParseSMI(t *testing.T) {
	ok := []struct {
		in          string
		total, used int
	}{
		{"8151, 2100\n", 8151, 2100},
		{"8151,2100", 8151, 2100},
		{"\r\n8151, 2100\r\n8192, 5\r\n", 8151, 2100}, // first GPU only
	}
	for _, c := range ok {
		tot, used, err := parseSMI([]byte(c.in))
		if err != nil || tot != c.total || used != c.used {
			t.Errorf("%q: %d %d %v", c.in, tot, used, err)
		}
	}
	for _, bad := range []string{"", "\n", "garbage", "8151", "8151, x", "a, 5", "8151, 2100, 3", "-1, 5"} {
		if _, _, err := parseSMI([]byte(bad)); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

// ---- stream, polling, age ----

func TestSnapshotOnConnectAndPush(t *testing.T) {
	e := newEnv(t)
	e.f.SetMembers(
		fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2),
		fakefleet.Relay("alienware-llm", "alienware-llm", "10.0.0.12", 8, 2),
		fakefleet.Relay("x", "mystery", "10.0.0.9", 8, 2),
	)
	c := e.start()
	eventually(t, func() bool { return len(c.Nodes()) == 2 }, "two nodes from the connect snapshot")
	if !c.Connected() {
		t.Error("not connected")
	}
	ns := c.Nodes()
	if ns[0].ID != "a8" || ns[1].ID != "alienware-llm" {
		t.Errorf("order/ids: %+v", ns)
	}

	e.f.Modify("a8", func(m *fakefleet.Member) { m.VRAMUsed = fakefleet.Ptr(10.0) })
	e.f.Push()
	eventually(t, func() bool { n, _ := e.nodeByID("a8"); return n.FreeMiB == 2048 }, "pushed snapshot applied")

	e.f.SetOnline("alienware-llm", false)
	e.f.Push()
	eventually(t, func() bool { n, _ := e.nodeByID("alienware-llm"); return !n.Online }, "offline applied")

	e.f.Remove("alienware-llm")
	e.f.Push()
	eventually(t, func() bool { _, ok := e.nodeByID("alienware-llm"); return !ok }, "removed member disappears")

	e.f.PushModels() // ignored, must not break anything
	e.f.Raw("event: surprise\ndata: {}\n\n")
	e.f.Raw("event: fleet\ndata: not-json\n\n")
	e.f.Push()
	eventually(t, func() bool { return e.log.count("bad fleet") >= 1 }, "bad json logged")
	if n, ok := e.nodeByID("a8"); !ok || n.FreeMiB != 2048 {
		t.Errorf("view damaged by junk events: %+v", c.Nodes())
	}
}

func TestSSEFramingAcrossChunks(t *testing.T) {
	e := newEnv(t)
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return len(c.Nodes()) == 1 }, "first snapshot")
	e.f.Remove("a8")
	e.f.Upsert(fakefleet.PS5("b9", "ps5-2", "10.0.0.3", 12, 0, 14, 0))
	// Build the event by hand and send it in odd pieces with CRLF line ends.
	e.f.Push()
	eventually(t, func() bool { _, ok := e.nodeByID("b9"); return ok }, "normal push")
	doc := `{"v":1,"updated":5,"count":1,"online_count":1,"members":[{"id":"c1","hostname":"ps5-3","ip":"10.0.0.4","role":"ps5-linux","source":"self","online":true,"rpc_port":50052,"vram_total":12,"vram_used":0,"ram_total_gb":14,"ram_used_gb":0}]}`
	e.f.Raw("event: fle")
	e.f.Raw("et\r\nid: 5\r\ndata: " + doc[:20])
	e.f.Raw(doc[20:] + "\r\n\r\n")
	eventually(t, func() bool { _, ok := e.nodeByID("c1"); return ok }, "chunked CRLF event")
}

func TestChangedCoalescesAndSkipsPings(t *testing.T) {
	e := newEnv(t)
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return len(c.Nodes()) == 1 }, "snapshot")
	// drain whatever the connect produced
	time.Sleep(20 * time.Millisecond)
	for len(c.Changed()) > 0 {
		<-c.Changed()
	}
	e.f.Ping()
	e.f.Push() // identical snapshot: no change
	e.f.Ping()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-c.Changed():
		t.Fatal("ping or unchanged snapshot fired Changed")
	default:
	}
	for i := 0; i < 5; i++ {
		used := float64(i + 1)
		e.f.Modify("a8", func(m *fakefleet.Member) { m.VRAMUsed = fakefleet.Ptr(used) })
		e.f.Push()
	}
	eventually(t, func() bool { n, _ := e.nodeByID("a8"); return n.FreeMiB == 7*1024 }, "last burst snapshot")
	select {
	case <-c.Changed():
	case <-time.After(time.Second):
		t.Fatal("burst did not fire Changed")
	}
	time.Sleep(20 * time.Millisecond)
	// at most one more pending (a late event can legitimately arrive after the first read), never five
	n := 0
	for len(c.Changed()) > 0 {
		<-c.Changed()
		n++
	}
	if n > 1 {
		t.Fatalf("not coalesced: %d extra wake-ups", n)
	}
}

func TestAgeCountsPingsAndReconnectTime(t *testing.T) {
	e := newEnv(t)
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return c.Connected() && len(c.Nodes()) == 1 }, "connected")
	t0 := e.clock.Now()
	if c.LastReconnect() != t0 {
		t.Fatalf("LastReconnect=%v want %v", c.LastReconnect(), t0)
	}
	eventually(t, func() bool { return c.Age() == 0 }, "age zero right after data")
	e.clock.Advance(12 * time.Second)
	if got := c.Age(); got != 12*time.Second {
		t.Fatalf("age=%v want 12s", got)
	}
	e.f.Ping()
	eventually(t, func() bool { return c.Age() == 0 }, "a ping resets the age")
	if c.LastReconnect() != t0 {
		t.Fatal("ping must not count as reconnect")
	}
}

func TestReconnectAfterDropResetsLastReconnect(t *testing.T) {
	e := newEnv(t)
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return c.Connected() }, "connected")
	e.clock.Advance(30 * time.Second)
	t1 := e.clock.Now()
	e.f.DropStreams()
	eventually(t, func() bool { return e.f.StreamConnections() >= 2 && c.Connected() }, "reconnected")
	if c.LastReconnect() != t1 {
		t.Fatalf("LastReconnect=%v want %v", c.LastReconnect(), t1)
	}
	if n := len(c.Nodes()); n != 1 {
		t.Fatalf("nodes after reconnect: %d", n)
	}
}

func TestBackoffGrowsAndResets(t *testing.T) {
	e := newEnv(t)
	e.opts.Backoff = []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	e.opts.PollInterval = time.Hour // isolate the stream attempts
	e.f.Fail(fakefleet.EpStream, 503)
	e.start()
	eventually(t, func() bool { return e.f.Requests(fakefleet.EpStream) >= 4 }, "four attempts")
	// with 5+10+20+20 ms the 4th attempt cannot be earlier than ~35ms; the first three not instant:
	if got := e.f.Requests(fakefleet.EpStream); got > 12 {
		t.Fatalf("no backoff: %d attempts", got)
	}
}

func TestPollsWhileDisconnectedAndStopsWhenBack(t *testing.T) {
	e := newEnv(t)
	e.f.Fail(fakefleet.EpStream, 503)
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return len(c.Nodes()) == 1 && e.f.Requests(fakefleet.EpFleet) >= 3 }, "nodes from polling, polled repeatedly")
	if c.Connected() {
		t.Fatal("must not report connected while the stream fails")
	}
	// a polled response is proof of life
	if c.Age() != 0 {
		t.Fatalf("age after poll = %v", c.Age())
	}
	e.f.Modify("a8", func(m *fakefleet.Member) { m.VRAMUsed = fakefleet.Ptr(10.0) })
	eventually(t, func() bool { n, _ := e.nodeByID("a8"); return n.FreeMiB == 2048 }, "poll sees change")

	e.f.Heal(fakefleet.EpStream)
	eventually(t, func() bool { return c.Connected() }, "stream back")
	time.Sleep(20 * time.Millisecond) // let an in-flight poll finish
	n1 := e.f.Requests(fakefleet.EpFleet)
	time.Sleep(40 * time.Millisecond)
	if n2 := e.f.Requests(fakefleet.EpFleet); n2 != n1 {
		t.Fatalf("still polling while connected: %d -> %d", n1, n2)
	}
}

func TestPollFailureKeepsLastViewAndAgeGrows(t *testing.T) {
	e := newEnv(t)
	e.f.Fail(fakefleet.EpStream, 503)
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return len(c.Nodes()) == 1 }, "first poll")
	e.f.Fail(fakefleet.EpFleet, 500)
	before := e.f.Requests(fakefleet.EpFleet)
	eventually(t, func() bool { return e.f.Requests(fakefleet.EpFleet) >= before+3 }, "failing polls")
	e.clock.Advance(9 * time.Second)
	if c.Age() != 9*time.Second {
		t.Fatalf("age %v: failed polls must not refresh", c.Age())
	}
	if len(c.Nodes()) != 1 {
		t.Fatal("last view must be kept")
	}
}

func TestIdleTimeoutReconnects(t *testing.T) {
	e := newEnv(t)
	e.opts.IdleTimeout = 60 * time.Millisecond
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return c.Connected() }, "connected")
	eventually(t, func() bool { return e.f.StreamConnections() >= 2 }, "silent stream dropped and reconnected")
}

func TestPingsKeepStreamAlive(t *testing.T) {
	e := newEnv(t)
	e.opts.IdleTimeout = 80 * time.Millisecond
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return c.Connected() }, "connected")
	for i := 0; i < 12; i++ {
		e.f.Ping()
		time.Sleep(15 * time.Millisecond)
	}
	if n := e.f.StreamConnections(); n != 1 {
		t.Fatalf("stream reconnected despite pings: %d connections", n)
	}
}

func TestHungStreamHeaderTimesOut(t *testing.T) {
	e := newEnv(t)
	e.opts.IdleTimeout = 50 * time.Millisecond
	e.f.Hang(fakefleet.EpStream)
	e.f.SetMembers(fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return e.f.Requests(fakefleet.EpStream) >= 2 }, "retried after hang")
	eventually(t, func() bool { return len(c.Nodes()) == 1 }, "polling fills the view meanwhile")
}

// ---- no-tier logging, FetchNode ----

func TestUnmatchedTierLoggedOncePerID(t *testing.T) {
	e := newEnv(t)
	e.f.SetMembers(fakefleet.Relay("x", "mystery", "10.0.0.9", 8, 2), fakefleet.Relay("y", "enigma", "10.0.0.8", 8, 2))
	c := e.start()
	eventually(t, func() bool { return e.log.count("no tier") >= 2 }, "both logged")
	for i := 0; i < 5; i++ {
		e.f.Modify("x", func(m *fakefleet.Member) { m.VRAMUsed = fakefleet.Ptr(float64(i)) })
		e.f.Push()
	}
	time.Sleep(30 * time.Millisecond)
	if n := e.log.count("no tier"); n != 2 {
		t.Fatalf("logged %d times: %s", n, e.log.String())
	}
	if len(c.Nodes()) != 0 {
		t.Fatalf("unmatched members must be excluded: %+v", c.Nodes())
	}
}

func TestFetchNode(t *testing.T) {
	e := newEnv(t)
	e.f.SetMembers(
		fakefleet.PS5("a8:5e", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2),
		fakefleet.Relay("alienware-llm", "alienware-llm", "10.0.0.12", 8, 2),
		fakefleet.Relay("x", "mystery", "10.0.0.9", 8, 2),
	)
	e.f.SetOnline("alienware-llm", false)
	c := e.start()
	ctx := context.Background()
	n, err := c.FetchNode(ctx, "a8:5e")
	if err != nil || n.FreeMiB != 7680 || n.Addr != "10.0.0.2:50052" || !n.Online {
		t.Fatalf("fetch: %+v %v", n, err)
	}
	n, err = c.FetchNode(ctx, "alienware-llm")
	if err != nil || n.Online {
		t.Fatalf("offline member: %+v %v", n, err)
	}
	if _, err = c.FetchNode(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if _, err = c.FetchNode(ctx, "x"); err == nil {
		t.Fatal("member without tier must be an error")
	}
	e.f.Fail(fakefleet.EpNode, 500)
	if _, err = c.FetchNode(ctx, "a8:5e"); err == nil {
		t.Fatal("500 must be an error")
	}
	e.f.Heal(fakefleet.EpNode)
	e.f.Hang(fakefleet.EpNode)
	start := time.Now()
	if _, err = c.FetchNode(ctx, "a8:5e"); err == nil {
		t.Fatal("hang must time out with an error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("hang took too long")
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err = c.FetchNode(cctx, "a8:5e"); err == nil {
		t.Fatal("ctx deadline must be honoured")
	}
}

// ---- resolver ----

func TestResolve(t *testing.T) {
	e := newEnv(t)
	e.f.SetMembers(
		fakefleet.PS5("a8:5e", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2),
		fakefleet.Relay("alienware-llm", "alienware-llm", "10.0.0.12", 8, 2),
		fakefleet.PS5("aa:01", "ps5-twin", "10.0.0.5", 12, 1, 14, 1),
		fakefleet.PS5("aa:02", "ps5-twin", "10.0.0.6", 12, 1, 14, 1),
		fakefleet.PS5("bb:01", "ps5-halfdead", "10.0.0.7", 12, 1, 14, 1),
		fakefleet.PS5("bb:02", "ps5-halfdead", "10.0.0.8", 12, 1, 14, 1),
	)
	e.f.SetOnline("bb:02", false)
	c := e.start()
	eventually(t, func() bool { return len(c.Nodes()) == 6 }, "members")

	for name, want := range map[string]string{"a8:5e": "a8:5e", "ps5-1": "a8:5e", "alienware-llm": "alienware-llm", "ps5-halfdead": "bb:01"} {
		got, err := c.Resolve(name)
		if err != nil || string(got) != want {
			t.Errorf("Resolve(%q)=%q,%v want %q", name, got, err, want)
		}
	}
	if _, err := c.Resolve("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	for i := 0; i < 4; i++ {
		_, err := c.Resolve("ps5-twin")
		var amb *AmbiguousError
		if !errors.As(err, &amb) || len(amb.IDs) != 2 || !strings.Contains(err.Error(), "aa:01") || !strings.Contains(err.Error(), "aa:02") {
			t.Fatalf("twin: %v", err)
		}
	}
	if n := e.log.count("ambiguous"); n != 1 {
		t.Fatalf("ambiguity logged %d times, want once per change: %s", n, e.log.String())
	}
	// once one twin goes offline the name resolves, and a later clash logs again
	e.f.SetOnline("aa:02", false)
	e.f.Push()
	eventually(t, func() bool { id, err := c.Resolve("ps5-twin"); return err == nil && id == "aa:01" }, "resolves after one goes offline")
	e.f.SetOnline("aa:02", true)
	e.f.Push()
	eventually(t, func() bool { _, err := c.Resolve("ps5-twin"); return err != nil }, "ambiguous again")
	if n := e.log.count("ambiguous"); n != 2 {
		t.Fatalf("expected a second log after the change, got %d", n)
	}
}

// ---- local node ----

func TestLocalProbePublishesLocalNodeAndHidesFleetEntry(t *testing.T) {
	e := newEnv(t)
	e.probe.set("8151, 2100\n8192, 7\n", nil)
	e.f.SetMembers(omenMember(), fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	e.f.SetOnline("3c:52:82:11", false) // the probe must not care
	c := e.start()
	eventually(t, func() bool { return len(c.Nodes()) == 2 }, "local + one remote")
	n, ok := e.nodeByID("3c:52:82:11")
	if !ok {
		t.Fatalf("local node missing: %+v", c.Nodes())
	}
	if !n.Local || !n.Online || n.Addr != "" || n.TotalMiB != 8151 || n.FreeMiB != 6051 || n.Priority != 3 || n.HeadroomMiB != 256 || n.Hostname != "omen-runner" {
		t.Fatalf("local node: %+v", n)
	}
	locals := 0
	for _, x := range c.Nodes() {
		if x.Local {
			locals++
		}
	}
	if locals != 1 {
		t.Fatalf("local count %d", locals)
	}
	// figures follow the probe
	e.probe.set("8151, 8000\n", nil)
	eventually(t, func() bool { n, _ := e.nodeByID("3c:52:82:11"); return n.FreeMiB == 151 }, "probe update")
	e.probe.set("8151, 9000\n", nil)
	eventually(t, func() bool { n, _ := e.nodeByID("3c:52:82:11"); return n.FreeMiB == 0 }, "never negative")
	if e.log.count("level=WARN") != 0 {
		t.Fatalf("unexpected warnings: %s", e.log.String())
	}
}

func TestLocalProbeWorksWithoutFleet(t *testing.T) {
	e := newEnv(t)
	e.probe.set("8151, 100\n", nil)
	e.f.Fail(fakefleet.EpStream, 503)
	e.f.Fail(fakefleet.EpFleet, 503)
	c := e.start()
	eventually(t, func() bool { n := c.Nodes(); return len(n) == 1 && n[0].Local && n[0].FreeMiB == 8051 }, "local node without any fleet")
	if n := c.Nodes()[0]; n.ID != "omen-runner" || n.Hostname != "omen-runner" {
		t.Fatalf("fallback identity: %+v", n)
	}
}

func TestLocalProbeFailureFallsBackToFleetAndWarnsOnce(t *testing.T) {
	e := newEnv(t) // probe fails by default
	e.f.SetMembers(omenMember(), fakefleet.PS5("a8", "ps5-1", "10.0.0.2", 12, 4.5, 14.1, 3.2))
	c := e.start()
	eventually(t, func() bool { return e.probe.count() >= 5 }, "several probe attempts")
	eventually(t, func() bool { return len(c.Nodes()) == 2 }, "fallback node")
	n, ok := e.nodeByID("3c:52:82:11")
	if !ok || !n.Local || n.Addr != "" || n.TotalMiB != 8192 || n.FreeMiB != 6144 || n.Priority != 3 {
		t.Fatalf("fallback local node: %+v", n)
	}
	if w := e.log.count("level=WARN"); w != 1 {
		t.Fatalf("warnings=%d: %s", w, e.log.String())
	}
	// recovery switches to the probe; a later failure warns again
	e.probe.set("8151, 1000\n", nil)
	eventually(t, func() bool { n, _ := e.nodeByID("3c:52:82:11"); return n.TotalMiB == 8151 }, "probe recovered")
	e.probe.set("", errors.New("gone again"))
	eventually(t, func() bool { return e.log.count("level=WARN") == 2 }, "second warning after recovery")
}

func TestLocalProbeGarbageCountsAsFailure(t *testing.T) {
	e := newEnv(t)
	e.probe.set("No devices were found\n", nil)
	e.f.SetMembers(omenMember())
	c := e.start()
	eventually(t, func() bool { n, ok := e.nodeByID("3c:52:82:11"); return ok && n.TotalMiB == 8192 && n.Local }, "fleet fallback after garbage")
	if e.log.count("level=WARN") != 1 {
		t.Fatalf("warnings: %s", e.log.String())
	}
	_ = c
}

func TestFetchNodeForLocalUsesProbe(t *testing.T) {
	e := newEnv(t)
	e.probe.set("8151, 100\n", nil)
	e.f.SetMembers(omenMember())
	c := e.start()
	eventually(t, func() bool { _, ok := e.nodeByID("3c:52:82:11"); return ok }, "local node")
	e.probe.set("8151, 4000\n", nil)
	n, err := c.FetchNode(context.Background(), "3c:52:82:11")
	if err != nil || !n.Local || n.FreeMiB != 4151 {
		t.Fatalf("%+v %v", n, err)
	}
	if e.f.Requests(fakefleet.EpNode) != 0 {
		t.Fatal("local fetch must not hit the console while the probe works")
	}
}

func TestResolveLocalNodeName(t *testing.T) {
	e := newEnv(t)
	e.probe.set("8151, 100\n", nil)
	e.f.SetMembers(omenMember())
	c := e.start()
	eventually(t, func() bool { _, ok := e.nodeByID("3c:52:82:11"); return ok }, "local node")
	if id, err := c.Resolve("omen-runner"); err != nil || id != "3c:52:82:11" {
		t.Fatalf("%q %v", id, err)
	}
}

// ---- shutdown ----

func TestRunReturnsOnCancelPromptlyEvenWhenHung(t *testing.T) {
	e := newEnv(t)
	e.f.Hang(fakefleet.EpStream)
	e.f.Hang(fakefleet.EpFleet)
	e.opts.RequestTimeout = time.Hour
	e.opts.IdleTimeout = time.Hour
	e.start()
	eventually(t, func() bool { return e.f.Requests(fakefleet.EpStream) >= 1 }, "request in flight")
	e.stop()
	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
