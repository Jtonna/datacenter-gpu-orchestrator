package bench

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// OpenShared opens a database whose bench migrations are ahead of this
// build with the bench part off (BenchOff names both versions) and the
// ledger untouched; OpenStore still refuses it; any other failure is
// OpenShared's too.
func TestOpenSharedKeepsANewerBenchLedgerOff(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	know, _ := st.SchemaVersion()
	if st.BenchOff() != nil {
		t.Fatal("a current database is off")
	}
	if _, err := st.db.Exec(`INSERT INTO migrations (version, name, applied_at) VALUES (?, 'newer', 0)`, know+1); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if _, err := OpenStore(dir); !errors.As(err, new(*NewerSchemaError)) {
		t.Fatalf("OpenStore: %v", err)
	}
	sh, err := OpenShared(dir)
	if err != nil {
		t.Fatal(err)
	}
	ne := sh.BenchOff()
	if ne == nil || ne.Have != know+1 || ne.Know != know {
		t.Fatalf("off %+v", ne)
	}
	if v, _ := sh.SchemaVersion(); v != know+1 {
		t.Fatalf("version %d", v)
	}
	want := "benchmarks are off: this database's benchmark tables are at version " + strconv.Itoa(know+1) + ", this version knows up to " + strconv.Itoa(know) + "; start a version that knows " + strconv.Itoa(know+1) + " or newer"
	if ne.BenchOffReason() != want {
		t.Fatalf("%q", ne.BenchOffReason())
	}
	sh.Close()

	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, DBFile), []byte("this is not a database, it is plain text and long enough to be read"), 0o644)
	if s, err := OpenShared(bad); err == nil {
		s.Close()
		t.Fatal("a damaged file opened")
	}
}
