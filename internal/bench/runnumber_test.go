package bench

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// B2: a run's number is its place in queue order and never changes for an
// existing run: not across a restart, not across a retention trim, not when
// new runs are added, and not when another run's row is removed (the removal
// is in the run_removals ledger, as every trim already is).
func TestRunNumber_StableAcrossRestartTrimNewRunsAndRemoval(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	add := func(st *Store, id string, at int64) {
		t.Helper()
		if err := st.CreateRun(&Run{ID: id, SuiteID: "g", State: RunCompleted, QueuedAt: at, FinishedAt: at + 1}); err != nil {
			t.Fatal(err)
		}
	}
	// Two runs queued in the same millisecond are ordered by id.
	add(st, "run-b", 100)
	add(st, "run-a", 100)
	add(st, "run-c", 200)
	add(st, "run-d", 300)
	nums := func(st *Store) map[string]int {
		t.Helper()
		n, err := st.RunNumbers()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	want := map[string]int{"run-a": 1, "run-b": 2, "run-c": 3, "run-d": 4}
	check := func(stage string, st *Store) {
		t.Helper()
		got := nums(st)
		for id, n := range want {
			if got[id] != n {
				t.Fatalf("%s: run %s is number %d, want %d (all %v)", stage, id, got[id], n, got)
			}
		}
	}
	check("first", st)

	// restart
	st.Close()
	st, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	check("after a restart", st)

	// retention trim of an early run
	if _, err := st.RemoveRun("run-a", "retention", "test", 1000); err != nil {
		t.Fatal(err)
	}
	check("after a trim", st)

	// new runs
	add(st, "run-e", 400)
	add(st, "run-f", 500)
	want["run-e"], want["run-f"] = 5, 6
	check("after new runs", st)

	// a run's row removed (no code path does this today; a removal is
	// logged in run_removals first, as RemoveRun does): no other number moves,
	// and the removed run's number is not given to anyone else.
	if _, err := st.RemoveRun("run-b", "test", "test", 1001); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM runs WHERE id='run-b'`); err != nil {
		t.Fatal(err)
	}
	delete(want, "run-b")
	check("after a row was removed", st)
	if _, err := st.RunIDByNumber(2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the number of a removed run: %v", err)
	}
	add(st, "run-g", 600)
	want["run-g"] = 7
	check("a new run after a removal", st)

	// GET by number
	if id, err := st.ResolveRunRef("3"); err != nil || id != "run-c" {
		t.Fatalf("number 3: %q %v", id, err)
	}
	if id, err := st.ResolveRunRef("run-d"); err != nil || id != "run-d" {
		t.Fatalf("an id is itself: %q %v", id, err)
	}
	if _, err := st.ResolveRunRef("99"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown number: %v", err)
	}
	if r, err := st.GetRun("run-e"); err != nil || r.Number != 5 {
		t.Fatalf("GetRun number: %+v %v", r, err)
	}
}

// The guarantee above holds only while no code deletes a run row without
// logging it in run_removals. Today nothing deletes a run row at all; this
// test fails the day one does, so that change has to answer for the numbers.
func TestRunNumber_NoCodeDeletesARunRow(t *testing.T) {
	re := regexp.MustCompile(`(?i)DELETE\s+FROM\s+runs\b`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if re.Match(b) {
			t.Errorf("%s deletes run rows: log the run in run_removals in the same transaction and change this test, or run numbers move (runfacts.go RunNumbers)", f)
		}
	}
}

// Entry addresses: a slug of the model's tidy name and the placement, with
// the step index appended only when two entries of the run share a slug.
func TestEntrySlugs(t *testing.T) {
	in := func(model, mode string, n int) *benchspec.Benchmark {
		p := benchspec.Placement{Mode: mode, N: n}
		return &benchspec.Benchmark{ID: "x", Recipe: "cold", Applied: &benchspec.Inputs{Model: model, Placement: &p}}
	}
	steps := []Step{
		{Idx: 0, ModelFile: "Qwen2.5-72B-Instruct-Q4_K_M-00001-of-00003.gguf", NodeCount: 6, Spec: in("Qwen2.5-72B-Instruct-Q4_K_M-00001-of-00003.gguf", benchspec.PlaceAll, 0)},
		{Idx: 1, ModelFile: "bartowski_gemma-3-12b-it.gguf", NodeCount: 2, Spec: in("bartowski_gemma-3-12b-it.gguf", benchspec.PlaceAny, 2)},
		{Idx: 2, ModelFile: "bartowski_gemma-3-12b-it.gguf", NodeCount: 2, Spec: in("bartowski_gemma-3-12b-it.gguf", benchspec.PlaceAny, 2)},
		{Idx: 3, Profile: "small:8k", ModelFile: "tiny.gguf", NodeCount: 1},
		{Idx: 4, Profile: "old"}, // no file recorded
	}
	got := EntrySlugs(steps)
	want := []string{"qwen2-5-72b-instruct-q4-k-m-all6", "gemma-3-12b-it-any2-1", "gemma-3-12b-it-any2-2", "tiny-1", "old"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: %q, want %q", i, got[i], want[i])
		}
	}
	// a parallel step: two members at one index with the same slug
	par := []Step{{Idx: 0, ModelFile: "a.gguf", NodeCount: 1}, {Idx: 0, ModelFile: "a.gguf", NodeCount: 1}}
	if g := EntrySlugs(par); g[0] == g[1] || g[0] != "a-1-0" || g[1] != "a-1-0-2" {
		t.Errorf("parallel members: %v", g)
	}
}
