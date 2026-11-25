package api

import (
	"os"
	"path/filepath"
	"testing"
)

// The catalog lists the files a profile names; a named path the shared path
// rule refuses (internal/modelpath) is listed as not there and never read,
// even when a file is at that path (the trash), so a stored profile cannot
// bring a dot-folder's file into the catalog.
func TestModelFileList_RefusedNamedPathIsNotRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".trash", "0123"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(".trash", "0123", "a.gguf"), "top.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("GGUF-not-really"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "s.gguf"), []byte("GGUF-not-really"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c modelFileCache
	got := map[string]ModelFile{}
	for _, m := range c.list(dir, []string{".trash/0123/a.gguf", "sub/s.gguf", "../x.gguf"}) {
		got[m.File] = m
	}
	if m, ok := got[".trash/0123/a.gguf"]; !ok || m.Exists || m.SizeBytes != 0 {
		t.Errorf("trash path: %+v %v, want listed as not there", m, ok)
	}
	if m, ok := got["../x.gguf"]; !ok || m.Exists {
		t.Errorf("../x.gguf: %+v %v, want listed as not there", m, ok)
	}
	if m := got["sub/s.gguf"]; !m.Exists || m.SizeBytes == 0 {
		t.Errorf("sub-folder file: %+v, want read", m)
	}
	if m := got["top.gguf"]; !m.Exists {
		t.Errorf("top.gguf: %+v", m)
	}
	if _, ok := c.seen[".trash/0123/a.gguf"]; ok {
		t.Error("the trash path was cached as read")
	}
}
