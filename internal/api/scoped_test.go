package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// consolesView is the fake fleet of three consoles beside the local node.
type consolesView struct{ ch chan struct{} }

func (v *consolesView) Nodes() []core.Node {
	out := []core.Node{{ID: "host-1", Hostname: "host-1", Local: true, Online: true, TotalMiB: 8151, FreeMiB: 1200,
		Priority: 1, HeadroomMiB: 100}}
	for i, n := range []string{"con-2", "con-3", "con-4"} {
		out = append(out, core.Node{ID: core.NodeID(n), Hostname: n, Online: true, Addr: "192.0.2." + string(rune('2'+i)) + ":50052",
			TotalMiB: 16000, FreeMiB: 15000, Priority: 1, HeadroomMiB: 256})
	}
	return out
}
func (v *consolesView) Age() time.Duration       { return 0 }
func (v *consolesView) Connected() bool          { return true }
func (v *consolesView) LastReconnect() time.Time { return time.Time{} }
func (v *consolesView) Changed() <-chan struct{} { return v.ch }
func (v *consolesView) Resolve(name string) (core.NodeID, error) {
	return core.NodeID(name), nil
}
func (v *consolesView) FetchNode(_ context.Context, id core.NodeID) (core.Node, error) {
	for _, n := range v.Nodes() {
		if n.ID == id {
			return n, nil
		}
	}
	return core.Node{}, nil
}

var _ fleet.View = (*consolesView)(nil)

// newConsolesEnv is newEnv on the consoles fleet, with the request log kept
// in memory.
func newConsolesEnv(t *testing.T, upstream http.Handler) *env {
	t.Helper()
	return newConsolesEnvNeeds(t, upstream, func(p *core.Profile) (scheduler.NeedInfo, error) {
		return scheduler.NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1 << 20}, nil
	})
}

// newConsolesEnvNeeds is newConsolesEnv with the need of each profile from needs.
func newConsolesEnvNeeds(t *testing.T, upstream http.Handler, needs scheduler.NeedFunc) *env {
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
	e := &env{t: t, cfg: cfg, upstream: up, logs: &syncBuf{}, clock: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC), launcher: &fakeLauncher{port: port}}
	opts := Options{
		Logger:       slog.New(slog.NewTextHandler(e.logs, nil)),
		Now:          func() time.Time { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.clock },
		PingInterval: 40 * time.Millisecond,
		Requests:     usage.NewRecorder(nil),
	}
	sched, err := scheduler.New(cfg, scheduler.Options{
		View: &consolesView{ch: make(chan struct{}, 1)}, Prober: noProber{}, Launcher: e.launcher,
		Needs: needs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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

func instancesOf(t *testing.T, e *env) map[string]map[string]any {
	t.Helper()
	var st struct {
		Instances []map[string]any `json:"instances"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/status"))), &st); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, in := range st.Instances {
		out[in["profile"].(string)] = in
	}
	return out
}

// Proof 5 with the fakes: a scoped load at the production shape on consoles
// production is not using, while a production request is being served:
// both answer, and neither is admitted to the other's instance. The request
// log names the production id for both; the scoped one keeps its name as
// the requested model.
func TestScopedLoad_BesideAServedProductionRequest(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 4)
	e := newConsolesEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct{ Model string }
		json.Unmarshal(b, &m)
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			started <- m.Model
			if m.Model == "alpha" {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	var wg sync.WaitGroup
	prodStatus := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(e.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(chat("alpha")))
		if err == nil {
			prodStatus = resp.StatusCode
			resp.Body.Close()
		}
	}()
	if m := <-started; m != "alpha" {
		t.Fatalf("first request %q", m)
	}
	prod := instancesOf(t, e)["alpha"]
	prodNodes, _ := json.Marshal(prod["nodes"])
	if strings.Contains(string(prodNodes), "con-4") {
		t.Fatalf("production placed on con-4: %s", prodNodes)
	}

	// The plan, then the load, of the scoped profile on con-4.
	if b := status(t, e.post("/api/plan", `{"model":"alpha","scope":"run1","nodes":["con-4"]}`), 200); !strings.Contains(b, `"action":"start"`) ||
		!strings.Contains(b, `"model":"alpha@run1"`) {
		t.Fatalf("plan %s", b)
	}
	b := status(t, e.post("/api/load", `{"model":"alpha","scope":"run1","nodes":["con-4"]}`), http.StatusAccepted)
	if !strings.Contains(b, `"model":"alpha@run1"`) || !strings.Contains(b, `"production":"alpha"`) || !strings.Contains(b, `"scope":"run1"`) {
		t.Fatalf("load %s", b)
	}
	eventually(t, "scoped ready", func() bool {
		in := instancesOf(t, e)["alpha@run1"]
		return in != nil && in["state"] == "ready"
	})
	scopedNodes, _ := json.Marshal(instancesOf(t, e)["alpha@run1"]["nodes"])
	if !strings.Contains(string(scopedNodes), "con-4") {
		t.Fatalf("scoped nodes %s", scopedNodes)
	}
	// The scoped request answers while production's is still in flight.
	req, _ := http.NewRequest("POST", e.front.URL+"/v1/chat/completions", strings.NewReader(chat("alpha@run1")))
	req.Header.Set("X-Bench-Run", "run1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("scoped request %d %s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	ins := instancesOf(t, e)
	if ins["alpha"]["in_flight"].(float64) != 1 || ins["alpha@run1"]["in_flight"].(float64) != 0 {
		t.Fatalf("in flight: production %v, scoped %v", ins["alpha"]["in_flight"], ins["alpha@run1"]["in_flight"])
	}
	if ins["alpha"]["instance_id"] != nil && ins["alpha"]["instance_id"] == ins["alpha@run1"]["instance_id"] {
		t.Fatal("one instance")
	}
	// The live request log: production's row names alpha on production's
	// instance.
	live := e.api.reqs.Live()
	if len(live) != 1 || live[0].Profile != "alpha" || live[0].Requested != "alpha" {
		t.Fatalf("live rows %+v", live)
	}
	prodInstance := live[0].Instance
	// A second production request goes to production's instance, never to
	// the scoped one (which is idle and would take it at once).
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(e.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(chat("alpha")))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	eventually(t, "two production rows", func() bool { return len(e.api.reqs.Live()) == 2 })
	for _, r := range e.api.reqs.Live() {
		if r.Instance != prodInstance || r.Profile != "alpha" {
			t.Fatalf("production request on instance %d (production's is %d): %+v", r.Instance, prodInstance, r)
		}
	}
	close(release)
	wg.Wait()
	if prodStatus != 200 {
		t.Fatalf("production request %d", prodStatus)
	}
	// Unload the scoped profile: it is gone, and its name answers 404.
	status(t, e.post("/api/unload", `{"model":"alpha@run1"}`), 200)
	eventually(t, "scoped gone", func() bool { return instancesOf(t, e)["alpha@run1"] == nil })
	eventually(t, "scoped name forgotten", func() bool {
		_, ok := e.sched.Scoped("alpha@run1")
		return !ok
	})
	resp = e.post("/v1/chat/completions", chat("alpha@run1"))
	if b := body(t, resp); resp.StatusCode != 404 || !strings.Contains(b, "model_not_found") && !strings.Contains(b, "not found") {
		t.Fatalf("after the scope ended: %d %s", resp.StatusCode, b)
	}
	// The configuration never held it.
	if b := body(t, e.get("/api/profiles")); strings.Contains(b, "alpha@run1") {
		t.Fatal("the scoped profile is listed as a configured profile")
	}
}

// Bodies: a bad scope and settings are 400; an unknown production model 404;
// a scoped load is audited with its scope and settings.
func TestScopedLoad_Refusals(t *testing.T) {
	e := newConsolesEnv(t, chatOK)
	status(t, e.post("/api/load", `{"model":"alpha","scope":"bad scope"}`), http.StatusBadRequest)
	status(t, e.post("/api/load", `{"model":"alpha","scope":"s","kv_quant":"q8_0"}`), http.StatusBadRequest)
	status(t, e.post("/api/plan", `{"model":"alpha","scope":"s","ctx":0}`), http.StatusBadRequest)
	status(t, e.post("/api/load", `{"model":"nope","scope":"s"}`), http.StatusNotFound)
	status(t, e.post("/api/load", `{"model":"alpha","scope":"s","nodes":["nowhere"]}`), http.StatusBadRequest)
	status(t, e.post("/api/load", `{"model":"alpha","scope":"s","ctx":8192,"nodes":["con-3"]}`), http.StatusAccepted)
	// the same name with other settings
	status(t, e.post("/api/load", `{"model":"alpha","scope":"s","ctx":2048,"nodes":["con-3"]}`), http.StatusConflict)
	// alias of the production profile: the same scoped name
	status(t, e.post("/api/load", `{"model":"alpha-old","scope":"s","ctx":8192,"nodes":["con-3"]}`), http.StatusAccepted)
	// a plain load of a scoped name attaches to it
	status(t, e.post("/api/load", `{"model":"alpha@s"}`), http.StatusAccepted)
	status(t, e.post("/api/load", `{"model":"alpha@other"}`), http.StatusNotFound)
}

// The request log row of a scoped request names the production id, keeps
// the scoped name as the requested model, and the bench run.
func TestScopedLoad_RequestLogRow(t *testing.T) {
	e := newConsolesEnv(t, chatOK)
	status(t, e.post("/api/load", `{"model":"alpha","scope":"r7","ctx":8192,"nodes":["con-2"]}`), http.StatusAccepted)
	eventually(t, "held", func() bool { _, ok := e.sched.Scoped("alpha@r7"); return ok })
	p, _, ok := e.api.resolveModel("alpha@r7")
	if !ok || p.Name != "alpha@r7" || p.Ctx != 8192 {
		t.Fatalf("resolve %+v %v", p, ok)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	tr := e.api.trackRequest(r, "/v1/chat/completions", usage.KindV1, "alpha@r7", p, "r7")
	live := e.api.reqs.Live()
	if len(live) != 1 || live[0].Profile != "alpha" || live[0].Requested != "alpha@r7" || live[0].BenchRun != "r7" || live[0].Ctx != 8192 {
		t.Fatalf("row %+v", live)
	}
	tr.Ended(usage.End{Status: usage.StatusDone})
}

// A scoped instance cut by a forced unload gives its caller the same 503
// "unloaded" answer as any instance; production's request in flight beside
// it is not cut by it.
func TestScopedLoad_CutByForcedUnload(t *testing.T) {
	u := &cutUpstream{started: make(chan struct{}, 4)}
	e := newConsolesEnv(t, u)
	body(t, e.post("/v1/chat/completions", chat("alpha"))) // loads production
	status(t, e.post("/api/load", `{"model":"alpha","scope":"cut1","nodes":["con-3"]}`), http.StatusAccepted)
	eventually(t, "scoped ready", func() bool {
		in := instancesOf(t, e)["alpha@cut1"]
		return in != nil && in["state"] == "ready"
	})
	prod := sendCut(t, e.front.URL, `{"model":"alpha","messages":[{"role":"user","content":"hold"}]}`)
	waitStarted(t, u)
	scoped := sendCut(t, e.front.URL, `{"model":"alpha@cut1","messages":[{"role":"user","content":"hold"}]}`)
	waitStarted(t, u)
	body(t, e.post("/api/unload", `{"model":"alpha@cut1","force":true}`))
	a := waitCut(t, scoped)
	if a.status != 503 || a.err["reason"] != "unloaded" || a.err["message"] != "unloaded: alpha@cut1 was unloaded" || a.retryAfter == "" {
		t.Fatalf("scoped cut: %d %q %s", a.status, a.retryAfter, a.body)
	}
	select {
	case p := <-prod:
		t.Fatalf("production's request ended with the scoped unload: %d %s", p.status, p.body)
	case <-time.After(300 * time.Millisecond):
	}
	if in := instancesOf(t, e)["alpha"]; in == nil || in["in_flight"].(float64) != 1 {
		t.Fatalf("production instance %v", in)
	}
	body(t, e.post("/api/unload", `{"model":"alpha","force":true}`))
	if p := waitCut(t, prod); p.status != 503 || p.err["message"] != "unloaded: alpha was unloaded" {
		t.Fatalf("production cut: %d %s", p.status, p.body)
	}
}

// The model file of a scoped load is always the production profile's: a
// body that names one of its own (dotted, absolute, or any other field this
// version does not know) is refused 400, never ignored; a path given as the
// model is no profile (404).
func TestScopedLoad_NoModelPathOverride(t *testing.T) {
	e := newConsolesEnv(t, chatOK)
	for _, b := range []string{
		`{"model":"alpha","scope":"p","model_file":"../../etc/passwd"}`,
		`{"model":"alpha","scope":"p","model_path":"/abs/x.gguf"}`,
		`{"model":"alpha","scope":"p","ctx":8192,"file":".hidden/x.gguf"}`,
	} {
		if b := status(t, e.post("/api/load", b), http.StatusBadRequest); !strings.Contains(b, "unknown field") {
			t.Fatalf("%s", b)
		}
		status(t, e.post("/api/plan", b), http.StatusBadRequest)
	}
	for _, m := range []string{"../a.gguf", "/models/a.gguf", ".x/a.gguf"} {
		status(t, e.post("/api/load", `{"model":"`+m+`","scope":"p"}`), http.StatusNotFound)
	}
	if _, ok := e.sched.Scoped("alpha@p"); ok {
		t.Fatal("a refused body left a scoped profile")
	}
}

// The caller's check (bench.HTTPAPI.LoadScoped) passes against this
// scheduler's real answer.
func TestScopedLoad_CallerCheckPassesOnThisImage(t *testing.T) {
	e := newConsolesEnv(t, chatOK)
	ctx := 8192
	ans, err := (&bench.HTTPAPI{Base: e.front.URL}).LoadScoped(context.Background(),
		bench.ScopedLoadRequest{Model: "alpha-old", Scope: "chk", Nodes: []string{"con-2"}, Ctx: &ctx})
	if err != nil || ans.Status != 202 || ans.Model != "alpha@chk" || ans.Production != "alpha" || ans.Scope != "chk" {
		t.Fatalf("%+v %v", ans, err)
	}
	if _, ok := e.sched.Scoped("alpha@chk"); !ok {
		t.Fatal("not held")
	}
}

// A scoped plan and load on a named console are fit-checked there whatever
// defaults.forced_fit_check says (here off): at 4 x ctx the need (the
// estimate, the written need not being used at that shape) does not fit
// con-4, so the plan answers reject with the figures, the load answers 503
// with them, and nothing is held. Production alpha is not touched.
func TestScoped_NamedConsoleFitCheckedOverTheRoutes(t *testing.T) {
	e := newConsolesEnvNeeds(t, http.NotFoundHandler(), func(p *core.Profile) (scheduler.NeedInfo, error) {
		if p.NeedsMiB > 0 {
			return scheduler.NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1 << 20}, nil
		}
		return scheduler.NeedInfo{Need: placer.Estimated(p.Ctx), FileBytes: 1 << 20}, nil // 1 MiB a token: 16384 at 4 x 4096
	})
	if e.cfg.Defaults.ForcedFitCheck != "" && e.cfg.Defaults.ForcedFitCheck != "off" {
		t.Fatalf("forced_fit_check %q in the test config", e.cfg.Defaults.ForcedFitCheck)
	}
	body := `{"model":"alpha","scope":"big","nodes":["con-4"],"ctx":16384}`
	b := status(t, e.post("/api/plan", body), 200)
	for _, want := range []string{`"action":"reject"`, `"model":"alpha@big"`, "a scoped load is always fit-checked on the zones it names", "MiB usable", "written need not used: shape differs from alpha"} {
		if !strings.Contains(b, want) {
			t.Fatalf("plan lacks %q: %s", want, b)
		}
	}
	b = status(t, e.post("/api/load", body), http.StatusServiceUnavailable)
	for _, want := range []string{"alpha@big", "a scoped load is always fit-checked on the zones it names", "need 17204 MiB", "15000 MiB usable"} {
		if !strings.Contains(b, want) {
			t.Fatalf("load answer lacks %q: %s", want, b)
		}
	}
	if len(instancesOf(t, e)) != 0 {
		t.Fatalf("instances after a refused scoped load: %v", instancesOf(t, e))
	}
	if _, ok := e.sched.Scoped("alpha@big"); ok {
		t.Fatal("a refused scoped load is held")
	}
}
