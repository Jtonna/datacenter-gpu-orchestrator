package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The structured load report on /running and /api/status, recent_loads, and
// the loadStarted / loadFinished events.

const (
	workerA = "192.0.2.101:50052"
	workerB = "192.0.2.122:50052"
)

type twoView struct{ fakeView }

func (v *twoView) Nodes() []core.Node {
	n := func(id, addr string) core.Node {
		return core.Node{ID: core.NodeID(id), Hostname: id, Online: true, Addr: addr, TotalMiB: 16000, FreeMiB: 15000,
			Priority: 1, HeadroomMiB: 1000, LinkMBs: 10, SharedMemory: true}
	}
	return []core.Node{n("ps5-a", workerA), n("ps5-b", workerB)}
}
func (v *twoView) FetchNode(_ context.Context, id core.NodeID) (core.Node, error) {
	for _, n := range v.Nodes() {
		if n.ID == id {
			return n, nil
		}
	}
	return core.Node{}, context.Canceled
}

// meterChild never becomes healthy and reports a fixed upload: worker A has
// received 5580799366 bytes, worker B nothing yet.
type meterChild struct{ slowChild }

func (meterChild) SentBytes([]string) (instance.SentCount, error) {
	return instance.SentCount{Bytes: 5580799366, Sockets: 1, Peers: map[string]int64{workerA: 5580799366, workerB: 0}}, nil
}

type meterLauncher struct{}

func (meterLauncher) Start(spec instance.StartSpec) (instance.Child, error) {
	argv, err := spec.Build(5998)
	if err != nil {
		return nil, err
	}
	return meterChild{slowChild{&fakeChild{port: 5998, argv: argv, exited: make(chan instance.ExitStatus, 1)}}}, nil
}

func twoWorkerEnv(t *testing.T) *env {
	t.Helper()
	cfg, err := config.ParseWithOptions([]byte(`
listen: ":0"
fleet: {url: "http://127.0.0.1:1", local_node: host-1}
models_dir: /models
defaults: {membership: fleet}
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
	sched, err := scheduler.New(cfg, scheduler.Options{
		View: &twoView{fakeView{ch: make(chan struct{}, 1)}}, Prober: noProber{}, Launcher: meterLauncher{},
		Needs: scheduler.NeedFunc(func(p *core.Profile) (scheduler.NeedInfo, error) {
			return scheduler.NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 11628266644}, nil
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
	return e
}

func pretty(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }

func TestLoadObjectOnRunningAndStatus(t *testing.T) {
	e := twoWorkerEnv(t)
	if err := e.sched.Load(context.Background(), "big"); err != nil {
		t.Fatal(err)
	}
	var running map[string]any
	eventually(t, "a measured load report with worker figures", func() bool {
		running = jsonOf(t, body(t, e.get("/running")))
		list := running["running"].([]any)
		if len(list) != 1 {
			return false
		}
		l, ok := list[0].(map[string]any)["load"].(map[string]any)
		if !ok || l["source"] != "measured" {
			return false
		}
		ws, _ := l["workers"].([]any)
		return len(ws) == 2 && ws[0].(map[string]any)["bytes_sent"] != nil
	})
	entry := running["running"].([]any)[0].(map[string]any)
	l := entry["load"].(map[string]any)
	if id, _ := l["session_id"].(string); id == "" {
		t.Fatalf("session_id missing: %v", l)
	}
	for _, k := range []string{"started_at", "elapsed_s", "bytes_total", "bytes_sent", "progress"} {
		if _, ok := l[k].(float64); !ok {
			t.Errorf("load.%s = %v, want a number", k, l[k])
		}
	}
	if l["bytes_total"] != float64(11628266644) {
		t.Errorf("load.bytes_total = %v", l["bytes_total"])
	}
	ws := l["workers"].([]any)
	w0, w1 := ws[0].(map[string]any), ws[1].(map[string]any)
	if w0["addr"] != workerA || w1["addr"] != workerB || w0["bytes_total"] != float64(5814133322) ||
		w0["bytes_sent"] != float64(5580799366) || w0["state"] != "receiving" || w1["bytes_sent"] != float64(0) || w1["state"] != "waiting" {
		t.Fatalf("workers %v", ws)
	}
	// The flat field stays.
	if _, ok := entry["load_progress"].(float64); !ok {
		t.Fatalf("load_progress is gone: %v", entry)
	}
	if rl, ok := running["recent_loads"].([]any); !ok || len(rl) != 0 {
		t.Fatalf("recent_loads = %v, want an empty list", running["recent_loads"])
	}

	st := jsonOf(t, body(t, e.get("/api/status")))
	i0 := st["instances"].([]any)[0].(map[string]any)
	sl, ok := i0["load"].(map[string]any)
	if !ok || sl["session_id"] != l["session_id"] || len(sl["workers"].([]any)) != 2 {
		t.Fatalf("status load = %v", i0["load"])
	}
	for _, k := range []string{"load_elapsed_s", "load_bytes_total", "load_bytes_sent", "load_progress", "load_eta_s", "load_progress_source"} {
		if _, ok := i0[k]; !ok {
			t.Errorf("flat field %s is gone from /api/status", k)
		}
	}
	if rl, ok := st["recent_loads"].([]any); !ok || len(rl) != 0 {
		t.Fatalf("status recent_loads = %v", st["recent_loads"])
	}
	t.Logf("/running:\n%s", pretty(running))
	t.Logf("/api/status instance load:\n%s", pretty(i0["load"]))
}

func TestRecentLoadsOnRunningAndStatus(t *testing.T) {
	e := newEnv(t, chatOK)
	run := jsonOf(t, body(t, e.get("/running")))
	if rl, ok := run["recent_loads"].([]any); !ok || len(rl) != 0 {
		t.Fatalf("recent_loads on an idle scheduler = %v", run["recent_loads"])
	}
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	var rl []any
	eventually(t, "the ready outcome", func() bool {
		run = jsonOf(t, body(t, e.get("/running")))
		rl, _ = run["recent_loads"].([]any)
		return len(rl) == 1
	})
	o := rl[0].(map[string]any)
	if o["model"] != "alpha" || o["outcome"] != "ready" || o["reason"] != "" || o["session_id"] == "" {
		t.Fatalf("outcome %v", o)
	}
	for _, k := range []string{"finished_at", "load_s"} {
		if _, ok := o[k].(float64); !ok {
			t.Errorf("%s = %v", k, o[k])
		}
	}
	if w, ok := o["workers"].([]any); !ok || len(w) != 0 {
		t.Errorf("workers = %v, want an empty list for a local load", o["workers"])
	}
	// The running entry itself is unchanged.
	r := run["running"].([]any)[0].(map[string]any)
	if _, ok := r["load"]; ok {
		t.Errorf("a ready entry has a load object: %v", r)
	}
	st := jsonOf(t, body(t, e.get("/api/status")))
	if srl, _ := st["recent_loads"].([]any); len(srl) != 1 || srl[0].(map[string]any)["session_id"] != o["session_id"] {
		t.Fatalf("status recent_loads = %v", st["recent_loads"])
	}
	t.Logf("recent_loads entry:\n%s", pretty(o))
}

func TestEventsLoadStartedAndFinished(t *testing.T) {
	e := twoWorkerEnv(t)
	c := openEvents(t, e)
	next := func(typ string) map[string]any {
		t.Helper()
		for {
			ty, d := c.parsed(5 * time.Second)
			if ty == typ {
				return d.(map[string]any)
			}
		}
	}
	if err := e.sched.Load(context.Background(), "big"); err != nil {
		t.Fatal(err)
	}
	st := next("loadStarted")
	ws, _ := st["workers"].([]any)
	if st["model"] != "big" || st["session_id"] == "" || len(ws) != 2 || ws[0] != workerA || ws[1] != workerB {
		t.Fatalf("loadStarted %v", st)
	}
	if _, ok := st["started_at"].(float64); !ok {
		t.Fatalf("loadStarted %v", st)
	}
	t.Logf("loadStarted data: %s", pretty(st))

	if err := e.sched.Unload(context.Background(), "big", true); err != nil {
		t.Fatal(err)
	}
	fin := next("loadFinished")
	if fin["session_id"] != st["session_id"] || fin["model"] != "big" || fin["outcome"] != "cancelled" || fin["reason"] == "" {
		t.Fatalf("loadFinished %v", fin)
	}
	run := jsonOf(t, body(t, e.get("/running")))
	rl := run["recent_loads"].([]any)
	if len(rl) != 1 || pretty(rl[0]) != pretty(fin) {
		t.Fatalf("loadFinished %v differs from the recent_loads entry %v", fin, rl)
	}
	t.Logf("loadFinished data: %s", pretty(fin))
}

// runningOnly is GET /running reduced to its `running` key (recent_loads is
// a sibling that changes as loads finish), as compact JSON.
func runningOnly(t *testing.T, e *env) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body(t, e.get("/running"))), &m); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]json.RawMessage{"running": m["running"]})
	return string(b)
}

// A stalled measured load shows stalled_s and leaves eta_s out.
func TestLoadObjectJSON_StalledOmitsEtaAndShowsStalledSeconds(t *testing.T) {
	total, sent, progress, stalled, src := int64(14334446752), int64(549), 0.0, 20.0, "measured"
	r := &scheduler.LoadReport{SessionID: "ld-1-1", StartedAt: 1000, ElapsedS: 732.5, BytesTotal: &total, BytesSent: &sent,
		Progress: &progress, StalledS: &stalled, Source: &src, Workers: []scheduler.LoadWorker{}}
	got := jsonView(r).(map[string]any)
	if _, has := got["eta_s"]; has {
		t.Fatalf("eta_s present: %v", got)
	}
	if got["stalled_s"] != 20.0 {
		t.Fatalf("stalled_s = %v", got["stalled_s"])
	}
	t.Logf("stalled load object:\n%s", pretty(got))
	eta := 143.0
	r.StalledS, r.EtaS = nil, &eta
	got = jsonView(r).(map[string]any)
	if _, has := got["stalled_s"]; has || got["eta_s"] != 143.0 {
		t.Fatalf("moving load: %v", got)
	}
}
