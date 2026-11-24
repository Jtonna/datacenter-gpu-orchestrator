package needstore

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

func hostSample(shape, node string, mib int) HostOverhead {
	return HostOverhead{Shape: shape, Node: node, MiB: mib, LogHostMiB: 120, MeasuredAt: t0, Release: "0.1.9",
		LlamaBuild: "aaaaaaa1", ModelBytes: 1 << 30, Profile: "q", Cause: CauseLoad}
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

// The measured host overhead lives in its own table under its own ledger
// (needs_host_migrations v1): the two ledgers an older build checks stay at
// version 1, so the older build opens the file and never looks at the new
// table. One row per shape and node; a write replaces it; Delete of a shape
// removes its host rows with its needs.
func TestHostOverhead_OwnLedgerAndRoundTrip(t *testing.T) {
	db := openDB(t)
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil { // twice migrates nothing
		t.Fatal(err)
	}
	for table, want := range map[string]int{"needs_migrations": 1, "needs_shape_migrations": 1, "needs_host_migrations": 1} {
		if got := ledger(t, db, table); len(got) != 1 || got[0] != want {
			t.Fatalf("%s at %v, want [%d]", table, got, want)
		}
	}
	if err := st.PutHost(hostSample("S", "a", 700)); err != nil {
		t.Fatal(err)
	}
	r := hostSample("S", "a", 640)
	r.Cause = "watchdog"
	if err := st.PutHost(r); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHost(hostSample("S", "b", 900)); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHost(hostSample("T", "a", 500)); err != nil {
		t.Fatal(err)
	}
	got, err := st.AllHost()
	if err != nil || len(got) != 3 {
		t.Fatalf("all %+v %v", got, err)
	}
	a := got[0]
	if a.Shape != "S" || a.Node != "a" || a.MiB != 640 || a.Cause != "watchdog" || a.LogHostMiB != 120 || !a.MeasuredAt.Equal(t0) ||
		a.Release != "0.1.9" || a.LlamaBuild != "aaaaaaa1" || a.ModelBytes != 1<<30 || a.Profile != "q" {
		t.Fatalf("round trip %+v", a)
	}
	if got[1].Node != "b" || got[2].Shape != "T" {
		t.Fatalf("order %+v", got)
	}
	// Forgetting a shape (POST /api/needs/forget, a replaced model file)
	// removes its host rows too; another shape's stay.
	if err := st.Delete("S"); err != nil {
		t.Fatal(err)
	}
	got, err = st.AllHost()
	if err != nil || len(got) != 1 || got[0].Shape != "T" {
		t.Fatalf("after delete %+v %v", got, err)
	}
}

// A database an older build wrote (no host ledger) opens and gains the table;
// a row that cannot be used (no figure, a figure out of range) is skipped.
func TestHostOverhead_OlderDatabaseAndBadRows(t *testing.T) {
	db := openDB(t)
	openV1(t, db, map[string]string{"m": docOf(sample("m"))}, nil)
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledger(t, db, "needs_host_migrations"); len(got) != 1 {
		t.Fatalf("host ledger %v", got)
	}
	if _, err := db.DB().Exec(`INSERT INTO needs_host_overhead (shape, node, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause)
		VALUES ('S','a',-5,0,0,'','',0,'q','load'), ('S','b',99999,0,0,'','',0,'q','load'), ('S','c',300,0,0,'','',0,'q','load')`); err != nil {
		t.Fatal(err)
	}
	got, err := st.AllHost()
	if err != nil || len(got) != 1 || got[0].Node != "c" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := st.PutHost(HostOverhead{Shape: "S", Node: "d", MiB: MaxHostOverheadMiB + 1}); err == nil {
		t.Fatal("a figure out of range is refused")
	}
}
