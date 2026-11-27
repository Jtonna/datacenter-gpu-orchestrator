package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// tasksChat answers like llama-server: a streamed answer that names the
// task it was asked, with a finish reason and usage.
var tasksChat = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/props" {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"n_ctx":4096}`)
		return
	}
	b, _ := io.ReadAll(r.Body)
	answer := "an answer"
	for _, id := range []string{"TASK-ONE", "TASK-TWO", "TASK-THREE"} {
		if strings.Contains(string(b), id) {
			answer = "def solve():  # " + id
		}
	}
	if !strings.Contains(string(b), `"stream":true`) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`, answer)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", answer)
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":12},\"timings\":{\"predicted_per_second\":21.5}}\n\ndata: [DONE]\n\n")
})

// benchEnvChat is benchEnv with its own model backend.
func benchEnvChat(t *testing.T, chatH http.HandlerFunc) *env {
	t.Helper()
	e := newEnv(t, chatH)
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
	e.api.opts.benchLocalOK = true // the test fleet is the scheduler's own machine
	return e
}

const apiTaskSet = `{"id":"coding","version":1,"title":"Coding","system":"You are an expert Python programmer.","max_tokens":256,
  "tasks":[{"id":"t1","prompt":"TASK-ONE"},{"id":"t2","prompt":"TASK-TWO"},{"id":"t3","prompt":"TASK-THREE"}]}`

func TestTaskSetsOff(t *testing.T) {
	e := newEnv(t, chatOK)
	for _, c := range [][2]string{{"GET", "/api/bench/task-sets"}, {"POST", "/api/bench/task-sets"}, {"GET", "/api/bench/task-sets/x"},
		{"GET", "/api/bench/runs/r/tasks"}, {"PUT", "/api/bench/runs/r/scores/k"}} {
		if st, b, _ := e.do(c[0], c[1], ""); st != 503 || !strings.Contains(b, "data_dir") {
			t.Errorf("%s %s: %d %s", c[0], c[1], st, b)
		}
	}
}

func TestTaskSetsAPI(t *testing.T) {
	e := benchEnvChat(t, tasksChat)
	rec := &auditRec{}
	e.api.SetAuditHook(rec)
	if st, b, _ := e.do("GET", "/api/bench/task-sets", ""); st != 200 || strings.TrimSpace(b) != `{"task_sets":[]}` {
		t.Fatalf("empty list: %d %s", st, b)
	}
	st, b, h := e.do("POST", "/api/bench/task-sets", apiTaskSet)
	m := jsonOf(t, b)
	if st != 201 || h.Get("Location") != "/api/bench/task-sets/coding?version=1" || len(m["sha256"].(string)) != 64 || m["created"] != true || m["format"] != "potluck-tasks/1" {
		t.Fatalf("create: %d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/task-sets", apiTaskSet); st != 200 || jsonOf(t, b)["created"] != false {
		t.Fatalf("the same set again: %d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/task-sets", strings.Replace(apiTaskSet, "TASK-TWO", "TASK-TWO changed", 1)); st != 409 || !strings.Contains(b, "new version") {
		t.Fatalf("changed text, same version: %d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/task-sets", `{"id":"x","version":1,"tasks":[]}`); st != 422 || !strings.Contains(b, "tasks") {
		t.Fatalf("invalid: %d %s", st, b)
	}
	if st, _, _ := e.do("POST", "/api/bench/task-sets", `{"id":"x","nope":1}`); st != 400 {
		t.Fatalf("unknown field: %d", st)
	}
	v2 := strings.Replace(strings.Replace(apiTaskSet, `"version":1`, `"version":2`, 1), "TASK-TWO", "TASK-TWO changed", 1)
	if st, b, _ := e.do("POST", "/api/bench/task-sets", v2); st != 201 {
		t.Fatalf("v2: %d %s", st, b)
	}
	if st, b, _ := e.do("GET", "/api/bench/task-sets/coding", ""); st != 200 || jsonOf(t, b)["version"] != 2.0 || !strings.Contains(b, "TASK-TWO changed") {
		t.Fatalf("newest: %d %s", st, b)
	}
	if st, b, _ := e.do("GET", "/api/bench/task-sets/coding?version=1", ""); st != 200 || jsonOf(t, b)["version"] != 1.0 {
		t.Fatalf("v1: %d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/task-sets/coding?version=x", ""); st != 400 {
		t.Fatalf("bad version: %d", st)
	}
	st, b, _ = e.do("GET", "/api/bench/task-sets", "")
	if st != 200 || strings.Contains(b, "TASK-ONE") || len(jsonOf(t, b)["task_sets"].([]any)) != 2 {
		t.Fatalf("list without prompts: %d %s", st, b)
	}
	if st, _, _ := e.do("DELETE", "/api/bench/task-sets/coding", ""); st != 400 {
		t.Fatalf("a delete names its version: %d", st)
	}
	if st, b, _ := e.do("DELETE", "/api/bench/task-sets/coding?version=2", ""); st != 200 {
		t.Fatalf("delete: %d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/task-sets/coding?version=2", ""); st != 404 {
		t.Fatalf("deleted: %d", st)
	}
	var targets []string
	for _, c := range rec.list() {
		targets = append(targets, c.action+" "+c.target)
	}
	if strings.Join(targets, ",") != "create task_set:coding@1,create task_set:coding@2,delete task_set:coding@2" {
		t.Fatalf("audit %v", targets)
	}
}

func TestBenchmarkMustNameAStoredTaskSet(t *testing.T) {
	e := benchEnvChat(t, tasksChat)
	body := `{"id":"tb","profile":"alpha","phases":[{"type":"tasks","task_set":"coding"}]}`
	if st, b, _ := e.do("POST", "/api/bench/benchmarks", body); st != 422 || !strings.Contains(b, `task set \"coding\" is not stored`) {
		t.Fatalf("%d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/task-sets", apiTaskSet); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/benchmarks", strings.Replace(body, `"task_set":"coding"`, `"task_set":"coding","task_set_version":4`, 1)); st != 422 || !strings.Contains(b, "version 4") {
		t.Fatalf("%d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/benchmarks", body); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	// An inline benchmark of a suite is checked the same way.
	if st, b, _ := e.do("POST", "/api/bench/suites", `{"id":"s2","benchmarks":[{"profile":"alpha","phases":[{"type":"tasks","task_set":"other"}]}]}`); st != 422 || !strings.Contains(b, `task set \"other\"`) {
		t.Fatalf("%d %s", st, b)
	}
}

// runTasks runs a suite with a tasks benchmark through the real scheduler
// and returns the run id.
func runTasks(t *testing.T, e *env) string {
	t.Helper()
	for _, c := range [][3]string{
		{"POST", "/api/bench/task-sets", apiTaskSet},
		{"POST", "/api/bench/benchmarks", `{"id":"tb","name":"Coding tasks","profile":"alpha","phases":[{"type":"load"},{"type":"tasks","task_set":"coding"},{"type":"unload"}]}`},
		{"POST", "/api/bench/suites", `{"id":"s","name":"Coding","benchmarks":["tb"],"cooldown_s":0,"only_when_idle":false}`},
	} {
		if st, b, _ := e.do(c[0], c[1], c[2]); st != 201 {
			t.Fatalf("%s %s: %d %s", c[0], c[1], st, b)
		}
	}
	_, b, _ := e.do("POST", "/api/bench/suites/s/run", "")
	id := jsonOf(t, b)["id"].(string)
	eventually(t, "run completed", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		return jsonOf(t, b)["state"] == "completed"
	})
	return id
}

func TestRunTasksAndScoresAPI(t *testing.T) {
	e := benchEnvChat(t, tasksChat)
	rec := &auditRec{}
	e.api.SetAuditHook(rec)
	id := runTasks(t, e)
	st, b, _ := e.do("GET", "/api/bench/runs/"+id+"/tasks", "")
	if st != 200 || strings.Contains(b, "def solve") {
		t.Fatalf("listing without answers: %d %s", st, b)
	}
	tasks := jsonOf(t, b)["tasks"].([]any)
	if len(tasks) != 3 {
		t.Fatalf("%s", b)
	}
	t1 := tasks[0].(map[string]any)
	if t1["task_id"] != "t1" || t1["state"] != "answered" || t1["finish_reason"] != "stop" || t1["completion_tokens"] != 12.0 || t1["prompt_tokens"] != 40.0 ||
		t1["decode_tps"] != 21.5 || t1["step"] != 0.0 || t1["phase"] != 1.0 || t1["benchmark_id"] != "tb" || t1["profile"] != "alpha" || len(t1["set_sha256"].(string)) != 64 {
		t.Fatalf("t1 %v", t1)
	}
	rid := fmt.Sprint(int64(t1["id"].(float64)))
	if st, b, _ := e.do("GET", "/api/bench/runs/"+id+"/tasks?answers=1", ""); st != 200 || !strings.Contains(b, "def solve():  # TASK-THREE") {
		t.Fatalf("with answers: %d %s", st, b)
	}
	if st, b, _ := e.do("GET", "/api/bench/runs/"+id+"/tasks?step=1", ""); st != 200 || len(jsonOf(t, b)["tasks"].([]any)) != 0 {
		t.Fatalf("another step: %d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs/"+id+"/tasks?step=x", ""); st != 400 {
		t.Fatalf("bad step: %d", st)
	}
	if st, b, _ := e.do("GET", "/api/bench/runs/"+id+"/tasks/"+rid, ""); st != 200 || jsonOf(t, b)["answer"] != "def solve():  # TASK-ONE" {
		t.Fatalf("one answer: %d %s", st, b)
	}
	st, b, h := e.do("GET", "/api/bench/runs/"+id+"/tasks/"+rid+"?format=text", "")
	if st != 200 || b != "def solve():  # TASK-ONE" || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Fatalf("as text: %d %q %v", st, b, h)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs/"+id+"/tasks/999999", ""); st != 404 {
		t.Fatalf("unknown result: %d", st)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs/nope/tasks", ""); st != 404 {
		t.Fatalf("unknown run: %d", st)
	}

	// Scores: posted by a scorer, idempotent, shown on the run.
	scores := `{"scorer_version":"score.py strict","scores":[{"step":0,"task":"t1","points":17,"max_points":17},{"step":0,"task":"t2","points":9,"max_points":17,"failed_tests":["test_x"]},{"result":` + fmt.Sprint(int64(tasks[2].(map[string]any)["id"].(float64))) + `,"points":0,"max_points":17,"notes":"no code block"}]}`
	for i := 0; i < 2; i++ {
		st, b, _ := e.do("PUT", "/api/bench/runs/"+id+"/scores/kit-tests", scores)
		tot := jsonOf(t, b)["total"].(map[string]any)
		if st != 200 || tot["points"] != 26.0 || tot["max_points"] != 51.0 || tot["tasks_scored"] != 3.0 {
			t.Fatalf("put %d: %d %s", i, st, b)
		}
	}
	st, b, _ = e.do("GET", "/api/bench/runs/"+id, "")
	if st != 200 || !strings.Contains(b, `"scorer":"kit-tests"`) || !strings.Contains(b, `"test_x"`) || strings.Contains(b, "def solve") {
		t.Fatalf("run with scores: %d %s", st, b)
	}
	if st, b, _ := e.do("GET", "/api/bench/runs/"+id+"/scores", ""); st != 200 || len(jsonOf(t, b)["scores"].([]any)) != 1 {
		t.Fatalf("scores: %d %s", st, b)
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/bench/runs/" + id + "/scores/Bad!", scores, 422},
		{"/api/bench/runs/" + id + "/scores/k", `{"scores":[{"step":0,"task":"t9","points":1,"max_points":1}]}`, 422},
		{"/api/bench/runs/" + id + "/scores/k", `{"scores":[],"extra":1}`, 400},
		{"/api/bench/runs/nope/scores/k", scores, 404},
	} {
		if st, b, _ := e.do("PUT", c.path, c.body); st != c.want {
			t.Errorf("%s: %d %s, want %d", c.path, st, b, c.want)
		}
	}
	if st, _, _ := e.do("DELETE", "/api/bench/runs/"+id+"/scores/kit-tests", ""); st != 200 {
		t.Fatalf("delete: %d", st)
	}
	if st, _, _ := e.do("DELETE", "/api/bench/runs/"+id+"/scores/kit-tests", ""); st != 404 {
		t.Fatalf("delete again: %d", st)
	}
	// The set has answers now: it is kept.
	if st, b, _ := e.do("DELETE", "/api/bench/task-sets/coding?version=1", ""); st != 409 || !strings.Contains(b, "stored answers") {
		t.Fatalf("delete a used set: %d %s", st, b)
	}
	var targets []string
	for _, c := range rec.list() {
		if strings.HasPrefix(c.target, "scores:") {
			targets = append(targets, c.action+" "+c.target)
		}
	}
	if strings.Join(targets, ",") != "update scores:"+id+"/kit-tests,update scores:"+id+"/kit-tests,delete scores:"+id+"/kit-tests" {
		t.Fatalf("audit %v", targets)
	}
}
