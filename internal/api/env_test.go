package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// ---- fakes for the scheduler's collaborators ----

type fakeView struct{ ch chan struct{} }

func (v *fakeView) Nodes() []core.Node {
	return []core.Node{{ID: "host-1", Hostname: "host-1", Local: true, Online: true, TotalMiB: 8151, FreeMiB: 8000,
		Priority: 1, HeadroomMiB: 256}}
}
func (v *fakeView) Age() time.Duration                       { return 0 }
func (v *fakeView) Connected() bool                          { return true }
func (v *fakeView) LastReconnect() time.Time                 { return time.Time{} }
func (v *fakeView) Changed() <-chan struct{}                 { return v.ch }
func (v *fakeView) Resolve(name string) (core.NodeID, error) { return core.NodeID(name), nil }
func (v *fakeView) FetchNode(context.Context, core.NodeID) (core.Node, error) {
	return v.Nodes()[0], nil
}

var _ fleet.View = (*fakeView)(nil)

type noProber struct{}

func (noProber) Probe(context.Context, string) (instance.ProbeResult, error) {
	return instance.ProbeResult{TotalMiB: 8000, FreeMiB: 8000}, nil
}

// fakeLauncher starts "children" that are just a port: the test's upstream.
// Test-only hooks (models slice 3, delete with interrupt): while hold is
// set, a new child stays loading (WaitHealthy waits for hold to close);
// with noExit a child ignores Stop (an unload that never completes).
type fakeLauncher struct {
	port   int
	mu     sync.Mutex
	n      int
	hold   chan struct{}
	noExit bool
}

func (l *fakeLauncher) setHold(ch chan struct{}) { l.mu.Lock(); defer l.mu.Unlock(); l.hold = ch }
func (l *fakeLauncher) setNoExit(v bool)         { l.mu.Lock(); defer l.mu.Unlock(); l.noExit = v }
func (l *fakeLauncher) holdCh() chan struct{}    { l.mu.Lock(); defer l.mu.Unlock(); return l.hold }
func (l *fakeLauncher) exits() bool              { l.mu.Lock(); defer l.mu.Unlock(); return !l.noExit }

func (l *fakeLauncher) Start(spec instance.StartSpec) (instance.Child, error) {
	l.mu.Lock()
	l.n++
	l.mu.Unlock()
	argv, err := spec.Build(l.port)
	if err != nil {
		return nil, err
	}
	return &fakeChild{port: l.port, argv: argv, exited: make(chan instance.ExitStatus, 1), hold: l.holdCh(), l: l}, nil
}

type fakeChild struct {
	port   int
	argv   []string
	exited chan instance.ExitStatus
	once   sync.Once
	hold   chan struct{} // nil: healthy at once
	l      *fakeLauncher // nil: Stop always exits
}

func (c *fakeChild) Port() int      { return c.port }
func (c *fakeChild) PID() int       { return 1 }
func (c *fakeChild) Argv() []string { return c.argv }
func (c *fakeChild) WaitHealthy(ctx context.Context) error {
	if c.hold == nil {
		return nil
	}
	select {
	case <-c.hold:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.exited:
		return context.Canceled
	}
}
func (c *fakeChild) Exited() <-chan instance.ExitStatus { return c.exited }
func (c *fakeChild) Tail(int) []string                  { return nil }
func (c *fakeChild) Load() instance.LoadStats           { return instance.LoadStats{} }
func (c *fakeChild) Stop(instance.StopMode) {
	if c.l != nil && !c.l.exits() {
		return
	}
	c.once.Do(func() {
		c.exited <- instance.ExitStatus{Requested: true}
		close(c.exited)
	})
}

const testYAML = `
listen: ":0"
fleet: {url: "http://127.0.0.1:1", local_node: host-1}
models_dir: /models
defaults: {ttl: 0s, min_hold: 1ms, max_hold: 1s, stall_timeout: 0s}
tiers:
  - match: {local: true}
    priority: 1
    headroom_mib: 256
profiles:
  alpha:
    model: a.gguf
    ctx: 4096
    needs_mib: 1000
    aliases: [alpha-old]
  "beta:64k":
    model: b.gguf
    ctx: 65536
    needs_mib: 7000
  gamma:
    model: g.gguf
    ctx: 4096
    needs_mib: 500
    stall_timeout: 150ms
`

// env is a real scheduler on fakes, with the API in front of it.
type env struct {
	t        *testing.T
	cfg      *config.Config
	sched    *scheduler.Scheduler
	api      *Server
	front    *httptest.Server
	upstream *httptest.Server
	logs     *syncBuf
	nowMu    sync.Mutex
	clock    time.Time
	launcher *fakeLauncher
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// newEnv builds the env. upstream handles whatever the "children" serve.
func newEnv(t *testing.T, upstream http.Handler, mod ...func(*Options)) *env {
	t.Helper()
	cfg, err := config.ParseWithOptions([]byte(testYAML), config.Options{FileExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	port := 0
	if i := strings.LastIndex(up.URL, ":"); i > 0 {
		for _, c := range up.URL[i+1:] {
			port = port*10 + int(c-'0')
		}
	}
	e := &env{t: t, cfg: cfg, upstream: up, logs: &syncBuf{}, clock: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), launcher: &fakeLauncher{port: port}}
	opts := Options{
		Logger:       slog.New(slog.NewTextHandler(e.logs, nil)),
		Now:          func() time.Time { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.clock },
		PingInterval: 40 * time.Millisecond,
	}
	for _, m := range mod {
		m(&opts)
	}
	so := scheduler.Options{
		View: &fakeView{ch: make(chan struct{}, 1)}, Prober: noProber{}, Launcher: e.launcher,
		Needs: scheduler.NeedFunc(func(p *core.Profile) (scheduler.NeedInfo, error) {
			return scheduler.NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1 << 20}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if opts.History != nil {
		so.History = opts.History
	}
	sched, err := scheduler.New(cfg, so)
	if err != nil {
		t.Fatal(err)
	}
	e.sched = sched
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	e.api = New(cfg, sched, opts)
	e.front = httptest.NewServer(e.api)
	t.Cleanup(func() {
		e.front.CloseClientConnections()
		e.front.Close()
		cancel()
		<-done
		e.api.Close()
	})
	return e
}

func (e *env) advance(d time.Duration) {
	e.nowMu.Lock()
	e.clock = e.clock.Add(d)
	e.nowMu.Unlock()
}

func (e *env) post(path, body string) *http.Response {
	e.t.Helper()
	resp, err := http.Post(e.front.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *env) get(path string) *http.Response {
	e.t.Helper()
	resp, err := http.Get(e.front.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// eventually polls until f is true.
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	agenttest.Eventually(t, 5*time.Second, what, f)
}

var chatOK = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/props":
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"n_ctx":4096,"from":"upstream"}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"chat.completion","usage":{"prompt_tokens":5,"completion_tokens":7},"timings":{"predicted_per_second":12.5}}`)
	}
})

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

var (
	schedErr502 = scheduler.Error{Status: 502, Reason: scheduler.ReasonLoadFailed, Message: "child exited during load",
		Tail: []string{"boom line 1", "boom line 2"}}
	schedErr503 = scheduler.Error{Status: 503, Reason: scheduler.ReasonClusterBusy, Message: "no free nodes", RetryAfter: 30 * time.Second}
)
