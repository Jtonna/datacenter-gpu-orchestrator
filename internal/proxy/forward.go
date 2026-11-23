package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
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
		go func() {
			select {
			case <-o.Gone:
				cut.Store(true)
				cancel()
			case <-ctx.Done():
			}
		}()
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
			if o.APIKey != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+o.APIKey)
			}
			if o.Path != "" {
				pr.Out.URL.Path, pr.Out.URL.RawPath = o.Path, o.RawPath
			}
			if o.Body != nil {
				pr.Out.Body = o.Body
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			stall := time.Duration(0)
			if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				stall = o.StallTimeout
			}
			body = &watchBody{rc: resp.Body, tail: tail, stall: stall}
			resp.Body = body
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			res.Err = err
			if client.Err() != nil {
				return // the client left; nobody to answer
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
		if res.Status == 0 {
			if client.Err() != nil {
				res.Status = 499
			} else {
				res.Status = http.StatusOK
			}
		}
		res.InputTokens, res.OutputTokens, res.TokensPerSecond = extractUsage(tail.bytes())
		res.Stalled = body != nil && body.stalled.Load()
	}()
	rp.ServeHTTP(sw, r)
	return res
}

// statusWriter records the status code and passes Flush and Unwrap through
// (the reverse proxy flushes through http.ResponseController).
type statusWriter struct {
	http.ResponseWriter
	status int
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
	return s.ResponseWriter.Write(p)
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
	}
	if b.stalled.Load() && n == 0 {
		return b.emitStall(p)
	}
	return n, err
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
// per second; -1 for each that is absent. The tail may start mid-document,
// so it searches for the keys instead of parsing the whole thing. Keys
// inside JSON strings are escaped (\"usage\") and so never match.
func extractUsage(tail []byte) (in, out int, tps float64) {
	in, out, tps = -1, -1, -1
	if u := lastObject(tail, `"usage"`); u != nil {
		in = firstInt(u, "prompt_tokens", "input_tokens")
		out = firstInt(u, "completion_tokens", "output_tokens")
	}
	if t := lastObject(tail, `"timings"`); t != nil {
		if v, ok := num(t["predicted_per_second"]); ok {
			tps = v
		}
		if in < 0 {
			in = firstInt(t, "prompt_n")
		}
		if out < 0 {
			out = firstInt(t, "predicted_n")
		}
	}
	return in, out, tps
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
