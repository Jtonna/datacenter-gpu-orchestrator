package diskstat

import (
	"os"
	"path/filepath"
	"testing"
)

// The models folder's file count and bytes (the catalog's disk block:
// files, model_bytes) are of the files directly in it: a file in
// .trash/<id>/ or any other folder is not counted (the trash has its own
// figures, trash_bytes and trash_count).
func TestDirFiles_NotInFolders(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.gguf"), []byte("abc"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".trash", "0123456789ab"), 0o755)
	os.WriteFile(filepath.Join(dir, ".trash", "0123456789ab", "b.gguf"), []byte("12345"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".incoming"), 0o755)
	os.WriteFile(filepath.Join(dir, ".incoming", "c.gguf"), []byte("1234567"), 0o644)
	if b, n := DirFiles(dir); b != 3 || n != 1 {
		t.Fatalf("DirFiles: %d bytes, %d files; want 3, 1", b, n)
	}
}
