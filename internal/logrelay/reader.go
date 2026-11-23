package logrelay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
)

// The read side (design section 7): Potluck serves the store's own search
// page and query API under /logs/ (a reverse proxy), and passes one LogsQL
// query through at GET /api/logs/query. Read only: nothing a reader sends can
// write to or administer the store.
//
// The store runs with -http.pathPrefix=/logs, so a path passes through
// unchanged: /logs/select/vmui/ here is /logs/select/vmui/ there (the store's
// address is http://potluck-logs:9428/logs; the path after /logs is appended
// to it).
//
// It is kept off the scheduler's paths like the intake: its own transport and
// connections, and at most MaxReads requests at once, the next one refused
// at once with 429.

// Limits of the read side.
const (
	// MaxReads is how many reads (proxied requests and queries together) may
	// run at once; the next one is answered 429 at once. A live tail of the
	// search page holds one for as long as it runs.
	MaxReads = 8
	// QueryTimeout bounds one GET /api/logs/query.
	QueryTimeout = 30 * time.Second
	// DefaultQueryLimit and MaxQueryLimit bound the lines of one query.
	DefaultQueryLimit = 1000
	MaxQueryLimit     = 10000
	// maxReadBody bounds the body of a proxied POST (a search page query).
	maxReadBody = 1 << 20
	// readHeaderTimeout bounds the wait for the store's answer to begin.
	readHeaderTimeout = 60 * time.Second
)

// Reason words of the read side.
const (
	ReasonReadOnly  = "read_only"      // 403: a path or method that is not a read
	ReasonBusyReads = "too_many_reads" // 429: MaxReads in flight
)

type reader struct {
	slots   chan struct{}
	client  *http.Client
	tr      http.RoundTripper
	timeout time.Duration
	// stopped ends every read in flight (StopReads, at shutdown).
	stopped context.Context
	stop    context.CancelFunc
}

func newReader(o Options) *reader {
	tr := o.ReadTransport
	if tr == nil {
		// Its own connections to the store, apart from the intake's and from
		// everything the scheduler uses.
		tr = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxConnsPerHost:       MaxReads,
			MaxIdleConns:          MaxReads,
			MaxIdleConnsPerHost:   MaxReads,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: readHeaderTimeout,
			DisableCompression:    true, // the reader's Accept-Encoding passes through as it is
		}
	}
	t := o.QueryTimeout
	if t <= 0 {
		t = QueryTimeout
	}
	stopped, stop := context.WithCancel(context.Background())
	return &reader{slots: make(chan struct{}, MaxReads), tr: tr, timeout: t, stopped: stopped, stop: stop,
		client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// take takes a read place without waiting; false when all are taken.
func (rd *reader) take() bool {
	select {
	case rd.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (rd *reader) give() { <-rd.slots }

// bind gives req a context that also ends with StopReads; call the returned
// function when the read is over.
func (rd *reader) bind(req *http.Request) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(rd.stopped, cancel)
	return req.WithContext(ctx), func() { stop(); cancel() }
}

// StopReads ends every read in flight (a streaming read of /logs/ may run as
// long as its reader stays) and refuses new ones with 503. Potluck calls it at
// shutdown, so shutting down never waits on the store or on a reader.
func (r *Relay) StopReads() { r.read.stop() }

// ReadsInFlight is how many reads run now.
func (r *Relay) ReadsInFlight() int { return len(r.read.slots) }

// readAllowed says whether a request on rest (the path after /logs) is a read
// the proxy passes on. Only these are:
//
//	GET, HEAD  /logs/  /logs/metrics  /logs/flags  /logs/health  /logs/robots.txt  /logs/favicon.ico
//	GET, HEAD, POST  /logs/select/...   (the search page posts its queries and its live tail)
//
// except /logs/select/vmalert/..., which would reach another program. Every
// other path is refused (403): /logs/insert/... (writing), /logs/delete/...
// and /logs/internal/... (deleting, merging, partitions: some of them answer
// a GET), /logs/debug/pprof/..., and anything the store adds later.
func readAllowed(method, rest string) bool {
	if !cleanPath(rest) {
		return false
	}
	sel := strings.HasPrefix(rest, "/select/") && !strings.HasPrefix(rest, "/select/vmalert/") && rest != "/select/vmalert"
	switch method {
	case http.MethodGet, http.MethodHead:
		switch rest {
		case "/", "/metrics", "/flags", "/health", "/robots.txt", "/favicon.ico":
			return true
		}
		return sel
	case http.MethodPost:
		return sel
	}
	return false
}

// cleanPath: rest starts with "/" and has no empty, "." or ".." segment
// (a trailing "/" is allowed), and no backslash. The store reads "//" as "/",
// so a path that is not clean could name another route than the one checked.
func cleanPath(rest string) bool {
	if !strings.HasPrefix(rest, "/") || strings.ContainsAny(rest, "\\\x00") {
		return false
	}
	segs := strings.Split(rest[1:], "/")
	for i, s := range segs {
		if s == "." || s == ".." || (s == "" && i != len(segs)-1) {
			return false
		}
	}
	return true
}

// readOff answers 404 while there is no store or logging is off.
func (r *Relay) readOff(w http.ResponseWriter, req *http.Request) bool {
	if r.passOn() {
		return false
	}
	notFound(w, req)
	return true
}

// ServePages is everything under /logs/: the store's own pages and query API,
// read only (readAllowed).
func (r *Relay) ServePages(w http.ResponseWriter, req *http.Request) {
	if r.readOff(w, req) {
		return
	}
	rest := strings.TrimPrefix(req.URL.Path, strings.TrimSuffix(agentproto.PathLogsPages, "/"))
	if !readAllowed(req.Method, rest) {
		refuse(w, req, http.StatusForbidden, ReasonReadOnly, "Potluck passes only reads to the log store: GET of its pages, and GET or POST under /logs/select/", 0)
		return
	}
	if !r.read.take() {
		refuse(w, req, http.StatusTooManyRequests, ReasonBusyReads, fmt.Sprintf("%d log reads are running; try again in a moment", MaxReads), 1)
		return
	}
	defer r.read.give()
	if r.read.stopped.Err() != nil {
		reply(w, http.StatusServiceUnavailable, ReasonStoreUnavailable, "Potluck is shutting down", 5)
		return
	}
	req, done := r.read.bind(req)
	defer done()
	target, err := url.Parse(r.base)
	if err != nil {
		reply(w, http.StatusServiceUnavailable, ReasonStoreUnavailable, "the log store address is not usable", 5)
		return
	}
	if req.Body != nil {
		req.Body = http.MaxBytesReader(w, req.Body, maxReadBody)
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The path that was checked, re-encoded: the store reads exactly it.
			pr.Out.URL = &url.URL{Scheme: target.Scheme, Host: target.Host, Path: target.Path + rest, RawQuery: pr.In.URL.RawQuery}
			pr.Out.Host = target.Host
			for _, k := range proxy.ClientCredentialHeaders { // the store gets none of the client's credentials
				pr.Out.Header.Del(k)
			}
		},
		Transport:     r.read.tr,
		FlushInterval: -1, // pass every write on at once: the search page tails logs
		ErrorLog:      nil,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			r.storeDown("op", "read", "err", err)
			reply(w, http.StatusServiceUnavailable, ReasonStoreUnavailable, "the log store did not answer", 5)
		},
	}
	p.ServeHTTP(w, req)
}

// ServeQuery is GET /api/logs/query?query=<LogsQL>[&limit=N][&start=T][&end=T]:
// one read-only query passed to the store's /select/logsql/query. limit is
// DefaultQueryLimit when absent and at most MaxQueryLimit (a larger one is
// cut to it; the answer's X-Potluck-Limit says the limit used). start and end
// are passed as given (the store reads RFC 3339, Unix seconds and relative
// forms such as 1h). The answer is the store's: JSON lines, one per log
// line, streamed; QueryTimeout bounds the whole query.
func (r *Relay) ServeQuery(w http.ResponseWriter, req *http.Request) {
	if r.readOff(w, req) {
		return
	}
	q := req.URL.Query()
	query := strings.TrimSpace(q.Get("query"))
	if query == "" || len(query) > 64<<10 {
		reply(w, http.StatusBadRequest, ReasonBadRequest, "query is required: a LogsQL query of at most 64 KiB, for example node_id:=\"02:00:00:00:00:01\" or _time:5m error", 0)
		return
	}
	limit := DefaultQueryLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			reply(w, http.StatusBadRequest, ReasonBadRequest, "limit must be a whole number of at least 1", 0)
			return
		}
		limit = min(n, MaxQueryLimit)
	}
	form := url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}}
	for _, k := range []string{"start", "end"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			if len(v) > 64 {
				reply(w, http.StatusBadRequest, ReasonBadRequest, k+" is too long", 0)
				return
			}
			form.Set(k, v)
		}
	}
	if !r.read.take() {
		reply(w, http.StatusTooManyRequests, ReasonBusyReads, fmt.Sprintf("%d log reads are running; try again in a moment", MaxReads), 1)
		return
	}
	defer r.read.give()
	if r.read.stopped.Err() != nil {
		reply(w, http.StatusServiceUnavailable, ReasonStoreUnavailable, "Potluck is shutting down", 5)
		return
	}
	req, done := r.read.bind(req)
	defer done()
	ctx, cancel := context.WithTimeout(req.Context(), r.read.timeout)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/select/logsql/query", strings.NewReader(form.Encode()))
	if err != nil {
		reply(w, http.StatusServiceUnavailable, ReasonStoreUnavailable, "the log store address is not usable", 5)
		return
	}
	out.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := r.read.client.Do(out)
	if err != nil {
		r.storeDown("op", "query", "err", err)
		reply(w, http.StatusServiceUnavailable, ReasonStoreUnavailable, "the log store did not answer", 5)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusBadRequest:
		var note [1024]byte
		n, _ := io.ReadFull(resp.Body, note[:])
		reply(w, http.StatusBadRequest, ReasonStoreRejected, "the log store refused the query: "+strings.TrimSpace(string(note[:n])), 0)
		return
	default:
		r.storeDown("op", "query", "status", resp.StatusCode)
		reply(w, http.StatusServiceUnavailable, ReasonStoreUnavailable, fmt.Sprintf("the log store answered %d", resp.StatusCode), 5)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/stream+json"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Potluck-Limit", strconv.Itoa(limit))
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			rc.Flush()
		}
		if err != nil {
			return // the end, or the time is up: the lines so far are the answer
		}
	}
}
