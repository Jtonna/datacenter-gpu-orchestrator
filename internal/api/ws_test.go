package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

type wsMsg struct {
	Type string          `json:"type"`
	TS   int64           `json:"ts"`
	Data json.RawMessage `json:"data"`
}

func wsURL(e *env) string { return "ws" + strings.TrimPrefix(e.front.URL, "http") + "/ws" }

// wsc is a client that pumps messages into a channel: coder/websocket closes
// the connection when the context of a Read ends, so tests never read with a deadline.
type wsc struct {
	conn *websocket.Conn
	ch   chan wsMsg
}

func dialWS(t *testing.T, e *env, hdr http.Header) *wsc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(e), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	w := &wsc{conn: conn, ch: make(chan wsMsg, 4096)}
	go func() {
		defer close(w.ch)
		for {
			_, b, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var m wsMsg
			if json.Unmarshal(b, &m) == nil {
				select {
				case w.ch <- m:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	return w
}

// next returns the next message or fails after within.
func (w *wsc) next(t *testing.T, within time.Duration) wsMsg {
	t.Helper()
	select {
	case m, ok := <-w.ch:
		if !ok {
			t.Fatal("the connection closed")
		}
		return m
	case <-time.After(within):
		t.Fatalf("no message within %v", within)
	}
	return wsMsg{}
}

// until reads until a message of the type arrives.
func (w *wsc) until(t *testing.T, typ string, within time.Duration) wsMsg {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			t.Fatalf("no %s message within %v", typ, within)
		}
		if m := w.next(t, left); m.Type == typ {
			return m
		}
	}
}

// collect gathers every message that arrives within d.
func (w *wsc) collect(d time.Duration) []wsMsg {
	var out []wsMsg
	end := time.After(d)
	for {
		select {
		case m, ok := <-w.ch:
			if !ok {
				return out
			}
			out = append(out, m)
		case <-end:
			return out
		}
	}
}

func (w *wsc) send(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func countType(ms []wsMsg, typ string) int {
	n := 0
	for _, m := range ms {
		if m.Type == typ {
			n++
		}
	}
	return n
}

func TestWSHelloThenSnapshotOnConnect(t *testing.T) {
	st := openStore(t)
	e := newEnv(t, chatOK, func(o *Options) { o.History = st; o.Version = "v-test" })
	c := dialWS(t, e, nil)
	hello := c.next(t, 3*time.Second)
	if hello.Type != "hello" || hello.TS == 0 {
		t.Fatalf("%+v", hello)
	}
	var hd map[string]any
	json.Unmarshal(hello.Data, &hd)
	t.Logf("hello: %s", hello.Data)
	if hd["version"] != "v-test" || hd["history"] != "on" || hd["bench"] != "off" || hd["started_at"] == nil || hd["server_time"] == nil {
		t.Fatalf("%v", hd)
	}
	status := c.next(t, 3*time.Second)
	if status.Type != "status" {
		t.Fatalf("second message %+v", status)
	}
	var sd map[string]any
	json.Unmarshal(status.Data, &sd)
	t.Logf("status keys: %v", keys(sd))
	api := getJSONBody(t, e, "/api/status") // the same object GET /api/status returns
	for k := range api {
		if _, ok := sd[k]; !ok {
			t.Errorf("status message lacks %q that /api/status has", k)
		}
	}
	if _, ok := sd["instances"]; !ok {
		t.Fatal("no instances")
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWSEventTopicCarriesHistoryRecords(t *testing.T) {
	st := openStore(t)
	e := newEnv(t, chatOK, func(o *Options) { o.History = st })
	c := dialWS(t, e, nil)
	c.until(t, "hello", 3*time.Second)
	eventually(t, "client registered", func() bool { return e.api.ws.clientCount() == 1 })
	st.Record(history.Event{Ts: 1234, Type: "loadFinished", Severity: "info", Profile: "gemma", Nodes: []string{"ps5-a"}, SessionID: "ld-1",
		Summary: "gemma finished loading in 61.0 s.", Data: map[string]any{"outcome": "ready", "load_s": 61.0}})
	m := c.until(t, "event", 3*time.Second)
	var ev history.Event
	if err := json.Unmarshal(m.Data, &ev); err != nil {
		t.Fatal(err)
	}
	t.Logf("event: %s", m.Data)
	if ev.Type != "loadFinished" || ev.Seq == 0 || ev.SessionID != "ld-1" || ev.Data["outcome"] != "ready" || ev.Nodes[0] != "ps5-a" {
		t.Fatalf("%+v", ev)
	}
	hist := getJSONBody(t, e, "/api/history?type=loadFinished") // it is the record /api/history returns
	if got := hist["events"].([]any)[0].(map[string]any)["seq"].(float64); got != float64(ev.Seq) {
		t.Fatalf("seq %v vs %d", got, ev.Seq)
	}
}

func TestWSStatusIsCoalescedAndOnlySentOnChange(t *testing.T) {
	var n atomic.Int64
	var constant atomic.Bool
	e := newEnv(t, chatOK, func(o *Options) {
		o.WSStatusInterval = 100 * time.Millisecond
		o.WSIdleInterval = time.Hour
		o.statusSource = func(ctx context.Context) (any, error) {
			if constant.Load() {
				return map[string]any{"time": time.Now().String(), "view": map[string]any{"age": time.Now().UnixNano()}, "instances": []any{}, "n": 1}, nil
			}
			return map[string]any{"n": n.Add(1), "instances": []any{}}, nil // different on every look
		}
	})
	c := dialWS(t, e, nil)
	c.until(t, "status", 3*time.Second) // the snapshot on connect
	// 1. a status that changes at every look: at most one message per tick (10 a second here)
	count := countType(c.collect(1100*time.Millisecond), "status")
	if count < 4 || count > 12 {
		t.Fatalf("%d status messages in 1.1 s with a 100 ms tick, want between 4 and 12 (coalesced, not one per change)", count)
	}
	// 2. an unchanged status (only its volatile fields move) sends nothing
	constant.Store(true)
	c.until(t, "status", 2*time.Second) // the first look at the new content
	c.collect(150 * time.Millisecond)   // anything the old content still had queued
	if n := countType(c.collect(700*time.Millisecond), "status"); n != 0 {
		t.Fatalf("%d status messages for an unchanged status", n)
	}
}

func TestWSStatusKeepsMovingWhileALoadRuns(t *testing.T) {
	var loading atomic.Bool
	e := newEnv(t, chatOK, func(o *Options) {
		o.WSStatusInterval = 20 * time.Millisecond
		o.WSLoadInterval = 150 * time.Millisecond
		o.WSIdleInterval = time.Hour
		o.statusSource = func(ctx context.Context) (any, error) {
			if loading.Load() {
				return map[string]any{"instances": []any{map[string]any{"profile": "m", "state": "starting", "load": map[string]any{"progress": 0.5}}}}, nil
			}
			return map[string]any{"instances": []any{}}, nil
		}
	})
	c := dialWS(t, e, nil)
	c.until(t, "status", 3*time.Second)
	loading.Store(true)
	c.until(t, "status", 2*time.Second) // the change
	for i := 0; i < 3; i++ {            // the content never changes again, yet a status arrives about every 150 ms
		c.until(t, "status", 600*time.Millisecond)
	}
}

func TestWSSubscribeFiltersTopics(t *testing.T) {
	st := openStore(t)
	var n atomic.Int64
	e := newEnv(t, chatOK, func(o *Options) {
		o.History = st
		o.WSStatusInterval = 20 * time.Millisecond
		o.statusSource = func(ctx context.Context) (any, error) {
			return map[string]any{"n": n.Add(1), "instances": []any{}}, nil
		}
	})
	c := dialWS(t, e, nil)
	c.until(t, "status", 3*time.Second)
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{"event"}})
	time.Sleep(100 * time.Millisecond) // statuses already in flight
	c.collect(100 * time.Millisecond)
	st.Record(history.Event{Ts: 1, Type: "x", Severity: "info", Summary: "X."})
	got := c.collect(600 * time.Millisecond)
	if countType(got, "status") != 0 {
		t.Fatalf("status messages after subscribing to event only: %d", countType(got, "status"))
	}
	if countType(got, "event") != 1 {
		t.Fatalf("the subscribed topic did not arrive: %+v", got)
	}
	// subscribing to status again sends a fresh snapshot at once; garbage and pong are ignored
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{"status", "event", "nonsense"}})
	c.until(t, "status", 2*time.Second)
	c.send(t, map[string]any{"type": "pong"})
	c.send(t, "not an object")
	c.until(t, "status", 2*time.Second)
}

func TestWSPing(t *testing.T) {
	e := newEnv(t, chatOK, func(o *Options) { o.WSPingInterval = 40 * time.Millisecond })
	c := dialWS(t, e, nil)
	if m := c.until(t, "ping", 2*time.Second); m.TS == 0 {
		t.Fatal("ping without ts")
	}
}

func TestWSClientLimit(t *testing.T) {
	e := newEnv(t, chatOK, func(o *Options) { o.WSMaxClients = 2 })
	dialWS(t, e, nil)
	dialWS(t, e, nil)
	eventually(t, "two clients", func() bool { return e.api.ws.clientCount() == 2 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(e), nil)
	if err == nil || resp == nil || resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third client: %v %+v", err, resp)
	}
}

func TestWSOriginCheck(t *testing.T) {
	cases := map[string]bool{
		"": true, "http://127.0.0.1:3122": true, "http://localhost:3122": true, "http://192.168.1.20:3122": true,
		"http://10.0.0.5": true, "http://172.20.1.1": true, "http://omen:3122": true, "http://omen.local:3122": true,
		"http://[::1]:3122": true, "https://evil.example.com": false, "http://8.8.8.8": false, "null": false, "http://203.0.113.9:80": false,
	}
	for origin, want := range cases {
		r, _ := http.NewRequest("GET", "http://scheduler.example:3122/ws", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if got := wsOriginAllowed(r); got != want {
			t.Errorf("origin %q: %v, want %v", origin, got, want)
		}
	}
	// the same origin always passes
	r, _ := http.NewRequest("GET", "http://scheduler.example:3122/ws", nil)
	r.Header.Set("Origin", "http://scheduler.example:3122")
	if !wsOriginAllowed(r) {
		t.Error("same origin refused")
	}
	e := newEnv(t, chatOK)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(e), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example.com"}}})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("foreign origin: %v %+v", err, resp)
	}
}

func TestSSEEventsStillWorkNextToWS(t *testing.T) {
	e := newEnv(t, chatOK)
	dialWS(t, e, nil)
	resp := e.get("/api/events")
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// ---- hub, with fake connections ----

type fakeConn struct {
	mu      sync.Mutex
	writes  [][]byte
	block   chan struct{} // writes wait for this to close when set
	closed  chan struct{}
	code    websocket.StatusCode
	reasons string
	once    sync.Once
}

func newFakeConn(block bool) *fakeConn {
	f := &fakeConn{closed: make(chan struct{})}
	if block {
		f.block = make(chan struct{})
	}
	return f
}

func (f *fakeConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-f.closed:
		return 0, nil, context.Canceled
	}
}
func (f *fakeConn) Write(ctx context.Context, typ websocket.MessageType, p []byte) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		case <-f.closed:
			return context.Canceled
		}
	}
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	f.mu.Unlock()
	return nil
}
func (f *fakeConn) Close(code websocket.StatusCode, reason string) error {
	f.once.Do(func() { f.code, f.reasons = code, reason; close(f.closed) })
	return nil
}
func (f *fakeConn) CloseNow() error    { return f.Close(websocket.StatusInternalError, "now") }
func (f *fakeConn) SetReadLimit(int64) {}
func (f *fakeConn) count() int         { f.mu.Lock(); defer f.mu.Unlock(); return len(f.writes) }

func testHub(queue int) *wsHub {
	h := &wsHub{max: 10, queue: queue, writeTimeout: time.Second, pingEvery: time.Hour, statusEvery: time.Hour, loadEvery: time.Hour, idleEvery: time.Hour,
		now: time.Now, clients: map[*wsClient]struct{}{}, quit: make(chan struct{}), log: func(string, ...any) {},
		hello: func() any { return map[string]any{"v": 1} }}
	return h
}

func TestHubDropsASlowClientWithoutBlockingOthers(t *testing.T) {
	h := testHub(8)
	slow, fast := newFakeConn(true), newFakeConn(false)
	for _, c := range []*fakeConn{slow, fast} {
		h.reserve()
		go h.serve(context.Background(), c)
	}
	eventually(t, "both registered", func() bool { return h.clientCount() == 2 })
	start := time.Now()
	for i := 0; i < 50; i++ {
		h.publishEvent(history.Event{Ts: int64(i), Type: "x", Summary: "X."})
		time.Sleep(time.Millisecond) // the fast client drains between events
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("broadcasting took %v: a slow client blocked the producer", d)
	}
	eventually(t, "slow client dropped", func() bool { return h.clientCount() == 1 })
	select {
	case <-slow.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the slow client's connection was not closed")
	}
	if slow.code != websocket.StatusPolicyViolation {
		t.Fatalf("closed with %d, want %d (policy violation)", slow.code, websocket.StatusPolicyViolation)
	}
	eventually(t, "fast client received all", func() bool { return fast.count() >= 51 }) // hello + 50 events
	close(slow.block)
	h.close()
}

func TestHubCloseDropsEveryoneWithGoingAway(t *testing.T) {
	h := testHub(8)
	c := newFakeConn(false)
	h.reserve()
	go h.serve(context.Background(), c)
	eventually(t, "registered", func() bool { return h.clientCount() == 1 })
	h.close()
	select {
	case <-c.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("not closed")
	}
	if c.code != websocket.StatusGoingAway {
		t.Fatalf("code %d", c.code)
	}
	h.close() // safe twice
	if h.reserve() {
		t.Fatal("a closed hub reserved a slot")
	}
}

func TestStatusSignatureIgnoresVolatileFields(t *testing.T) {
	a := map[string]any{"time": "t1", "view": map[string]any{"age": 1.0, "stale": false}, "queue": []any{map[string]any{"profile": "p", "age": 1.0, "clock": 2.0, "waiters": 1.0}}, "x": 1.0}
	b := map[string]any{"time": "t2", "view": map[string]any{"age": 9.0, "stale": false}, "queue": []any{map[string]any{"profile": "p", "age": 5.0, "clock": 6.0, "waiters": 1.0}}, "x": 1.0}
	if statusSignature(a) != statusSignature(b) {
		t.Fatal("volatile fields changed the signature")
	}
	b["queue"] = []any{map[string]any{"profile": "p", "waiters": 2.0}}
	if statusSignature(a) == statusSignature(b) {
		t.Fatal("a real change did not change the signature")
	}
}
