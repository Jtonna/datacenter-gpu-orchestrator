package bench

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

const coldLoadRecipe = `{"id":"cold-load","name":"Cold load","inputs":{"ctx":8192,"placement":"any:2"},
	"phases":[{"type":"load"},{"type":"requests","requests":[{"name":"q","prompt":"hi","max_tokens":8,"stream":true}]},{"type":"unload"}]}`

func consoles() []Console {
	return []Console{
		{ID: "02:00:00:00:00:01", Name: "ps5-1", Placeable: true, MemAvailMiB: 9000},
		{ID: "02:00:00:00:00:02", Name: "ps5-2", Placeable: true, MemAvailMiB: 12000},
		{ID: "02:00:00:00:00:03", Name: "ps5-3", Unplaced: "paused"},
		{ID: "02:00:00:00:00:04", Name: "ps5-4", Placeable: true, MemAvailMiB: 12000},
	}
}

// A group with a nested group, a recipe used with inputs, a matrix entry and
// a plain benchmark: the run executes the expanded list, makes each recipe's
// profile from its inputs on the consoles its placement names, records a
// placement that cannot be met as a refusal, and stores the measurements
// under the recipe with the variant and the inputs.
func TestGroupOfRecipesRuns(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.consoles = consoles()
	tm := newTestMgr(t, api)
	tm.bench(coldLoadRecipe)
	tm.bench(`{"id":"plain","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	tm.suite(`{"id":"inner","benchmarks":[{"benchmark":"cold-load","inputs":{"model":"g.gguf","thinking":false}}]}`)
	tm.suite(`{"id":"outer","cooldown_s":0,"only_when_idle":false,"benchmarks":[{"group":"inner"},
		{"benchmark":"cold-load","inputs":{"ctx":4096,"model":"a.gguf"},"matrix":{"placement":["all","nodes:ps5-3","any:4"]}},"plain"]}`)
	r := tm.finished(tm.run("outer").ID)
	if r.State != RunCompleted || len(r.Steps) != 5 {
		t.Fatalf("%s %s, %d steps", r.State, r.Error, len(r.Steps))
	}
	type want struct{ profile, outcome, reason string }
	for i, w := range []want{
		{"bench-g-any2-8k-nothink", OutcomeReady, ""},
		{"bench-a-all-4k", OutcomeReady, ""},
		{"bench-a-on-ps5-3-4k", OutcomeRefused, `console "ps5-3" cannot take a load now (paused)`},
		{"bench-a-any4-4k", OutcomeRefused, "any 4 consoles: only 3 can take a load now"},
		{"gemma", OutcomeReady, ""},
	} {
		st := r.Steps[i]
		if st.Profile != w.profile || st.Outcome != w.outcome || st.Reason != w.reason {
			t.Errorf("step %d: %s %s %q, want %+v", i, st.Profile, st.Outcome, st.Reason, w)
		}
	}
	s0 := r.Steps[0]
	if s0.Spec == nil || s0.Spec.Recipe != "cold-load" || s0.Spec.Applied == nil || s0.Spec.Applied.Model != "g.gguf" || !strings.HasPrefix(s0.BenchmarkID, "cold-load.") ||
		s0.Name != "Cold load (g.gguf, 8k, any:2, thinking off)" || s0.ModelFile != "g.gguf" {
		t.Fatalf("step 0: %+v", s0)
	}
	wantSpec := map[string]any{"model": "g.gguf", "ctx": 8192, "generated": true, "stage": "test", "force_nodes": []string{"ps5-2", "ps5-4"}, "extra_args": []string{"--reasoning-budget", "0"}}
	if got := api.made["bench-g-any2-8k-nothink"]; !reflect.DeepEqual(got, wantSpec) {
		t.Errorf("made %v, want %v", got, wantSpec)
	}
	if got := api.made["bench-a-all-4k"]["force_nodes"]; !reflect.DeepEqual(got, []string{"ps5-1", "ps5-2", "ps5-4"}) {
		t.Errorf("all: %v", got)
	}
	if _, ok := api.made["bench-a-on-ps5-3-4k"]; ok {
		t.Error("a refused placement made a profile")
	}

	// The run keeps the expanded list it executed.
	if !strings.Contains(string(r.Suite), `"recipe":"cold-load"`) || !strings.Contains(string(r.Suite), `"applied_inputs"`) {
		t.Errorf("snapshot: %s", r.Suite)
	}

	// Measurements: under the recipe, with variant and inputs.
	zero := 0
	ms, err := tm.store.RunMeasurements(r.ID, MeasureQuery{Step: &zero, Metrics: []string{"load_s", "ttft_ms"}})
	if err != nil || len(ms) != 2 {
		t.Fatalf("%v %+v", err, ms)
	}
	for _, m := range ms {
		if m.BenchmarkID != "cold-load" || m.Variant != s0.BenchmarkID || m.Ctx != 8192 || m.Placement != "any:2" || m.Conditions["thinking_input"] != "off" {
			t.Errorf("%+v", m)
		}
	}
	rows, err := tm.store.QueryMeasurements(MeasureFilter{Benchmark: "cold-load", Placement: "all", Metrics: []string{"load_s"}})
	if err != nil || len(rows) != 1 || rows[0].Ctx != 4096 {
		t.Fatalf("history by placement: %v %+v", err, rows)
	}
	rows, _ = tm.store.QueryMeasurements(MeasureFilter{Variant: s0.BenchmarkID, Metrics: []string{"load_s"}})
	if len(rows) != 1 {
		t.Fatalf("history by variant: %+v", rows)
	}
	rows, _ = tm.store.QueryMeasurements(MeasureFilter{Ctx: 8192, Metrics: []string{"load_s"}})
	if len(rows) != 1 {
		t.Fatalf("history by ctx: %+v", rows)
	}
	runs, _ := tm.store.ListRuns(RunFilter{Benchmark: "cold-load"})
	if len(runs) != 1 || runs[0].ID != r.ID {
		t.Fatalf("runs of the recipe: %+v", runs)
	}

	// A second run with the same inputs gets the same variant ids, and the
	// profile that is already right is not written again (the fake counts the calls).
	r2 := tm.finished(tm.run("outer").ID)
	if r2.Steps[0].BenchmarkID != s0.BenchmarkID || r2.Steps[1].BenchmarkID != r.Steps[1].BenchmarkID {
		t.Fatal("variant ids differ between runs")
	}
}

func TestRunARecipeDirectly(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.consoles = consoles()
	tm := newTestMgr(t, api)
	tm.bench(coldLoadRecipe)
	tm.bench(`{"id":"plain","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	no := false
	if _, err := tm.m.EnqueueBenchmark("cold-load", BenchRunOptions{OnlyWhenIdle: &no}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "needs the input model") {
		t.Fatalf("no model: %v", err)
	}
	if _, err := tm.m.EnqueueBenchmark("plain", BenchRunOptions{OnlyWhenIdle: &no, Inputs: &benchspec.Inputs{Ctx: 4096}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "fixed profile") {
		t.Fatalf("inputs on a fixed benchmark: %v", err)
	}
	q, err := tm.m.EnqueueBenchmark("cold-load", BenchRunOptions{OnlyWhenIdle: &no, Inputs: &benchspec.Inputs{Model: "d.gguf", Placement: &benchspec.Placement{Mode: benchspec.PlaceAny, N: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	r := tm.finished(q.ID)
	if r.State != RunCompleted || r.Kind != RunKindBenchmark || r.BenchmarkID != "cold-load" || len(r.Steps) != 1 || r.Steps[0].Profile != "bench-d-any1-8k" || r.Steps[0].Outcome != OutcomeReady {
		t.Fatalf("%+v", r)
	}
	if got := api.made["bench-d-any1-8k"]["force_nodes"]; !reflect.DeepEqual(got, []string{"ps5-2"}) {
		t.Errorf("any:1 picked %v (most memory, then name)", got)
	}
	runs, _ := tm.store.ListRuns(RunFilter{Kind: RunKindBenchmark, Benchmark: "cold-load"})
	if len(runs) != 1 {
		t.Fatalf("%+v", runs)
	}
}

// noMaker is a scheduler API that cannot make profiles.
type noMaker struct{ API }

func TestRecipeWithoutAProfileMakerIsAnError(t *testing.T) {
	api := newFake()
	tm := newTestMgr(t, api, func(o *ManagerOptions) { o.API = noMaker{api} })
	tm.bench(coldLoadRecipe)
	no := false
	q, err := tm.m.EnqueueBenchmark("cold-load", BenchRunOptions{OnlyWhenIdle: &no, Inputs: &benchspec.Inputs{Model: "d.gguf"}})
	if err != nil {
		t.Fatal(err)
	}
	r := tm.finished(q.ID)
	if len(r.Steps) != 1 || r.Steps[0].Outcome != OutcomeError || !strings.Contains(r.Steps[0].Error, "cannot make profiles") {
		t.Fatalf("%+v", r.Steps)
	}
}

func TestPlaceOn(t *testing.T) {
	cons := consoles()
	for _, c := range []struct {
		p    string
		want string
		err  string
	}{
		{"auto", "", ""},
		{"any:2", "ps5-2,ps5-4", ""},
		{"any:3", "ps5-1,ps5-2,ps5-4", ""},
		{"all", "ps5-1,ps5-2,ps5-4", ""},
		{"nodes:ps5-4,02:00:00:00:00:01", "ps5-4,ps5-1", ""},
		{"nodes:ps5-9", "", "is not a console"},
		{"any:4", "", "only 3"},
	} {
		var p benchspec.Placement
		if err := p.UnmarshalJSON([]byte(`"` + c.p + `"`)); err != nil {
			t.Fatal(err)
		}
		got, err := placeOn(p, cons)
		if c.err != "" {
			if !errors.Is(err, ErrPlacement) || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %v", c.p, err)
			}
			continue
		}
		if err != nil || strings.Join(got, ",") != c.want {
			t.Errorf("%s: %v %v, want %s", c.p, got, err, c.want)
		}
	}
	if _, err := placeOn(benchspec.Placement{Mode: benchspec.PlaceAll}, nil); !errors.Is(err, ErrPlacement) {
		t.Errorf("all of none: %v", err)
	}
}

func TestSameProfile(t *testing.T) {
	spec := ProfileSpecOf(benchspec.Inputs{Model: "m.gguf", Ctx: 4096}, []string{"ps5-1"})
	cur := map[string]any{"kind": "llama", "model": "m.gguf", "ctx": 4096.0, "parallel": 1.0, "kv_quant": "f16", "flash_attn": false, "generated": true, "force_nodes": []any{"ps5-1"}, "extra_args": []any{}}
	if !sameProfile(cur, spec) {
		t.Error("equal profiles differ")
	}
	cur["force_nodes"] = []any{"ps5-2"}
	if sameProfile(cur, spec) {
		t.Error("other nodes are the same")
	}
}
