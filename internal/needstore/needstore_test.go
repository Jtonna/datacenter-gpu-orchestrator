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

// Opening a database whose rows are keyed by profile copies every usable row
// to its shape; two profiles of one shape keep the higher total (here also
// the newer row); a row that cannot be read is reported, not lost. Every
// row stays under its profile (an older build still reads them).
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
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM learned_needs`).Scan(&left); err != nil || left != 4 {
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

func docOf(r Record) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func maxVersion(t *testing.T, db *bench.Store) int {
	t.Helper()
	var v int
	if err := db.DB().QueryRow(`SELECT COALESCE(MAX(version),0) FROM needs_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// SB1: one shape keeps one row per device count; a 1-node measurement does
// not replace a 3-node one.
func TestStore_OneRowPerDeviceCount(t *testing.T) {
	st, err := Open(openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	three := sample("three")
	three.Placement, three.Split, three.Nodes, three.TotalMiB = []string{"a", "b", "c"}, []float64{0.34, 0.33, 0.33}, nil, 13281
	one := sample("one")
	one.Model, one.Placement, one.Split, one.Nodes, one.TotalMiB = three.Model, []string{"a"}, []float64{1}, nil, 7600
	for _, r := range []Record{three, one} {
		if err := st.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := st.All()
	if err != nil || len(got) != 2 {
		t.Fatalf("all %+v err %v", got, err)
	}
	by := map[int]Record{}
	for _, r := range got {
		by[r.DeviceCount()] = r
	}
	if by[1].TotalMiB != 7600 || by[3].TotalMiB != 13281 {
		t.Fatalf("per device count %+v", by)
	}
}

// SB1 and SS1: the migration keeps the higher total when two profiles of one
// shape and device count have rows (not the newer), and leaves the profile
// table and its version as they were: an older build still opens the
// database and reads its own rows.
func TestStore_MigrationKeepsTheHigherAndLeavesTheOldTable(t *testing.T) {
	db := openDB(t)
	high, low := sample("high"), sample("low")
	low.Model, high.TotalMiB, low.TotalMiB = high.Model, 12800, 11000
	openV1(t, db, map[string]string{"high": docOf(high), "low": docOf(low)}, map[string]int64{"high": 1, "low": 2})
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.All()
	if len(got) != 1 || got[0].TotalMiB != 12800 || got[0].Profile != "high" {
		t.Fatalf("all %+v, want the higher total 12800", got)
	}
	var left int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM learned_needs`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("rows left under their profiles: %d %v, want both", left, err)
	}
	if v := maxVersion(t, db); v != 1 {
		t.Fatalf("learned_needs at version %d: an older build (which knows 1) cannot open it", v)
	}
}

// SS1: every write also goes to the profile table under the measuring
// profile, so an older build reads that profile's latest measurement; such
// rows are not read back as rows an older build wrote.
func TestStore_WritesThroughForAnOlderBuild(t *testing.T) {
	db := openDB(t)
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	r := sample("m")
	r.TotalMiB = 12000
	if err := st.Put(r); err != nil {
		t.Fatal(err)
	}
	r.TotalMiB = 11500 // a later, lower figure replaces it (the scheduler decides)
	if err := st.Put(r); err != nil {
		t.Fatal(err)
	}
	var doc string
	if err := db.DB().QueryRow(`SELECT record FROM learned_needs WHERE profile = 'm'`).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	var old Record
	if why := usable("m", doc, &old); why != "" || old.TotalMiB != 11500 {
		t.Fatalf("the older build reads %+v (%s), want 11500", old, why)
	}
	st, err = Open(db) // a restart: the write-through row is not imported over the shape row
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.All(); len(got) != 1 || got[0].TotalMiB != 11500 {
		t.Fatalf("after a restart %+v", got)
	}
}

// SB1 and SS1: a row an older build wrote after a rollback is read at the
// next start, and the higher total wins against the shape's row.
func TestStore_RowsAnOlderBuildWroteKeepTheHigher(t *testing.T) {
	for _, total := range []int{12500, 11000} {
		db := openDB(t)
		st, err := Open(db)
		if err != nil {
			t.Fatal(err)
		}
		r := sample("m")
		if err := st.Put(r); err != nil { // 11800
			t.Fatal(err)
		}
		legacy := sample("legacy")
		legacy.Model, legacy.TotalMiB = r.Model, total
		if err := db.Write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO learned_needs (profile, updated_at, record) VALUES (?,?,?)`,
				"legacy", time.Now().Add(time.Hour).UnixMilli(), docOf(legacy))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		st, err = Open(db)
		if err != nil {
			t.Fatal(err)
		}
		got, _, _ := st.All()
		want := max(total, 11800)
		if len(got) != 1 || got[0].TotalMiB != want {
			t.Fatalf("older build wrote %d: all %+v, want %d", total, got, want)
		}
		if err := st.Delete(r.Shape()); err != nil {
			t.Fatal(err)
		}
		st, _ = Open(db)
		if got, _, _ := st.All(); len(got) != 0 {
			t.Fatalf("after delete and a restart %+v", got)
		}
	}
}

// SS2: a shape keeps at most MaxRowsPerShape rows; the oldest go first.
func TestStore_RowsPerShapeAreCapped(t *testing.T) {
	st, err := Open(openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	for k := 1; k <= MaxRowsPerShape+2; k++ {
		r := sample("m")
		r.Placement, r.Nodes, r.Split, r.TotalMiB = nil, nil, nil, 10000+k
		r.Devices = k
		if err := st.Put(r); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct updated_at
	}
	got, _, _ := st.All()
	if len(got) != MaxRowsPerShape {
		t.Fatalf("%d rows, want %d", len(got), MaxRowsPerShape)
	}
	for _, r := range got {
		if r.DeviceCount() <= 2 {
			t.Fatalf("an oldest row was kept: %+v", r)
		}
	}
}

// SS2: the llama build a measurement was made with survives the round trip.
func TestStore_KeepsTheLlamaBuild(t *testing.T) {
	st, err := Open(openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	r := sample("m")
	r.LlamaBuild = "a01665d3"
	if err := st.Put(r); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.All(); len(got) != 1 || got[0].LlamaBuild != "a01665d3" {
		t.Fatalf("all %+v", got)
	}
}

// A database a development build of this branch left at learned-need
// version 2 (rows in needs_by_shape) is brought back to version 1, its rows
// kept.
func TestStore_ADevelopmentVersion2DatabaseOpens(t *testing.T) {
	db := openDB(t)
	openV1(t, db, nil, nil)
	r := sample("m")
	if err := db.Write(func(tx *sql.Tx) error {
		for _, q := range []string{
			`CREATE TABLE needs_by_shape (shape TEXT PRIMARY KEY, profile TEXT NOT NULL, updated_at INTEGER NOT NULL, record TEXT NOT NULL)`,
			`INSERT INTO needs_migrations (version, applied_at) VALUES (2, 0)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO needs_by_shape (shape, profile, updated_at, record) VALUES (?,?,?,?)`, r.Shape(), "m", 1, docOf(r))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.All(); len(got) != 1 || got[0].TotalMiB != 11800 {
		t.Fatalf("all %+v", got)
	}
	if v := maxVersion(t, db); v != 1 {
		t.Fatalf("version %d, want 1", v)
	}
}

// TS2 (third review): the row written through for an older build is the
// highest total over the shape's device counts, not the latest count's:
// the old binary has one figure for every count, and must never place on
// less than either binary would.
func TestStore_WriteThroughIsTheShapesHighest(t *testing.T) {
	db := openDB(t)
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	three := sample("x")
	three.Placement, three.Split, three.Devices, three.TotalMiB = []string{"a", "b", "c"}, []float64{1, 1, 1}, 3, 9000
	three.Nodes = []Node{{ID: "a", Fraction: 1.0 / 3, MiB: 3000}, {ID: "b", Fraction: 1.0 / 3, MiB: 3000}, {ID: "c", Fraction: 1.0 / 3, MiB: 3000}}
	one := sample("x")
	one.Placement, one.Split, one.Devices, one.TotalMiB = []string{"a"}, []float64{1}, 1, 7600
	one.Nodes = []Node{{ID: "a", Fraction: 1, MiB: 7600}}
	for _, r := range []Record{three, one} {
		if err := st.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	var doc string
	if err := db.DB().QueryRow(`SELECT record FROM learned_needs WHERE profile = 'x'`).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	var old Record
	if why := usable("x", doc, &old); why != "" || old.TotalMiB != 9000 || old.Profile != "x" {
		t.Fatalf("the older build reads %+v (%s), want the 3-device 9000", old, why)
	}
	if got, _, _ := st.All(); len(got) != 2 {
		t.Fatalf("shape rows %+v, want both counts", got)
	}
}
