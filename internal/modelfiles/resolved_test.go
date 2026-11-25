package modelfiles

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The one rule: "Remove for good" and the clearing act only on a path that
// is a regular file directly inside .trash/<id>/, not a link, whose resolved
// path is checked to be under the models folder at the moment of removal.
// This is the last part: at the unlink, the file actually open must resolve
// to <models, resolved>/.trash/<id>/<name>; anything else is refused and
// nothing is removed.
func TestRemoveTrashed_ResolvedPathCheckedAtRemoval(t *testing.T) {
	t.Run("a file resolving elsewhere is refused", func(t *testing.T) {
		d, root := trashed(t, "a.gguf", "GGUF-a")
		outside := t.TempDir()
		resolvedHook = func(string) string { return filepath.Join(outside, "a.gguf") }
		defer func() { resolvedHook = nil }()
		if err := d.RemoveTrashed(rid, "a.gguf", 6); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("resolved outside: %v, want ErrUnsafe", err)
		}
		if !exists(filepath.Join(root, TrashDir, rid, "a.gguf")) {
			t.Fatal("removed although it resolved outside the models folder")
		}
		if b, err := os.ReadFile(filepath.Join(root, "a.gguf")); err != nil || string(b) != "TOP-GGUF-a" {
			t.Fatalf("the top file: %q %v", b, err)
		}
	})
	t.Run("a file resolving to the top of the models folder is refused", func(t *testing.T) {
		d, root := trashed(t, "a.gguf", "GGUF-a")
		resolvedHook = func(string) string { return filepath.Join(root, "a.gguf") }
		defer func() { resolvedHook = nil }()
		if err := d.RemoveTrashed(rid, "a.gguf", 6); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("resolved to the top file: %v, want ErrUnsafe", err)
		}
		if !exists(filepath.Join(root, TrashDir, rid, "a.gguf")) {
			t.Fatal("removed")
		}
	})
	t.Run("the models folder itself reached by a link still works", func(t *testing.T) {
		real := t.TempDir()
		root := filepath.Join(t.TempDir(), "models")
		if err := os.Symlink(real, root); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		d := Dir{Root: root}
		if err := d.PrepareTrash(rid); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(real, TrashDir, rid, "a.gguf"), "GGUF-a")
		if err := d.RemoveTrashed(rid, "a.gguf", 6); err != nil {
			t.Fatalf("models folder by a link: %v", err)
		}
		if exists(filepath.Join(real, TrashDir, rid, "a.gguf")) {
			t.Fatal("not removed")
		}
	})
}
