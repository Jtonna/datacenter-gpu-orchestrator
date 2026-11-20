package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Worker health on GET /api/status and the workerHealth event (design 8.4a).

// healthView is two PS5 workers whose online flag the test can flip.
type healthView struct {
	twoView
	mu      sync.Mutex
	offline map[core.NodeID]bool
	outcome map[core.NodeID]core.RecoveryOutcome
}

func (v *healthView) Nodes() []core.Node {
	v.mu.Lock()
	defer v.mu.Unlock()
	ns := v.twoView.Nodes()
	for i := range ns {
		ns[i].Online = !v.offline[ns[i].ID]
		ns[i].Recovery = v.outcome[ns[i].ID]
	}
	return ns
}

func (v *healthView) setOutcome(id core.NodeID, o core.RecoveryOutcome) {
	v.mu.Lock()
	v.outcome[id] = o
	v.mu.Unlock()
	select {
	case v.ch <- struct{}{}:
	default:
	}
}

func (v *healthView) setOffline(id core.NodeID) {
	v.mu.Lock()
	v.offline[id] = true
	v.mu.Unlock()
	select {
	case v.ch <- struct{}{}:
	default:
	}
}

// downProber fails the connect probe for the addresses in down.
type downProber struct {
	mu   sync.Mutex
	down map[string]bool
}

func (p *downProber) Connect(_ context.Context, addr string, _ time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down[addr] {
		return errors.New("connect: i/o timeout")
	}
	return nil
}

func (p *downProber) setDown(addr string) { p.mu.Lock(); p.down[addr] = true; p.mu.Unlock() }

func healthEnv(t *testing.T) (*env, *healthView, *downProber) {
	t.Helper()
	return healthEnvWith(t, "", nil)
}

// healthEnvWith is healthEnv with extra top-level YAML (say a recovery block)
// and a recovery poster.
func healthEnvWith(t *testing.T, extra string, poster scheduler.RecoveryPoster) (*env, *healthView, *downProber) {
	t.Helper()
	cfg, err := config.ParseWithOptions([]byte(`
listen: ":0"
fleet: {url: "http://127.0.0.1:1", local_node: omen}
models_dir: /models
`+extra+`
defaults: {ttl: 0s, worker_probe_interval: 20ms, worker_silence: 200ms}
tiers:
  - match: {hostname: "ps5-*"}
    priority: 1
    headroom_mib: 1000
    link_mb_s: 10
profiles:
  big:
    model: big.gguf
    ctx: 4096
    needs_mib: 5000
    force_nodes: [ps5-a, ps5-b]
    tensor_split: [1, 1]
`), config.Options{FileExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	view := &healthView{twoView: twoView{fakeView{ch: make(chan struct{}, 1)}}, offline: map[core.NodeID]bool{}, outcome: map[core.NodeID]core.RecoveryOutcome{}}
	view.twoView.fakeView.ch = view.ch
	probe := &downProber{down: map[string]bool{}}
	sched, err := scheduler.New(cfg, scheduler.Options{
		View: view, Prober: noProber{}, Launcher: &fakeLauncher{port: 5998}, WorkerProber: probe, RecoveryPoster: poster,
		Needs: scheduler.NeedFunc(func(p *core.Profile) (scheduler.NeedInfo, error) {
			return scheduler.NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1 << 20}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	e := &env{t: t, cfg: cfg, sched: sched, logs: &syncBuf{}, clock: time.Now()}
	e.api = New(cfg, sched, Options{Logger: slog.New(slog.NewTextHandler(e.logs, nil)), PingInterval: 40 * time.Millisecond})
	e.front = httptest.NewServer(e.api)
	t.Cleanup(func() {
		e.front.CloseClientConnections()
		e.front.Close()
		cancel()
		<-done
		e.api.Close()
	})
	return e, view, probe
}

func statusNode(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	st := jsonOf(t, body(t, e.get("/api/status")))
	for _, n := range st["view"].(map[string]any)["nodes"].([]any) {
		if n := n.(map[string]any); n["id"] == id {
			return n
		}
	}
	t.Fatalf("node %s not in status", id)
	return nil
}

func TestWorkerHealthStatusAndEvent(t *testing.T) {
	e, view, probe := healthEnv(t)
	c := openEvents(t, e)
	if err := e.sched.Load(context.Background(), "big"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the instance ready and both workers reported ok", func() bool {
		a, b := statusNode(t, e, "ps5-a"), statusNode(t, e, "ps5-b")
		ha, _ := a["health"].(map[string]any)
		hb, _ := b["health"].(map[string]any)
		return ha != nil && hb != nil && ha["state"] == "ok" && hb["state"] == "ok" &&
			ha["channels"].(map[string]any)["probe"] == "ok"
	})
	ok := statusNode(t, e, "ps5-b")["health"].(map[string]any)
	if _, has := ok["silent_for_s"].(map[string]any); !has || len(ok["silent_for_s"].(map[string]any)) != 0 {
		t.Fatalf("ok health %v: want an empty silent_for_s", ok)
	}

	// The worker stops answering and the machine stops reporting.
	probe.setDown(workerA)
	view.setOffline("ps5-a")

	var ev map[string]any
	for {
		typ, d := c.parsed(5 * time.Second)
		if typ != "workerHealth" {
			continue
		}
		ev = d.(map[string]any)
		if h := ev["health"].(map[string]any); ev["node"] == "ps5-a" && h["state"] == "hung" {
			break
		}
	}
	if ev["addr"] != workerA {
		t.Fatalf("event %v", ev)
	}
	t.Logf("workerHealth event data:\n%s", pretty(ev))

	h := statusNode(t, e, "ps5-a")["health"].(map[string]any)
	ch := h["channels"].(map[string]any)
	if h["state"] != "hung" || ch["heartbeat"] != "silent" || ch["probe"] != "silent" || ch["upload"] != "unknown" {
		t.Fatalf("health %v", h)
	}
	if _, ok := h["since"].(float64); !ok {
		t.Fatalf("since = %v, want unix ms", h["since"])
	}
	sf := h["silent_for_s"].(map[string]any)
	if _, ok := sf["heartbeat"].(float64); !ok || len(sf) != 2 {
		t.Fatalf("silent_for_s %v", sf)
	}
	if hb := statusNode(t, e, "ps5-b")["health"].(map[string]any); hb["state"] != "ok" {
		t.Fatalf("b health %v", hb)
	}
	t.Logf("hung node health:\n%s", pretty(h))
	// Every earlier node field is still there.
	for _, k := range []string{"id", "hostname", "online", "addr", "eligible", "quarantined", "missing_since"} {
		if _, ok := statusNode(t, e, "ps5-a")[k]; !ok {
			t.Errorf("node field %s is gone", k)
		}
	}
}

func TestWorkerHealthOmittedForANodeNoInstanceUses(t *testing.T) {
	e, _, _ := healthEnv(t)
	for _, id := range []string{"ps5-a", "ps5-b"} {
		if _, has := statusNode(t, e, id)["health"]; has {
			t.Fatalf("%s has health with no instance", id)
		}
	}
}
