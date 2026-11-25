package bench

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// Benchmarks slice 2 (B1, B2, B3): the run list carries what the boards draw
// on every row: outcome counts (also while a run runs), a run-level result,
// the cold summary, a direct run's inputs, a stable number, and the list can
// be paged with a total and filtered by result.

// factsStore is a store with hand-made runs, steps and phases.
type factsStore struct {
	t  *testing.T
	st *Store
}

func newFactsStore(t *testing.T) *factsStore {
	t.Helper()
	st, _ := openTemp(t)
	return &factsStore{t: t, st: st}
}

// plan is a run snapshot with n plain items.
func planOf(t *testing.T, ids ...string) json.RawMessage {
	t.Helper()
	p := runPlan{Suite: &benchspec.Suite{ID: "g"}, Benchmarks: map[string]*benchspec.Benchmark{}}
	for _, id := range ids {
		p.Suite.Benchmarks = append(p.Suite.Benchmarks, benchspec.SuiteItem{Ref: id})
		p.Benchmarks[id] = &benchspec.Benchmark{ID: id, Profile: "p-" + id}
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *factsStore) run(r *Run) {
	f.t.Helper()
	if r.QueuedAt == 0 {
		r.QueuedAt = 1000
	}
	if err := f.st.CreateRun(r); err != nil {
		f.t.Fatal(err)
	}
}

// step adds a step with its phases.
func (f *factsStore) step(st *Step, phases ...PhaseResult) {
	f.t.Helper()
	if err := f.st.AddStep(st); err != nil {
		f.t.Fatal(err)
	}
	for i := range phases {
		p := phases[i]
		p.StepID, p.RunID, p.Idx = st.ID, st.RunID, i
		if err := f.st.AddPhase(&p); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *factsStore) list(fl RunFilter) ([]Run, int) {
	f.t.Helper()
	rs, total, err := f.st.ListRunsPage(fl)
	if err != nil {
		f.t.Fatal(err)
	}
	return rs, total
}

func (f *factsStore) one(id string) Run {
	f.t.Helper()
	rs, _ := f.list(RunFilter{Limit: 1000})
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	f.t.Fatalf("run %s not listed", id)
	return Run{}
}

func okPhase(typ string) PhaseResult {
	return PhaseResult{Type: typ, State: PhaseDone, Outcome: PhaseOK}
}

func coldLoad(verified bool) PhaseResult {
	p := okPhase("load")
	p.Cold = &ColdResult{Requested: true, Verified: verified}
	return p
}

func TestRunFacts_CountsResultColdAndInputs(t *testing.T) {
	f := newFactsStore(t)

	// A finished group run: one clean step, one ready step whose requests
	// phase had a failed request (an issue), one failed, one refused, one
	// skipped.
	f.run(&Run{ID: "mixed", SuiteID: "g", SuiteName: "G", State: RunCompleted, Suite: planOf(t, "a", "b", "c", "d", "e")})
	withFail := okPhase("requests")
	withFail.Stats = &Stats{Requests: 3, Failed: 1}
	f.step(&Step{RunID: "mixed", Idx: 0, BenchmarkID: "a", State: StepDone, Outcome: OutcomeReady}, coldLoad(true), okPhase("requests"))
	f.step(&Step{RunID: "mixed", Idx: 1, BenchmarkID: "b", State: StepDone, Outcome: OutcomeReady}, coldLoad(true), withFail)
	f.step(&Step{RunID: "mixed", Idx: 2, BenchmarkID: "c", State: StepDone, Outcome: OutcomeFailed, Reason: "child exited during load"}, PhaseResult{Type: "load", State: PhaseFailed})
	f.step(&Step{RunID: "mixed", Idx: 3, BenchmarkID: "d", State: StepDone, Outcome: OutcomeRefused, Reason: "does not fit"})
	f.step(&Step{RunID: "mixed", Idx: 4, BenchmarkID: "e", State: StepSkipped, Outcome: OutcomeSkipped})

	r := f.one("mixed")
	want := RunCounts{Steps: 5, Done: 5, Completed: 2, Passed: 1, WithIssues: 1, Failed: 1, Refused: 1, Skipped: 1}
	if r.Counts == nil || *r.Counts != want {
		t.Fatalf("counts %+v, want %+v", r.Counts, want)
	}
	if r.Result != ResultWithIssues {
		t.Errorf("result %q, want %q", r.Result, ResultWithIssues)
	}
	if r.Cold == nil || *r.Cold != (ColdSummary{Loads: 3, Cold: 2, Verified: 2, State: ColdSome}) {
		t.Errorf("cold %+v", r.Cold)
	}
	if r.Inputs != nil {
		t.Errorf("a group run has no inputs: %+v", r.Inputs)
	}
	if r.Suite != nil {
		t.Error("the list must stay small: no snapshot on rows")
	}

	// A clean group run: passed.
	f.run(&Run{ID: "clean", SuiteID: "g", State: RunCompleted, Suite: planOf(t, "a")})
	f.step(&Step{RunID: "clean", Idx: 0, BenchmarkID: "a", State: StepDone, Outcome: OutcomeReady}, coldLoad(false), okPhase("unload"))
	if r := f.one("clean"); r.Result != ResultPassed || r.Counts.Passed != 1 || *r.Cold != (ColdSummary{Loads: 1, Cold: 1, State: ColdSome}) {
		t.Errorf("clean: %q %+v %+v", r.Result, r.Counts, r.Cold)
	}

	// A direct run of a recipe whose one step was refused: the run reads
	// completed, the result says refused, and the inputs come from the
	// snapshot (the expanded recipe's applied inputs) and the step.
	in := &benchspec.Inputs{Model: "m.gguf", Ctx: 8192, Placement: &benchspec.Placement{Mode: benchspec.PlaceAny, N: 2}}
	p := runPlan{Suite: &benchspec.Suite{Benchmarks: []benchspec.SuiteItem{{Inline: &benchspec.Benchmark{ID: "cold.ab12cd", Recipe: "cold", Applied: in, Profile: "bench-m"}}}},
		Benchmarks: map[string]*benchspec.Benchmark{}}
	snap, _ := json.Marshal(p)
	f.run(&Run{ID: "direct", Kind: RunKindBenchmark, BenchmarkID: "cold", SuiteName: "Cold load", State: RunCompleted, Suite: snap})
	f.step(&Step{RunID: "direct", Idx: 0, BenchmarkID: "cold.ab12cd", State: StepDone, Outcome: OutcomeRefused, ModelFile: "m.gguf", NodeCount: 0})
	r = f.one("direct")
	if r.Result != ResultRefused || r.State != RunCompleted {
		t.Errorf("direct refused: %q %q", r.Result, r.State)
	}
	if r.Inputs == nil || *r.Inputs != (RunInputs{Model: "m.gguf", Ctx: 8192, Placement: "any:2", Recipe: "cold", Profile: "bench-m"}) {
		t.Errorf("inputs %+v", r.Inputs)
	}

	// A direct run of a fixed-profile benchmark that failed: result failed,
	// inputs name the profile, its model file and the machines used.
	f.run(&Run{ID: "fixed", Kind: RunKindBenchmark, BenchmarkID: "x", State: RunCompleted, Suite: planOf(t, "x")})
	f.step(&Step{RunID: "fixed", Idx: 0, BenchmarkID: "x", Profile: "p-x", State: StepDone, Outcome: OutcomeFailed, ModelFile: "x.gguf", NodeCount: 4})
	if r := f.one("fixed"); r.Result != ResultFailed || r.Inputs == nil || *r.Inputs != (RunInputs{Model: "x.gguf", Profile: "p-x", NodeCount: 4}) {
		t.Errorf("fixed: %q %+v", r.Result, r.Inputs)
	}

	// A running group run: counts so far and what is still to go, from the
	// snapshot's three items.
	f.run(&Run{ID: "running", SuiteID: "g", State: RunRunning, Suite: planOf(t, "a", "b", "c")})
	f.step(&Step{RunID: "running", Idx: 0, BenchmarkID: "a", State: StepDone, Outcome: OutcomeReady}, okPhase("load"))
	f.step(&Step{RunID: "running", Idx: 1, BenchmarkID: "b", State: StepRunning})
	r = f.one("running")
	if r.Result != ResultRunning || *r.Counts != (RunCounts{Steps: 3, Done: 1, ToGo: 2, Completed: 1, Passed: 1}) {
		t.Errorf("running: %q %+v", r.Result, r.Counts)
	}

	for id, want := range map[string]string{"q": ResultQueued, "w": ResultWaiting, "c": ResultCancelled, "f": ResultFailed, "s": ResultSkipped} {
		state := map[string]string{"q": RunQueued, "w": RunPaused, "c": RunCancelled, "f": RunFailed, "s": RunSkipped}[id]
		f.run(&Run{ID: id, SuiteID: "g", State: state, Suite: planOf(t, "a")})
		if r := f.one(id); r.Result != want {
			t.Errorf("state %s: result %q, want %q", state, r.Result, want)
		}
	}
	// queued: nothing done, everything to go
	if r := f.one("q"); *r.Counts != (RunCounts{Steps: 1, ToGo: 1}) {
		t.Errorf("queued counts %+v", r.Counts)
	}

	// GetRun carries the same facts.
	g, err := f.st.GetRun("mixed")
	if err != nil {
		t.Fatal(err)
	}
	if g.Counts == nil || *g.Counts != want || g.Result != ResultWithIssues || g.Number == 0 {
		t.Errorf("GetRun facts %+v %q %d", g.Counts, g.Result, g.Number)
	}
}

// An issue inside a ready step is found from the phase results, which a
// retention trim keeps: the result does not change when the requests go.
func TestRunFacts_IssuesSurviveATrim(t *testing.T) {
	f := newFactsStore(t)
	conv := okPhase("conversation")
	conv.Turns = []TurnResult{{Turn: 1, Status: 200}, {Turn: 2, Status: 500}}
	f.run(&Run{ID: "r", SuiteID: "g", State: RunCompleted, FinishedAt: 5, Suite: planOf(t, "a")})
	f.step(&Step{RunID: "r", Idx: 0, BenchmarkID: "a", State: StepDone, Outcome: OutcomeReady}, conv)
	if r := f.one("r"); r.Result != ResultWithIssues {
		t.Fatalf("before the trim: %q", r.Result)
	}
	if _, err := f.st.RemoveRun("r", "test", "test", 10); err != nil {
		t.Fatal(err)
	}
	if r := f.one("r"); r.Result != ResultWithIssues || r.TrimmedAt == 0 {
		t.Fatalf("after the trim: %q trimmed %d", r.Result, r.TrimmedAt)
	}
	for _, c := range []struct {
		name string
		p    PhaseResult
	}{
		{"concurrent errors", PhaseResult{Type: "concurrent_requests", State: PhaseDone, Concurrent: &ConcurrentResult{N: 4, Errors: 1}}},
		{"tasks not answered", PhaseResult{Type: "tasks", State: PhaseDone, Tasks: &TasksResult{Total: 3, TimedOut: 1}}},
		{"failed phase", PhaseResult{Type: "hold", State: PhaseFailed}},
	} {
		id := "r-" + c.name
		f.run(&Run{ID: id, SuiteID: "g", State: RunCompleted, Suite: planOf(t, "a")})
		f.step(&Step{RunID: id, Idx: 0, BenchmarkID: "a", State: StepDone, Outcome: OutcomeReady}, c.p)
		if r := f.one(id); r.Result != ResultWithIssues || r.Counts.WithIssues != 1 {
			t.Errorf("%s: %q %+v", c.name, r.Result, r.Counts)
		}
	}
	// A step of the first runner (no phases): its own stats say.
	f.run(&Run{ID: "old", SuiteID: "g", State: RunCompleted, Suite: planOf(t, "a")})
	f.step(&Step{RunID: "old", Idx: 0, BenchmarkID: "a", State: StepDone, Outcome: OutcomeReady, Stats: &Stats{Requests: 2, Failed: 1}, Load: &LoadResult{LoadS: 3}})
	if r := f.one("old"); r.Result != ResultWithIssues || r.Cold == nil || r.Cold.Loads != 1 {
		t.Errorf("old step: %q %+v", r.Result, r.Cold)
	}
}

func TestListRunsPage_OffsetTotalAndResultFilter(t *testing.T) {
	f := newFactsStore(t)
	for i := 1; i <= 7; i++ {
		id := string(rune('a' + i - 1))
		f.run(&Run{ID: id, SuiteID: "g", State: RunCompleted, QueuedAt: int64(i), Suite: planOf(t, "x")})
		out := OutcomeReady
		if i%3 == 0 {
			out = OutcomeFailed
		}
		f.step(&Step{RunID: id, Idx: 0, BenchmarkID: "x", State: StepDone, Outcome: out})
	}
	ids := func(rs []Run) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	rs, total := f.list(RunFilter{Limit: 3})
	if total != 7 || !slices.Equal(ids(rs), []string{"g", "f", "e"}) {
		t.Fatalf("page 1: %v of %d", ids(rs), total)
	}
	rs, total = f.list(RunFilter{Limit: 3, Offset: 6})
	if total != 7 || !slices.Equal(ids(rs), []string{"a"}) {
		t.Fatalf("page 3: %v of %d", ids(rs), total)
	}
	// runs c and f failed a step: result with_issues; the filter pages after filtering
	rs, total = f.list(RunFilter{Limit: 1, Result: ResultWithIssues})
	if total != 2 || !slices.Equal(ids(rs), []string{"f"}) {
		t.Fatalf("with_issues: %v of %d", ids(rs), total)
	}
	rs, total = f.list(RunFilter{Limit: 10, Offset: 1, Result: ResultWithIssues})
	if total != 2 || !slices.Equal(ids(rs), []string{"c"}) {
		t.Fatalf("with_issues page 2: %v of %d", ids(rs), total)
	}
	rs, total = f.list(RunFilter{Limit: 10, Result: ResultHasFailed})
	if total != 2 {
		t.Fatalf("has_failed: %v of %d", ids(rs), total)
	}
	rs, total = f.list(RunFilter{Limit: 10, Result: ResultPassed})
	if total != 5 || len(rs) != 5 {
		t.Fatalf("passed: %v of %d", ids(rs), total)
	}
	if !ValidResultFilter(ResultHasRefused) || ValidResultFilter("bogus") || !ValidResultFilter("") {
		t.Error("ValidResultFilter")
	}
	// the old ListRuns still answers the rows with their facts
	old, err := f.st.ListRuns(RunFilter{Limit: 2})
	if err != nil || len(old) != 2 || old[0].Counts == nil || old[0].Number != 7 {
		t.Fatalf("ListRuns: %v %+v", err, old)
	}
}

// B3: a benchmark's last use is the newest step that ran it, by its own id
// or as the recipe of an expanded use; a skipped step is not a use.
func TestLastUses_ByIdAndRecipe(t *testing.T) {
	f := newFactsStore(t)
	recipe := func(id string) *benchspec.Benchmark {
		return &benchspec.Benchmark{ID: id, Recipe: "cold", Profile: "bench-x"}
	}
	f.run(&Run{ID: "r1", SuiteID: "g", State: RunCompleted, QueuedAt: 10})
	f.step(&Step{RunID: "r1", Idx: 0, BenchmarkID: "plain", State: StepDone, Outcome: OutcomeReady, StartedAt: 11, FinishedAt: 41011})
	f.step(&Step{RunID: "r1", Idx: 1, BenchmarkID: "cold.aa", Spec: recipe("cold.aa"), State: StepDone, Outcome: OutcomeFailed, StartedAt: 50, FinishedAt: 60})
	f.run(&Run{ID: "r2", SuiteID: "g", State: RunCompleted, QueuedAt: 20})
	f.step(&Step{RunID: "r2", Idx: 0, BenchmarkID: "cold.bb", Spec: recipe("cold.bb"), State: StepDone, Outcome: OutcomeRefused, StartedAt: 21, FinishedAt: 22})
	f.step(&Step{RunID: "r2", Idx: 1, BenchmarkID: "plain", State: StepSkipped, Outcome: OutcomeSkipped})
	f.run(&Run{ID: "r3", SuiteID: "g", State: RunRunning, QueuedAt: 30})
	f.step(&Step{RunID: "r3", Idx: 0, BenchmarkID: "busy", State: StepRunning, StartedAt: 31})

	got, err := f.st.LastUses()
	if err != nil {
		t.Fatal(err)
	}
	if u := got["plain"]; u == nil || *u != (LastUse{RunID: "r1", RunNumber: 1, Step: 0, At: 11, Outcome: OutcomeReady, DurationS: 41}) {
		t.Errorf("plain: %+v", u)
	}
	if u := got["cold"]; u == nil || *u != (LastUse{RunID: "r2", RunNumber: 2, Step: 0, At: 21, Outcome: OutcomeRefused, DurationS: 0.001}) {
		t.Errorf("recipe cold: %+v", u)
	}
	if u := got["cold.aa"]; u == nil || u.RunID != "r1" {
		t.Errorf("the expanded id: %+v", u)
	}
	if u := got["busy"]; u == nil || u.Outcome != "running" || u.DurationS != 0 {
		t.Errorf("running: %+v", u)
	}
	if got["never"] != nil {
		t.Error("never run")
	}
}
