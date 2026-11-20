package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
)

// The live channel (design 9.2): GET /ws, a WebSocket that carries JSON text
// frames {"type", "ts", "data"} from the server: hello, status, event, bench
// and ping. The client may send {"type":"subscribe","topics":[...]} and
// {"type":"pong"}. The SSE feed /api/events is untouched.

// WebSocket topics.
const (
	TopicHello  = "hello"
	TopicStatus = "status"
	TopicEvent  = "event"
	TopicBench  = "bench"
	TopicConfig = "config"
	TopicPing   = "ping"
	// TopicModels carries the model catalog: the whole document of GET
	// /api/models when a client subscribes, then {version, base, changed,
	// removed, order} when entries change. A client asks for it by name; it is
	// not among the topics a new connection gets by default, so a client that
	// does not know it never receives the large first frame.
	TopicModels = "models"
)

// subscribable are the topics a client can switch on and off; hello and ping always arrive.
var subscribable = map[string]bool{TopicStatus: true, TopicEvent: true, TopicBench: true, TopicConfig: true, TopicModels: true}

// wsConn is what the hub needs of a connection (*websocket.Conn satisfies it).
type wsConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
	CloseNow() error
	SetReadLimit(n int64)
}

type wsClient struct {
	conn wsConn
	send chan []byte
	done chan struct{}
	once sync.Once

	mu     sync.Mutex
	topics map[string]bool
}

func (c *wsClient) wants(topic string) bool {
	switch topic {
	case TopicHello, TopicPing:
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.topics[topic]
}

type wsHub struct {
	max          int
	queue        int
	writeTimeout time.Duration
	pingEvery    time.Duration
	statusEvery  time.Duration
	loadEvery    time.Duration
	idleEvery    time.Duration
	now          func() time.Time

	hello    func() any
	status   func(ctx context.Context) (any, error)
	snapshot func() []map[string]any          // extra messages for a new client (bench state)
	models   func(ctx context.Context) []byte // the whole catalog as a frame's data, or nil
	onStatus func(v any)                      // every status the loop looked at
	log      func(msg string, args ...any)

	mu       sync.Mutex
	clients  map[*wsClient]struct{}
	reserved int
	closed   bool
	quit     chan struct{}
	wg       sync.WaitGroup
	cancels  []func()

	// the last status sent, to send only changes
	lastSig  string
	lastSent time.Time
}

func wsFrame(typ string, ts time.Time, data any) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "ts": ts.UnixMilli(), "data": data})
	return b
}

// reserve claims a client slot; false when the hub is full or closed.
func (h *wsHub) reserve() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.clients)+h.reserved >= h.max {
		return false
	}
	h.reserved++
	return true
}

func (h *wsHub) release() {
	h.mu.Lock()
	h.reserved--
	h.mu.Unlock()
}

func (h *wsHub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// drop removes a client and closes its connection with a code and reason. It
// never blocks the caller.
func (h *wsHub) drop(c *wsClient, code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		close(c.done)
		go func() {
			done := make(chan struct{})
			go func() { c.conn.Close(code, reason); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				c.conn.CloseNow()
			}
		}()
	})
}

// enqueue puts a frame on a client's bounded queue; a client that cannot keep
// up is dropped with a policy-violation close, never waited for.
func (h *wsHub) enqueue(c *wsClient, b []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.send <- b:
	default:
		h.log("websocket client dropped: it cannot keep up")
		h.drop(c, websocket.StatusPolicyViolation, "too slow: the send queue is full")
	}
}

func (h *wsHub) broadcast(topic string, b []byte) {
	h.mu.Lock()
	cs := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c.wants(topic) {
			cs = append(cs, c)
		}
	}
	h.mu.Unlock()
	for _, c := range cs {
		h.enqueue(c, b)
	}
}

// publishEvent sends a history event as recorded (the shape of /api/history).
func (h *wsHub) publishEvent(e history.Event) {
	if h.clientCount() == 0 {
		return
	}
	h.broadcast(TopicEvent, wsFrame(TopicEvent, h.now(), e))
}

// publishBench sends a benchmark event: {"kind": ...}.
func (h *wsHub) publishBench(ev map[string]any) {
	if h.clientCount() == 0 {
		return
	}
	h.broadcast(TopicBench, wsFrame(TopicBench, h.now(), ev))
}

// publishConfig sends a configuration change announcement.
func (h *wsHub) publishConfig(ev map[string]any) {
	if h.clientCount() == 0 {
		return
	}
	h.broadcast(TopicConfig, wsFrame(TopicConfig, h.now(), ev))
}

// serve runs one accepted connection until it ends.
func (h *wsHub) serve(ctx context.Context, conn wsConn) {
	conn.SetReadLimit(4096)
	c := &wsClient{conn: conn, send: make(chan []byte, h.queue), done: make(chan struct{}),
		topics: map[string]bool{TopicStatus: true, TopicEvent: true, TopicBench: true, TopicConfig: true}}
	h.mu.Lock()
	h.reserved--
	if h.closed {
		h.mu.Unlock()
		conn.Close(websocket.StatusGoingAway, "the scheduler is shutting down")
		return
	}
	h.clients[c] = struct{}{}
	h.wg.Add(1)
	h.mu.Unlock()

	// hello, then a full snapshot of what the client subscribed to
	c.send <- wsFrame(TopicHello, h.now(), h.hello())
	h.sendSnapshot(c, true, true)

	go func() { // writer
		defer h.wg.Done()
		for {
			select {
			case b := <-c.send:
				wctx, cancel := context.WithTimeout(ctx, h.writeTimeout)
				err := conn.Write(wctx, websocket.MessageText, b)
				cancel()
				if err != nil {
					h.drop(c, websocket.StatusPolicyViolation, "write failed or timed out")
					return
				}
			case <-c.done:
				return
			case <-h.quit:
				return
			}
		}
	}()
	// reader: subscribe, pong; anything else is ignored
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			h.drop(c, websocket.StatusNormalClosure, "")
			return
		}
		var m struct {
			Type   string   `json:"type"`
			Topics []string `json:"topics"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "subscribe":
			next := map[string]bool{}
			for _, t := range m.Topics {
				if subscribable[t] {
					next[t] = true
				}
			}
			c.mu.Lock()
			prev := c.topics
			c.topics = next
			c.mu.Unlock()
			h.sendSnapshot(c, next[TopicStatus] && !prev[TopicStatus], next[TopicBench] && !prev[TopicBench])
			if next[TopicModels] && !prev[TopicModels] {
				h.sendModels(c)
			}
		case "pong":
		}
	}
}

// sendSnapshot queues the current status and the benchmark state for a client.
func (h *wsHub) sendSnapshot(c *wsClient, withStatus, withBench bool) {
	if withStatus && c.wants(TopicStatus) && h.status != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		v, err := h.status(ctx)
		cancel()
		if err == nil {
			h.enqueue(c, wsFrame(TopicStatus, h.now(), v))
		}
	}
	if withBench && c.wants(TopicBench) && h.snapshot != nil {
		for _, ev := range h.snapshot() {
			h.enqueue(c, wsFrame(TopicBench, h.now(), ev))
		}
	}
}

// rawFrame is wsFrame for data that is already JSON.
func rawFrame(typ string, ts time.Time, data []byte) []byte {
	b := make([]byte, 0, len(data)+64)
	b = append(b, `{"type":"`+typ+`","ts":`...)
	b = strconv.AppendInt(b, ts.UnixMilli(), 10)
	b = append(b, `,"data":`...)
	b = append(b, bytes.TrimSpace(data)...)
	return append(b, '}')
}

// sendModels queues the whole catalog for a client that asked for the topic.
// The catalog comes from memory; when it is not built yet it is built off
// this goroutine and sent when ready, so the connection is never held up.
func (h *wsHub) sendModels(c *wsClient) {
	if h.models == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if b := h.models(ctx); b != nil && c.wants(TopicModels) {
			h.enqueue(c, rawFrame(TopicModels, h.now(), b))
		}
	}()
}

// publishModels sends the entries that changed between two catalogs.
func (h *wsHub) publishModels(snap, prev *catalogSnap) {
	if h.clientCount() == 0 {
		return
	}
	if prev == nil {
		return // the first catalog: a client gets it when it subscribes (sendModels)
	}
	h.broadcast(TopicModels, wsFrame(TopicModels, h.now(), snap.delta(prev)))
}

// volatile are the status fields that change on every look; a change in them
// alone is not a change.
func statusSignature(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		b, _ := json.Marshal(v)
		return string(b)
	}
	cp := make(map[string]any, len(m))
	for k, val := range m {
		cp[k] = val
	}
	delete(cp, "time")
	if view, ok := cp["view"].(map[string]any); ok {
		vc := make(map[string]any, len(view))
		for k, val := range view {
			vc[k] = val
		}
		delete(vc, "age")
		cp["view"] = vc
	}
	if q, ok := cp["queue"].([]any); ok {
		qc := make([]any, 0, len(q))
		for _, e := range q {
			if em, ok := e.(map[string]any); ok {
				ec := make(map[string]any, len(em))
				for k, val := range em {
					ec[k] = val
				}
				delete(ec, "age")
				delete(ec, "clock")
				qc = append(qc, ec)
			} else {
				qc = append(qc, e)
			}
		}
		cp["queue"] = qc
	}
	b, _ := json.Marshal(cp)
	return string(b)
}

func anyLoading(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	insts, _ := m["instances"].([]any)
	for _, i := range insts {
		if im, ok := i.(map[string]any); ok && im["state"] == "starting" && im["load"] != nil {
			return true
		}
	}
	return false
}

// statusLoop sends the status when it changed, at most once per tick; while a
// load is in progress at least every loadEvery, and at least every idleEvery.
func (h *wsHub) statusLoop() {
	defer h.wg.Done()
	t := time.NewTicker(h.statusEvery)
	defer t.Stop()
	for {
		select {
		case <-h.quit:
			return
		case <-t.C:
		}
		if h.clientCount() == 0 || h.status == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		v, err := h.status(ctx)
		cancel()
		if err != nil {
			continue
		}
		if h.onStatus != nil {
			h.onStatus(v)
		}
		sig := statusSignature(v)
		now := h.now()
		due := sig != h.lastSig || now.Sub(h.lastSent) >= h.idleEvery || (anyLoading(v) && now.Sub(h.lastSent) >= h.loadEvery)
		if !due {
			continue
		}
		h.lastSig, h.lastSent = sig, now
		h.broadcast(TopicStatus, wsFrame(TopicStatus, now, v))
	}
}

func (h *wsHub) pingLoop() {
	defer h.wg.Done()
	t := time.NewTicker(h.pingEvery)
	defer t.Stop()
	for {
		select {
		case <-h.quit:
			return
		case <-t.C:
			h.broadcast(TopicPing, wsFrame(TopicPing, h.now(), map[string]any{}))
		}
	}
}

func (h *wsHub) start() {
	h.wg.Add(2)
	go h.statusLoop()
	go h.pingLoop()
}

// close drops every client and stops the loops. Safe to call twice.
func (h *wsHub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	cs := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		cs = append(cs, c)
	}
	cancels := h.cancels
	h.cancels = nil
	close(h.quit)
	h.mu.Unlock()
	for _, f := range cancels {
		f()
	}
	for _, c := range cs {
		h.drop(c, websocket.StatusGoingAway, "the scheduler is shutting down")
	}
	h.wg.Wait()
}

// wsOriginAllowed is the origin check: no Origin header (not a browser), the
// same origin as the request, or an origin on a loopback or private address.
// This is a LAN tool with no authentication, so another site's page in a
// browser on the LAN must not be able to open it from the internet.
func wsOriginAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".lan") ||
		strings.HasSuffix(h, ".home.arpa") || strings.HasSuffix(h, ".internal") {
		return true
	}
	return !strings.Contains(h, ".") // a single-label name such as "host-1" is a LAN host
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !wsOriginAllowed(r) {
		proxy.WriteError(w, http.StatusForbidden, "this origin may not open the live channel: only the same origin and loopback or private-network origins are allowed", "invalid_request_error", "forbidden origin", nil)
		return
	}
	if s.shutting.Load() {
		proxy.WriteError(w, http.StatusServiceUnavailable, "the scheduler is shutting down", "server_error", "shutting down", nil)
		return
	}
	if !s.ws.reserve() {
		w.Header().Set("Retry-After", "5")
		proxy.WriteError(w, http.StatusServiceUnavailable, "too many live-channel clients (ws_max_clients)", "server_error", "too many clients", nil)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		s.ws.release()
		return // Accept has answered
	}
	s.ws.serve(r.Context(), conn)
}

func orDur(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func (s *Server) newWSHub() *wsHub {
	o := s.opts
	max := o.WSMaxClients
	if max <= 0 {
		max = 32
	}
	queue := o.WSQueue
	if queue <= 0 {
		queue = 64
	}
	h := &wsHub{max: max, queue: queue, writeTimeout: orDur(o.WSWriteTimeout, 10*time.Second), pingEvery: orDur(o.WSPingInterval, 15*time.Second),
		statusEvery: orDur(o.WSStatusInterval, 250*time.Millisecond), loadEvery: orDur(o.WSLoadInterval, time.Second), idleEvery: orDur(o.WSIdleInterval, 5*time.Second),
		now: time.Now, clients: map[*wsClient]struct{}{}, quit: make(chan struct{}),
		log: func(msg string, args ...any) { s.log.Warn(msg, args...) }}
	h.hello = func() any {
		hist, benchState := "off", "off"
		if s.hist.On() {
			hist = "on"
		}
		if s.benchOn() {
			benchState = "on"
		}
		return map[string]any{"version": o.Version, "started_at": s.start.UnixMilli(), "server_time": s.opts.Now().UnixMilli(),
			"history": hist, "bench": benchState, "topics": []string{TopicStatus, TopicEvent, TopicBench, TopicConfig, TopicPing, TopicModels}, "ws_max_clients": max, "config_revision": s.configRevision()}
	}
	h.status = o.statusSource
	if h.status == nil {
		h.status = func(ctx context.Context) (any, error) {
			return s.statusView(ctx)
		}
	}
	h.onStatus = s.catalogStatus
	h.models = func(ctx context.Context) []byte {
		snap, err := s.catalogGet(ctx)
		if err != nil {
			return nil
		}
		return snap.list
	}
	if s.hist.On() {
		h.cancels = append(h.cancels, s.hist.Subscribe(h.publishEvent))
	}
	if s.benchOn() {
		h.cancels = append(h.cancels, o.Bench.Subscribe(h.publishBench))
		h.snapshot = func() []map[string]any {
			return []map[string]any{{"kind": "state", "state": o.Bench.State()}}
		}
	}
	h.start()
	return h
}
