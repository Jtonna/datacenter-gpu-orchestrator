package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/access"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// Benchmarks slice 2 backend (B1, B2, B3, B12 and the pin permission): the
// facts the run, group and benchmark boards draw, read-only.

// withBench gives an env a bench manager and store (cfgEnv has keys, benchEnv has not).
func withBench(t *testing.T, e *env) *bench.Store {
	t.Helper()
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
	return store
}

// seedRuns stores finished runs by hand: nothing runs.
func seedRuns(t *testing.T, st *bench.Store) {
	t.Helper()
	add := func(id, suite, state string, at int64, outcomes ...string) {
		if err := st.CreateRun(&bench.Run{ID: id, SuiteID: suite, SuiteName: suite, State: state, QueuedAt: at, FinishedAt: at + 5}); err != nil {
			t.Fatal(err)
		}
		for i, o := range outcomes {
			s := &bench.Step{RunID: id, Idx: i, BenchmarkID: "b" + string(rune('0'+i)), State: bench.StepDone, Outcome: o, StartedAt: at + 1, FinishedAt: at + 2001,
				ModelFile: "lab_tiny-1b-Q4_K_M.gguf", NodeCount: 2}
			if err := st.AddStep(s); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("run-1", "g", bench.RunCompleted, 100, bench.OutcomeReady, bench.OutcomeReady)
	add("run-2", "g", bench.RunCompleted, 200, bench.OutcomeReady, bench.OutcomeFailed)
	add("run-3", "h", bench.RunCancelled, 300, bench.OutcomeReady, bench.OutcomeCancelled)
	add("run-4", "g", bench.RunCompleted, 400, bench.OutcomeRefused)
}

func TestBenchSlice2_RunListFactsPagingAndResultFilter(t *testing.T) {
	e := benchEnv(t)
	seedRuns(t, e.api.opts.BenchStore)
	get := func(path string, want int) map[string]any {
		t.Helper()
		st, b, _ := e.do("GET", path, "")
		if st != want {
			t.Fatalf("%s: %d %s", path, st, b)
		}
		var m map[string]any
		json.Unmarshal([]byte(b), &m)
		return m
	}
	m := get("/api/bench/runs?limit=2", 200)
	rows := m["runs"].([]any)
	if m["total"] != 4.0 || m["offset"] != 0.0 || len(rows) != 2 {
		t.Fatalf("page 1: %v", m)
	}
	r4 := rows[0].(map[string]any)
	if r4["id"] != "run-4" || r4["number"] != 4.0 || r4["result"] != "with_issues" {
		t.Fatalf("row: %v", r4)
	}
	c := r4["counts"].(map[string]any)
	if c["refused"] != 1.0 || c["done"] != 1.0 || c["to_go"] != 0.0 {
		t.Fatalf("counts: %v", c)
	}
	if r4["cold"].(map[string]any)["state"] != "unknown" {
		t.Fatalf("cold: %v", r4["cold"])
	}
	m = get("/api/bench/runs?limit=2&offset=2", 200)
	if rows := m["runs"].([]any); m["total"] != 4.0 || len(rows) != 2 || rows[1].(map[string]any)["id"] != "run-1" || rows[1].(map[string]any)["result"] != "passed" {
		t.Fatalf("page 2: %v", m)
	}
	m = get("/api/bench/runs?result=has_failed", 200)
	if rows := m["runs"].([]any); m["total"] != 1.0 || rows[0].(map[string]any)["id"] != "run-2" {
		t.Fatalf("has_failed: %v", m)
	}
	m = get("/api/bench/runs?result=cancelled", 200)
	if rows := m["runs"].([]any); m["total"] != 1.0 || rows[0].(map[string]any)["id"] != "run-3" {
		t.Fatalf("cancelled: %v", m)
	}
	for _, bad := range []string{"result=bogus", "offset=-1", "offset=x"} {
		get("/api/bench/runs?"+bad, 400)
	}

	// a run by its number; ?requests=0 leaves the request rows out; every step has its slug
	one := get("/api/bench/runs/2", 200)
	if one["id"] != "run-2" || one["number"] != 2.0 || one["result"] != "with_issues" {
		t.Fatalf("by number: %v", one)
	}
	steps := one["steps"].([]any)
	if steps[0].(map[string]any)["slug"] != "tiny-1b-q4-k-m-2-0" || steps[1].(map[string]any)["slug"] != "tiny-1b-q4-k-m-2-1" {
		t.Fatalf("slugs: %v %v", steps[0].(map[string]any)["slug"], steps[1].(map[string]any)["slug"])
	}
	get("/api/bench/runs/99", 404)
	_, b, _ := e.do("GET", "/api/bench/runs/run-2?requests=0", "")
	if !strings.Contains(b, `"number":2`) || strings.Contains(b, `"requests":[`) {
		t.Fatalf("requests=0: %s", b)
	}
	get("/api/bench/runs/run-2?requests=x", 400)
}

func TestBenchSlice2_BenchmarkLastUseAndGroupExpandedCount(t *testing.T) {
	e := benchEnv(t)
	for _, b := range []string{
		`{"id":"used","profile":"alpha","requests":[{"prompt":"hi","max_tokens":4}]}`,
		`{"id":"never","profile":"alpha","requests":[{"prompt":"hi","max_tokens":4}]}`,
	} {
		if st, body, _ := e.do("POST", "/api/bench/benchmarks", b); st != 201 {
			t.Fatalf("%d %s", st, body)
		}
	}
	if st, body, _ := e.do("POST", "/api/bench/groups", `{"id":"two","benchmarks":["used","never"]}`); st != 201 {
		t.Fatalf("%d %s", st, body)
	}
	st := e.api.opts.BenchStore
	if err := st.CreateRun(&bench.Run{ID: "run-x", SuiteID: "two", SuiteName: "two", State: bench.RunCompleted, QueuedAt: 10, FinishedAt: 20}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddStep(&bench.Step{RunID: "run-x", Idx: 0, BenchmarkID: "used", State: bench.StepDone, Outcome: bench.OutcomeReady, StartedAt: 11, FinishedAt: 3011}); err != nil {
		t.Fatal(err)
	}
	_, b, _ := e.do("GET", "/api/bench/benchmarks", "")
	var bl struct {
		Benchmarks []map[string]any `json:"benchmarks"`
	}
	json.Unmarshal([]byte(b), &bl)
	seen := 0
	for _, x := range bl.Benchmarks {
		switch x["id"] {
		case "used":
			seen++
			u, _ := x["last_use"].(map[string]any)
			if u == nil || u["run_id"] != "run-x" || u["run_number"] != 1.0 || u["outcome"] != "ready" || u["duration_s"] != 3.0 {
				t.Errorf("used: %v", x["last_use"])
			}
		case "never":
			seen++
			if _, ok := x["last_use"]; ok {
				t.Errorf("never run: %v", x["last_use"])
			}
		}
	}
	if seen != 2 {
		t.Fatalf("benchmarks: %s", b)
	}
	for _, path := range []string{"/api/bench/groups", "/api/bench/suites"} {
		_, b, _ = e.do("GET", path, "")
		var gl map[string][]map[string]any
		json.Unmarshal([]byte(b), &gl)
		var row map[string]any
		for _, rows := range gl {
			for _, r := range rows {
				if r["id"] == "two" {
					row = r
				}
			}
		}
		if row == nil || row["expanded_count"] != 2.0 {
			t.Fatalf("%s: %v", path, row)
		}
		lr, _ := row["last_run"].(map[string]any)
		if lr == nil || lr["result"] != "passed" || lr["number"] != 1.0 || lr["counts"] == nil {
			t.Fatalf("%s last_run: %v", path, row["last_run"])
		}
	}
}

// Pinning a run takes the permission models, as a model pin does (it was
// settings): a key with settings only is refused now.
func TestBenchSlice2_PinRunNeedsModels(t *testing.T) {
	e, _, _ := cfgEnv(t)
	st := withBench(t, e)
	if err := st.CreateRun(&bench.Run{ID: "run-p", SuiteID: "g", State: bench.RunCompleted, QueuedAt: 1, FinishedAt: 2}); err != nil {
		t.Fatal(err)
	}
	settingsOnly, _ := newKey(t, e, `{"name":"tuner","permissions":{"settings":"allow"}}`)
	if code, m := e.cdo("PUT", "/api/bench/runs/run-p/pin", ``, bearer(settingsOnly)); code != 403 || errOf(m)["scope"] != "models" {
		t.Fatalf("settings only: %d %v", code, m)
	}
	mover, _ := newKey(t, e, `{"name":"mover","permissions":{"models":"allow"}}`)
	if code, m := e.cdo("PUT", "/api/bench/runs/run-p/pin", ``, bearer(mover)); code != 200 || m["pinned"] != true {
		t.Fatalf("models: %d %v", code, m)
	}
	if code, m := e.cdo("DELETE", "/api/bench/runs/run-p/pin", ``, bearer(mover)); code != 200 || m["pinned"] != false {
		t.Fatalf("unpin with models: %d %v", code, m)
	}
	if routeScopes["PUT /api/bench/runs/{id}/pin"] != access.Models || routeScopes["DELETE /api/bench/runs/{id}/pin"] != access.Models {
		t.Fatal("routeScopes")
	}
}
