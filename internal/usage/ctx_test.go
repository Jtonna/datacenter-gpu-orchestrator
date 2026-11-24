package usage

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A request row keeps the profile's ctx as it was when the request came in,
// and carries the model's short name, so a run whose profile was deleted
// later still reads "qwen3.5-4b:8k".
func TestRequestCtx_StoredAtRequestTimeWithTheShortName(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)}
	r, _ := recorderT(t, t.TempDir(), c)
	b := begin("qwen-8k")
	b.ModelFile, b.Ctx = "/models/Qwen_Qwen3.5-4B-Q4_K_M.gguf", 8192
	run := r.Queued(b)
	cmd := r.Queued(begin("a-command")) // no model file, no ctx

	live := r.Live()
	if len(live) != 2 || live[0].ShortName != "qwen3.5-4b" || live[0].Ctx != 8192 || live[1].ShortName != "" || live[1].Ctx != 0 {
		t.Fatalf("live rows %+v", live)
	}
	run.Ended(done(100, 0, 10))
	cmd.Ended(done(5, 0, 1))
	if err := r.Flush(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(run.ID())
	if err != nil || got.ShortName != "qwen3.5-4b" || got.Ctx != 8192 {
		t.Fatalf("stored %+v %v", got, err)
	}
	page, err := r.List(Filter{})
	if err != nil || len(page.Requests) != 2 {
		t.Fatalf("list %+v %v", page, err)
	}
	js, _ := json.Marshal(page.Requests)
	if !strings.Contains(string(js), `"short_name":"qwen3.5-4b"`) || !strings.Contains(string(js), `"ctx":8192`) || strings.Count(string(js), `"ctx"`) != 1 {
		t.Fatalf("rows as JSON: %s", js)
	}
}

func columnsOf(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		cols = append(cols, n)
	}
	return strings.Join(cols, ",")
}

// The ctx column has its own ledger (usage_ext_migrations), so
// usage_migrations stays at 4 and the previous image (v44), which knows
// version 4, keeps its history on with the newer file. Its rows have no ctx;
// this build reads them with ctx 0.
func TestRequestCtx_OwnLedgerAndThePreviousImage(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)}
	st := openT(t, Options{Dir: dir, Now: c.now})
	var v, ext int
	st.DB().QueryRow(`SELECT MAX(version) FROM usage_migrations`).Scan(&v)
	st.DB().QueryRow(`SELECT MAX(version) FROM usage_ext_migrations`).Scan(&ext)
	if v != 4 || SchemaVersion() != 4 || ext != ExtVersion() || ext != 1 {
		t.Fatalf("usage_migrations %d (know %d), usage_ext_migrations %d", v, SchemaVersion(), ext)
	}
	if !strings.HasSuffix(columnsOf(t, st.DB(), "client_requests"), ",bench_run,ctx") {
		t.Fatalf("columns %s", columnsOf(t, st.DB(), "client_requests"))
	}
	var begins int64
	st.DB().QueryRow(`SELECT value FROM usage_meta WHERE key = 'history_begins_at'`).Scan(&begins)
	st.Close()

	// The previous image on this file: its ledger check (the same list,
	// version 4) passes, so its history stays on, and it writes rows with
	// its own column list.
	db, err := sql.Open("sqlite", "file:"+dir+"/"+FileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, c.now(), nil); err != nil {
		t.Fatalf("the previous image's migration refused the file: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO client_requests (`+rowColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		9001, StatusDone, nil, nil, 200, KindV1, "/v1/chat/completions", 0, c.now().UnixMilli(), nil, nil, c.now().UnixMilli(),
		"qwen-8k", nil, "/models/Qwen_Qwen3.5-4B-Q4_K_M.gguf", 1, 0, 1, nil, nil, nil, nil, 0, 0, nil, nil, "[]", "anonymous", "default", nil, nil); err != nil {
		t.Fatalf("the previous image's insert: %v", err)
	}
	// A file the previous image made from the start: no ctx column, no ledger.
	if _, err := db.Exec(`ALTER TABLE client_requests DROP COLUMN ctx`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE usage_ext_migrations`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Forward again: the column comes back, the row is kept and reads ctx 0,
	// the history keeps its beginning.
	st2 := openT(t, Options{Dir: dir, Now: c.now})
	r := NewRecorder(st2)
	got, err := r.Get(9001)
	if err != nil || got.Ctx != 0 || got.ShortName != "qwen3.5-4b" {
		t.Fatalf("the previous image's row %+v %v", got, err)
	}
	var begins2 int64
	st2.DB().QueryRow(`SELECT value FROM usage_meta WHERE key = 'history_begins_at'`).Scan(&begins2)
	st2.DB().QueryRow(`SELECT MAX(version) FROM usage_migrations`).Scan(&v)
	st2.DB().QueryRow(`SELECT MAX(version) FROM usage_ext_migrations`).Scan(&ext)
	if begins2 != begins || v != 4 || ext != 1 {
		t.Fatalf("begins %d then %d; usage_migrations %d; ext %d", begins, begins2, v, ext)
	}
}

// A newer build's extra ledger step is left alone: the store is off and the
// file is untouched, as for usage_migrations.
func TestRequestCtx_NewerExtLedgerIsKeptAndOff(t *testing.T) {
	dir := t.TempDir()
	st := openT(t, Options{Dir: dir})
	exec(t, st, `INSERT INTO usage_ext_migrations (version, name, applied_at) VALUES (?, 'future', 0)`, ExtVersion()+3)
	st.Close()
	log, buf := newLog()
	st2 := openT(t, Options{Dir: dir, Logger: log})
	if st2.On() || st2.Stats().MovedAside != "" || !strings.Contains(buf.String(), "newer scheduler") {
		t.Fatalf("on=%v stats=%+v log:\n%s", st2.On(), st2.Stats(), buf)
	}
}
