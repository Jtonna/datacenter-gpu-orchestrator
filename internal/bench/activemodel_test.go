package bench

import (
	"reflect"
	"testing"
)

// A model file is not deleted while a queued, running or paused run touches
// it: by a step on the file or a profile of it, or by a suite snapshot that
// names one of them before its steps exist. Finished runs do not count.
func TestActiveRunsNaming(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, q := range []string{
		`INSERT INTO runs (id, suite_id, suite_name, state, queued_at, started_at, finished_at, version, suite) VALUES
		 ('r-done','s','S','completed',1,1,2,'v','{}'),
		 ('r-step','s','S','running',2,2,0,'v','{}'),
		 ('r-prof','s','S','paused',3,3,0,'v','{}'),
		 ('r-queued','s','S','queued',4,0,0,'v','{"steps":[{"profile":"alpha"}]}'),
		 ('r-other','s','S','queued',5,0,0,'v','{"steps":[{"profile":"alphabet"}]}')`,
		`INSERT INTO run_steps (run_id, idx, benchmark_id, name, profile, state, outcome, started_at, finished_at, model_file) VALUES
		 ('r-done',0,'b','n','alpha','completed','',1,2,'a.gguf'),
		 ('r-step',0,'b','n','zz','running','',2,0,'a.gguf'),
		 ('r-prof',0,'b','n','alpha','completed','',3,4,'')`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ActiveRunsNaming("a.gguf", []string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"r-step", "r-prof", "r-queued"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, _ := st.ActiveRunsNaming("b.gguf", nil); len(got) != 0 {
		t.Fatalf("b.gguf: %v", got)
	}
}
