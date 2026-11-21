package bench

import (
	"testing"
)

// A step written by the schema before lowest-available memory, roles and the
// reload figure existed still loads; it simply has none of them (the UI says
// "not recorded"). Its load JSON has no load_mode either.
func TestStepFactsOldRowsLoadWithout(t *testing.T) {
	dir := t.TempDir()
	old, err := openStore(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO runs (id, suite_id, suite_name, state, queued_at, started_at, finished_at, version, suite) VALUES ('run-old','s','S','completed',100,110,200,'v0','{}')`,
		`INSERT INTO run_steps (id, run_id, idx, benchmark_id, name, profile, state, outcome, started_at, finished_at, load, model_file, node_count) VALUES
		  (7,'run-old',0,'gemma','Gemma','gemma-4','done','ready',111,190,'{"load_s":66.1,"nodes":[{"node":"ps5-a","memory_mib":7900}]}','g.gguf',1)`,
	} {
		if _, err := old.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.GetRun("run-old")
	if err != nil {
		t.Fatal(err)
	}
	st := r.Steps[0]
	if st.MemLow != nil || st.Roles != nil || st.Reload != nil || st.Load == nil || st.Load.LoadMode != "" || st.Load.LoadS != 66.1 {
		t.Fatalf("%+v", st)
	}
	res := ResultsOf([]Run{*r}, "gemma")
	if len(res) != 1 || res[0].LowestMiB != nil || res[0].LoadMode != "" || res[0].Host != "" {
		t.Fatalf("results %+v", res)
	}
}

// The new figures are stored with the step and read back.
func TestStepFactsRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.CreateRun(&Run{ID: "r1", SuiteID: "s", SuiteName: "S", State: RunRunning, QueuedAt: 1, Suite: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	lo, lo2, step := int64(2100), int64(5000), int64(1900)
	mem := 7900.0
	pct := 63.0
	st := &Step{RunID: "r1", Idx: 0, BenchmarkID: "b", Profile: "p", State: StepRunning}
	if err := s.AddStep(st); err != nil {
		t.Fatal(err)
	}
	st.State, st.Outcome = StepDone, OutcomeReady
	st.Load = &LoadResult{LoadS: 20, LoadMode: LoadModeFileRead}
	st.MemLow = []NodeMemLow{{ID: "n1", Node: "ps5-a", LoadMiB: &lo, StepMiB: &step, Samples: 40}, {ID: "n0", Node: "host-1", LoadMiB: &lo2, StepMiB: &lo2, Samples: 40}}
	st.Roles = []NodeRole{{ID: "n0", Node: "host-1", Role: RoleHost}, {ID: "n1", Node: "ps5-a", Role: RoleWorker, MemoryMiB: &mem}}
	st.Reload = []NodeReload{{ID: "n1", Node: "ps5-a", Verdict: "partial", CachedPct: &pct}}
	if err := s.UpdateStep(st); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRun("r1")
	if err != nil {
		t.Fatal(err)
	}
	got := r.Steps[0]
	if len(got.MemLow) != 2 || *got.MemLow[0].StepMiB != 1900 || *got.MemLow[0].LoadMiB != 2100 || got.MemLow[0].Samples != 40 {
		t.Fatalf("mem_low %+v", got.MemLow)
	}
	if len(got.Roles) != 2 || got.Roles[0].Role != RoleHost || *got.Roles[1].MemoryMiB != 7900 {
		t.Fatalf("roles %+v", got.Roles)
	}
	if len(got.Reload) != 1 || got.Reload[0].Verdict != "partial" || *got.Reload[0].CachedPct != 63 || got.Load.LoadMode != LoadModeFileRead {
		t.Fatalf("reload %+v load %+v", got.Reload, got.Load)
	}
	res := ResultsOf([]Run{*r}, "b")
	if len(res) != 1 || res[0].LowestMiB == nil || *res[0].LowestMiB != 1900 || res[0].LoadMode != LoadModeFileRead ||
		res[0].Host != "host-1" || len(res[0].Workers) != 1 || res[0].Workers[0] != "ps5-a" || len(res[0].MemLow) != 2 || len(res[0].Reload) != 1 {
		t.Fatalf("results %+v", res)
	}
}
