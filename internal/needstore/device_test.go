package needstore

import (
	"database/sql"
	"slices"
	"testing"
)

// The per-device overhead table under its own ledger: the release before
// (v53: four ledgers, none of them device) opens the file before and after
// this build ran on it; rows by shape, node and device count; Delete(shape)
// and DeleteDevice(node) drop them.
func TestDeviceOverhead_OwnLedgerAndTheWayBack(t *testing.T) {
	db := openDB(t)
	v53 := func() error {
		return db.Write(func(tx *sql.Tx) error {
			for _, l := range []struct {
				table string
				list  []func(*sql.Tx) error
			}{{"needs_migrations", migrations}, {"needs_shape_migrations", shapeMigrations}, {"needs_host_migrations", hostMigrations},
				{"needs_host_count_migrations", hostCountMigrations}} {
				if err := migrate(tx, l.table, l.list); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := v53(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"needs_migrations", "needs_shape_migrations", "needs_host_migrations", "needs_host_count_migrations", "needs_device_migrations"} {
		if v := ledger(t, db, table); !slices.Equal(v, []int{1}) {
			t.Fatalf("%s %v", table, v)
		}
	}
	a1, a2, b1 := hostSample("S", "a", 1, 450), hostSample("S", "a", 2, 380), hostSample("T", "b", 1, 700)
	for _, d := range []DeviceOverhead{a1, a2, b1} {
		if err := st.PutDevice(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutDevice(hostSample("S", "a", 1, MaxDeviceOverheadMiB+1)); err == nil {
		t.Fatal("stored a figure over the cap")
	}
	if err := st.PutDevice(hostSample("S", "a", 0, 100)); err == nil {
		t.Fatal("stored a row with no device count")
	}
	got, err := st.AllDevice()
	if err != nil || len(got) != 3 || got[0].MiB != 450 || got[1].Devices != 2 || got[1].MiB != 380 || got[0].LogHostMiB != 120 || len(got[0].Readings) != 1 {
		t.Fatalf("rows %+v %v", got, err)
	}
	if h, _ := st.AllHost(); len(h) != 0 {
		t.Fatalf("host rows %+v", h)
	}
	// The release before opens the file this build wrote (it never looks at
	// the device ledger), and this build opens it again with the rows kept.
	if err := v53(); err != nil {
		t.Fatalf("the release before: %v", err)
	}
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.AllDevice(); len(again) != 3 {
		t.Fatalf("forward again %+v", again)
	}
	if err := st.Delete("S"); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.AllDevice(); len(left) != 1 || left[0].Shape != "T" {
		t.Fatalf("after Delete(S) %+v", left)
	}
	if n, err := st.DeleteDevice("b"); err != nil || n != 1 {
		t.Fatalf("DeleteDevice %d %v", n, err)
	}
	if left, _ := st.AllDevice(); len(left) != 0 {
		t.Fatalf("after DeleteDevice %+v", left)
	}
}
