package logship

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

const testNode = "02:00:00:00:00:01"

// T0 is Potluck's time in the tests: 2026-11-21 10:00:00 UTC.
var T0 = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

type sinkReq struct {
	query  string
	header http.Header
	lines  []map[string]any
	at     time.Time
	wire   int64 // body bytes as sent
}

// sink is a fake Potluck: it takes log pushes and serves the logs object.
type sink struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []sinkReq
	answer func(n int, lines []map[string]any) (int, http.Header)
	obj    *agentproto.Logs
	gets   int
	hang   chan struct{} // when set, every push waits for it
}

func newSink(t *testing.T) *sink {
	s := &sink{t: t}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.mu.Lock()
		if s.hang != nil {
			select {
			case <-s.hang:
			default:
				close(s.hang)
			}
		}
		s.mu.Unlock()
		s.srv.Close()
	})
	return s
}

func (s *sink) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.mu.Lock()
		s.gets++
		obj := s.obj
		s.mu.Unlock()
		if obj == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(obj)
		return
	}
	s.mu.Lock()
	hang := s.hang
	s.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		z, err := gzip.NewReader(r.Body)
		if err != nil {
			s.t.Errorf("gzip: %v", err)
			return
		}
		body = z
	}
	var lines []map[string]any
	sc := bufio.NewScanner(body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			s.t.Errorf("a line is not JSON: %q", sc.Text())
		}
		lines = append(lines, m)
	}
	s.mu.Lock()
	n := len(s.reqs)
	s.reqs = append(s.reqs, sinkReq{query: r.URL.RawQuery, header: r.Header.Clone(), lines: lines, at: time.Now(), wire: r.ContentLength})
	ans := s.answer
	s.mu.Unlock()
	st, hdr := http.StatusNoContent, http.Header(nil)
	if ans != nil {
		st, hdr = ans(n, lines)
	}
	for k, v := range hdr {
		w.Header()[k] = v
	}
	w.WriteHeader(st)
}

func (s *sink) setAnswer(f func(n int, lines []map[string]any) (int, http.Header)) {
	s.mu.Lock()
	s.answer = f
	s.mu.Unlock()
}

func (s *sink) nGets() int { s.mu.Lock(); defer s.mu.Unlock(); return s.gets }

func (s *sink) requests() []sinkReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkReq(nil), s.reqs...)
}

// accepted is every line of every request answered 2xx, in order.
func (s *sink) msgs(filter func(sinkReq) bool) []map[string]any {
	var out []map[string]any
	for _, r := range s.requests() {
		if filter == nil || filter(r) {
			out = append(out, r.lines...)
		}
	}
	return out
}

func testObj(rev int64) *agentproto.Logs {
	return &agentproto.Logs{Revision: rev, URL: "/api/pool/nodes/" + testNode + "/logs", Format: agentproto.LogsFormatJSONLine,
		Limits:    agentproto.LogsLimits{MaxBatchBytes: 64 << 10, MaxLineBytes: 1024, MaxBytesPerS: 1 << 20, EveryS: 0.02},
		DenyUnits: []string{}}
}

type rig struct {
	t     *testing.T
	sink  *sink
	clock *osx.FakeClock
	ship  *Shipper
	stop  func()
}

func newRig(t *testing.T, buffer int) *rig {
	r := &rig{t: t, sink: newSink(t), clock: osx.NewFakeClock(10 * time.Second)}
	r.ship = New(Config{Base: r.sink.srv.URL, StartID: "s1", Now: r.clock.Now, BufferBytes: buffer,
		FirstPause: 50 * time.Millisecond, MaxPause: 2 * time.Second, HTTP: &http.Client{Timeout: 3 * time.Second}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.ship.Run(ctx); close(done) }()
	r.stop = func() { cancel(); <-done }
	t.Cleanup(r.stop)
	return r
}

// register gives the shipper the object and Potluck's time T0 at the
// current agent clock.
func (r *rig) register(obj *agentproto.Logs) {
	now := r.clock.Now()
	r.ship.Registered(testNode, obj, T0.UnixMilli(), now, now)
}

func (r *rig) agentLine(msg string) {
	r.ship.Add(AgentKey, Line{Mono: r.clock.Now(), Msg: msg, Level: "info"})
}

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func lineTime(t *testing.T, m map[string]any) time.Time {
	t.Helper()
	s, _ := m["_time"].(string)
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("_time %q: %v", s, err)
	}
	return tm
}

func msgOf(m map[string]any) string { s, _ := m["_msg"].(string); return s }

func hasMsg(lines []map[string]any, sub string) bool {
	for _, l := range lines {
		if strings.Contains(msgOf(l), sub) {
			return true
		}
	}
	return false
}

var _ = bytes.NewReader
