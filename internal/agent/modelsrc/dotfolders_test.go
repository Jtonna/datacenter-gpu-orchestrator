package modelsrc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The models folder holds Potluck's .trash/<id>/ (models slice 3) and other
// dot-folders (the owner's .incoming): the share probe never reads a file in
// them, nor a dot-file at the top. A folder with only those is "no model
// file"; with a model file too, the probe reads that one.
func TestProbeRead_SkipsDotFolders(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{".trash/0123456789ab/a.gguf", ".incoming/b.gguf", ".potluck-uploads/c.gguf"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte("GGUF"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, ".hidden.gguf"), []byte("GGUF"), 0o644)
	if err := ProbeRead(dir); !errors.Is(err, ErrNoModelFile) {
		t.Fatalf("only dot-names: %v, want ErrNoModelFile", err)
	}
	os.WriteFile(filepath.Join(dir, "z.gguf"), []byte("GGUF"), 0o644)
	if err := ProbeRead(dir); err != nil {
		t.Fatalf("with a model file: %v", err)
	}
}
