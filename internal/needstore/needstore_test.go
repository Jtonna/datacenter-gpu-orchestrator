package needstore

import (
	"database/sql"
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

// A record survives the round trip, Put replaces the profile's row, Delete
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
	if err := st.Delete("m"); err != nil {
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
