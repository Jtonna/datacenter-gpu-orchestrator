package logrelay

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// readStore is a fake log store behind -http.pathPrefix=/logs: it records
// each request and answers by path. Paths without the prefix are an error,
// as on the real store.
type readStore struct {
	srv *httptest.Server

	mu    sync.Mutex
	seen  []seenReq
	flags string
	// block, when set, holds every /logs/select/ request after its first
	// line until it is closed.
	block chan struct{}
	// queryStatus is the answer of /logs/select/logsql/query.
	queryStatus int
	queryHang   bool

	in atomic.Int32
}

type seenReq struct {
	method, path, rawQuery, body string
	form                         url.Values
	header                       http.Header
}

func newReadStore(t *testing.T) *readStore {
	s := &readStore{queryStatus: 200, flags: "-retentionPeriod=\"14d\"\n-retention.maxDiskSpaceUsageBytes=\"20GiB\"\n-http.pathPrefix=\"/logs\"\n"}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.in.Add(1)
		defer s.in.Add(-1)
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		s.mu.Lock()
		s.seen = append(s.seen, seenReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b), form, r.Header.Clone()})
		block, qs, hang, flags := s.block, s.queryStatus, s.queryHang, s.flags
		s.mu.Unlock()
		switch {
		case !strings.HasPrefix(r.URL.Path, "/logs/"):
			http.Error(w, "missing -http.pathPrefix", 400)
		case r.URL.Path == "/logs/flags":
			io.WriteString(w, flags)
		case r.URL.Path == "/logs/select/logsql/query":
			if hang {
				<-r.Context().Done()
				return
			}
			if qs != 200 {
				http.Error(w, "cannot parse query: unexpected token", qs)
				return
			}
			w.Header().Set("Content-Type", "application/stream+json")
			io.WriteString(w, `{"_msg":"one","_time":"2026-11-21T10:00:00Z"}`+"\n"+`{"_msg":"two","_time":"2026-11-21T10:00:01Z"}`+"\n")
		case strings.HasPrefix(r.URL.Path, "/logs/select/"):
			io.WriteString(w, "first line\n")
			w.(http.Flusher).Flush()
			if block != nil {
				select {
				case <-block:
				case <-r.Context().Done():
					return
				}
			}
			io.WriteString(w, "last line\n")
		default:
			io.WriteString(w, "page "+r.URL.Path)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *readStore) requests() []seenReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenReq(nil), s.seen...)
}

type readRig struct {
	relay *Relay
	front *httptest.Server
	store *readStore
	mu    sync.Mutex
	cfg   config.Logs
}

// newReadRig builds a relay in front of the fake store. The front calls the
// relay's handlers directly, without a ServeMux, so a path that is not clean
// reaches the relay as it was sent (the scheduler's mux would redirect it;
// the relay must refuse it either way).
func newReadRig(t *testing.T, storeURL func(*readStore) string, mod ...func(*Options)) *readRig {
	t.Helper()
	g := &readRig{store: newReadStore(t), cfg: config.DefaultLogs()}
	o := Options{StoreURL: storeURL(g.store), Config: func() config.Logs { g.mu.Lock(); defer g.mu.Unlock(); return g.cfg }}
	for _, m := range mod {
		m(&o)
	}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	g.relay = r
	g.front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/api/logs/query" {
			r.ServeQuery(w, q)
			return
		}
		r.ServePages(w, q)
	}))
	t.Cleanup(g.front.Close)
	return g
}

func withPrefix(s *readStore) string { return s.srv.URL + "/logs" }

func doReq(t *testing.T, method, u, body string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func reasonOf(body string) string {
	var e struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	json.Unmarshal([]byte(body), &e)
	return e.Error.Reason
}

// The reads the search page makes pass through with their path unchanged
// (the store runs with -http.pathPrefix=/logs), their query string and, for
// a POST, their body.
func TestReadProxyPassesReads(t *testing.T) {
	g := newReadRig(t, withPrefix)
	for _, c := range []struct{ method, path, body, want string }{
		{"GET", "/logs/", "", "page /logs/"},
		{"GET", "/logs/select/vmui/", "", "first line\nlast line\n"},
		{"GET", "/logs/select/vmui/assets/index.js?x=1", "", "first line\nlast line\n"},
		{"HEAD", "/logs/select/buildinfo", "", ""},
		{"POST", "/logs/select/logsql/hits", "query=error&step=1m", "first line\nlast line\n"},
		{"GET", "/logs/flags", "", "-retentionPeriod"},
		{"GET", "/logs/metrics", "", "page /logs/metrics"},
	} {
		st, body, _ := doReq(t, c.method, g.front.URL+c.path, c.body, "Content-Type", "application/x-www-form-urlencoded", "Cookie", "secret=1")
		if st != 200 || !strings.Contains(body, c.want) {
			t.Errorf("%s %s: %d %q", c.method, c.path, st, body)
		}
	}
	seen := g.store.requests()
	if len(seen) != 7 {
		t.Fatalf("the store saw %d requests, want 7", len(seen))
	}
	if s := seen[2]; s.path != "/logs/select/vmui/assets/index.js" || s.rawQuery != "x=1" {
		t.Errorf("path or query changed on the way: %+v", s)
	}
	if s := seen[4]; s.method != "POST" || s.form.Get("query") != "error" {
		t.Errorf("the POST of the search page: %+v", s)
	}
	for _, s := range seen {
		if s.header.Get("Cookie") != "" {
			t.Errorf("%s: a cookie reached the store", s.path)
		}
	}
}

// Nothing but reads reaches the store: writing, deleting, the store's
// internal and debug routes, other methods and paths that are not clean are
// all refused with 403, and the store never sees them.
func TestReadProxyRefusesWritesAndAdmin(t *testing.T) {
	g := newReadRig(t, withPrefix)
	for _, c := range []struct{ method, path string }{
		{"POST", "/logs/insert/jsonline"},
		{"GET", "/logs/insert/jsonline"},
		{"POST", "/logs/insert/elasticsearch/_bulk"},
		{"GET", "/logs/delete/run_task?filter=*"},
		{"POST", "/logs/delete/run_task"},
		{"GET", "/logs/internal/force_merge"},
		{"POST", "/logs/internal/force_flush"},
		{"POST", "/logs/internal/partition/detach"},
		{"GET", "/logs/internal/select/query"},
		{"GET", "/logs/debug/pprof/heap"},
		{"GET", "/logs/select/vmalert/api/v1/rules"},
		{"PUT", "/logs/select/logsql/query"},
		{"DELETE", "/logs/select/logsql/query"},
		{"PATCH", "/logs/select/vmui/"},
		{"POST", "/logs/metrics"},
		{"POST", "/logs/"},
		{"GET", "/logs/unknown"},
		{"GET", "/logs/select/../insert/jsonline"},
		{"POST", "/logs/select/../internal/force_merge"},
		{"POST", "/logs/select//insert/jsonline"},
		{"GET", "/logs//internal/force_merge"},
		{"GET", "/logs/select/./x"},
	} {
		st, body, _ := doReq(t, c.method, g.front.URL+c.path, "x")
		if st != http.StatusForbidden || reasonOf(body) != ReasonReadOnly {
			t.Errorf("%s %s: %d %s, want 403 %s", c.method, c.path, st, body, ReasonReadOnly)
		}
	}
	if n := len(g.store.requests()); n != 0 {
		t.Errorf("the store saw %d refused requests: %+v", n, g.store.requests())
	}
}

// Without a store address, or with logs_enabled off, the read side answers
// 404 and nothing reaches the store.
func TestReadSideOff(t *testing.T) {
	g := newReadRig(t, func(*readStore) string { return "" })
	for _, p := range []string{"/logs/select/vmui/", "/api/logs/query?query=*"} {
		if st, _, _ := doReq(t, "GET", g.front.URL+p, ""); st != 404 {
			t.Errorf("no store: %s answered %d", p, st)
		}
	}
	g2 := newReadRig(t, withPrefix)
	g2.mu.Lock()
	g2.cfg.Enabled = false
	g2.mu.Unlock()
	for _, p := range []string{"/logs/select/vmui/", "/api/logs/query?query=*"} {
		if st, body, _ := doReq(t, "GET", g2.front.URL+p, ""); st != 404 || reasonOf(body) != ReasonLogsOff {
			t.Errorf("logs_enabled off: %s answered %d %s", p, st, body)
		}
	}
	if n := len(g2.store.requests()); n != 0 {
		t.Errorf("the store saw %d requests while logging was off", n)
	}
	// on again: the same Potluck serves the page
	g2.mu.Lock()
	g2.cfg.Enabled = true
	g2.mu.Unlock()
	if st, _, _ := doReq(t, "GET", g2.front.URL+"/logs/", ""); st != 200 {
		t.Errorf("logs_enabled on again: %d", st)
	}
}

// A streaming answer (the page's live tail) passes through as the store
// writes it: the reader gets the first line while the store still holds the
// rest.
func TestReadProxyStreams(t *testing.T) {
	g := newReadRig(t, withPrefix)
	block := make(chan struct{})
	g.store.mu.Lock()
	g.store.block = block
	g.store.mu.Unlock()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", g.front.URL+"/logs/select/logsql/tail", strings.NewReader("query=*"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "first line\n" {
		t.Fatalf("the first line did not come through while the store held the rest: %q %v", line, err)
	}
	if g.relay.ReadsInFlight() != 1 {
		t.Errorf("reads in flight = %d, want 1", g.relay.ReadsInFlight())
	}
}

// At most MaxReads reads run at once; the next one is answered 429 at once,
// without waiting for a place, and a place freed is usable again.
func TestReadProxyCap(t *testing.T) {
	g := newReadRig(t, withPrefix)
	block := make(chan struct{})
	g.store.mu.Lock()
	g.store.block = block
	g.store.mu.Unlock()
	var wg sync.WaitGroup
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 64}}
	for range MaxReads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(g.front.URL + "/logs/select/logsql/tail")
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for g.store.in.Load() < MaxReads {
		if time.Now().After(deadline) {
			close(block)
			t.Fatalf("only %d reads reached the store", g.store.in.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, p := range []string{"/logs/select/vmui/", "/api/logs/query?query=*"} {
		t0 := time.Now()
		st, body, h := doReq(t, "GET", g.front.URL+p, "")
		if st != 429 || reasonOf(body) != ReasonBusyReads || h.Get("Retry-After") != "1" {
			t.Errorf("%s with %d reads running: %d %s", p, MaxReads, st, body)
		}
		if d := time.Since(t0); d > 2*time.Second {
			t.Errorf("the refusal took %v: it must not wait for a place", d)
		}
	}
	if n := g.relay.Status().ReadsInFlight; n != MaxReads {
		t.Errorf("status reads_in_flight = %d", n)
	}
	close(block)
	wg.Wait()
	if st, _, _ := doReq(t, "GET", g.front.URL+"/logs/", ""); st != 200 {
		t.Errorf("after the reads ended: %d", st)
	}
}

// GET /api/logs/query passes the query to the store's query API with a
// limit of at most MaxQueryLimit (DefaultQueryLimit when absent), and start
// and end as given; the store's lines come back as they are.
func TestQueryPassThrough(t *testing.T) {
	g := newReadRig(t, withPrefix)
	q := url.Values{"query": {`node_id:="02:00:00:00:00:01" _time:5m`}, "limit": {"50000"}, "start": {"2026-11-21T10:00:00Z"}, "end": {"1h"}}
	st, body, h := doReq(t, "GET", g.front.URL+"/api/logs/query?"+q.Encode(), "")
	if st != 200 || strings.Count(body, "\n") != 2 || !strings.Contains(body, `"_msg":"one"`) || h.Get("X-Potluck-Limit") != "10000" {
		t.Fatalf("query: %d %q %v", st, body, h)
	}
	st, _, h = doReq(t, "GET", g.front.URL+"/api/logs/query?query=error", "")
	if st != 200 || h.Get("X-Potluck-Limit") != "1000" {
		t.Fatalf("default limit: %d %v", st, h)
	}
	seen := g.store.requests()
	if len(seen) != 2 {
		t.Fatalf("the store saw %d requests", len(seen))
	}
	f := seen[0].form
	if seen[0].method != "POST" || seen[0].path != "/logs/select/logsql/query" || f.Get("query") != q.Get("query") || f.Get("limit") != "10000" ||
		f.Get("start") != "2026-11-21T10:00:00Z" || f.Get("end") != "1h" {
		t.Errorf("first query reached the store as %+v", seen[0])
	}
	if f := seen[1].form; f.Get("limit") != "1000" || f.Has("start") || f.Has("end") {
		t.Errorf("second query: %v", f)
	}
	for _, bad := range []string{"", "?query=", "?query=x&limit=0", "?query=x&limit=ten", "?query=x&start=" + strings.Repeat("9", 65)} {
		if st, body, _ := doReq(t, "GET", g.front.URL+"/api/logs/query"+bad, ""); st != 400 || reasonOf(body) != ReasonBadRequest {
			t.Errorf("%q: %d %s", bad, st, body)
		}
	}
	if n := len(g.store.requests()); n != 2 {
		t.Errorf("a refused query reached the store (%d requests)", n)
	}
	// the store refuses the query: 400 with its words
	g.store.mu.Lock()
	g.store.queryStatus = 400
	g.store.mu.Unlock()
	if st, body, _ := doReq(t, "GET", g.front.URL+"/api/logs/query?query=(", ""); st != 400 || reasonOf(body) != ReasonStoreRejected || !strings.Contains(body, "cannot parse query") {
		t.Errorf("store 400: %d %s", st, body)
	}
	g.store.mu.Lock()
	g.store.queryStatus = 500
	g.store.mu.Unlock()
	if st, body, _ := doReq(t, "GET", g.front.URL+"/api/logs/query?query=x", ""); st != 503 || reasonOf(body) != ReasonStoreUnavailable {
		t.Errorf("store 500: %d %s", st, body)
	}
}

// A query the store does not answer ends at the query timeout with 503 and
// gives its place back.
func TestQueryTimeout(t *testing.T) {
	g := newReadRig(t, withPrefix, func(o *Options) { o.QueryTimeout = 300 * time.Millisecond })
	g.store.mu.Lock()
	g.store.queryHang = true
	g.store.mu.Unlock()
	t0 := time.Now()
	st, body, _ := doReq(t, "GET", g.front.URL+"/api/logs/query?query=x", "")
	if st != 503 || reasonOf(body) != ReasonStoreUnavailable {
		t.Errorf("hung store: %d %s", st, body)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("the query took %v with a timeout of 300ms", d)
	}
	if n := g.relay.ReadsInFlight(); n != 0 {
		t.Errorf("%d reads still in flight", n)
	}
}

// The store's retention and disk cap are read from its /flags page; a
// difference from the settings is a warning in the status.
func TestStoreSettingsWarning(t *testing.T) {
	g := newReadRig(t, withPrefix)
	st := g.relay.ReadStoreFlags(context.Background())
	if st.ReadError != "" || st.RetentionDays == nil || *st.RetentionDays != 14 || st.MaxDiskGiB == nil || *st.MaxDiskGiB != 20 {
		t.Fatalf("read: %+v", st)
	}
	if s := g.relay.Status(); len(s.Warnings) != 0 || s.StoreSettings == nil {
		t.Errorf("same settings: %+v", s)
	}
	g.mu.Lock()
	g.cfg.RetentionDays, g.cfg.MaxDiskGiB = 30, 5
	g.mu.Unlock()
	ws := g.relay.Status().Warnings
	if len(ws) != 2 || !strings.Contains(ws[0], "logs_retention_days is 30") || !strings.Contains(ws[0], "14 days") ||
		!strings.Contains(ws[1], "logs_max_disk_gib is 5") || !strings.Contains(ws[1], "20 GiB") {
		t.Errorf("warnings: %q", ws)
	}
	// a store without the flags set runs with its defaults
	g.store.mu.Lock()
	g.store.flags = "-http.pathPrefix=\"/logs\"\n"
	g.store.mu.Unlock()
	st = g.relay.ReadStoreFlags(context.Background())
	if *st.RetentionDays != 7 || *st.MaxDiskGiB != 0 {
		t.Errorf("defaults: %+v", st)
	}
	// a store that cannot be read: one warning that says so
	g.store.srv.Close()
	st = g.relay.ReadStoreFlags(context.Background())
	if ws := g.relay.Status().Warnings; st.ReadError == "" || len(ws) != 1 || !strings.Contains(ws[0], "could not be read") {
		t.Errorf("unreadable: %+v %q", st, ws)
	}
}

func TestParseStoreFlagValues(t *testing.T) {
	for v, want := range map[string]float64{"14d": 14, "2w": 14, "1y": 365, "336h": 14, "1": 31, "2M": 62, "1w2d": 9} {
		if got, ok := parseRetention(v); !ok || got != want {
			t.Errorf("retention %q = %v %v, want %v", v, got, ok, want)
		}
	}
	for _, v := range []string{"", "x", "14q", "d"} {
		if _, ok := parseRetention(v); ok {
			t.Errorf("retention %q read", v)
		}
	}
	for v, want := range map[string]float64{"20GiB": 20 << 30, "20gib": 20 << 30, "1GB": 1e9, "512MiB": 512 << 20, "1024": 1024} {
		if got, ok := parseBytes(v); !ok || got != want {
			t.Errorf("bytes %q = %v %v, want %v", v, got, ok, want)
		}
	}
}
