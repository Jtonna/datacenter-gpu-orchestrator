package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// Models slice 1 (M1, read additions): ttl_s on every profile row of
// /api/profiles and /api/models, added_at per file, disk.host, the runs list
// filtered on a model file and bench.reports in the catalog entry.
func TestModelsSlice1_ReadAdditions(t *testing.T) {
	e := benchEnv(t)
	cfg := e.sched.Config()
	models := t.TempDir()
	os.WriteFile(filepath.Join(models, "a.gguf"), make([]byte, 4096), 0o644)
	when := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	os.Chtimes(filepath.Join(models, "a.gguf"), when, when)
	cfg.ModelsDir = models
	ten := 10 * time.Minute
	cfg.Profiles["gamma"].TTL = &ten

	// benchmark runs: r1 completed on a.gguf, r2 failed on a.gguf, r3
	// completed with a step recorded without its file (profile alpha), r4
	// completed on another file
	st := e.api.opts.BenchStore
	run := func(id, state, file, profile string, at int64) {
		if err := st.CreateRun(&bench.Run{ID: id, SuiteID: "s", State: state, QueuedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddStep(&bench.Step{RunID: id, BenchmarkID: "b", Profile: profile, ModelFile: file, State: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	run("r1", bench.RunCompleted, "a.gguf", "alpha", 1)
	run("r2", bench.RunFailed, "a.gguf", "alpha", 2)
	run("r3", bench.RunCompleted, "", "alpha", 3)
	run("r4", bench.RunCompleted, "g.gguf", "gamma", 4)
	e.api.catalogDirty(true)

	// /api/profiles: the effective idle unload in seconds, 0 = never
	var profs struct{ Profiles []map[string]any }
	if err := json.Unmarshal([]byte(body(t, e.get("/api/profiles"))), &profs); err != nil {
		t.Fatal(err)
	}
	ttl := map[string]any{}
	for _, p := range profs.Profiles {
		ttl[p["id"].(string)] = p["ttl_s"]
	}
	if ttl["alpha"] != 0.0 || ttl["gamma"] != 600.0 || ttl["beta:64k"] != 0.0 {
		t.Errorf("/api/profiles ttl_s: %v", ttl)
	}

	// /api/models
	var doc struct {
		Disk   map[string]any
		Models []map[string]any
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/models"))), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Disk["host"] != "host-1" {
		t.Errorf("disk.host: %v", doc.Disk)
	}
	by := map[string]map[string]any{}
	for _, m := range doc.Models {
		by[m["key"].(string)] = m
	}
	a, g, b := by["a.gguf"], by["g.gguf"], by["b.gguf"]
	if a == nil || g == nil || b == nil {
		t.Fatalf("models %v", by)
	}
	if a["added_at"] != float64(when.UnixMilli()) {
		t.Errorf("a.gguf added_at %v, want %d", a["added_at"], when.UnixMilli())
	}
	if v, has := b["added_at"]; !has || v != nil {
		t.Errorf("a missing file: added_at %v (present %v), want null", v, has)
	}
	if r := a["profiles"].([]any)[0].(map[string]any); r["ttl_s"] != 0.0 {
		t.Errorf("a.gguf profile row: %v", r)
	}
	if r := g["profiles"].([]any)[0].(map[string]any); r["ttl_s"] != 600.0 {
		t.Errorf("g.gguf profile row: %v", r)
	}
	if bm, _ := a["bench"].(map[string]any); bm == nil || bm["reports"] != 2.0 {
		t.Errorf("a.gguf bench: %v (want reports 2: r1 and r3)", a["bench"])
	}
	if bm, _ := g["bench"].(map[string]any); bm == nil || bm["reports"] != 1.0 {
		t.Errorf("g.gguf bench: %v", g["bench"])
	}
	if b["bench"] != nil {
		t.Errorf("b.gguf bench: %v", b["bench"])
	}

	// /api/bench/runs?model=
	ids := func(q string) []string {
		var d struct{ Runs []bench.Run }
		code, raw, _ := e.do("GET", "/api/bench/runs"+q, "")
		if code != 200 || json.Unmarshal([]byte(raw), &d) != nil {
			t.Fatalf("%s: %d %s", q, code, raw)
		}
		var out []string
		for _, r := range d.Runs {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids("?model=a.gguf"); !slices.Equal(got, []string{"r1", "r2", "r3"}) {
		t.Errorf("?model=a.gguf: %v", got)
	}
	if got := ids("?model=a.gguf&state=completed"); !slices.Equal(got, []string{"r1", "r3"}) {
		t.Errorf("?model=a.gguf&state=completed: %v", got)
	}
	if got := ids("?model=none.gguf"); len(got) != 0 {
		t.Errorf("?model=none.gguf: %v", got)
	}
	if got := ids(""); len(got) != 4 {
		t.Errorf("no filter: %v", got)
	}
}

