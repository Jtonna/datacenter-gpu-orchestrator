package bench

import (
	"slices"
	"strings"
	"testing"
)

// Models slice 1: the runs list filters on a model file (the step's file,
// or for a step recorded without one, its profile among that file's
// profiles), and the completed runs' (file, profile) pairs are listed once.
func TestStore_RunsByModelAndCompletedRunModels(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run := func(id, state string, steps ...[2]string) {
		if err := st.CreateRun(&Run{ID: id, SuiteID: "s", State: state, QueuedAt: int64(len(id))}); err != nil {
			t.Fatal(err)
		}
		for i, s := range steps {
			if err := st.AddStep(&Step{RunID: id, Idx: i, BenchmarkID: "b", Profile: s[1], ModelFile: s[0], State: "done"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	run("r1", RunCompleted, [2]string{"a.gguf", "a:8k"}, [2]string{"a.gguf", "a:8k"}, [2]string{"b.gguf", "b:8k"})
	run("r2", RunFailed, [2]string{"a.gguf", "a:4k"})
	run("r3", RunCompleted, [2]string{"", "a:old"}) // recorded before steps carried their file
	run("r4", RunCompleted, [2]string{"b.gguf", "b:8k"})

	ids := func(f RunFilter) []string {
		rs, err := st.ListRuns(f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rs {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(RunFilter{Model: "a.gguf"}); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Errorf("a.gguf: %v", got)
	}
	if got := ids(RunFilter{Model: "a.gguf", ModelProfiles: []string{"a:old", "a:8k"}}); !slices.Equal(got, []string{"r1", "r2", "r3"}) {
		t.Errorf("a.gguf with its profiles: %v", got)
	}
	if got := ids(RunFilter{Model: "a.gguf", State: RunCompleted}); !slices.Equal(got, []string{"r1"}) {
		t.Errorf("a.gguf completed: %v", got)
	}
	if got := ids(RunFilter{Model: "none.gguf"}); len(got) != 0 {
		t.Errorf("unknown file: %v", got)
	}
	ms, err := st.CompletedRunModels()
	if err != nil {
		t.Fatal(err)
	}
	want := []RunStepModel{{"r1", "a.gguf", "a:8k"}, {"r1", "b.gguf", "b:8k"}, {"r3", "", "a:old"}, {"r4", "b.gguf", "b:8k"}}
	slices.SortFunc(ms, func(x, y RunStepModel) int {
		if x.RunID != y.RunID {
			return strings.Compare(x.RunID, y.RunID)
		}
		return strings.Compare(x.ModelFile, y.ModelFile)
	})
	if !slices.Equal(ms, want) {
		t.Errorf("completed: %v", ms)
	}
}
