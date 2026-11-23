package api

import (
	"strings"
	"testing"
)

// Data model step 1 over REST: run one benchmark directly, list its runs,
// read a run's measurements and the history of a metric.
func TestRunOneBenchmarkAndReadMeasurements(t *testing.T) {
	e := benchEnv(t)
	rec := &auditRec{}
	e.api.SetAuditHook(rec)
	if st, b, _ := e.do("POST", "/api/bench/benchmarks", `{"id":"lo","name":"Load only","profile":"alpha","phases":[{"type":"load"},{"type":"unload"}]}`); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	st, b, h := e.do("POST", "/api/bench/benchmarks/lo/run", `{"note":"direct","only_when_idle":false}`)
	run := jsonOf(t, b)
	if st != 202 || run["kind"] != "benchmark" || run["benchmark_id"] != "lo" || run["suite_id"] != "" || run["suite_name"] != "Load only" ||
		h.Get("Location") != "/api/bench/runs/"+run["id"].(string) {
		t.Fatalf("run: %d %s", st, b)
	}
	id := run["id"].(string)
	eventually(t, "run completed", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		return jsonOf(t, b)["state"] == "completed"
	})
	if st, b, _ := e.do("POST", "/api/bench/benchmarks/nope/run", ""); st != 404 {
		t.Fatalf("unknown: %d %s", st, b)
	}
	if st, _, _ := e.do("POST", "/api/bench/benchmarks/lo/run", `{"nope":1}`); st != 400 {
		t.Fatalf("bad body: %d", st)
	}
	if st, b, _ := e.do("POST", "/api/bench/benchmarks/lo/run", `{"idle_wait_s":-1}`); st != 422 {
		t.Fatalf("bad idle_wait_s: %d %s", st, b)
	}
	var starts []string
	for _, c := range rec.list() {
		if c.action == "run_start" {
			starts = append(starts, c.target)
		}
	}
	if strings.Join(starts, ",") != "benchmark:lo" {
		t.Fatalf("audit %v", starts)
	}

	// listings
	if st, b, _ := e.do("GET", "/api/bench/runs?kind=benchmark&benchmark=lo", ""); st != 200 || len(jsonOf(t, b)["runs"].([]any)) != 1 {
		t.Fatalf("by kind: %d %s", st, b)
	}
	if st, b, _ := e.do("GET", "/api/bench/runs?kind=suite", ""); st != 200 || len(jsonOf(t, b)["runs"].([]any)) != 0 {
		t.Fatalf("suite runs: %d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs?kind=group", ""); st != 400 {
		t.Fatalf("bad kind: %d", st)
	}

	// a run's measurements
	st, b, _ = e.do("GET", "/api/bench/runs/"+id+"/measurements", "")
	ms := jsonOf(t, b)["measurements"].([]any)
	if st != 200 || len(ms) == 0 {
		t.Fatalf("measurements: %d %s", st, b)
	}
	st, b, _ = e.do("GET", "/api/bench/runs/"+id+"/measurements?metric=load_s&step=0", "")
	ms = jsonOf(t, b)["measurements"].([]any)
	if st != 200 || len(ms) != 1 || ms[0].(map[string]any)["unit"] != "s" || ms[0].(map[string]any)["phase"] != 0.0 {
		t.Fatalf("load_s: %d %s", st, b)
	}
	for _, c := range []struct {
		path string
		want int
	}{
		{"/api/bench/runs/" + id + "/measurements?metric=nope", 400},
		{"/api/bench/runs/" + id + "/measurements?step=x", 400},
		{"/api/bench/runs/nope/measurements", 404},
		{"/api/bench/measurements?metric=nope", 400},
		{"/api/bench/measurements?since=yesterday-ish", 400},
		{"/api/bench/measurements?limit=0", 400},
		{"/api/bench/measurements?kind=x", 400},
	} {
		if st, b, _ := e.do("GET", c.path, ""); st != c.want {
			t.Errorf("%s: %d %s, want %d", c.path, st, b, c.want)
		}
	}

	// history
	st, b, _ = e.do("GET", "/api/bench/measurements?benchmark=lo&metric=load_s&metric=unload_s&since=7d", "")
	hist := jsonOf(t, b)["measurements"].([]any)
	if st != 200 || len(hist) != 2 || hist[0].(map[string]any)["run_kind"] != "benchmark" || hist[0].(map[string]any)["release"] != "t" {
		t.Fatalf("history: %d %s", st, b)
	}
	if st, b, _ := e.do("GET", "/api/bench/measurements?profile=other", ""); st != 200 || len(jsonOf(t, b)["measurements"].([]any)) != 0 {
		t.Fatalf("profile filter: %d %s", st, b)
	}

	// the catalogue
	st, b, _ = e.do("GET", "/api/bench/metrics", "")
	if st != 200 || !strings.Contains(b, `"name":"ttft_ms"`) || !strings.Contains(b, `"name":"task_points"`) {
		t.Fatalf("metrics: %d %s", st, b)
	}
}

func TestBenchModelOff(t *testing.T) {
	e := newEnv(t, chatOK)
	for _, c := range [][2]string{{"POST", "/api/bench/benchmarks/x/run"}, {"GET", "/api/bench/runs/r/measurements"}, {"GET", "/api/bench/measurements"}, {"GET", "/api/bench/metrics"}} {
		if st, b, _ := e.do(c[0], c[1], ""); st != 503 {
			t.Errorf("%s %s: %d %s", c[0], c[1], st, b)
		}
	}
}
