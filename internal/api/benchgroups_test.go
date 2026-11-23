package api

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// benchCfgEnv is a scheduler with the configuration service (so the runner
// can make profiles from a recipe's inputs) and the benchmark feature on.
func benchCfgEnv(t *testing.T) *env {
	t.Helper()
	e, _, _ := cfgEnv(t)
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

// Data model step 2 over REST: a recipe, groups inside groups with inputs
// and a matrix, the expanded preview, the run that makes the profiles, a
// direct run with inputs, and the history by inputs.
func TestGroupsOfRecipesOverREST(t *testing.T) {
	e := benchCfgEnv(t)
	mustDo := func(method, path, body string, want int) map[string]any {
		t.Helper()
		st, b, _ := e.do(method, path, body)
		if st != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, st, b, want)
		}
		return jsonOf(t, b)
	}
	mustDo("POST", "/api/bench/benchmarks", `{"id":"lo-recipe","name":"Load","inputs":{"ctx":4096},"phases":[{"type":"load"},{"type":"unload"}]}`, 201)
	mustDo("POST", "/api/bench/groups", `{"id":"inner","benchmarks":[{"benchmark":"lo-recipe","inputs":{"model":"m1.gguf"}}]}`, 201)
	mustDo("POST", "/api/bench/groups", `{"id":"outer","cooldown_s":0,"only_when_idle":false,"benchmarks":[{"group":"inner"},{"benchmark":"lo-recipe","matrix":{"model":["m2.gguf","m3.gguf"]}}]}`, 201)
	if st, b, _ := e.do("POST", "/api/bench/groups", `{"id":"bad","benchmarks":["lo-recipe"]}`); st != 422 || !strings.Contains(b, "needs the input model") {
		t.Fatalf("incomplete inputs: %d %s", st, b)
	}
	if st, b, _ := e.do("PUT", "/api/bench/groups/inner", `{"id":"inner","benchmarks":[{"group":"outer"}]}`); st != 422 || !strings.Contains(b, "contains itself") {
		t.Fatalf("cycle: %d %s", st, b)
	}
	if st, b, _ := e.do("DELETE", "/api/bench/groups/inner", ""); st != 409 || !strings.Contains(b, "outer") {
		t.Fatalf("delete a used group: %d %s", st, b)
	}
	if g := mustDo("GET", "/api/bench/groups", "", 200); len(g["groups"].([]any)) != 2 {
		t.Fatalf("groups: %v", g)
	}
	if g := mustDo("GET", "/api/bench/suites", "", 200); len(g["suites"].([]any)) != 2 {
		t.Fatalf("suites: %v", g)
	}

	ex := mustDo("GET", "/api/bench/groups/outer/expand", "", 200)
	entries := ex["entries"].([]any)
	if ex["count"] != 3.0 || len(entries) != 3 {
		t.Fatalf("expand: %v", ex)
	}
	e0 := entries[0].(map[string]any)
	if e0["recipe"] != "lo-recipe" || e0["profile"] != "bench-m1-auto-4k" || e0["inputs"].(map[string]any)["placement"] != "auto" {
		t.Fatalf("entry 0: %v", e0)
	}

	run := mustDo("POST", "/api/bench/groups/outer/run", `{"note":"group"}`, 202)
	id := run["id"].(string)
	eventually(t, "group run completed", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		return jsonOf(t, b)["state"] == "completed"
	})
	got := mustDo("GET", "/api/bench/runs/"+id, "", 200)
	var profiles []string
	for _, s := range got["steps"].([]any) {
		st := s.(map[string]any)
		if st["outcome"] != "ready" {
			t.Fatalf("step: %v", st)
		}
		profiles = append(profiles, st["profile"].(string))
	}
	if strings.Join(profiles, " ") != "bench-m1-auto-4k bench-m2-auto-4k bench-m3-auto-4k" {
		t.Fatalf("profiles: %v", profiles)
	}
	p := mustDo("GET", "/api/config/profiles/bench-m2-auto-4k", "", 200)["profile"].(map[string]any)
	if p["model"] != "m2.gguf" || p["ctx"] != 4096.0 || p["generated"] != true {
		t.Fatalf("made profile: %v", p)
	}

	// Run the recipe directly with inputs.
	if st, b, _ := e.do("POST", "/api/bench/benchmarks/lo-recipe/run", `{"only_when_idle":false}`); st != 422 || !strings.Contains(b, "needs the input model") {
		t.Fatalf("no model: %d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/benchmarks/lo-recipe/run", `{"inputs":{"model":"m4.gguf","ctx":5}}`); st != 422 || !strings.Contains(b, "inputs.ctx") {
		t.Fatalf("bad ctx: %d %s", st, b)
	}
	d := mustDo("POST", "/api/bench/benchmarks/lo-recipe/run", `{"only_when_idle":false,"inputs":{"model":"m4.gguf","ctx":8192}}`, 202)
	eventually(t, "direct run completed", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+d["id"].(string), "")
		return jsonOf(t, b)["state"] == "completed"
	})

	// History by recipe and by inputs.
	if h := mustDo("GET", "/api/bench/measurements?benchmark=lo-recipe&metric=load_s", "", 200)["measurements"].([]any); len(h) != 4 {
		t.Fatalf("history: %v", h)
	}
	h := mustDo("GET", "/api/bench/measurements?benchmark=lo-recipe&metric=load_s&ctx=8192&placement=auto", "", 200)["measurements"].([]any)
	if len(h) != 1 || h[0].(map[string]any)["model"] != "m4.gguf" || h[0].(map[string]any)["run_kind"] != "benchmark" {
		t.Fatalf("history by inputs: %v", h)
	}
	v := h[0].(map[string]any)["variant"].(string)
	if h := mustDo("GET", "/api/bench/measurements?variant="+v, "", 200)["measurements"].([]any); len(h) == 0 {
		t.Fatal("history by variant")
	}
	if st, _, _ := e.do("GET", "/api/bench/measurements?ctx=x", ""); st != 400 {
		t.Fatalf("bad ctx: %d", st)
	}
	if runs := mustDo("GET", "/api/bench/runs?benchmark=lo-recipe", "", 200)["runs"].([]any); len(runs) != 2 {
		t.Fatalf("runs of the recipe: %v", runs)
	}
}
