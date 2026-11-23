package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// auditRec is a recorder that remembers every call.
type auditRec struct {
	mu    sync.Mutex
	calls []auditCall
}

type auditCall struct {
	action, section, target string
	before, after           any
	method, path            string
}

func (a *auditRec) Record(r *http.Request, action, section, target string, before, after any) {
	a.mu.Lock()
	a.calls = append(a.calls, auditCall{action, section, target, before, after, r.Method, r.URL.Path})
	a.mu.Unlock()
}

func (a *auditRec) list() []auditCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]auditCall(nil), a.calls...)
}

func TestSchemaEndpointDescribesPhasesAndTemplates(t *testing.T) {
	e := benchEnv(t)
	st, b, _ := e.do("GET", "/api/bench/schema", "")
	if st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	t.Logf("GET /api/bench/schema -> %s", b)
	m := jsonOf(t, b)
	types := m["phase_types"].([]any)
	var names []string
	for _, p := range types {
		pm := p.(map[string]any)
		names = append(names, pm["type"].(string))
		if pm["description"] == "" || pm["title"] == "" || len(pm["options"].([]any)) == 0 {
			t.Errorf("%v", pm)
		}
	}
	if strings.Join(names, ",") != "load,warmup,requests,conversation,hold,concurrent_requests,tasks,unload" {
		t.Fatalf("%v", names)
	}
	hold := types[4].(map[string]any)
	var secs map[string]any
	for _, o := range hold["options"].([]any) {
		if o.(map[string]any)["name"] == "seconds" {
			secs = o.(map[string]any)
		}
	}
	if secs == nil || secs["type"] != "integer" || secs["default"].(float64) != 60 || secs["min"].(float64) != 1 || secs["max"].(float64) != 3600 || secs["unit"] != "s" || secs["description"] == "" {
		t.Fatalf("seconds option %v", secs)
	}
	if len(m["expect_keys"].([]any)) < 8 || len(m["templates"].([]any)) != 6 || m["limits"] == nil {
		t.Fatalf("%v", m)
	}
	// the load phase's cold option, the suite options and the cold, reload, loaded template
	if !strings.Contains(b, `{"name":"cold","type":"boolean","default":false,"description":"Drop the model file from the file cache`) ||
		!strings.Contains(b, `"suite_options":[`) || !strings.Contains(b, `{"name":"evict_others","type":"boolean"`) ||
		!strings.Contains(b, `"id":"cold-reload-loaded","name":"Cold, reload, loaded"`) || !strings.Contains(b, `{"type":"load","label":"cold load","cold":true}`) {
		t.Fatalf("cold, evict_others or the template missing: %s", b)
	}
	// every template can be saved as it is, once its profile is replaced
	for _, tp := range m["templates"].([]any) {
		tm := tp.(map[string]any)
		js := strings.Replace(mustJSON(t, tm["benchmark"]), "your-profile", "alpha", 1)
		if st, b, _ := e.do("POST", "/api/bench/benchmarks", js); st != 201 {
			t.Errorf("template %v: %d %s", tm["id"], st, b)
		}
	}
	if st, _, _ := e.do("GET", "/api/bench/benchmarks", ""); st != 200 {
		t.Fatal(st)
	}
}

func TestPhasedBenchmarksThroughTheRestApi(t *testing.T) {
	e := benchEnv(t)
	bad := map[string]struct{ body, want string }{
		"unknown type":  {`{"id":"a","profile":"alpha","phases":[{"type":"dance"}]}`, "is not one of load, warmup"},
		"wrong option":  {`{"id":"a","profile":"alpha","phases":[{"type":"load","seconds":5}]}`, `option \"seconds\" does not belong`},
		"mixed forms":   {`{"id":"a","profile":"alpha","repeats":3,"phases":[{"type":"load"}]}`, "phases replaces"},
		"turns missing": {`{"id":"a","profile":"alpha","phases":[{"type":"conversation"}]}`, "turns: between 1 and 64"},
		"hold request":  {`{"id":"a","profile":"alpha","phases":[{"type":"hold","seconds":5}]}`, "request is required here"},
		"ttft":          {`{"id":"a","profile":"alpha","expect":{"max_ttft_ms":10},"phases":[{"type":"load"}]}`, "max_ttft_ms needs a phase that sets stream"},
	}
	for name, tc := range bad {
		st, b, _ := e.do("POST", "/api/bench/benchmarks", tc.body)
		if st != 422 || !strings.Contains(b, tc.want) {
			t.Errorf("%s: %d %s", name, st, b)
		}
	}
	if st, b, _ := e.do("POST", "/api/bench/benchmarks", `{"id":"a","profile":"alpha","phases":[{"type":"load","bogus":1}]}`); st != 400 {
		t.Errorf("unknown field: %d %s", st, b)
	}
	loadOnly := `{"id":"load-only","name":"Load only","description":"just the load","profile":"alpha","phases":[{"type":"load"},{"type":"unload"}]}`
	st, b, _ := e.do("POST", "/api/bench/benchmarks", loadOnly)
	if st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	m := jsonOf(t, b)
	if m["description"] != "just the load" || len(m["phases"].([]any)) != 2 || m["phases"].([]any)[0].(map[string]any)["type"] != "load" {
		t.Fatalf("%v", m)
	}
	if st, _, _ := e.do("POST", "/api/bench/suites", `{"id":"lo","benchmarks":["load-only"],"cooldown_s":0,"only_when_idle":false}`); st != 201 {
		t.Fatal(st)
	}
	st, b, _ = e.do("POST", "/api/bench/suites/lo/run", "")
	if st != 202 {
		t.Fatalf("%d %s", st, b)
	}
	id := jsonOf(t, b)["id"].(string)
	var detail map[string]any
	eventually(t, "run completed", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		detail = jsonOf(t, b)
		return detail["state"] == "completed" || detail["state"] == "failed"
	})
	if detail["state"] != "completed" {
		t.Fatalf("%v", detail)
	}
	step := detail["steps"].([]any)[0].(map[string]any)
	phases := step["phases"].([]any)
	if len(phases) != 2 || phases[0].(map[string]any)["type"] != "load" || phases[0].(map[string]any)["load"] == nil || phases[1].(map[string]any)["unload"] == nil {
		t.Fatalf("%v", phases)
	}
	if reqs, _ := step["requests"].([]any); len(reqs) != 0 {
		t.Fatalf("a load-only benchmark sends no request: %v", reqs)
	}
	if step["model_file"] != "a.gguf" || detail["env"] == nil || detail["env"].(map[string]any)["scheduler_version"] != "t" {
		t.Fatalf("%v %v", step["model_file"], detail["env"])
	}
	// exports
	st, md, _ := e.do("GET", "/api/bench/runs/"+id+"/export?format=md", "")
	if st != 200 || !strings.Contains(md, "## Load only phases") || !strings.Contains(md, "| 1 | load | done |") {
		t.Fatalf("%d %s", st, md)
	}
	st, c, hh := e.do("GET", "/api/bench/runs/"+id+"/export?format=csv&detail=phases", "")
	if st != 200 || !strings.Contains(c, "run_id,suite,step_idx,benchmark,phase_idx,type") || strings.Count(c, "\n") != 3 || !strings.HasPrefix(hh.Get("Content-Type"), "text/csv") {
		t.Fatalf("%d %s", st, c)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs/"+id+"/export?format=csv&detail=bogus", ""); st != 400 {
		t.Fatal(st)
	}
	if st, _, _ := e.do("GET", "/api/bench/trends?benchmark=load-only&metric=unload_s", ""); st != 200 {
		t.Fatal(st)
	}
}

func TestAggregatesEndpoint(t *testing.T) {
	e := benchEnv(t)
	e.do("POST", "/api/bench/benchmarks", apiBench)
	e.do("POST", "/api/bench/suites", `{"id":"nightly","name":"Nightly","benchmarks":["alpha-cold"],"cooldown_s":0,"only_when_idle":false}`)
	for i := 0; i < 2; i++ {
		_, b, _ := e.do("POST", "/api/bench/suites/nightly/run", "")
		id := jsonOf(t, b)["id"].(string)
		eventually(t, "run completed", func() bool {
			_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
			return jsonOf(t, b)["state"] == "completed"
		})
	}
	st, b, _ := e.do("GET", "/api/bench/aggregates", "")
	if st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	t.Logf("GET /api/bench/aggregates -> %s", b)
	m := jsonOf(t, b)
	if m["runs_considered"].(float64) != 2 {
		t.Fatalf("%v", m)
	}
	bm := m["benchmarks"].([]any)[0].(map[string]any)
	d := bm["metrics"].(map[string]any)["decode_tps"].(map[string]any)
	if bm["benchmark_id"] != "alpha-cold" || bm["runs"].(float64) != 2 || bm["pass_rate"].(float64) != 1 || d["count"].(float64) != 2 || d["last"].(float64) != 12.5 ||
		len(d["points"].([]any)) != 2 || d["best"].(float64) != 12.5 {
		t.Fatalf("%v", bm)
	}
	if su := m["suites"].([]any)[0].(map[string]any); su["suite_id"] != "nightly" || su["runs"].(float64) != 2 || su["states"].(map[string]any)["completed"].(float64) != 2 || su["last_run"] == nil {
		t.Fatalf("%v", su)
	}
	if bn := m["by_model_nodes"].([]any); len(bn) != 1 || bn[0].(map[string]any)["model_file"] != "a.gguf" || bn[0].(map[string]any)["nodes"].(float64) < 0 {
		t.Fatalf("%v", m["by_model_nodes"])
	}
	for q, want := range map[string]float64{"suite=none": 0, "benchmark=alpha-cold&limit_runs=1": 1, "profile=alpha": 2, "env=same": 2, "since=1h": 2, "until=2001-01-01T00:00:00Z": 0} {
		st, b, _ := e.do("GET", "/api/bench/aggregates?"+q, "")
		if st != 200 || jsonOf(t, b)["runs_considered"].(float64) != want {
			t.Errorf("%s: %d %s", q, st, b)
		}
	}
	for _, q := range []string{"since=yesterday", "until=zzz", "limit_runs=0", "limit_runs=5000", "limit_runs=x", "env=other"} {
		if st, _, _ := e.do("GET", "/api/bench/aggregates?"+q, ""); st != 400 {
			t.Errorf("%s: %d", q, st)
		}
	}
}

func TestBenchStateCarriesTheProgressObject(t *testing.T) {
	e := benchEnv(t)
	st, b, _ := e.do("GET", "/api/bench/state", "")
	m := jsonOf(t, b)
	if v, ok := m["run"]; st != 200 || !ok || v != nil {
		t.Fatalf("idle: run must be present and null: %d %s", st, b)
	}
}

func TestAuditHookSeesEveryBenchmarkChange(t *testing.T) {
	e := benchEnv(t)
	rec := &auditRec{}
	e.api.SetAuditHook(rec)
	e.do("POST", "/api/bench/benchmarks", apiBench)
	e.do("PUT", "/api/bench/benchmarks/alpha-cold", strings.Replace(apiBench, `"repeats":2`, `"repeats":5`, 1))
	e.do("POST", "/api/bench/suites", `{"id":"s1","benchmarks":["alpha-cold"],"cooldown_s":0,"only_when_idle":false}`)
	e.do("PUT", "/api/bench/suites/s1", `{"id":"s1","name":"Renamed","benchmarks":["alpha-cold"],"cooldown_s":0,"only_when_idle":false}`)
	st, b, _ := e.do("POST", "/api/bench/suites/s1/run", `{"note":"audit me"}`)
	if st != 202 {
		t.Fatalf("%d %s", st, b)
	}
	id := jsonOf(t, b)["id"].(string)
	e.do("POST", "/api/bench/runs/"+id+"/cancel", "") // may be accepted or too late; both are recorded when it works
	eventually(t, "run end", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		s := jsonOf(t, b)["state"]
		return s == "completed" || s == "cancelled" || s == "failed"
	})
	// failed requests change nothing and are not recorded
	e.do("POST", "/api/bench/benchmarks", `{"id":"bad"}`)
	e.do("DELETE", "/api/bench/suites/s1", "")
	e.do("DELETE", "/api/bench/benchmarks/alpha-cold", "")
	e.do("DELETE", "/api/bench/benchmarks/ghost", "")
	var got []string
	for _, c := range rec.list() {
		if c.section != "bench" {
			t.Errorf("section %q", c.section)
		}
		got = append(got, c.action+" "+c.target)
	}
	want := []string{"create benchmark:alpha-cold", "update benchmark:alpha-cold", "create suite:s1", "update suite:s1", "run_start suite:s1"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("calls %v, want to start with %v", got, want)
		}
	}
	tail := strings.Join(got[len(want):], ",")
	if !strings.Contains(tail, "delete suite:s1") || !strings.Contains(tail, "delete benchmark:alpha-cold") || strings.Contains(tail, "ghost") || strings.Contains(tail, "bad") {
		t.Fatalf("calls %v", got)
	}
	calls := rec.list()
	if calls[0].before != nil || calls[0].after.(map[string]any)["id"] != "alpha-cold" {
		t.Fatalf("create: %+v", calls[0])
	}
	if calls[1].before.(map[string]any)["repeats"] != 2.0 || calls[1].after.(map[string]any)["repeats"] != 5.0 {
		t.Fatalf("update shows before and after: %+v", calls[1])
	}
	if a := calls[4].after.(map[string]any); a["run_id"] != id || a["note"] != "audit me" || calls[4].method != "POST" || calls[4].path != "/api/bench/suites/s1/run" {
		t.Fatalf("run_start: %+v", calls[4])
	}
	// the default hook does nothing and removing the recorder stops the calls
	e.api.SetAuditHook(nil)
	n := len(rec.list())
	e.do("POST", "/api/bench/benchmarks", apiBench)
	if len(rec.list()) != n {
		t.Fatal("a removed recorder was still called")
	}
}

func TestAuditHookSeesRunCancel(t *testing.T) {
	e := benchEnv(t)
	rec := &auditRec{}
	e.api.SetAuditHook(rec)
	e.do("POST", "/api/bench/benchmarks", `{"id":"slow","profile":"alpha","phases":[{"type":"load"},{"type":"hold","seconds":30,"request":{"prompt":"x"}}],"timeout_s":120}`)
	e.do("POST", "/api/bench/suites", `{"id":"s","benchmarks":["slow"],"only_when_idle":false}`)
	_, b, _ := e.do("POST", "/api/bench/suites/s/run", "")
	id := jsonOf(t, b)["id"].(string)
	eventually(t, "the run to be running", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		return jsonOf(t, b)["state"] == "running"
	})
	if st, b, _ := e.do("POST", "/api/bench/runs/"+id+"/cancel", ""); st != 202 {
		t.Fatalf("%d %s", st, b)
	}
	eventually(t, "cancelled", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		return jsonOf(t, b)["state"] == "cancelled"
	})
	var found *auditCall
	for _, c := range rec.list() {
		if c.action == "run_cancel" {
			c := c
			found = &c
		}
	}
	if found == nil || found.target != "run:"+id || found.before.(map[string]any)["state"] == "" || found.after.(map[string]any)["cancelling"] != id {
		t.Fatalf("%+v", found)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
