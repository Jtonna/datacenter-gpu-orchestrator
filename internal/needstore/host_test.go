package needstore

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

func hostSample(shape, node string, devices, mib int) HostOverhead {
	h := HostOverhead{Shape: shape, Node: node, Devices: devices, LogHostMiB: 120, Release: "0.1.9",
		LlamaBuild: "aaaaaaa1", ModelBytes: 1 << 30, Profile: "q"}
	h.Add(HostReading{MiB: mib, At: t0, Cause: CauseLoad}, false)
	return h
}

func ledger(t *testing.T, db *bench.Store, table string) []int {
	t.Helper()
	rows, err := db.DB().Query(`SELECT version FROM ` + table + ` ORDER BY version`)
	if err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// openV51 builds the file the release before device counts leaves: its
// three ledgers at 1 and one host row per (shape, node).
func openV51(t *testing.T, db *bench.Store, rows ...[2]any) {
	t.Helper()
	err := db.Write(func(tx *sql.Tx) error {
		for table, list := range map[string][]func(*sql.Tx) error{"needs_migrations": migrations, "needs_shape_migrations": shapeMigrations,
			"needs_host_migrations": hostMigrations} {
			if err := migrate(tx, table, list); err != nil {
				return err
			}
		}
		for _, r := range rows {
			if _, err := tx.Exec(`INSERT INTO needs_host_overhead (shape, node, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause)
				VALUES (?,?,?,150,?,'0.1.9','aaaaaaa1',1073741824,'q','load')`, r[0], "a", r[1], t0.UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func oldRow(t *testing.T, db *bench.Store, shape, node string) (mib int, at int64, ok bool) {
	t.Helper()
	err := db.DB().QueryRow(`SELECT mib, measured_at FROM needs_host_overhead WHERE shape = ? AND node = ?`, shape, node).Scan(&mib, &at)
	return mib, at, err == nil
}

// The keep-the-higher rule.
func TestHostOverhead_KeepTheHigher(t *testing.T) {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	h := HostOverhead{}
	steps := []struct {
		mib   int
		reset bool
		want  string
		fig   int
	}{
		{401, false, HostFirst, 401},
		{298, false, HostKept, 401},   // lower: kept
		{450, false, HostRaised, 450}, // higher: replaces
		{300, false, HostKept, 450},
		{320, false, HostKept, 450},    // two lower, the third reading is 450 itself
		{310, false, HostLowered, 320}, // three consecutive lower: the highest of them
		{200, true, HostReset, 200},    // a new llama build or model file
	}
	for i, s := range steps {
		if got := h.Add(HostReading{MiB: s.mib, At: at(i)}, s.reset); got != s.want || h.MiB != s.fig {
			t.Fatalf("step %d (%d): %s figure %d, want %s %d; readings %+v", i, s.mib, got, h.MiB, s.want, s.fig, h.Readings)
		}
	}
	if len(h.Readings) != 1 || !h.MeasuredAt.Equal(at(6)) {
		t.Fatalf("after reset %+v", h)
	}
	// The date in force is the reading's the figure came from.
	h = HostOverhead{}
	h.Add(HostReading{MiB: 401, At: at(0)}, false)
	h.Add(HostReading{MiB: 298, At: at(1)}, false)
	if h.MiB != 401 || !h.MeasuredAt.Equal(at(0)) || len(h.Readings) != 2 || h.Readings[0].MiB != 298 {
		t.Fatalf("%+v", h)
	}
}

// Rows by shape, node and device count under their own ledger; the ledgers
// the release before device counts checks stay at 1. A one-device row is
// written through to that release's table (the figure in force); a
// two-device row is not. Delete of a shape removes both.
func TestHostOverhead_ByCountAndWriteThrough(t *testing.T) {
	db := openDB(t)
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"needs_migrations": 1, "needs_shape_migrations": 1, "needs_host_migrations": 1, "needs_host_count_migrations": 1} {
		if got := ledger(t, db, table); len(got) != 1 || got[0] != want {
			t.Fatalf("%s at %v, want [%d]", table, got, want)
		}
	}
	one := hostSample("S", "a", 1, 401)
	one.Add(HostReading{MiB: 298, At: t0.Add(time.Hour), Cause: CauseLoad}, false)
	if err := st.PutHost(one); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHost(hostSample("S", "a", 2, 700)); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHost(hostSample("T", "b", 1, 500)); err != nil {
		t.Fatal(err)
	}
	got, err := st.AllHost()
	if err != nil || len(got) != 3 {
		t.Fatalf("all %+v %v", got, err)
	}
	a1 := got[0]
	if a1.Devices != 1 || a1.MiB != 401 || !a1.MeasuredAt.Equal(t0) || len(a1.Readings) != 2 || a1.Readings[0].MiB != 298 ||
		!a1.Readings[0].At.Equal(t0.Add(time.Hour)) || a1.LlamaBuild != "aaaaaaa1" || a1.ModelBytes != 1<<30 {
		t.Fatalf("round trip %+v", a1)
	}
	if got[1].Devices != 2 || got[1].MiB != 700 {
		t.Fatalf("%+v", got[1])
	}
	if mib, at, ok := oldRow(t, db, "S", "a"); !ok || mib != 401 || at != t0.Add(time.Hour).UnixMilli() {
		t.Fatalf("write-through %d %d %v: the figure in force, dated by the newest reading", mib, at, ok)
	}
	if err := st.PutHost(HostOverhead{Shape: "S", Node: "a", Devices: 0, MiB: 1}); err == nil {
		t.Fatal("no device count is refused")
	}
	if err := st.Delete("S"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.AllHost(); len(got) != 1 || got[0].Shape != "T" {
		t.Fatalf("after delete %+v", got)
	}
	if _, _, ok := oldRow(t, db, "S", "a"); ok {
		t.Fatal("the older table's row stays after delete")
	}
}

// The file of the release before device counts: every row becomes the
// one-device row, with no readings (its history is read at start); that
// release's table and ledgers are as they were. A reading that release
// writes after a rollback is taken in at the next start by the rule; this
// build's own write-through is not.
func TestHostOverhead_MigrationAndRollback(t *testing.T) {
	db := openDB(t)
	openV51(t, db, [2]any{"S", 298}, [2]any{"T", 600})
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.AllHost()
	if err != nil || len(got) != 2 || got[0].Devices != 1 || got[0].MiB != 298 || !got[0].Pending() || !got[0].MeasuredAt.Equal(t0) {
		t.Fatalf("migrated %+v %v", got, err)
	}
	for _, table := range []string{"needs_migrations", "needs_shape_migrations", "needs_host_migrations"} {
		if v := ledger(t, db, table); !slices.Equal(v, []int{1}) {
			t.Fatalf("%s %v", table, v)
		}
	}
	// This build sets the figure in force to 401 (start-up correction) and
	// writes it through.
	h := got[0]
	h.Readings = []HostReading{{MiB: 298, At: t0}, {MiB: 401, At: t0.Add(-time.Hour)}}
	h.MiB, h.MeasuredAt = 401, t0.Add(-time.Hour)
	if err := st.PutHost(h); err != nil {
		t.Fatal(err)
	}
	if mib, _, _ := oldRow(t, db, "S", "a"); mib != 401 {
		t.Fatalf("older table %d", mib)
	}
	// Back on the older release: it replaces its row with a reading of 350.
	if _, err := db.DB().Exec(`UPDATE needs_host_overhead SET mib = 350, measured_at = ? WHERE shape = 'S'`, t0.Add(2*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil { // forward again
		t.Fatal(err)
	}
	got, _ = st.AllHost()
	if got[0].MiB != 401 || len(got[0].Readings) != 3 || got[0].Readings[0].MiB != 350 {
		t.Fatalf("after the rollback's reading %+v", got[0])
	}
	// A second start takes in nothing more (the write-through is dated by
	// the newest reading).
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.AllHost(); len(again[0].Readings) != 3 || again[0].MiB != 401 {
		t.Fatalf("second start %+v", again[0])
	}
}
