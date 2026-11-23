// Package logrelay is Potluck's pass-through in front of the log store
// (CR-10, agent contract 11.3): it hands out
// the logs object, checks who sends, keeps the limits, adds the trusted
// fields and streams the body to the store.
//
// It shares no lock with the pool or the scheduler. What it knows about a
// node it reads through Options.Node and Options.Launch, which must not block
// (the pool publishes a snapshot for them). Its own mutex guards its counters
// and limits only and is never held across a network call.
package logrelay

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
)

// The limits of design section 4.3 and 6.
const (
	MaxBodyBytes   = agentproto.LogsMaxBatchBytes // one request body, counted while streaming
	NodeBytesPerS  = agentproto.LogsMaxBytesPerS  // per node
	NodeBurstBytes = 2 << 20
	// MaxInFlight is the most log requests in flight in the whole process;
	// the next one is refused at once.
	MaxInFlight = 8
	// MaxClientInFlight is how many of those may be client requests, so that
	// clients, who are not identified, cannot take every place from the nodes.
	MaxClientInFlight    = 2
	ForwardTimeout       = 5 * time.Second // the whole forward
	ClientBytesPerS      = 64 << 10        // per client address
	ClientBurstBytes     = 1 << 20
	AllClientsBytesPerS  = 1 << 20 // all clients together
	AllClientsBurstBytes = 2 << 20
	// maxClientAddrs bounds the per-address state.
	maxClientAddrs = 1024
)

// Reason words of the answers, also the keys of the refusal counters.
const (
	ReasonLogsOff          = "logs_off"             // 404
	ReasonUnknownSender    = "unknown_sender"       // 410
	ReasonBadRequest       = "bad_request"          // 400
	ReasonBadEncoding      = "unsupported_encoding" // 415
	ReasonTooLarge         = "too_large"            // 413
	ReasonRateLimited      = "rate_limited"         // 429 with Retry-After
	ReasonNodeBusy         = "node_busy"            // 429: the node has a request in flight
	ReasonTooManyRequests  = "too_many_requests"    // 429: the cap of MaxInFlight
	ReasonStoreUnavailable = "store_unavailable"    // 503
	ReasonStoreRejected    = "store_rejected"       // 400: the store answered 4xx
	ReasonBodyRead         = "body_read"            // 400: the sender's body could not be read in time
)

// Node is what the relay needs to know about a registered node.
type Node struct{ StartID, Name, Platform, Release string }

// Options configure a Relay.
type Options struct {
	// StoreURL is the address at which Potluck reaches the log store, for
	// example http://potluck-logs:9428. Empty: there is no store.
	StoreURL string
	// Config returns the logging settings in force. It must not block.
	Config func() config.Logs
	// Node looks a node up by id; false for a node that would get 410 on a
	// heartbeat. Launch returns the profile of a launch of that node. Both
	// must not block. Nil: no node is known.
	Node   func(id string) (Node, bool)
	Launch func(launchID, nodeID string) (profile string, ok bool)
	Logger *slog.Logger
	Now    func() time.Time
	// Transport replaces the relay's own transport to the store (tests).
	Transport http.RoundTripper
	// ForwardTimeout replaces ForwardTimeout (tests).
	ForwardTimeout time.Duration
	// ReadTransport replaces the read side's own transport to the store
	// (tests). QueryTimeout replaces QueryTimeout (tests).
	ReadTransport http.RoundTripper
	QueryTimeout  time.Duration
}

// Counters are the figures of one sender.
type Counters struct {
	AcceptedBytes    int64            `json:"accepted_bytes"`
	AcceptedRequests int64            `json:"accepted_requests"`
	Refused          map[string]int64 `json:"refused"` // by reason word
}

func (c *Counters) refuse(reason string) {
	if c.Refused == nil {
		c.Refused = map[string]int64{}
	}
	c.Refused[reason]++
}

func (c Counters) clone() Counters {
	out := Counters{AcceptedBytes: c.AcceptedBytes, AcceptedRequests: c.AcceptedRequests, Refused: map[string]int64{}}
	for k, v := range c.Refused {
		out.Refused[k] = v
	}
	return out
}

type nodeState struct {
	bucket bucket
	busy   bool
	c      Counters
}

// Relay is the pass-through. Create it with New.
type Relay struct {
	o      Options
	log    *slog.Logger
	insert string // <store>/insert/jsonline, "" without a store
	base   string // the store address without a trailing slash, "" without a store
	client *http.Client
	read   *reader    // the read side: /logs/ and /api/logs/query (reader.go)
	flags  storeFlags // what the store says its retention and disk cap are (storeflags.go)

	mu             sync.Mutex
	inFlight       int
	clientInFlight int
	peak           int
	nodes          map[string]*nodeState
	clients        map[string]*bucket // by client address
	allClients     bucket
	clientC        Counters
	unknown        int64 // requests answered 410
	lastWarn       time.Time
}

// New builds a relay. A StoreURL that is not an http or https address is an
// error.
func New(o Options) (*Relay, error) {
	if o.Config == nil {
		return nil, errors.New("logrelay: Config is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ForwardTimeout <= 0 {
		o.ForwardTimeout = ForwardTimeout
	}
	if o.Node == nil {
		o.Node = func(string) (Node, bool) { return Node{}, false }
	}
	if o.Launch == nil {
		o.Launch = func(string, string) (string, bool) { return "", false }
	}
	r := &Relay{o: o, log: o.Logger, nodes: map[string]*nodeState{}, clients: map[string]*bucket{}}
	if s := strings.TrimSpace(o.StoreURL); s != "" {
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("logrelay: the log store address must look like http://host:port, got %q", o.StoreURL)
		}
		r.base = strings.TrimRight(u.String(), "/")
		r.insert = r.base + "/insert/jsonline"
	}
	tr := o.Transport
	if tr == nil {
		// The relay's own connections to the store: nothing here is shared
		// with the scheduler's or the pool's clients.
		tr = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          MaxInFlight,
			MaxIdleConnsPerHost:   MaxInFlight,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: o.ForwardTimeout,
			DisableCompression:    true,
			WriteBufferSize:       16 << 10,
			ReadBufferSize:        4 << 10,
		}
	}
	r.client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r.read = newReader(o)
	return r, nil
}

// HasStore reports whether Potluck has a log store address.
func (r *Relay) HasStore() bool { return r.insert != "" }

// On reports whether logging is on: logs_enabled, and Potluck has a store
// address or logs_push_url names another address. While it is off no logs
// object is handed out and every log route answers 404.
func (r *Relay) On() bool {
	c := r.o.Config()
	return c.Enabled && (r.insert != "" || c.PushURL != "")
}

// passOn reports whether the pass-through routes are on: logging is on and
// Potluck has a store to forward to.
func (r *Relay) passOn() bool { return r.insert != "" && r.o.Config().Enabled }

// StoreOn reports whether Potluck has a log store and logging_enabled is on:
// then its own lines go to the store and the store's pages are served.
func (r *Relay) StoreOn() bool { return r.passOn() }

// InsertURL is the store's intake address with these fields set for the whole
// request (and every other trusted field ignored), as the pass-through sends
// it. "" without a store. Potluck's own sender posts there directly.
func (r *Relay) InsertURL(fields []Field) string {
	if r.insert == "" {
		return ""
	}
	return r.insert + "?" + storeQuery(fields)
}

// Revision is the revision of the logs object, 0 while logging is off. It is
// a figure computed from every logging setting and from whether there is a
// store, so it changes whenever one of them changes and is the same again
// after a restart with the same settings. It is not a counter.
func (r *Relay) Revision() int64 {
	if !r.On() {
		return 0
	}
	c := r.o.Config()
	h := fnv.New32a()
	fmt.Fprintf(h, "v1|%t|%d|%d|%q|%t|%t|%d|%d|%d|%d", c.Enabled, c.RetentionDays, c.MaxDiskGiB, c.PushURL, c.ClientIntake, r.insert != "",
		MaxBodyBytes, agentproto.LogsMaxLineBytes, NodeBytesPerS, agentproto.LogsEveryS)
	rev := int64(h.Sum32() & 0x7fffffff)
	if rev == 0 {
		rev = 1
	}
	return rev
}

// Object is the logs object of a node, nil while logging is off.
func (r *Relay) Object(nodeID string) *agentproto.Logs {
	rev := r.Revision()
	if rev == 0 {
		return nil
	}
	u := r.o.Config().PushURL
	if u == "" {
		u = fmt.Sprintf(agentproto.PathNodeLogsFmt, url.PathEscape(nodeID))
	}
	return &agentproto.Logs{Revision: rev, URL: u, Format: agentproto.LogsFormatJSONLine,
		Limits: agentproto.LogsLimits{MaxBatchBytes: MaxBodyBytes, MaxLineBytes: agentproto.LogsMaxLineBytes,
			MaxBytesPerS: NodeBytesPerS, EveryS: agentproto.LogsEveryS},
		DenyUnits: []string{}}
}

// ClientObject is the logs object of a client, nil while logging or the
// client intake is off.
func (r *Relay) ClientObject() *agentproto.Logs {
	rev := r.Revision()
	c := r.o.Config()
	if rev == 0 || !c.ClientIntake {
		return nil
	}
	u := c.PushURL
	if u == "" {
		u = agentproto.PathClientLogs
	}
	return &agentproto.Logs{Revision: rev, URL: u, Format: agentproto.LogsFormatJSONLine,
		Limits: agentproto.LogsLimits{MaxBatchBytes: MaxBodyBytes, MaxLineBytes: agentproto.LogsMaxLineBytes,
			MaxBytesPerS: ClientBytesPerS, EveryS: agentproto.LogsEveryS},
		DenyUnits: []string{}}
}

// ---- answers ----

// keepAliveBodyBytes: a refused request with a declared body of at most this
// size keeps its connection (the server reads the rest of the small body
// away). A larger body, or one without a declared length, is not read: the
// connection is closed after the answer.
const keepAliveBodyBytes = 64 << 10

// refuse answers a request whose body was not read. It never reads a large
// body to its end.
func refuse(w http.ResponseWriter, req *http.Request, status int, reason, msg string, retryAfter int) {
	if req.ContentLength < 0 || req.ContentLength > keepAliveBodyBytes {
		w.Header().Set("Connection", "close")
	}
	reply(w, status, reason, msg, retryAfter)
}

// reply writes a refusal and leaves the connection as it is.
func reply(w http.ResponseWriter, status int, reason, msg string, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	proxy.WriteError(w, status, msg, typ, reason, nil)
}

func notFound(w http.ResponseWriter, req *http.Request) {
	refuse(w, req, http.StatusNotFound, ReasonLogsOff, "central logging is off on this Potluck", 0)
}

// node checks the sender of a node route like the heartbeat does: the id of
// a registered node and its agent_start_id, here in a header.
func (r *Relay) node(w http.ResponseWriter, req *http.Request, id string) (Node, bool) {
	n, ok := r.o.Node(id)
	if ok && req.Header.Get(agentproto.HeaderAgentStartID) == n.StartID {
		return n, true
	}
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.unknown++
	}()
	msg := "unknown agent; register again"
	if ok {
		msg = "agent_start_id changed; register again"
	}
	refuse(w, req, http.StatusGone, ReasonUnknownSender, msg, 0)
	return Node{}, false
}

// ServeNodeObject is GET /api/pool/nodes/{id}/logs.
func (r *Relay) ServeNodeObject(w http.ResponseWriter, req *http.Request, id string) {
	if !r.On() {
		notFound(w, req)
		return
	}
	if _, ok := r.node(w, req, id); !ok {
		return
	}
	writeJSON(w, r.Object(id))
}

// ServeClientObject is GET /api/logs/config.
func (r *Relay) ServeClientObject(w http.ResponseWriter, req *http.Request) {
	o := r.ClientObject()
	if o == nil {
		notFound(w, req)
		return
	}
	writeJSON(w, o)
}

// ServeNode is POST /api/pool/nodes/{id}/logs?source=agent|launch|journal[&launch_id=...].
func (r *Relay) ServeNode(w http.ResponseWriter, req *http.Request, id string) {
	if !r.passOn() {
		notFound(w, req)
		return
	}
	n, ok := r.node(w, req, id)
	if !ok {
		return
	}
	q := req.URL.Query()
	source := q.Get("source")
	fields := []Field{{FieldNodeID, id}, {FieldNodeName, n.Name}, {FieldPlatform, n.Platform}, {FieldRelease, n.Release}, {FieldSource, source}}
	switch source {
	case agentproto.LogSourceAgent, agentproto.LogSourceJournal:
	case agentproto.LogSourceLaunch:
		lid := q.Get("launch_id")
		if !validLaunchID(lid) {
			r.refuseNode(w, req, id, http.StatusBadRequest, ReasonBadRequest, "source=launch needs a launch_id of 1 to 128 letters, digits, dots, dashes, underscores or colons", 0)
			return
		}
		fields = append(fields, Field{FieldLaunchID, lid})
		if prof, ok := r.o.Launch(lid, id); ok {
			fields = append(fields, Field{FieldProfile, prof})
		}
	default:
		r.refuseNode(w, req, id, http.StatusBadRequest, ReasonBadRequest, "source must be agent, launch or journal", 0)
		return
	}
	r.pass(w, req, id, false, fields)
}

// ServeClient is POST /api/logs.
func (r *Relay) ServeClient(w http.ResponseWriter, req *http.Request) {
	if !r.passOn() || !r.o.Config().ClientIntake {
		notFound(w, req)
		return
	}
	addr, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		addr = req.RemoteAddr
	}
	r.pass(w, req, addr, true, []Field{{FieldSource, agentproto.LogSourceClient}, {FieldClientAddr, addr}, {FieldActor, req.Header.Get("X-Actor")}})
}

func validLaunchID(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_', c == ':':
		default:
			return false
		}
	}
	return true
}

// refuseNode counts a refusal for a node and answers it.
func (r *Relay) refuseNode(w http.ResponseWriter, req *http.Request, id string, status int, reason, msg string, retryAfter int) {
	r.count(id, false, reason)
	refuse(w, req, status, reason, msg, retryAfter)
}

func (r *Relay) count(key string, client bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if client {
		r.clientC.refuse(reason)
		return
	}
	r.nodeLocked(key).c.refuse(reason)
}

func (r *Relay) nodeLocked(id string) *nodeState {
	s := r.nodes[id]
	if s == nil {
		s = &nodeState{}
		r.nodes[id] = s
	}
	return s
}

// ---- limits ----

// need is what a request must find in a bucket to be let in: its declared
// length, or one byte when the length is not declared (it is then charged
// while it streams).
func need(contentLength int64) float64 {
	if contentLength > 0 {
		return float64(contentLength)
	}
	return 1
}

func retrySeconds(wait time.Duration) int {
	return int(math.Min(math.Max(math.Ceil(wait.Seconds()), 1), 60))
}

// admit lets a request in or says why not. key is the node id, or the
// client's address. On success the request holds one of the MaxInFlight
// places until release is called.
func (r *Relay) admit(key string, client bool, contentLength int64) (reason string, retryAfter int) {
	now := r.o.Now()
	n := need(contentLength)
	charge := float64(max(contentLength, 0))
	r.mu.Lock()
	defer r.mu.Unlock()
	if client {
		if r.inFlight >= MaxInFlight || r.clientInFlight >= MaxClientInFlight {
			r.clientC.refuse(ReasonTooManyRequests)
			return ReasonTooManyRequests, 1
		}
		b := r.clients[key]
		if b == nil {
			if len(r.clients) >= maxClientAddrs {
				r.pruneClientsLocked(now)
			}
			if len(r.clients) >= maxClientAddrs {
				r.clientC.refuse(ReasonTooManyRequests)
				return ReasonTooManyRequests, 1
			}
			b = &bucket{}
			r.clients[key] = b
		}
		w1 := b.wait(now, n, ClientBytesPerS, ClientBurstBytes)
		w2 := r.allClients.wait(now, n, AllClientsBytesPerS, AllClientsBurstBytes)
		if w := max(w1, w2); w > 0 {
			r.clientC.refuse(ReasonRateLimited)
			return ReasonRateLimited, retrySeconds(w)
		}
		b.tokens -= charge
		r.allClients.tokens -= charge
		r.clientInFlight++
	} else {
		s := r.nodeLocked(key)
		if r.inFlight >= MaxInFlight {
			s.c.refuse(ReasonTooManyRequests)
			return ReasonTooManyRequests, 1
		}
		if s.busy {
			s.c.refuse(ReasonNodeBusy)
			return ReasonNodeBusy, 1
		}
		if w := s.bucket.wait(now, n, NodeBytesPerS, NodeBurstBytes); w > 0 {
			s.c.refuse(ReasonRateLimited)
			return ReasonRateLimited, retrySeconds(w)
		}
		s.bucket.tokens -= charge
		s.busy = true
	}
	r.inFlight++
	r.peak = max(r.peak, r.inFlight)
	return "", 0
}

// release gives the place back. read is what the request streamed; a request
// without a declared length is charged now. refused, when set, is counted;
// otherwise the request counts as accepted.
func (r *Relay) release(key string, client bool, contentLength, read int64, refused string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight--
	late := 0.0
	if contentLength < 0 {
		late = float64(read)
	}
	c := &r.clientC
	if client {
		r.clientInFlight--
		if b := r.clients[key]; b != nil {
			b.tokens -= late
		}
		r.allClients.tokens -= late
	} else {
		s := r.nodeLocked(key)
		s.busy = false
		s.bucket.tokens -= late
		c = &s.c
	}
	if refused != "" {
		c.refuse(refused)
		return
	}
	c.AcceptedBytes += read
	c.AcceptedRequests++
}

// pruneClientsLocked forgets the client addresses whose bucket is full again.
func (r *Relay) pruneClientsLocked(now time.Time) {
	for k, b := range r.clients {
		if b.wait(now, ClientBurstBytes, ClientBytesPerS, ClientBurstBytes) == 0 {
			delete(r.clients, k)
		}
	}
}

// ---- the forward ----

var errTooLarge = errors.New("logrelay: the body is larger than the limit")

// limitBody counts the body while the transport streams it and stops it at
// max bytes. It holds no data. Its fields are atomic because the transport
// may still read after the forward has returned.
type limitBody struct {
	src      io.Reader
	max      int64
	n        atomic.Int64
	tooLarge atomic.Bool
	failed   atomic.Bool // the sender's body could not be read
	eof      atomic.Bool // the whole body was read
	onEOF    func()      // called once, when the whole body was read
}

func (b *limitBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if n > 0 && b.n.Add(int64(n)) > b.max {
		b.tooLarge.Store(true)
		return 0, errTooLarge
	}
	switch {
	case err == io.EOF:
		if b.eof.CompareAndSwap(false, true) && b.onEOF != nil {
			b.onEOF()
		}
	case err != nil:
		b.failed.Store(true)
	}
	return n, err
}

// Close does nothing: the incoming body belongs to the server.
func (b *limitBody) Close() error { return nil }

// pass checks the limits and forwards the request. The body is streamed to
// the store as it is: never decoded, re-encoded or held in memory.
func (r *Relay) pass(w http.ResponseWriter, req *http.Request, key string, client bool, fields []Field) {
	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	if enc == "identity" {
		enc = ""
	}
	if enc != "" && enc != "gzip" {
		r.count(key, client, ReasonBadEncoding)
		refuse(w, req, http.StatusUnsupportedMediaType, ReasonBadEncoding, "Content-Encoding must be gzip or absent", 0)
		return
	}
	if req.ContentLength > MaxBodyBytes {
		r.count(key, client, ReasonTooLarge)
		refuse(w, req, http.StatusRequestEntityTooLarge, ReasonTooLarge, fmt.Sprintf("the body is larger than %d bytes", MaxBodyBytes), 0)
		return
	}
	if req.ContentLength == 0 {
		w.WriteHeader(http.StatusNoContent) // nothing to store
		return
	}
	if reason, retry := r.admit(key, client, req.ContentLength); reason != "" {
		msg := "too many log requests are in flight; send again later"
		switch reason {
		case ReasonNodeBusy:
			msg = "this node already has a log request in flight; send one at a time"
		case ReasonRateLimited:
			msg = "this sender is over its byte rate; send again after Retry-After"
		}
		refuse(w, req, http.StatusTooManyRequests, reason, msg, retry)
		return
	}
	body := &limitBody{src: req.Body, max: MaxBodyBytes}
	status, reason, msg := r.forward(w, req, body, enc, fields)
	r.release(key, client, req.ContentLength, body.n.Load(), reason)
	if reason == "" {
		if !body.eof.Load() {
			w.Header().Set("Connection", "close") // the store answered before the body ended
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	retry := 0
	if status == http.StatusServiceUnavailable {
		retry = 5
	}
	switch {
	case body.eof.Load(): // the whole body was read: the connection is good for the next request
		reply(w, status, reason, msg, retry)
	case body.tooLarge.Load() || body.failed.Load():
		w.Header().Set("Connection", "close") // the rest of that body is not read
		reply(w, status, reason, msg, retry)
	default:
		refuse(w, req, status, reason, msg, retry)
	}
}

// forward sends the body to the store within ForwardTimeout. reason is empty
// when the store took it.
func (r *Relay) forward(w http.ResponseWriter, req *http.Request, body *limitBody, enc string, fields []Field) (status int, reason, msg string) {
	limit := r.o.ForwardTimeout
	// The sender's body must arrive within that time too, so a sender that
	// stalls cannot hold its place longer. Its deadline is a little earlier
	// than the store's, so that a stalled sender is told so (400) and is not
	// taken for a silent store (503). The deadline is lifted as soon as the
	// whole body is here; a body that never came whole leaves it in place and
	// the connection is closed with the answer (pass).
	rc := http.NewResponseController(w)
	if rc.SetReadDeadline(time.Now().Add(limit-limit/10)) == nil {
		body.onEOF = func() { rc.SetReadDeadline(time.Time{}) }
	}
	ctx, cancel := context.WithTimeout(req.Context(), limit)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, r.insert+"?"+storeQuery(fields), body)
	if err != nil {
		return http.StatusServiceUnavailable, ReasonStoreUnavailable, "the log store address is not usable"
	}
	out.ContentLength = req.ContentLength // -1: not declared, sent chunked
	out.Header.Set("Content-Type", "application/stream+json")
	if enc != "" {
		out.Header.Set("Content-Encoding", enc)
	}
	resp, err := r.client.Do(out)
	if err != nil {
		switch {
		case body.tooLarge.Load():
			return http.StatusRequestEntityTooLarge, ReasonTooLarge, fmt.Sprintf("the body is larger than %d bytes", MaxBodyBytes)
		case body.failed.Load():
			return http.StatusBadRequest, ReasonBodyRead, fmt.Sprintf("the body could not be read within %v", limit)
		}
		r.storeDown("err", err)
		return http.StatusServiceUnavailable, ReasonStoreUnavailable, "the log store did not answer"
	}
	defer resp.Body.Close()
	var note [512]byte
	n, _ := io.ReadFull(resp.Body, note[:])
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return http.StatusNoContent, "", ""
	case resp.StatusCode == http.StatusBadRequest:
		// The batch itself is bad (no line of it is JSON): sending it again
		// would not help. Every other answer is taken as the store's fault,
		// so that a store behind a wrong address does not make senders drop
		// their lines.
		r.log.Debug("log store refused a batch", "status", resp.StatusCode, "answer", string(note[:n]))
		return http.StatusBadRequest, ReasonStoreRejected, "the log store refused the batch (400)"
	}
	r.storeDown("status", resp.StatusCode, "answer", string(note[:n]))
	return http.StatusServiceUnavailable, ReasonStoreUnavailable, fmt.Sprintf("the log store answered %d", resp.StatusCode)
}

// storeDown logs that the store did not take a batch, at most once a minute.
func (r *Relay) storeDown(attrs ...any) {
	now := r.o.Now()
	var quiet bool
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		quiet = !r.lastWarn.IsZero() && now.Sub(r.lastWarn) < time.Minute
		if !quiet {
			r.lastWarn = now
		}
	}()
	if !quiet {
		r.log.Warn("the log store did not take a batch; senders keep their lines and try again (logged at most once a minute)", attrs...)
	}
}

// ---- status ----

// NodeCounters are the figures of a node; false when it never sent.
func (r *Relay) NodeCounters(id string) (Counters, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.nodes[id]
	if s == nil {
		return Counters{}, false
	}
	return s.c.clone(), true
}

// Status is the relay's own part of GET /api/status.
type Status struct {
	Enabled       bool     `json:"enabled"`
	Store         bool     `json:"store"` // Potluck has a log store address
	Revision      int64    `json:"revision"`
	InFlight      int      `json:"in_flight"`
	PeakInFlight  int      `json:"peak_in_flight"`
	MaxInFlight   int      `json:"max_in_flight"`
	ClientIntake  bool     `json:"client_intake"`
	Clients       Counters `json:"clients"`
	UnknownSender int64    `json:"unknown_sender"` // requests answered 410
	// ReadsInFlight and MaxReads: the read side (/logs/, /api/logs/query).
	ReadsInFlight int `json:"reads_in_flight"`
	MaxReads      int `json:"max_reads"`
	// StoreSettings is what the store runs with (its /flags page), absent
	// until it was read; Warnings says where the settings differ from it.
	StoreSettings *StoreSettings `json:"store_settings,omitempty"`
	Warnings      []string       `json:"warnings,omitempty"`
}

// Status returns the relay's figures.
func (r *Relay) Status() Status {
	c := r.o.Config()
	st := Status{Enabled: r.On(), Store: r.insert != "", Revision: r.Revision(), MaxInFlight: MaxInFlight,
		ClientIntake: c.ClientIntake && r.On(), ReadsInFlight: r.ReadsInFlight(), MaxReads: MaxReads}
	st.StoreSettings, st.Warnings = r.settingsWarnings()
	r.mu.Lock()
	defer r.mu.Unlock()
	st.InFlight, st.PeakInFlight, st.Clients, st.UnknownSender = r.inFlight, r.peak, r.clientC.clone(), r.unknown
	return st
}
