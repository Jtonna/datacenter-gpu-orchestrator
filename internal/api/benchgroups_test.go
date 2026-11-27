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
	e.api.opts.benchLocalOK = true // the test fleet is the scheduler's own machine
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

// Transcripts over REST: the list, the stored file and the readable chat.
func TestTranscriptsOverREST(t *testing.T) {
	e := benchCfgEnv(t)
	if st, b, _ := e.do("POST", "/api/bench/benchmarks", `{"id":"ask","inputs":{"ctx":4096,"model":"m1.gguf"},"phases":[{"type":"load"},{"type":"requests","requests":[{"name":"hi","prompt":"Say hi.","max_tokens":8}]},{"type":"unload"}]}`); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	st, b, _ := e.do("POST", "/api/bench/benchmarks/ask/run", `{"only_when_idle":false}`)
	if st != 202 {
		t.Fatalf("%d %s", st, b)
	}
	id := jsonOf(t, b)["id"].(string)
	eventually(t, "run completed", func() bool {
		_, b, _ := e.do("GET", "/api/bench/runs/"+id, "")
		return jsonOf(t, b)["state"] == "completed"
	})
	_, b, _ = e.do("GET", "/api/bench/runs/"+id, "")
	if step := jsonOf(t, b)["steps"].([]any)[0].(map[string]any); step["outcome"] != "ready" {
		t.Fatalf("step %v", step)
	}
	st, b, _ = e.do("GET", "/api/bench/runs/"+id+"/transcripts", "")
	if st != 200 || !strings.Contains(b, `"step":0`) || !strings.Contains(b, `"entries":1`) {
		t.Fatalf("list: %d %s", st, b)
	}
	st, b, _ = e.do("GET", "/api/bench/runs/"+id+"/steps/0/transcript", "")
	if st != 200 || !strings.Contains(b, `"format":"potluck-transcript/1"`) || !strings.Contains(b, `"content":"Say hi."`) {
		t.Fatalf("json: %d %s", st, b)
	}
	st, b, _ = e.do("GET", "/api/bench/runs/"+id+"/steps/0/transcript?format=md", "")
	if st != 200 || !strings.Contains(b, "> Say hi.") {
		t.Fatalf("md: %d %s", st, b)
	}
	if st, b, _ := e.do("GET", "/api/bench/runs/"+id+"/steps/5/transcript", ""); st != 404 || !strings.Contains(b, "no transcript for step 5") {
		t.Fatalf("missing: %d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/runs/nope/transcripts", ""); st != 404 {
		t.Fatalf("unknown run: %d", st)
	}

	// The run's report: a direct run's own report is its benchmark report.
	st, b, _ = e.do("GET", "/api/bench/runs/"+id+"/report", "")
	if st != 200 || jsonOf(t, b)["kind"] != "benchmark" || !strings.Contains(b, `"transcript":"/api/bench/runs/`+id+`/steps/0/transcript?format=md"`) {
		t.Fatalf("report: %d %s", st, b)
	}
	st, raw, hdr := e.hdo("GET", "/api/bench/runs/"+id+"/report?format=md", nil, nil)
	if st != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/markdown") || !strings.Contains(string(raw), "## Inputs") {
		t.Fatalf("md: %d %s", st, raw)
	}
	st, raw, hdr = e.hdo("GET", "/api/bench/runs/"+id+"/report?format=html&step=0&phase=1", nil, nil)
	if st != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") || !strings.Contains(string(raw), "Download as PDF") || !strings.Contains(string(raw), "format=md") {
		t.Fatalf("html: %d %s", st, raw)
	}
	// Pin and unpin; the removal log; the retention setting.
	if st, b, _ := e.do("PUT", "/api/bench/runs/"+id+"/pin", ""); st != 200 || !strings.Contains(b, `"pinned":true`) {
		t.Fatalf("pin: %d %s", st, b)
	}
	if _, b, _ := e.do("GET", "/api/bench/runs/"+id, ""); jsonOf(t, b)["pinned"] != true {
		t.Fatalf("run not pinned: %s", b)
	}
	if st, b, _ := e.do("DELETE", "/api/bench/runs/"+id+"/pin", ""); st != 200 || !strings.Contains(b, `"pinned":false`) {
		t.Fatalf("unpin: %d %s", st, b)
	}
	if st, _, _ := e.do("PUT", "/api/bench/runs/nope/pin", ""); st != 404 {
		t.Fatalf("pin unknown: %d", st)
	}
	if st, b, _ := e.do("GET", "/api/bench/removals", ""); st != 200 || !strings.Contains(b, `"removals":[]`) {
		t.Fatalf("removals: %d %s", st, b)
	}
	// The preview: the run finished just now, so 1 day trims nothing; it changes nothing either way.
	if st, b, _ := e.do("GET", "/api/bench/removals/preview?days=1", ""); st != 200 || !strings.Contains(b, `"count":0`) || !strings.Contains(b, `"would_trim":[]`) {
		t.Fatalf("preview: %d %s", st, b)
	}
	if st, _, _ := e.do("GET", "/api/bench/removals/preview?days=0", ""); st != 400 {
		t.Fatalf("preview days=0: %d", st)
	}
	// Off by default.
	if _, b, _ := e.do("GET", "/api/config/settings", ""); !strings.Contains(b, `"bench_retention_days":0`) {
		t.Fatalf("settings: %s", b)
	}
	if st, b, _ := e.do("PATCH", "/api/config/settings", `{"bench_retention_days":183}`); st != 200 {
		t.Fatalf("turn on: %d %s", st, b)
	}
	if st, b, _ := e.do("PATCH", "/api/config/settings", `{"bench_retention_days":0}`); st != 200 {
		t.Fatalf("turn off: %d %s", st, b)
	}
	if st, b, _ := e.do("PATCH", "/api/config/settings", `{"bench_retention_days":-3}`); st == 200 {
		t.Fatalf("negative retention accepted: %s", b)
	}
	for q, want := range map[string]int{"?step=7": 404, "?step=0&phase=9": 404, "?phase=1": 400, "?format=pdf": 400, "?step=x": 400} {
		if st, b, _ := e.do("GET", "/api/bench/runs/"+id+"/report"+q, ""); st != want {
			t.Errorf("%s: %d %s, want %d", q, st, b, want)
		}
	}
}
