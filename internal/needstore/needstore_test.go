package needstore

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

var t0 = time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

func sample(profile string) Record {
	return Record{Schema: RecordSchema, Profile: profile, Kind: "llama", Model: profile + ".gguf", ModelBytes: 1 << 30,
		Ctx: 8192, Parallel: 1, KVQuant: "f16", TotalMiB: 11800, Placement: []string{"a", "b"}, Split: []float64{0.5, 0.5},
		Nodes: []Node{{ID: "a", Fraction: 0.5, MiB: 5900}, {ID: "b", Fraction: 0.5, MiB: 5900}},
		Cause: CauseLoad, MeasuredAt: t0, Release: "0.1.3"}
}

func openDB(t *testing.T) *bench.Store {
	t.Helper()
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// A record survives the round trip, Put replaces the shape's row, Delete
// removes it, and opening twice migrates nothing.
func TestStoreRoundTrip(t *testing.T) {
	db := openDB(t)
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(sample("m")); err != nil {
		t.Fatal(err)
	}
	r := sample("m")
	r.TotalMiB, r.Cause = 13000, "watchdog"
	if err := st.Put(r); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(sample("n")); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := st.All()
	if err != nil || len(skipped) != 0 || len(got) != 2 {
		t.Fatalf("all %+v skipped %+v err %v", got, skipped, err)
	}
	m := got[0]
	if m.Profile != "m" || m.TotalMiB != 13000 || m.Cause != "watchdog" || !m.MeasuredAt.Equal(t0) || m.Release != "0.1.3" ||
		len(m.Nodes) != 2 || m.Nodes[1].MiB != 5900 || m.Split[0] != 0.5 || m.ModelBytes != 1<<30 {
		t.Fatalf("round trip %+v", m)
	}
	if err := st.Delete(sample("m").Shape()); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("absent"); err != nil {
		t.Fatalf("deleting an absent row: %v", err)
	}
	got, _, _ = st.All()
	if len(got) != 1 || got[0].Profile != "n" {
		t.Fatalf("after delete %+v", got)
	}
}

// A row that cannot be read, or that has another schema, is skipped with its
// reason and does not stop the others.
func TestStoreSkipsBadRows(t *testing.T) {
	db := openDB(t)
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(sample("good")); err != nil {
		t.Fatal(err)
	}
	err = db.Write(func(tx *sql.Tx) error {
		for _, row := range [][2]string{
			{"corrupt", `{"schema":1,"profile":"corrupt","total_mib":"lots"`},
			{"older", `{"schema":0,"profile":"older","total_mib":9000}`},
			{"newer", `{"schema":99,"profile":"newer","total_mib":9000}`},
			{"empty", `{"schema":1,"profile":"empty","total_mib":0}`},
		} {
			if _, err := tx.Exec(`INSERT INTO learned_needs (profile, updated_at, record) VALUES (?,?,?)`, row[0], 0, row[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, skipped, err := st.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Profile != "good" {
		t.Fatalf("kept %+v", got)
	}
	if len(skipped) != 4 {
		t.Fatalf("skipped %+v", skipped)
	}
	for _, s := range skipped {
		if s.Profile == "" || s.Reason == "" {
			t.Fatalf("skip without a name or reason: %+v", s)
		}
	}
}

// openV1 writes a database as a build before shape keys left it (learned-need
// tables at version 1) with the given rows under their profiles.
func openV1(t *testing.T, db *bench.Store, rows map[string]string, at map[string]int64) {
	t.Helper()
	err := db.Write(func(tx *sql.Tx) error {
		for _, q := range []string{
			`CREATE TABLE needs_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
			`CREATE TABLE learned_needs (profile TEXT PRIMARY KEY, updated_at INTEGER NOT NULL, record TEXT NOT NULL)`,
			`INSERT INTO needs_migrations (version, applied_at) VALUES (1, 0)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		for profile, doc := range rows {
			if _, err := tx.Exec(`INSERT INTO learned_needs (profile, updated_at, record) VALUES (?,?,?)`, profile, at[profile], doc); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A database written before peaks were kept opens unchanged: its rows read
// with no peak. A record with peaks keeps them through the round trip.
func TestStorePeaksAndOldRows(t *testing.T) {
	db := openDB(t)
	old := `{"schema":1,"profile":"old","kind":"llama","model":"old.gguf","ctx":8192,"parallel":1,"kv_quant":"f16","flash_attn":false,` +
		`"total_mib":11800,"placement":["a","b"],"split":[0.5,0.5],"nodes":[{"node":"a","fraction":0.5,"mib":5900},{"node":"b","fraction":0.5,"mib":5900}],` +
		`"cause":"load","measured_at":"2026-11-20T09:30:00Z","release":"0.1.3"}`
	openV1(t, db, map[string]string{"old": old}, nil)
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	r := sample("new")
	r.Nodes[0].PeakMiB, r.Nodes[1].PeakMiB = 6151, 6100
	r.PeakAt = t0.Add(time.Hour)
	if err := st.Put(r); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := st.All()
	if err != nil || len(skipped) != 0 || len(got) != 2 {
		t.Fatalf("all %+v skipped %+v err %v", got, skipped, err)
	}
	n, o := got[0], got[1]
	if n.Profile != "new" || n.Nodes[0].PeakMiB != 6151 || n.Nodes[1].PeakMiB != 6100 || !n.PeakAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("peaks lost: %+v", n)
	}
	if o.Profile != "old" || o.TotalMiB != 11800 || o.Nodes[0].MiB != 5900 || o.Nodes[0].PeakMiB != 0 || !o.PeakAt.IsZero() {
		t.Fatalf("old row read as %+v", o)
	}
}

// The shape key: the settings that change the need are in it, the profile
// name, the model size and the placement are not; an empty KV type is f16
// and no slot count is one.
func TestShapeKey(t *testing.T) {
	a := sample("a")
	b := sample("b")
	b.Model, b.ModelBytes, b.Placement, b.Nodes, b.Split = a.Model, 2<<30, []string{"c"}, nil, nil
	if a.Shape() != b.Shape() {
		t.Fatalf("same shape differs: %s %s", a.Shape(), b.Shape())
	}
	c := a
	c.KVQuant, c.Parallel = "", 0
	if c.Shape() != a.Shape() {
		t.Fatalf("defaults differ: %s %s", c.Shape(), a.Shape())
	}
	for name, mod := range map[string]func(*Record){
		"kind": func(r *Record) { r.Kind = "command" }, "model": func(r *Record) { r.Model = "x.gguf" },
		"ctx": func(r *Record) { r.Ctx = 16384 }, "parallel": func(r *Record) { r.Parallel = 2 },
		"kv_quant": func(r *Record) { r.KVQuant = "q8_0" }, "flash_attn": func(r *Record) { r.FlashAttn = true },
		"extra_args": func(r *Record) { r.ExtraArgs = []string{"--no-mmap"} },
	} {
		d := sample("a")
		mod(&d)
		if d.Shape() == a.Shape() {
			t.Errorf("%s does not change the shape", name)
		}
	}
}

// Opening a database whose rows are keyed by profile moves every usable row
// to its shape; two profiles of one shape keep the newer row; a row that
// cannot be read stays under its profile and is reported, not lost.
func TestStoreMigratesProfileRowsToShapes(t *testing.T) {
	db := openDB(t)
	doc := func(r Record) string {
		b, _ := json.Marshal(r)
		return string(b)
	}
	older, newer, other := sample("older"), sample("newer"), sample("other")
	newer.Model, newer.TotalMiB = older.Model, 12345 // same shape as "older"
	openV1(t, db, map[string]string{
		"older": doc(older), "newer": doc(newer), "other": doc(other),
		"corrupt": `{"schema":1,"profile":"corrupt","total_mib":"lots"`,
	}, map[string]int64{"older": 1, "newer": 2, "other": 1, "corrupt": 1})
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	got, skipped, err := st.All()
	if err != nil || len(got) != 2 {
		t.Fatalf("all %+v skipped %+v err %v", got, skipped, err)
	}
	byShape := map[string]Record{}
	for _, r := range got {
		byShape[r.Shape()] = r
	}
	if r := byShape[older.Shape()]; r.Profile != "newer" || r.TotalMiB != 12345 {
		t.Fatalf("shared shape kept %+v, want the newer row", r)
	}
	if r := byShape[other.Shape()]; r.Profile != "other" || r.TotalMiB != 11800 {
		t.Fatalf("other %+v", r)
	}
	if len(skipped) != 1 || skipped[0].Profile != "corrupt" {
		t.Fatalf("skipped %+v", skipped)
	}
	var left int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM learned_needs`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("rows left under a profile: %d %v", left, err)
	}
	// opening again changes nothing
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := st.All(); len(again) != 2 {
		t.Fatalf("after a second open %+v", again)
	}
}

// A database that holds a usable row in both tables (an older build wrote
// under the profile after the migration) gives each measurement once: the
// newer row wins, whichever table it is in. Delete removes both.
func TestStoreBothTablesNewerWins(t *testing.T) {
	for _, legacyNewer := range []bool{true, false} {
		db := openDB(t)
		st, err := Open(db)
		if err != nil {
			t.Fatal(err)
		}
		r := sample("m")
		if err := st.Put(r); err != nil { // updated_at is now
			t.Fatal(err)
		}
		legacy := sample("legacy") // same shape as "m", other profile name
		legacy.Model, legacy.TotalMiB = r.Model, 12500
		b, _ := json.Marshal(legacy)
		at := time.Now().Add(-time.Hour).UnixMilli()
		if legacyNewer {
			at = time.Now().Add(time.Hour).UnixMilli()
		}
		if err := db.Write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO learned_needs (profile, updated_at, record) VALUES (?,?,?)`, "legacy", at, string(b))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		got, skipped, err := st.All()
		if err != nil || len(skipped) != 0 || len(got) != 1 {
			t.Fatalf("legacy newer %v: all %+v skipped %+v err %v", legacyNewer, got, skipped, err)
		}
		if want := map[bool]string{true: "legacy", false: "m"}[legacyNewer]; got[0].Profile != want {
			t.Fatalf("legacy newer %v: kept %s, want %s", legacyNewer, got[0].Profile, want)
		}
		if err := st.Delete(r.Shape()); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := st.All(); len(got) != 0 {
			t.Fatalf("after delete %+v", got)
		}
	}
}
