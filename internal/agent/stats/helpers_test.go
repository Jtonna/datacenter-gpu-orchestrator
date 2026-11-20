package stats

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

func parseFile(t *testing.T, name string, keep func(string) bool) (Metrics, int) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, bad, err := Parse(f, keep)
	if err != nil {
		t.Fatal(err)
	}
	return m, bad
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func parseText(t *testing.T, text string, keep func(string) bool) Metrics {
	t.Helper()
	m, bad, err := Parse(strings.NewReader(text), keep)
	if err != nil || bad != 0 {
		t.Fatalf("parse: err=%v bad=%d", err, bad)
	}
	return m
}

func okSources(names ...string) map[string]agentproto.Source {
	out := map[string]agentproto.Source{}
	for _, n := range names {
		a, s := int64(10), int64(5)
		out[n] = agentproto.Source{Status: agentproto.SourceOK, AgeMS: &a, ScrapeMS: &s}
	}
	return out
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func wantF(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: omitted, want %v", what, want)
	}
	if !approx(*got, want) {
		t.Fatalf("%s = %v, want %v", what, *got, want)
	}
}

func wantNil[T any](t *testing.T, what string, got *T) {
	t.Helper()
	if got != nil {
		t.Fatalf("%s = %v, want omitted", what, *got)
	}
}

// assertNoNull walks decoded JSON and fails on any null.
func assertNoNull(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case nil:
			t.Fatalf("null at %s in %s", path, raw)
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		}
	}
	walk("$", v)
	return v.(map[string]any)
}

// fakeClock is a hand-advanced monotonic clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Duration
}

func (c *fakeClock) Now() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t += d
	c.mu.Unlock()
}

// fakeExporter serves a settable body and advances the fake clock by cost on
// every request, so scrape durations are deterministic.
type fakeExporter struct {
	clock *fakeClock
	srv   *httptest.Server

	mu          sync.Mutex
	body        string
	status      int
	cost        time.Duration
	hang        bool          // block until the client gives up
	gate        chan struct{} // if non-nil, wait for it to close
	hits        int
	inflight    int
	maxInflight int
}

func newFakeExporter(t *testing.T, clock *fakeClock, body string) *fakeExporter {
	f := &fakeExporter{clock: clock, body: body, status: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeExporter) URL() string { return f.srv.URL + "/metrics" }

func (f *fakeExporter) set(fn func(f *fakeExporter)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

func (f *fakeExporter) stats() (hits, maxInflight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, f.maxInflight
}

func (f *fakeExporter) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits++
	f.inflight++
	if f.inflight > f.maxInflight {
		f.maxInflight = f.inflight
	}
	body, status, cost, hang, gate := f.body, f.status, f.cost, f.hang, f.gate
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()
	if f.clock != nil {
		f.clock.Advance(cost)
	}
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if hang {
		<-r.Context().Done()
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
