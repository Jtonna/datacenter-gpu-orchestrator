package bench

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func bm(t *testing.T, id, profile string) *benchspec.Benchmark {
	t.Helper()
	b, err := benchspec.ParseBenchmarkJSON([]byte(`{"id":"` + id + `","profile":"` + profile + `","requests":[{"prompt":"hi","max_tokens":8}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(nil); err != nil {
		t.Fatal(err)
	}
	return b
}

func suite(t *testing.T, id string, refs ...string) *benchspec.Suite {
	t.Helper()
	s := &benchspec.Suite{ID: id, Name: id}
	for _, r := range refs {
		s.Benchmarks = append(s.Benchmarks, benchspec.SuiteItem{Ref: r})
	}
	return s
}

func TestMigrationsAreVersionedAndIdempotent(t *testing.T) {
	s, dir := openTemp(t)
	v, err := s.SchemaVersion()
	if err != nil || v != len(migrations) || v < 1 {
		t.Fatalf("version %d err %v, want %d", v, err, len(migrations))
	}
	s.Close()
	s2, err := OpenStore(dir) // opening again applies nothing new
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if v2, _ := s2.SchemaVersion(); v2 != v {
		t.Fatalf("reopened at version %d, want %d", v2, v)
	}
	if _, err := os.Stat(filepath.Join(dir, "scheduler.db")); err != nil {
		t.Fatal(err)
	}
	rows, err := s2.db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		have[n] = true
	}
	for _, want := range []string{"migrations", "benchmarks", "suites", "runs", "run_steps", "requests"} {
		if !have[want] {
			t.Errorf("table %s missing; have %v", want, have)
		}
	}
	var mode string
	s2.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %s, want wal", mode)
	}
}

func TestOpenFailsOnAnUnusableDirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, []byte("x"), 0o644)
	if _, err := OpenStore(filepath.Join(f, "sub")); err == nil {
		t.Fatal("opened a store under a regular file")
	}
	if _, err := OpenStore(""); err == nil {
		t.Fatal("opened a store with no directory")
	}
}

func TestBenchmarkCRUDAndSources(t *testing.T) {
	s, _ := openTemp(t)
	b := bm(t, "a", "p")
	if err := s.SaveBenchmark(b, SourceAPI, 1000); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBenchmark("a")
	if err != nil || got.Spec.Profile != "p" || got.Source != SourceAPI || got.CreatedAt != 1000 || got.UpdatedAt != 1000 {
		t.Fatalf("%+v %v", got, err)
	}
	b2 := bm(t, "a", "q")
	if err := s.SaveBenchmark(b2, SourceAPI, 2000); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBenchmark("a")
	if got.Spec.Profile != "q" || got.CreatedAt != 1000 || got.UpdatedAt != 2000 {
		t.Fatalf("update: %+v", got)
	}
	// Config seeding never overwrites an api item.
	if err := s.SeedBenchmark(bm(t, "a", "from-config"), 3000); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBenchmark("a")
	if got.Spec.Profile != "q" || got.Source != SourceAPI {
		t.Fatalf("config overwrote an api benchmark: %+v", got)
	}
	// ...but it does update its own.
	s.SeedBenchmark(bm(t, "c", "one"), 3000)
	s.SeedBenchmark(bm(t, "c", "two"), 4000)
	got, _ = s.GetBenchmark("c")
	if got.Spec.Profile != "two" || got.Source != SourceConfig {
		t.Fatalf("%+v", got)
	}
	list, err := s.ListBenchmarks()
	if err != nil || len(list) != 2 || list[0].Spec.ID != "a" || list[1].Spec.ID != "c" {
		t.Fatalf("%+v %v", list, err)
	}
	if _, err := s.GetBenchmark("zzz"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	if err := s.DeleteBenchmark("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBenchmark("a"); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSuiteCRUDAndReferences(t *testing.T) {
	s, _ := openTemp(t)
	s.SaveBenchmark(bm(t, "a", "p"), SourceAPI, 1)
	s.SaveBenchmark(&benchspec.Benchmark{ID: "pair", Parallel: []string{"a", "b"}}, SourceAPI, 1)
	if err := s.SaveSuite(suite(t, "s1", "a", "pair"), SourceAPI, 10); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSuite("s1")
	if err != nil || len(got.Spec.Benchmarks) != 2 || got.Spec.Benchmarks[1].Ref != "pair" {
		t.Fatalf("%+v %v", got, err)
	}
	users, _ := s.SuitesUsing("a")
	if len(users) != 1 || users[0] != "s1" {
		t.Fatalf("suites using a: %v", users)
	}
	users, _ = s.SuitesUsing("b") // only through the parallel step "pair"? no: b is not stored
	if len(users) != 0 {
		t.Fatalf("%v", users)
	}
	pairs, _ := s.BenchmarksUsing("a")
	if len(pairs) != 1 || pairs[0] != "pair" {
		t.Fatalf("benchmarks using a: %v", pairs)
	}
	if err := s.SeedSuite(suite(t, "s1", "a"), 20); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSuite("s1")
	if len(got.Spec.Benchmarks) != 2 {
		t.Fatal("config seeding overwrote an api suite")
	}
	if err := s.DeleteSuite("s1"); err != nil {
		t.Fatal(err)
	}
}

func newRun(id, suite string, at int64) *Run {
	return &Run{ID: id, SuiteID: suite, SuiteName: suite, State: RunQueued, QueuedAt: at, Trigger: "manual", Suite: []byte(`{}`)}
}

func TestRunLifecycleStepsAndRequests(t *testing.T) {
	s, _ := openTemp(t)
	r := newRun("run-1", "s1", 100)
	if err := s.CreateRun(r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunState("run-1", RunRunning, 110, 0, ""); err != nil {
		t.Fatal(err)
	}
	st := &Step{RunID: "run-1", Idx: 0, BenchmarkID: "a", Name: "A", Profile: "p", State: StepRunning, StartedAt: 111}
	if err := s.AddStep(st); err != nil || st.ID == 0 {
		t.Fatalf("%v %d", err, st.ID)
	}
	rq := &RequestResult{StepID: st.ID, RunID: "run-1", Seq: 1, Name: "short", Phase: PhaseWarmup, Status: 200, WallMS: 12.5, DecodeTPS: 30, OutputSHA256: "ab", StartedAt: 112}
	if err := s.AddRequest(rq); err != nil {
		t.Fatal(err)
	}
	ttft := 40.0
	s.AddRequest(&RequestResult{StepID: st.ID, RunID: "run-1", Seq: 2, Name: "short", Phase: PhaseMeasured, Status: 200, WallMS: 10, TTFTMS: &ttft, DecodeTPS: 31, Timings: []byte(`{"prompt_n":10}`)})
	st.State, st.Outcome, st.FinishedAt = StepDone, "ready", 120
	st.Load = &LoadResult{LoadS: 9.5, Bytes: 100, AvgMBs: 10.5}
	st.Stats = &Stats{Decode: &Summary{N: 1, Mean: 31}}
	if err := s.UpdateStep(st); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunState("run-1", RunCompleted, 0, 130, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RunCompleted || got.StartedAt != 110 || got.FinishedAt != 130 || len(got.Steps) != 1 {
		t.Fatalf("%+v", got)
	}
	g := got.Steps[0]
	if g.Outcome != "ready" || g.Load == nil || g.Load.LoadS != 9.5 || g.Stats.Decode.Mean != 31 || len(g.Requests) != 2 ||
		g.Requests[0].Phase != PhaseWarmup || g.Requests[1].TTFTMS == nil || *g.Requests[1].TTFTMS != 40 || string(g.Requests[1].Timings) != `{"prompt_n":10}` {
		t.Fatalf("%+v", g)
	}
	runs, err := s.ListRuns(RunFilter{Suite: "s1"})
	if err != nil || len(runs) != 1 || runs[0].Steps != nil {
		t.Fatalf("%+v %v", runs, err)
	}
	if runs, _ = s.ListRuns(RunFilter{State: RunFailed}); len(runs) != 0 {
		t.Fatalf("state filter: %+v", runs)
	}
	if _, err := s.GetRun("nope"); err != ErrNotFound {
		t.Fatalf("%v", err)
	}
}

func TestPreviousCompletedRunForBaseline(t *testing.T) {
	s, _ := openTemp(t)
	for i, id := range []string{"r1", "r2", "r3", "r4"} {
		s.CreateRun(newRun(id, "s", int64(100+i)))
	}
	s.SetRunState("r1", RunCompleted, 100, 110, "")
	s.SetRunState("r2", RunFailed, 101, 111, "boom")
	s.SetRunState("r3", RunCompleted, 102, 112, "")
	s.SetRunState("r4", RunRunning, 103, 0, "")
	prev, err := s.PreviousCompleted("s", "r4")
	if err != nil || prev == nil || prev.ID != "r3" {
		t.Fatalf("%+v %v", prev, err)
	}
	prev, _ = s.PreviousCompleted("s", "r3")
	if prev == nil || prev.ID != "r1" {
		t.Fatalf("%+v", prev)
	}
	if prev, _ = s.PreviousCompleted("other", "x"); prev != nil {
		t.Fatalf("%+v", prev)
	}
}

func TestInterruptedRunsAreMarkedOnOpen(t *testing.T) {
	s, dir := openTemp(t)
	s.CreateRun(newRun("run-q", "s", 1))
	s.CreateRun(newRun("run-r", "s", 2))
	s.SetRunState("run-r", RunRunning, 3, 0, "")
	st := &Step{RunID: "run-r", Idx: 0, BenchmarkID: "a", State: StepRunning}
	s.AddStep(st)
	s.CreateRun(newRun("run-p", "s", 4))
	s.SetRunState("run-p", RunPaused, 5, 0, "")
	s.CreateRun(newRun("run-done", "s", 6))
	s.SetRunState("run-done", RunCompleted, 6, 7, "")
	s.Close()

	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, err := s2.RecoverInterrupted(9000)
	if err != nil || n != 3 {
		t.Fatalf("recovered %d err %v", n, err)
	}
	for id, want := range map[string]string{"run-r": RunFailed, "run-p": RunFailed, "run-q": RunCancelled, "run-done": RunCompleted} {
		r, _ := s2.GetRun(id)
		if r.State != want {
			t.Errorf("%s: %s, want %s", id, r.State, want)
		}
		if want == RunFailed && (r.Error != "failed (interrupted)" || r.FinishedAt != 9000) {
			t.Errorf("%s: error %q finished %d", id, r.Error, r.FinishedAt)
		}
	}
	r, _ := s2.GetRun("run-r")
	if r.Steps[0].State != StepFailed {
		t.Errorf("running step left %s", r.Steps[0].State)
	}
}

func TestTrendPoints(t *testing.T) {
	s, _ := openTemp(t)
	for i, id := range []string{"r1", "r2", "r3"} {
		s.CreateRun(newRun(id, "s", int64(i+1)))
		s.SetRunState(id, RunCompleted, int64(10*(i+1)), int64(10*(i+1)+5), "")
		st := &Step{RunID: id, Idx: 0, BenchmarkID: "a", Profile: "p", State: StepDone, Outcome: "ready", StartedAt: int64(10 * (i + 1))}
		s.AddStep(st)
		st.Load = &LoadResult{LoadS: float64(60 + i)}
		st.Stats = &Stats{Decode: &Summary{N: 3, Mean: float64(30 - i)}, Prefill: &Summary{N: 3, Mean: 200}}
		s.UpdateStep(st)
	}
	pts, err := s.Trend("a", "load_s", 10)
	if err != nil || len(pts) != 3 || pts[0].RunID != "r1" || pts[2].Value != 62 {
		t.Fatalf("%+v %v", pts, err)
	}
	pts, _ = s.Trend("a", "decode_tps", 2)
	if len(pts) != 2 || pts[0].RunID != "r2" || pts[1].Value != 28 { // the newest two, oldest first
		t.Fatalf("%+v", pts)
	}
	if _, err := s.Trend("a", "bogus", 5); err == nil {
		t.Fatal("bad metric accepted")
	}
}

func TestStoreReportsSizeAndPath(t *testing.T) {
	s, dir := openTemp(t)
	if s.Path() != filepath.Join(dir, "scheduler.db") {
		t.Fatal(s.Path())
	}
	if n, err := s.SizeBytes(); err != nil || n <= 0 {
		t.Fatalf("%d %v", n, err)
	}
}
