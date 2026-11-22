package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// isolate resets the supervisor and makes an escalation fail the test.
func isolate(t *testing.T) {
	t.Helper()
	supervise.ResetForTest()
	restore := supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backoff: func(int) time.Duration { return time.Millisecond },
		Fatal:   func(r string) { t.Errorf("escalated: %s", r) }})
	t.Cleanup(func() { restore(); supervise.ResetForTest() })
}

// modelsClient dials the live channel, subscribes to the catalog and returns
// the client and the version of the full document it got.
func modelsClient(t *testing.T, e *env) (*wsc, int64) {
	t.Helper()
	w := dialWS(t, e, nil)
	w.until(t, "hello", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribe","topics":["models"]}`)); err != nil {
		t.Fatal(err)
	}
	var full struct{ Version int64 }
	if err := json.Unmarshal(w.until(t, "models", 3*time.Second).Data, &full); err != nil || full.Version < 1 {
		t.Fatalf("the snapshot: %v %+v", err, full)
	}
	return w, full.Version
}

func taskStats(name string) supervise.TaskStats {
	for _, s := range supervise.Stats() {
		if s.Name == name {
			return s
		}
	}
	return supervise.TaskStats{}
}

// The catalog rebuild loop fails in the middle of a build (after it took the
// dirty flag): it is restarted alone after its rebuild, and the next build
// still publishes the change to the live channel.
func TestCatalogLoopPanicRebuildsAndPublishes(t *testing.T) {
	isolate(t)
	st := openStore(t)
	e := histEnv(t, st)
	w, v0 := modelsClient(t, e)
	h, fired := supervise.PanicAtForTest("api.catalog.build", 1)
	defer supervise.SetHookForTest(h)()
	st.Record(history.Event{Ts: time.Now().UnixMilli(), Type: history.TLoadFinished, Severity: "info", Profile: "beta", ModelFile: "b.gguf", Nodes: []string{"host-1"},
		Data: map[string]any{"outcome": "failed", "load_s": 3.0, "reason": "x"}})
	var d struct {
		Version, Base int64
		Changed       []map[string]any
	}
	if err := json.Unmarshal(w.until(t, "models", 5*time.Second).Data, &d); err != nil {
		t.Fatal(err)
	}
	if fired() != 1 {
		t.Fatalf("the injected panic fired %d times", fired())
	}
	if d.Base != v0 || d.Version != v0+1 || len(d.Changed) != 1 || d.Changed[0]["key"] != "b.gguf" {
		t.Fatalf("after the restart: %+v", d)
	}
	if s := taskStats("api.catalog"); s.Restarts != 1 || s.Failures != 1 || s.State != "running" {
		t.Fatalf("task stats %+v", s)
	}
}

// A catalog subscriber that panics on an event (on the recording goroutine,
// the scheduler loop in production) does not stop Record; the catalog drops
// its copy of the events, reads them again from the store, and still shows
// the event.
func TestCatalogSubscriberPanicInvalidates(t *testing.T) {
	isolate(t)
	st := openStore(t)
	e := histEnv(t, st)
	w, v0 := modelsClient(t, e)
	h, fired := supervise.PanicAtForTest("api.catalog.event", 1)
	defer supervise.SetHookForTest(h)()
	before := e.api.catalog.histQueries.Load()
	st.Record(history.Event{Ts: time.Now().UnixMilli(), Type: history.TLoadFinished, Severity: "info", Profile: "beta", ModelFile: "b.gguf", Nodes: []string{"host-1"},
		Data: map[string]any{"outcome": "failed", "load_s": 3.0, "reason": "x"}})
	var d struct {
		Version, Base int64
		Changed       []map[string]any
	}
	if err := json.Unmarshal(w.until(t, "models", 5*time.Second).Data, &d); err != nil {
		t.Fatal(err)
	}
	if fired() != 1 {
		t.Fatalf("the injected panic fired %d times", fired())
	}
	if d.Version != v0+1 || len(d.Changed) != 1 || d.Changed[0]["key"] != "b.gguf" {
		t.Fatalf("after the invalidation: %+v", d)
	}
	if e.api.catalog.histQueries.Load() != before+1 {
		t.Fatalf("the events were not read again from the store")
	}
	if s := taskStats("api.catalog.event"); s.Failures != 1 || s.Policy != "guard" {
		t.Fatalf("task stats %+v", s)
	}
}

// GET /api/system lists the supervised tasks with their restarts and
// failures, and the total of recovered failures.
func TestSystemListsBackgroundTasks(t *testing.T) {
	isolate(t)
	st := openStore(t)
	e := histEnv(t, st)
	supervise.Guard("t.example", func() { panic("example failure") }, nil)
	sys := jsonOf(t, body(t, e.get("/api/system")))
	if sys["internal_failures_total"] != float64(1) {
		t.Fatalf("internal_failures_total %v", sys["internal_failures_total"])
	}
	byName := map[string]map[string]any{}
	tasks, _ := sys["tasks"].([]any)
	for _, x := range tasks {
		m := x.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if c := byName["api.catalog"]; c == nil || c["state"] != "running" || c["policy"] != "restart" || c["restarts"] != float64(0) {
		t.Fatalf("the catalog loop: %v (all: %v)", c, tasks)
	}
	if g := byName["t.example"]; g == nil || g["failures"] != float64(1) || g["failures_last_min"] != float64(1) ||
		g["last_failure"] != "example failure" || g["last_failure_at"] == nil {
		t.Fatalf("the failed task: %v", g)
	}
}

// The status loop fails once: it restarts alone, and since its rebuild
// forgets what it sent, the next tick sends every client a full status even
// though nothing changed.
func TestWSStatusLoopRestartsWithAFullStatus(t *testing.T) {
	isolate(t)
	e := newEnv(t, chatOK, func(o *Options) {
		o.WSStatusInterval = 50 * time.Millisecond
		o.WSIdleInterval = time.Hour
		o.statusSource = func(ctx context.Context) (any, error) {
			return map[string]any{"n": 1, "instances": []any{}}, nil
		}
	})
	c := dialWS(t, e, nil)
	c.until(t, "status", 3*time.Second) // the snapshot on connect
	c.collect(300 * time.Millisecond)   // the loop's first send of this content, if any
	if n := countType(c.collect(300*time.Millisecond), "status"); n != 0 {
		t.Fatalf("%d status messages for an unchanged status before the failure", n)
	}
	h, fired := supervise.PanicAtForTest("api.ws.status", 1)
	defer supervise.SetHookForTest(h)()
	c.until(t, "status", 3*time.Second)
	if fired() != 1 {
		t.Fatalf("fired %d", fired())
	}
	if s := taskStats("api.ws.status"); s.Restarts != 1 || s.State != "running" {
		t.Fatalf("task stats %+v", s)
	}
}
