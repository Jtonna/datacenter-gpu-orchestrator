package api

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelfiles"
)

// models_writable: whether the models folder can be written, read without
// writing (modelfiles.Writable: the mount's ro/rw, else access(W_OK)), in the
// catalog's disk block and in GET /api/models/trash, so the page can say
// "read-only" before an owner tries. Absent when it cannot be told.
func TestModelsWritableFlag(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	read := func() (disk, trash map[string]any) {
		t.Helper()
		_, m := te.cdo("GET", "/api/models", "", nil)
		_, tr := te.cdo("GET", "/api/models/trash", "", nil)
		return m["disk"].(map[string]any), tr
	}
	disk, tr := read()
	if disk["models_writable"] != true || tr["models_writable"] != true {
		t.Fatalf("a writable folder: disk %v, trash %v", disk["models_writable"], tr["models_writable"])
	}
	te.ro = true
	disk, tr = read()
	if disk["models_writable"] != false || tr["models_writable"] != false {
		t.Fatalf("a read-only folder: disk %v, trash %v", disk["models_writable"], tr["models_writable"])
	}
	if !te.has("a.gguf") || te.has(".trash") {
		t.Fatal("reading the flag wrote something")
	}
	te.unknown = true
	disk, tr = read()
	if _, ok := disk["models_writable"]; ok {
		t.Fatalf("no answer, but the disk block has the field: %v", disk)
	}
	if _, ok := tr["models_writable"]; ok {
		t.Fatalf("no answer, but the trash list has the field: %v", tr)
	}
}

// Without the test hook the server asks modelfiles.Writable: the field is
// what it answers on this machine (on Windows: absent), and it writes nothing.
func TestModelsWritableFlagOnThisMachine(t *testing.T) {
	te := newTrashEnv(t)
	te.api.opts.modelsWritable = nil
	te.setup()
	want, ok := modelfiles.Writable(te.dir)
	_, m := te.cdo("GET", "/api/models", "", nil)
	got, has := m["disk"].(map[string]any)["models_writable"]
	if has != ok || (ok && got != want) {
		t.Fatalf("disk models_writable %v (present %v); modelfiles.Writable says %v (ok %v)", got, has, want, ok)
	}
	if te.has(".trash") {
		t.Fatal("reading the flag made the trash folder")
	}
}
