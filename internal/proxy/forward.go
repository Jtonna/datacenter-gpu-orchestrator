package proxy

import (
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"math"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// TailBytes is how much of the end of each upstream response is kept for
// token accounting (design A.4).
const TailBytes = 16 << 10

// Options say where and how to forward one request.
type Options struct {
	// Addr is the upstream host:port (the lease address).
	Addr string
	// Path and RawPath replace the request's path when Path is non-empty
	// (used to strip the /upstream/{model} prefix). The query is kept.
	Path, RawPath string
	// Body replaces the request body (the replay body from Extract).
	Body io.ReadCloser
	// StallTimeout is the longest gap between bytes of a streaming response
	// after its first chunk; 0 disables the check (design 8.4).
	StallTimeout time.Duration
	// APIKey, when set, is sent upstream as "Authorization: Bearer <key>"
	// in place of whatever the client sent (a host on a node, phase 6).
	APIKey string
	// Gone, when it closes, ends the forwarded request: the instance behind
	// Addr was removed (its node was lost, it exited). Nil never closes.
	Gone <-chan struct{}
	// Live, when set, is updated while the response streams (the request
	// log's live rows): no allocation, no lock, nothing written anywhere.
	Live *Live
	// BodyLen, when above 0, is Body's length: the Content-Length of a body
	// that differs from the client's (Extracted.AddTimingsPerToken).
	BodyLen int64
	// CutBy, read when the upstream fails, says whether the instance behind
	// Addr was stopped on purpose (a forced unload) and what the caller is
	// told then, instead of the broken connection: before any byte was
	// sent, that status and message; in an event stream already under way,
	// a final SSE error event with them. Nil, or nil from it: not on purpose.
	CutBy func() *CutAnswer
	// OnResponse, when set, is called once the model server's status and
	// headers have arrived, just before the status is written to the caller
	// (tests cut a request at exactly that point).
	OnResponse func()
}

// CutAnswer is what a request cut on purpose is told (Options.CutBy).
type CutAnswer struct {
	Status     int
	Message    string
	Type       string
	Reason     string
	RetryAfter time.Duration
}

// Live is a forwarded request's progress while it runs. Read it with its
// methods from any goroutine.
type Live struct {
	firstNS atomic.Int64 // first response body bytes, unix ns; 0 before
	chunks  atomic.Int64 // SSE data events seen (about one per output token)
	prefill atomic.Pointer[prefill]
}

// prefill is what the stream said about the prompt so far.
type prefill struct {
	total, reused, processed int
	tps                      float64
}

// Prefill is what llama-server said about the prompt while it streams:
// every prompt token (reused ones included), the reused ones, how many are
// done, and the prefill rate (0 before it is known). llama-server says it
// only when the request asked: "return_progress" (prompt_progress events
// while the prompt is read) or "timings_per_token" (timings on every
// event, from the first token on). ok is false until then.
func (l *Live) Prefill() (total, reused, processed int, tps float64, ok bool) {
	if l == nil {
		return 0, 0, 0, 0, false
	}
	p := l.prefill.Load()
	if p == nil {
		return 0, 0, 0, 0, false
	}
	return p.total, p.reused, p.processed, p.tps, true
}

// noteStream reads the prompt figures in one read of an event stream: the
// last prompt_progress object ({total, cache, processed, time_ms}; processed
// counts the reused tokens) or the last timings object (prompt_n, the tokens
// read, and cache_n, the reused ones; prompt_per_second). Objects split
// across two reads are skipped; the next event carries newer figures.
func (l *Live) noteStream(b []byte) {
	if bytes.Contains(b, timingsKey) {
		if t := lastObject(b, string(timingsKey)); t != nil {
			if n, c := firstInt(t, "prompt_n"), firstInt(t, "cache_n"); n >= 0 {
				p := &prefill{total: n + max(c, 0), reused: max(c, 0)}
				p.processed = p.total
				if v, ok := num(t["prompt_per_second"]); ok && v > 0 {
					p.tps = v
				}
				l.prefill.Store(p)
				return
			}
		}
	}
	if bytes.Contains(b, progressKey) {
		if t := lastObject(b, string(progressKey)); t != nil {
			total, cache, done := firstInt(t, "total"), firstInt(t, "cache"), firstInt(t, "processed")
			if total < 0 || done < 0 {
				return
			}
			p := &prefill{total: total, reused: max(cache, 0), processed: done}
			if ms, ok := num(t["time_ms"]); ok && ms > 0 && done > p.reused {
				p.tps = float64(done-p.reused) / (ms / 1000)
			}
			l.prefill.Store(p)
		}
	}
}

var (
	timingsKey  = []byte(`"timings"`)
	progressKey = []byte(`"prompt_progress"`)
)

// FirstByte is when the first bytes of the response body arrived (zero
// before): for a stream, about the first token.
func (l *Live) FirstByte() time.Time {
	if l == nil || l.firstNS.Load() == 0 {
		return time.Time{}
	}
	return time.Unix(0, l.firstNS.Load())
}

// Chunks is the number of SSE data events streamed so far: an estimate of
// the output tokens while a stream runs (one event is usually one token).
func (l *Live) Chunks() int64 {
	if l == nil {
		return 0
	}
	return l.chunks.Load()
}

// Result describes how one forwarded request ended.
type Result struct {
	// Status is the status sent to the client (0 if nothing was sent, which
	// happens when the client left first; Forward then reports 499).
	Status int
	// InputTokens, OutputTokens and TokensPerSecond come from the tail of
	// the response (usage, timings); -1 when absent.
	InputTokens, OutputTokens int
	TokensPerSecond           float64
	// Stalled is true when the response was cut by the stall timeout.
	Stalled bool
	// Aborted is true when the copy to the client failed after the headers
	// went out. The caller should panic(http.ErrAbortHandler) once its own
	// bookkeeping is done, so the client sees a truncated stream, not a
	// clean end.
	Aborted bool
	// Err is the transport error when the upstream could not be reached.
	Err error
	// Cut is true when Options.Gone ended the request.
	Cut bool
	// Unloaded is true when the request was cut because its instance was
	// stopped on purpose, and the caller was told so (Options.CutBy).
	Unloaded bool
	// ClientLeft is true when the client went away: its request context
	// ended or a write to it failed (before or in the middle of the
	// answer). The model server and the scheduler did not fail.
	ClientLeft bool
	// Timings from the tail (llama-server's "timings"); -1 when absent.
	// CachedTokens are prompt tokens reused from the prompt cache (cache_n);
	// PromptMS and PredictedMS are the prefill and decode times, and
	// PromptPerSecond the prefill rate.
	CachedTokens          int
	PromptMS, PredictedMS float64
	PromptPerSecond       float64
	// Stream is true when the response was an event stream.
	Stream bool
}

// Reason names how the forwarded request failed, in the request log's words;
// "" when it did not fail on the proxy's side. A status the upstream sent
// (4xx, 5xx) is the caller's to read from Status.
func (r Result) Reason() string {
	switch {
	case r.Stalled:
		return "stall"
	case r.Unloaded:
		return "unloaded"
	case r.Cut:
		return "cut"
	case r.ClientLeft:
		return "client_left"
	case r.Err != nil:
		return "upstream_error"
	case r.Status == 499:
		return "client_left"
	case r.Aborted:
		return "aborted"
	}
	return ""
}

// ClientCredentialHeaders are the request headers that carry a client's
// credentials. None of them is passed on to a model server: they belong to
// the client (often another provider's real key, sent by habit), and the
// model server is Potluck's. A server with a launch key gets Potluck's own
// "Authorization: Bearer <launch key>" instead, never the client's.
var ClientCredentialHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"X-Api-Key",
	"Api-Key",        // Azure OpenAI style
	"X-Goog-Api-Key", // Google style
	"X-Auth-Token",
	"Cookie",
	"X-Agent-Key", // a Potluck agent key: for Potluck, not for the model server
}

// PotluckHeaders are Potluck's own request headers: who made a change, why,
// and from where (the web UI). Potluck reads them for its audit and request
// log; the model server has no use for them, so they are not passed on.
var PotluckHeaders = []string{"X-Actor", "X-Comment", "X-Source"}

// dropClientCredentials removes, from the copy sent to the model server
// (never the request Potluck reads), the client's credentials and Potluck's
// own headers.
func dropClientCredentials(h http.Header) {
	for _, k := range ClientCredentialHeaders {
		h.Del(k)
	}
	for _, k := range PotluckHeaders {
		h.Del(k)
	}
}

// transport is shared by every proxied request. No ResponseHeaderTimeout:
// prompt processing can take minutes before the first byte. Compression is
// left to the client and upstream (the body is passed through verbatim).
var transport = &http.Transport{
	Proxy:               nil,
	DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 16,
	IdleConnTimeout:     90 * time.Second,
	DisableCompression:  true,
}

// Forward proxies r to opts.Addr and streams the response back with
// immediate flushing. It returns when the response has ended or the client
// has gone; the caller then releases the lease. Client cancellation reaches
// the upstream through the request context.
func Forward(w http.ResponseWriter, r *http.Request, o Options) (res Result) {
	sw := &statusWriter{ResponseWriter: w}
	tail := &tailBuf{}
	var body *watchBody
	client := r.Context() // the client's own context: did the client leave?
	var cut atomic.Bool
	if o.Gone != nil {
		ctx, cancel := context.WithCancel(client)
		defer cancel()
		// Ends with the request: a select, nothing that can fail.
		go supervise.Once("proxy.gone_watch", func() {
			select {
			case <-o.Gone:
				cut.Store(true)
				cancel()
			case <-ctx.Done():
			}
		}, nil)
		r = r.WithContext(ctx)
	}

	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Transport:     transport,
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = o.Addr
			pr.Out.Host = o.Addr
			dropClientCredentials(pr.Out.Header)
			if o.APIKey != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+o.APIKey)
			}
			if o.Path != "" {
				pr.Out.URL.Path, pr.Out.URL.RawPath = o.Path, o.RawPath
			}
			if o.Body != nil {
				pr.Out.Body = o.Body
				if o.BodyLen > 0 {
					pr.Out.ContentLength = o.BodyLen
					pr.Out.Header.Set("Content-Length", strconv.FormatInt(o.BodyLen, 10))
					pr.Out.TransferEncoding = nil
				}
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			stall := time.Duration(0)
			if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				stall = o.StallTimeout
			}
			body = &watchBody{rc: resp.Body, tail: tail, stall: stall, live: o.Live, cutBy: o.CutBy,
				sse: strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")}
			resp.Body = body
			if o.OnResponse != nil {
				o.OnResponse()
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			res.Err = err
			if client.Err() != nil {
				return // the client left; nobody to answer
			}
			if o.CutBy != nil {
				if a := o.CutBy(); a != nil {
					res.Unloaded = true
					if a.RetryAfter > 0 {
						w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(a.RetryAfter.Seconds()))))
					}
					WriteError(w, a.Status, a.Message, a.Type, a.Reason, nil)
					return
				}
			}
			WriteError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "server_error", "upstream unavailable", nil)
		},
	}

	defer func() {
		if rec := recover(); rec != nil {
			if rec != http.ErrAbortHandler {
				panic(rec)
			}
			res.Aborted = true
		}
		res.Status = sw.status
		res.Cut = cut.Load()
		// The client went away before the whole answer reached it: its
		// context ended or a write to it failed, and the copy was cut
		// short (or never began). A client that hangs up just after the
		// last byte got the whole answer: that is done.
		res.ClientLeft = (client.Err() != nil || sw.writeFailed) && (res.Aborted || res.Err != nil || sw.status == 0)
		if res.Status == 0 {
			if client.Err() != nil {
				res.Status = 499
			} else {
				res.Status = http.StatusOK
			}
		}
		res.InputTokens, res.OutputTokens, res.TokensPerSecond = extractUsage(tail.bytes())
		res.CachedTokens, res.PromptMS, res.PredictedMS, res.PromptPerSecond = extractTimings(tail.bytes())
		res.Stream = body != nil && body.sse
		res.Stalled = body != nil && body.stalled.Load()
		res.Unloaded = res.Unloaded || body != nil && body.cutSent
	}()
	rp.ServeHTTP(sw, r)
	return res
}

// statusWriter records the status code and passes Flush and Unwrap through
// (the reverse proxy flushes through http.ResponseController).
type statusWriter struct {
	http.ResponseWriter
	status      int
	writeFailed bool // a write to the client failed: it is gone
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	if err != nil {
		s.writeFailed = true
	}
	return n, err
}

func (s *statusWriter) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	http.NewResponseController(s.ResponseWriter).Flush()
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// tailBuf keeps the last TailBytes bytes written to it.
type tailBuf struct{ buf []byte }

func (t *tailBuf) Write(p []byte) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*TailBytes {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-TailBytes:]...)
	}
}

func (t *tailBuf) bytes() []byte {
	if len(t.buf) > TailBytes {
		return t.buf[len(t.buf)-TailBytes:]
	}
	return t.buf
}

// watchBody wraps the upstream response body. It records the tail without
// touching the bytes, and (for streams) enforces the stall timeout.
//
// The stall timer only runs while a Read is blocked on the upstream, so a
// slow client is never mistaken for a stalled model, and it is armed only
// after the first chunk (prompt processing is never timed). When it fires
// it closes the upstream body, which unblocks the Read; the next Read then
// delivers one SSE error event and ends the stream. Doing that inside Read
// keeps every write to the client on the proxy's own goroutine.
type watchBody struct {
	rc    io.ReadCloser
	tail  *tailBuf
	stall time.Duration
	live  *Live // nil: not watched
	sse   bool  // an event stream: data events are counted in live
	// cutBy says whether a broken upstream was stopped on purpose; a stream
	// then ends with an SSE error event saying so (cutSent).
	cutBy   func() *CutAnswer
	cutAns  *CutAnswer
	cutSent bool

	gotFirst bool
	timer    *time.Timer
	stalled  atomic.Bool
	pending  []byte
	finished bool
}

func (b *watchBody) Read(p []byte) (int, error) {
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	if b.finished {
		return 0, io.EOF
	}
	if b.cutAns != nil {
		return b.emitCut(p)
	}
	if b.stalled.Load() {
		return b.emitStall(p)
	}
	if b.stall > 0 && b.gotFirst {
		if b.timer == nil {
			b.timer = time.AfterFunc(b.stall, func() {
				b.stalled.Store(true)
				b.rc.Close()
			})
		} else {
			b.timer.Reset(b.stall)
		}
	}
	n, err := b.rc.Read(p)
	if b.timer != nil {
		b.timer.Stop()
	}
	if n > 0 {
		b.gotFirst = true
		b.tail.Write(p[:n])
		if b.live != nil {
			b.watchLive(p[:n])
		}
	}
	if b.stalled.Load() && n == 0 {
		return b.emitStall(p)
	}
	// An event stream whose instance was stopped on purpose: the bytes read
	// go out, then a final error event says why, and the stream ends cleanly.
	// Any other body (not an event stream) is cut as before.
	if err != nil && err != io.EOF && b.sse && b.cutBy != nil {
		if a := b.cutBy(); a != nil {
			b.cutAns = a
			if n > 0 {
				return n, nil
			}
			return b.emitCut(p)
		}
	}
	return n, err
}

// emitCut ends an event stream with an error event: the same message,
// reason and status a request cut before its first byte gets.
func (b *watchBody) emitCut(p []byte) (int, error) {
	a := b.cutAns
	b.cutAns, b.finished, b.cutSent = nil, true, true
	msg, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": a.Message, "type": a.Type, "code": a.Status, "reason": a.Reason,
	}})
	b.pending = []byte("event: error\ndata: " + string(msg) + "\n\n")
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// watchLive notes a read in b.live: the first byte, and for an event stream
// the data events and the prompt figures. Progress events (prompt_progress,
// sent while the prompt is read) are neither tokens nor the first token.
func (b *watchBody) watchLive(p []byte) {
	if !b.sse {
		b.live.firstNS.CompareAndSwap(0, time.Now().UnixNano())
		return
	}
	events := int64(bytes.Count(p, dataPrefix))
	progress := int64(0)
	if bytes.Contains(p, progressKey) {
		progress = int64(bytes.Count(p, progressKey))
	}
	if events > progress || events == 0 && progress == 0 {
		b.live.firstNS.CompareAndSwap(0, time.Now().UnixNano())
	}
	if events > progress {
		b.live.chunks.Add(events - progress)
	}
	b.live.noteStream(p)
}

func (b *watchBody) emitStall(p []byte) (int, error) {
	b.finished = true
	msg, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "upstream stalled: no data for " + b.stall.String() + " after the first chunk",
		"type":    "server_error", "code": http.StatusGatewayTimeout, "reason": "stall timeout",
	}})
	b.pending = []byte("event: error\ndata: " + string(msg) + "\n\n")
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *watchBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	return b.rc.Close()
}

// WriteError writes an OpenAI-format JSON error:
// {"error":{"message","type","code","reason"[,"tail"]}}.
func WriteError(w http.ResponseWriter, status int, message, typ, reason string, tail []string) {
	e := map[string]any{"message": message, "type": typ, "param": nil, "code": status}
	if reason != "" {
		e["reason"] = reason
	}
	if len(tail) > 0 {
		e["tail"] = tail
	}
	b, _ := json.Marshal(map[string]any{"error": e})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// extractUsage finds the last usable "usage" and "timings" objects in the
// tail of a response (a JSON body, or the final data: chunks of an event
// stream) and returns prompt tokens, completion tokens and predicted tokens
// per second; -1 for each that is absent. Prompt tokens are the whole
// prompt, reused ones included: usage.prompt_tokens, or, for an answer with
// no usage object (the model server's native /completion route), the
// timings' prompt_n + cache_n. The tail may start mid-document, so it
// searches for the keys instead of parsing the whole thing. Keys inside
// JSON strings are escaped (\"usage\") and so never match.
func extractUsage(tail []byte) (in, out int, tps float64) {
	in, out, tps = -1, -1, -1
	u := lastObject(tail, `"usage"`)
	if u != nil {
		in = firstInt(u, "prompt_tokens", "input_tokens")
		out = firstInt(u, "completion_tokens", "output_tokens")
	}
	if t := lastObject(tail, `"timings"`); t != nil {
		if v, ok := num(t["predicted_per_second"]); ok {
			tps = v
		}
		if in < 0 {
			in = firstInt(t, "prompt_n")
			if c := firstInt(t, "cache_n"); in >= 0 && c > 0 && u == nil {
				in += c // prompt_n leaves out the reused tokens; usage.prompt_tokens counts them
			}
		}
		if out < 0 {
			out = firstInt(t, "predicted_n")
		}
	}
	return in, out, tps
}

// dataPrefix starts an SSE data line (a data line split across two reads is
// missed: the count is an estimate, replaced by the real figure at the end).
var dataPrefix = []byte("data: {")

// extractTimings reads llama-server's "timings" object in the tail: prompt
// tokens reused from the cache (cache_n), prefill and decode times in ms and
// the prefill rate; -1 for each that is absent.
func extractTimings(tail []byte) (cached int, promptMS, predictedMS, promptTPS float64) {
	cached, promptMS, predictedMS, promptTPS = -1, -1, -1, -1
	t := lastObject(tail, `"timings"`)
	if t == nil {
		return
	}
	cached = firstInt(t, "cache_n")
	if v, ok := num(t["prompt_ms"]); ok {
		promptMS = v
	}
	if v, ok := num(t["predicted_ms"]); ok {
		predictedMS = v
	}
	if v, ok := num(t["prompt_per_second"]); ok {
		promptTPS = v
	}
	return
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if f, ok := num(m[k]); ok && f >= 0 {
			return int(f)
		}
	}
	return -1
}

// lastObject decodes the JSON object that follows the last occurrence of
// key (and a colon) that is followed by a complete object.
func lastObject(tail []byte, key string) map[string]any {
	end := len(tail)
	for end > 0 {
		i := bytes.LastIndex(tail[:end], []byte(key))
		if i < 0 {
			return nil
		}
		end = i
		rest := bytes.TrimLeft(tail[i+len(key):], " \t\r\n")
		if len(rest) == 0 || rest[0] != ':' {
			continue
		}
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
		if len(rest) == 0 || rest[0] != '{' {
			continue
		}
		var m map[string]any
		if err := json.NewDecoder(bytes.NewReader(rest)).Decode(&m); err == nil {
			return m
		} else if errors.Is(err, io.EOF) {
			continue
		}
	}
	return nil
}
