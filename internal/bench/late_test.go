package bench

import (
	"database/sql"
	"testing"
)

// A write after Close is refused and counted (LateWrites), so a shutdown
// that lets a writer run past the store's close is seen.
func TestStore_AWriteAfterCloseIsRefusedAndCounted(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(func(*sql.Tx) error { return nil }); err != nil || st.LateWrites() != 0 {
		t.Fatalf("before close: %v, late %d", err, st.LateWrites())
	}
	st.Close()
	if err := st.Write(func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("a write after close was accepted")
	}
	if st.LateWrites() != 1 {
		t.Fatalf("late writes %d, want 1", st.LateWrites())
	}
}
