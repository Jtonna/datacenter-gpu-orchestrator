package api

import (
	"path/filepath"
	"strings"
	"testing"
)

// GET /api/bench/results/models on a real run (the store's own rules are in
// internal/bench TestResultsByModel): the run's model file with its step,
// the load's seconds, and the figures it did not measure left out (a tasks
// phase is not a request phase: no tok/s, no first token). The group list
// carries task_count, the tasks a run of the group sends (the board's
// "72 tasks").
func TestBenchResultsByModelAndTaskCount(t *testing.T) {
	e := benchEnvChat(t, tasksChat)
	if st, b, _ := e.do("GET", "/api/bench/results/models", ""); st != 200 || strings.TrimSpace(b) != `{"ctx":0,"models":[]}` {
		t.Fatalf("empty: %d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/results/models?ctx=x", ""); st != 400 {
		t.Fatalf("ctx=x: %d", st)
	}
	id := runTasks(t, e)
	st, b, _ := e.do("GET", "/api/bench/results/models", "")
	if st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	models := jsonOf(t, b)["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models %s", b)
	}
	m := models[0].(map[string]any)
	lat := m["latest"].(map[string]any)
	if m["model"] == "" || lat["run_id"] != id || lat["outcome"] != "ready" || lat["cold_load_s"] == nil {
		t.Fatalf("row %s", b)
	}
	for _, k := range []string{"decode_tps", "ttft_ms", "reload_s"} {
		if _, has := lat[k]; has {
			t.Fatalf("%s given for a run that did not measure it: %s", k, b)
		}
	}
	// the test scheduler loads locally (no zones): no zone count, so no smallest fit
	if _, has := m["smallest_fit"]; has || len(m["by_nodes"].([]any)) != 0 {
		t.Fatalf("a local load has a smallest fit or zone cells: %s", b)
	}

	// the model file: in the models folder, no mark; gone from it, the row
	// says so (file_gone) and is listed after the models still here
	file := filepath.Base(m["model"].(string))
	e.api.opts.modelFiles = func() []ModelFile {
		return []ModelFile{{File: file, Exists: true}, {File: "other.gguf", Exists: true}}
	}
	_, b, _ = e.do("GET", "/api/bench/results/models", "")
	if _, has := jsonOf(t, b)["models"].([]any)[0].(map[string]any)["file_gone"]; has {
		t.Fatalf("a model still here is marked gone: %s", b)
	}
	e.api.opts.modelFiles = func() []ModelFile { return []ModelFile{{File: "other.gguf", Exists: true}, {File: file, Exists: false}} }
	_, b, _ = e.do("GET", "/api/bench/results/models", "")
	if jsonOf(t, b)["models"].([]any)[0].(map[string]any)["file_gone"] != true {
		t.Fatalf("a model whose file is gone is not marked: %s", b)
	}

	// the group's task count: the stored set's three tasks; plus an inline set of two
	_, b, _ = e.do("GET", "/api/bench/groups", "")
	if g := jsonOf(t, b)["groups"].([]any)[0].(map[string]any); g["task_count"] != float64(3) {
		t.Fatalf("task_count %v: %s", g["task_count"], b)
	}
	for _, c := range [][3]string{
		{"POST", "/api/bench/benchmarks", `{"id":"ti","profile":"alpha","phases":[{"type":"tasks","tasks":{"id":"inl","version":1,"tasks":[{"id":"a","prompt":"TASK-ONE"},{"id":"b","prompt":"TASK-TWO"}]}}]}`},
		{"POST", "/api/bench/benchmarks", `{"id":"lo","profile":"alpha","phases":[{"type":"load"},{"type":"unload"}]}`},
		{"POST", "/api/bench/suites", `{"id":"both","benchmarks":["tb","ti","lo"],"cooldown_s":0}`},
		{"POST", "/api/bench/suites", `{"id":"loads","benchmarks":["lo"],"cooldown_s":0}`},
	} {
		if st, b, _ := e.do(c[0], c[1], c[2]); st != 201 {
			t.Fatalf("%s %s: %d %s", c[0], c[1], st, b)
		}
	}
	_, b, _ = e.do("GET", "/api/bench/groups", "")
	for _, g := range jsonOf(t, b)["groups"].([]any) {
		g := g.(map[string]any)
		switch g["id"] {
		case "both":
			if g["task_count"] != float64(5) {
				t.Fatalf("both: task_count %v", g["task_count"])
			}
		case "loads":
			if _, has := g["task_count"]; has {
				t.Fatalf("a group without tasks has task_count %v", g["task_count"])
			}
		}
	}
}
