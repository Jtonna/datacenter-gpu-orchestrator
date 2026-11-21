package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// A load that is still uploading to a remote worker: /api/status shows the
// load_* fields and /running gains load_progress, with its llama-swap
// fields unchanged.

type remoteView struct{ fakeView }

func (v *remoteView) Nodes() []core.Node {
	return []core.Node{{ID: "ps5-a", Hostname: "ps5-a", Online: true, Addr: "192.0.2.2:50052", TotalMiB: 16000, FreeMiB: 15000,
		Priority: 1, HeadroomMiB: 1000, LinkMBs: 10, SharedMemory: true}}
}
func (v *remoteView) FetchNode(context.Context, core.NodeID) (core.Node, error) {
	return v.Nodes()[0], nil
}

// slowChild never becomes healthy until it is stopped.
type slowChild struct{ *fakeChild }

func (c slowChild) WaitHealthy(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

type slowLauncher struct{}

func (slowLauncher) Start(spec instance.StartSpec) (instance.Child, error) {
	argv, err := spec.Build(5999)
	if err != nil {
		return nil, err
	}
	return slowChild{&fakeChild{port: 5999, argv: argv, exited: make(chan instance.ExitStatus, 1)}}, nil
}

func loadingEnv(t *testing.T) *env {
	t.Helper()
	cfg, err := config.ParseWithOptions([]byte(`
listen: ":0"
fleet: {url: "http://127.0.0.1:1", local_node: host-1}
models_dir: /models
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
`), config.Options{FileExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(cfg, scheduler.Options{
		View: &remoteView{fakeView{ch: make(chan struct{}, 1)}}, Prober: noProber{}, Launcher: slowLauncher{},
		Needs: scheduler.NeedFunc(func(p *core.Profile) (scheduler.NeedInfo, error) {
			return scheduler.NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 100_000_000}, nil
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
	e.api = New(cfg, sched, Options{Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
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

func TestLoadingInstanceShowsProgressInStatusAndRunning(t *testing.T) {
	e := loadingEnv(t)
	if err := e.sched.Load(context.Background(), "big"); err != nil {
		t.Fatal(err)
	}
	var running map[string]any
	eventually(t, "child loading with a progress figure", func() bool {
		running = jsonOf(t, body(t, e.get("/running")))
		list := running["running"].([]any)
		if len(list) != 1 {
			return false
		}
		// The structured report ("load") can appear a moment after the figure.
		m := list[0].(map[string]any)
		_, ok := m["load_progress"]
		_, rep := m["load"]
		return ok && rep
	})
	r := running["running"].([]any)[0].(map[string]any)
	keys := []string{}
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"cmd", "load", "load_progress", "model", "name", "state"}
	if len(keys) != len(want) {
		t.Fatalf("/running entry keys %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("/running entry keys %v, want %v", keys, want)
		}
	}
	if r["model"] != "big" || r["name"] != "big" || r["state"] != "starting" {
		t.Fatalf("llama-swap fields changed: %v", r)
	}
	if p, ok := r["load_progress"].(float64); !ok || p < 0 || p >= 1 {
		t.Fatalf("load_progress = %v", r["load_progress"])
	}

	st := jsonOf(t, body(t, e.get("/api/status")))
	i0 := st["instances"].([]any)[0].(map[string]any)
	if i0["state"] != "starting" {
		t.Fatalf("instance = %v", i0)
	}
	if i0["load_bytes_total"] != float64(100_000_000) || i0["load_progress_source"] != "estimated" {
		t.Fatalf("load fields: %v", i0)
	}
	for _, k := range []string{"load_elapsed_s", "load_progress", "load_eta_s"} {
		if _, ok := i0[k].(float64); !ok {
			t.Errorf("%s = %v, want a number", k, i0[k])
		}
	}
	if _, ok := i0["load_bytes_sent"]; ok {
		t.Errorf("load_bytes_sent present for an estimate: %v", i0["load_bytes_sent"])
	}
}

func TestReadyInstanceHasNoLoadFields(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	r := jsonOf(t, body(t, e.get("/running")))["running"].([]any)[0].(map[string]any)
	if _, ok := r["load_progress"]; ok {
		t.Fatalf("ready entry has load_progress: %v", r)
	}
	i0 := jsonOf(t, body(t, e.get("/api/status")))["instances"].([]any)[0].(map[string]any)
	for k := range i0 {
		if len(k) > 5 && k[:5] == "load_" && k != "load_duration" && k != "load_deadline" {
			t.Errorf("ready instance has %s", k)
		}
	}
}
