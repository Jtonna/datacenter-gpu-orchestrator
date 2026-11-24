package usage

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelname"
)

type rowWatch struct{ rows []Request }

func (w *rowWatch) Watching() bool                 { return true }
func (w *rowWatch) Changed(kind string, r Request) { w.rows = append(w.rows, r) }

// short_name is not stored: it is derived when a row is read, with the
// namer the API sets (the catalog as it is then). A row written before the
// catalog had a colliding file reads with the suffix once that file is
// there, as /api/models does.
func TestShortName_DerivedOnReadWithTheCatalogsNamer(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)}
	r, _ := recorderT(t, t.TempDir(), c)
	w := &rowWatch{}
	r.SetWatcher(w)
	it := "/models/Gemma-4-27B-it-Q4_K_M.gguf"
	catalog := []string{it}
	r.SetNamer(func(file string) string { return modelname.NewNamer(catalog).Name(file) })

	b := begin("gemma")
	b.ModelFile, b.Ctx = it, 32768
	run := r.Queued(b)
	if live := r.Live(); len(live) != 1 || live[0].ShortName != "gemma-4-27b" {
		t.Fatalf("live %+v", live)
	}
	run.Ended(done(10, 0, 1))
	if err := r.Flush(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if len(w.rows) == 0 || w.rows[0].ShortName != "gemma-4-27b" {
		t.Fatalf("watched %+v", w.rows)
	}
	if got, _ := r.Get(run.ID()); got.ShortName != "gemma-4-27b" {
		t.Fatalf("before the colliding file: %q", got.ShortName)
	}

	catalog = append(catalog, "/models/gemma-4-27b-Q8_0.gguf") // the base model arrives
	got, _ := r.Get(run.ID())
	page, _ := r.List(Filter{})
	if got.ShortName != "gemma-4-27b-it" || got.Ctx != 32768 || len(page.Requests) != 1 || page.Requests[0].ShortName != "gemma-4-27b-it" {
		t.Fatalf("after the colliding file: get %q, list %+v", got.ShortName, page.Requests)
	}
	// The row as stored: no short name column; the file name is kept.
	var cols string
	r.Store().DB().QueryRow(`SELECT group_concat(name) FROM pragma_table_info('client_requests') WHERE name LIKE '%short%'`).Scan(&cols)
	if cols != "" || got.ModelFile != it {
		t.Fatalf("stored columns %q, file %q", cols, got.ModelFile)
	}
}
