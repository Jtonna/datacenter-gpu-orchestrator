package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
)

func benchEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, chatOK)
	store, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m, err := bench.NewManager(bench.ManagerOptions{Store: store, API: &bench.HTTPAPI{Base: e.front.URL}, Version: "t",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdlePoll: 5 * time.Millisecond, LoadPoll: 5 * time.Millisecond,
		ProfileOK: e.api.profileOK})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop(context.Background()) })
	e.api.opts.Bench, e.api.opts.BenchStore = m, store
	return e
}

func (e *env) do(method, path, body string) (int, string, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.front.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestBenchEndpointsAnswer503WhenOff(t *testing.T) {
	e := newEnv(t, chatOK)
	for _, c := range [][2]string{
		{"GET", "/api/bench/benchmarks"}, {"POST", "/api/bench/benchmarks"}, {"GET", "/api/bench/benchmarks/x"}, {"PUT", "/api/bench/benchmarks/x"},
		{"DELETE", "/api/bench/benchmarks/x"}, {"GET", "/api/bench/suites"}, {"POST", "/api/bench/suites"}, {"POST", "/api/bench/suites/x/run"},
		{"GET", "/api/bench/runs"}, {"GET", "/api/bench/runs/x"}, {"POST", "/api/bench/runs/x/cancel"}, {"GET", "/api/bench/runs/x/export"},
		{"GET", "/api/bench/trends"}, {"GET", "/api/bench/state"}, {"GET", "/api/bench/aggregates"}, {"GET", "/api/bench/schema"},
	} {
		st, b, _ := e.do(c[0], c[1], "")
		if st != 503 || !strings.Contains(b, "data_dir") {
			t.Errorf("%s %s: %d %s", c[0], c[1], st, b)
		}
	}
	if st, _, _ := e.do("GET", "/api/status", ""); st != 200 {
		t.Fatal("everything else must keep working")
	}
}

const apiBench = `{"id":"alpha-cold","name":"Alpha cold","profile":"alpha","repeats":2,"requests":[{"name":"q","prompt":"hello","max_tokens":8}]}`

func TestBenchmarkRESTValidationAndCRUD(t *testing.T) {
	e := benchEnv(t)
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"unknown profile":  {`{"id":"a","profile":"ghost","requests":[{"prompt":"x","max_tokens":1}]}`, 422},
		"bad id":           {`{"id":"A B","profile":"alpha","requests":[{"prompt":"x","max_tokens":1}]}`, 422},
		"unknown field":    {`{"id":"a","profile":"alpha","nope":1,"requests":[{"prompt":"x","max_tokens":1}]}`, 400},
		"not json":         {`{`, 400},
		"parallel missing": {`{"id":"p","parallel":["x","y"]}`, 422},
	} {
		if st, b, _ := e.do("POST", "/api/bench/benchmarks", tc.body); st != tc.want {
			t.Errorf("%s: %d %s", name, st, b)
		}
	}
	st, b, h := e.do("POST", "/api/bench/benchmarks", apiBench)
	if st != 201 || h.Get("Location") != "/api/bench/benchmarks/alpha-cold" {
		t.Fatalf("%d %s", st, b)
	}
	m := jsonOf(t, b)
	if m["source"] != "api" || m["profile"] != "alpha" || m["created_at"] == nil {
		t.Fatalf("%v", m)
	}
	if st, _, _ := e.do("POST", "/api/bench/benchmarks", apiBench); st != 409 {
		t.Fatalf("duplicate: %d", st)
	}
	if st, b, _ := e.do("PUT", "/api/bench/benchmarks/alpha-cold", strings.Replace(apiBench, `"repeats":2`, `"repeats":5`, 1)); st != 200 || jsonOf(t, b)["repeats"].(float64) != 5 {
		t.Fatalf("%d %s", st, b)
	}
	if st, _, _ := e.do("PUT", "/api/bench/benchmarks/other", apiBench); st != 404 {
		t.Fatalf("put missing: %d", st)
	}
	if st, _, _ := e.do("PUT", "/api/bench/benchmarks/alpha-cold", strings.Replace(apiBench, `"alpha-cold"`, `"zzz"`, 1)); st != 400 {
		t.Fatalf("id mismatch: %d", st)
	}
	if st, b, _ := e.do("GET", "/api/bench/benchmarks", ""); st != 200 || len(jsonOf(t, b)["benchmarks"].([]any)) != 1 {
		t.Fatalf("%d %s", st, b)
	}
	// referenced benchmarks cannot be deleted
	if st, _, _ := e.do("POST", "/api/bench/suites", `{"id":"s1","benchmarks":["alpha-cold"]}`); st != 201 {
		t.Fatal(st)
	}
	if st, b, _ := e.do("DELETE", "/api/bench/benchmarks/alpha-cold", ""); st != 409 || !strings.Contains(b, "s1") {
		t.Fatalf("%d %s", st, b)
	}
	if st, _, _ := e.do("DELETE", "/api/bench/suites/s1", ""); st != 200 {
		t.Fatal(st)
	}
	if st, _, _ := e.do("DELETE", "/api/bench/benchmarks/alpha-cold", ""); st != 200 {
		t.Fatal(st)
	}
	if st, _, _ := e.do("GET", "/api/bench/benchmarks/alpha-cold", ""); st != 404 {
		t.Fatal(st)
	}
	// suites reject unknown benchmark references and bad schedules
	if st, _, _ := e.do("POST", "/api/bench/suites", `{"id":"s2","benchmarks":["nope"]}`); st != 422 {
		t.Fatal(st)
	}
	if st, _, _ := e.do("POST", "/api/bench/suites", `{"id":"s3","benchmarks":[{"profile":"alpha","requests":[{"prompt":"x","max_tokens":1}]}],"schedule":{"every":"1s"}}`); st != 422 {
		t.Fatal(st)
	}
}

func TestSuiteRunThroughTheRealSchedulerAndExports(t *testing.T) {
	e := benchEnv(t)
	e.do("POST", "/api/bench/benchmarks", apiBench)
	if st, b, _ := e.do("POST", "/api/bench/suites", `{"id":"nightly","name":"Nightly","benchmarks":["alpha-cold"],"cooldown_s":0,"only_when_idle":false}`); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	if st, _, _ := e.do("POST", "/api/bench/suites/nightly/run", `{"bogus":1}`); st != 400 {
		t.Fatalf("strict body: %d", st)
	}
	if st, _, _ := e.do("POST", "/api/bench/suites/none/run", ""); st != 404 {
		t.Fatal(st)
	}
	st, b, h := e.do("POST", "/api/bench/suites/nightly/run", `{"note":"first"}`)
	if st != 202 {
		t.Fatalf("%d %s", st, b)
	}
	run := jsonOf(t, b)
	id := run["id"].(string)
	if run["state"] != "queued" || h.Get("Location") != "/api/bench/runs/"+id || run["note"] != "first" {
		t.Fatalf("%v", run)
	}
	var detail map[string]any
	eventually(t, "run completed", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		detail = jsonOf(t, b)
		return detail["state"] == "completed"
	})
	steps := detail["steps"].([]any)
	step := steps[0].(map[string]any)
	if step["outcome"] != "ready" || step["stats"] == nil || len(step["requests"].([]any)) != 4 { // load + warmup + 2 measured
		t.Fatalf("%v", step)
	}
	if lr := step["load"]; lr == nil {
		t.Fatalf("no load result: %v", step)
	}
	if st, _, _ := e.do("POST", "/api/bench/runs/"+id+"/cancel", ""); st != 409 {
		t.Fatalf("cancel finished run: %d", st)
	}
	st, b, _ = e.do("GET", "/api/bench/runs?suite=nightly&state=completed&limit=5", "")
	if st != 200 || len(jsonOf(t, b)["runs"].([]any)) != 1 {
		t.Fatalf("%d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs?state=weird", ""); st != 400 {
		t.Fatal(st)
	}
	st, md, hh := e.do("GET", "/api/bench/runs/"+id+"/export?format=md", "")
	if st != 200 || !strings.Contains(md, "| Benchmark | Profile |") || !strings.Contains(md, "Alpha cold") || !strings.HasPrefix(hh.Get("Content-Type"), "text/markdown") {
		t.Fatalf("%d %s", st, md)
	}
	t.Logf("markdown export:\n%s", md)
	if st, c, hh := e.do("GET", "/api/bench/runs/"+id+"/export?format=csv", ""); st != 200 || !strings.HasPrefix(c, "run_id,") || !strings.HasPrefix(hh.Get("Content-Type"), "text/csv") {
		t.Fatalf("%d %s", st, c)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs/"+id+"/export?format=json", ""); st != 200 {
		t.Fatal(st)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs/"+id+"/export?format=xml", ""); st != 400 {
		t.Fatal(st)
	}
	st, b, _ = e.do("GET", "/api/bench/trends?benchmark=alpha-cold&metric=decode_tps", "")
	if pts := jsonOf(t, b)["points"].([]any); st != 200 || len(pts) != 1 || pts[0].(map[string]any)["value"].(float64) != 12.5 {
		t.Fatalf("%d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/trends?metric=load_s", ""); st != 400 {
		t.Fatal(st)
	}
	if st, _, _ := e.do("GET", "/api/bench/trends?benchmark=a&metric=nope", ""); st != 400 {
		t.Fatal(st)
	}
	st, b, _ = e.do("GET", "/api/bench/state", "")
	sd := jsonOf(t, b)
	if st != 200 || sd["enabled"] != true || sd["queue"] == nil || sd["next_scheduled"] == nil {
		t.Fatalf("%d %s", st, b)
	}
	// the history tags what the run did
	if st, b, _ := e.do("GET", "/api/bench/profiles", ""); st != 200 || !strings.Contains(b, `"alpha"`) {
		t.Fatalf("%d %s", st, b)
	}
}

func TestProfilesEndpoint(t *testing.T) {
	e := newEnv(t, chatOK)
	st, b, _ := e.do("GET", "/api/profiles", "")
	if st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	t.Logf("GET /api/profiles -> %s", b)
	var alpha map[string]any
	for _, p := range jsonOf(t, b)["profiles"].([]any) {
		if p.(map[string]any)["id"] == "alpha" {
			alpha = p.(map[string]any)
		}
	}
	if alpha == nil || alpha["model"] != "a.gguf" || alpha["ctx"].(float64) != 4096 || alpha["needs_mib"].(float64) != 1000 ||
		alpha["needs_mib_configured"].(float64) != 1000 || alpha["needs_basis"] == "" || alpha["aliases"].([]any)[0] != "alpha-old" {
		t.Fatalf("%v", alpha)
	}
	plan := alpha["plan"].(map[string]any)
	if plan["action"] != "start" || plan["would_start"] != true || len(plan["nodes"].([]any)) != 1 {
		t.Fatalf("%v", plan)
	}
}

func TestStatusIsModelCentric(t *testing.T) {
	e := newEnv(t, chatOK)
	if r := e.post("/v1/chat/completions", chat("alpha")); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	var in map[string]any
	eventually(t, "last_request", func() bool {
		m := getJSONBody(t, e, "/api/status")
		for _, i := range m["instances"].([]any) {
			if i.(map[string]any)["last_request"] != nil {
				in = i.(map[string]any)
				return true
			}
		}
		return false
	})
	t.Logf("instance: %v", in)
	if in["profile"] != "alpha" || in["model_file"] != "a.gguf" || in["requests_served"].(float64) != 1 || in["state"] != "ready" ||
		in["ready_since"] == nil || in["last_used"] == nil || in["in_flight"].(float64) != 0 || in["load_duration"] == nil ||
		len(in["nodes"].([]any)) != 1 || in["last_request"].(map[string]any)["tokens_per_second"].(float64) != 12.5 {
		t.Fatalf("%v", in)
	}
}
