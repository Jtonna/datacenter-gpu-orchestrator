package modelfiles

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

const rid = "0123456789ab"

// trashed makes <root>/.trash/<rid>/<name> holding body, and a file of the
// same name at the top of the models folder (it must always survive).
func trashed(t *testing.T, name, body string) (Dir, string) {
	t.Helper()
	root := t.TempDir()
	d := Dir{Root: root}
	if err := d.PrepareTrash(rid); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, TrashDir, rid, name), body)
	write(t, filepath.Join(root, name), "TOP-"+body)
	return d, root
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// RemoveTrashed erases exactly .trash/<id>/<name> and nothing else: not the
// file of the same name at the top of the folder, not the id directory, not
// a neighbour.
func TestRemoveTrashed(t *testing.T) {
	d, root := trashed(t, "a.gguf", "GGUF-a")
	write(t, filepath.Join(root, TrashDir, rid, "z.gguf"), "neighbour")
	if err := d.RemoveTrashed(rid, "a.gguf", 6); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, TrashDir, rid, "a.gguf")) {
		t.Fatal("the trashed file is still there")
	}
	for _, p := range []string{filepath.Join(root, "a.gguf"), filepath.Join(root, TrashDir, rid, "z.gguf"), filepath.Join(root, TrashDir, rid)} {
		if !exists(p) {
			t.Fatalf("%s was removed", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.gguf")); string(b) != "TOP-GGUF-a" {
		t.Fatalf("the top file changed: %q", b)
	}
	// again: nothing there
	if err := d.RemoveTrashed(rid, "a.gguf", 6); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
}

// Hostile input: every refusal leaves every file in place, and none of them
// reaches the unlink.
func TestRemoveTrashedHostile(t *testing.T) {
	survivors := func(t *testing.T, root string) {
		t.Helper()
		for _, p := range []string{filepath.Join(root, "a.gguf")} {
			if b, err := os.ReadFile(p); err != nil || string(b) != "TOP-GGUF-a" {
				t.Fatalf("%s: %q %v", p, b, err)
			}
		}
	}
	t.Run("bad ids and names", func(t *testing.T) {
		d, root := trashed(t, "a.gguf", "GGUF-a")
		for _, c := range []struct{ id, name string }{
			{"", "a.gguf"}, {"..", "a.gguf"}, {"../x", "a.gguf"}, {".", "a.gguf"}, {"0123456789AB", "a.gguf"}, {"0123456789abc", "a.gguf"},
			{"0123456789a/", "a.gguf"}, {TrashDir, "a.gguf"}, {rid + "/..", "a.gguf"},
			{rid, ""}, {rid, ".."}, {rid, "../a.gguf"}, {rid, "../../a.gguf"}, {rid, "a.txt"}, {rid, ".a.gguf"}, {rid, "sub/a.gguf"},
			{rid, "/etc/a.gguf"}, {rid, `..\a.gguf`}, {rid, "C:a.gguf"}, {rid, "."}, {rid, rid},
		} {
			if err := d.RemoveTrashed(c.id, c.name, -1); !errors.Is(err, ErrBadName) {
				t.Errorf("%q %q: %v, want ErrBadName", c.id, c.name, err)
			}
		}
		survivors(t, root)
		if !exists(filepath.Join(root, TrashDir, rid, "a.gguf")) {
			t.Fatal("the trashed file went")
		}
	})
	t.Run("a directory where the file should be", func(t *testing.T) {
		root := t.TempDir()
		d := Dir{Root: root}
		d.PrepareTrash(rid)
		os.Mkdir(filepath.Join(root, TrashDir, rid, "a.gguf"), 0o755)
		write(t, filepath.Join(root, TrashDir, rid, "a.gguf", "inner.gguf"), "x")
		write(t, filepath.Join(root, "a.gguf"), "TOP-GGUF-a")
		if err := d.RemoveTrashed(rid, "a.gguf", -1); !errors.Is(err, ErrNotRegular) {
			t.Fatalf("directory: %v", err)
		}
		if !exists(filepath.Join(root, TrashDir, rid, "a.gguf", "inner.gguf")) {
			t.Fatal("the directory's content went")
		}
		survivors(t, root)
	})
	t.Run("the size is not the one recorded", func(t *testing.T) {
		d, root := trashed(t, "a.gguf", "GGUF-a")
		if err := d.RemoveTrashed(rid, "a.gguf", 7); !errors.Is(err, ErrChanged) {
			t.Fatalf("size: %v", err)
		}
		if !exists(filepath.Join(root, TrashDir, rid, "a.gguf")) {
			t.Fatal("removed a file of another size")
		}
	})
	t.Run("read-only folder", func(t *testing.T) {
		d, root := trashed(t, "a.gguf", "GGUF-a")
		calls := 0
		d.Ops = &Ops{Access: func(p string) error {
			calls++
			return &fs.PathError{Op: "access", Path: p, Err: syscall.EROFS}
		}}
		if err := d.RemoveTrashed(rid, "a.gguf", 6); !errors.Is(err, ErrReadOnly) || calls == 0 {
			t.Fatalf("read-only: %v (access called %d)", err, calls)
		}
		if err := d.TrashWritable(rid); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("TrashWritable: %v", err)
		}
		if err := d.TrashWritable(""); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("TrashWritable(.trash): %v", err)
		}
		if !exists(filepath.Join(root, TrashDir, rid, "a.gguf")) {
			t.Fatal("removed on a read-only folder")
		}
		d.Ops = nil
		if err := d.TrashWritable(rid); err != nil {
			t.Fatalf("writable folder: %v", err)
		}
	})

	// links at every level
	link := func(t *testing.T, target, at string) {
		t.Helper()
		if err := os.Symlink(target, at); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}
	t.Run("the file is a link to the top file", func(t *testing.T) {
		root := t.TempDir()
		d := Dir{Root: root}
		d.PrepareTrash(rid)
		write(t, filepath.Join(root, "a.gguf"), "TOP-GGUF-a")
		link(t, filepath.Join(root, "a.gguf"), filepath.Join(root, TrashDir, rid, "a.gguf"))
		if err := d.RemoveTrashed(rid, "a.gguf", -1); !errors.Is(err, ErrNotRegular) {
			t.Fatalf("link file: %v", err)
		}
		survivors(t, root)
		if !exists(filepath.Join(root, TrashDir, rid, "a.gguf")) {
			t.Fatal("the link itself went")
		}
	})
	t.Run("the id directory is a link", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		d := Dir{Root: root}
		os.Mkdir(filepath.Join(root, TrashDir), 0o755)
		write(t, filepath.Join(outside, "a.gguf"), "OUT")
		write(t, filepath.Join(root, "a.gguf"), "TOP-GGUF-a")
		link(t, outside, filepath.Join(root, TrashDir, rid))
		if err := d.RemoveTrashed(rid, "a.gguf", -1); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("link id dir: %v", err)
		}
		if !exists(filepath.Join(outside, "a.gguf")) {
			t.Fatal("removed through a link")
		}
		survivors(t, root)
	})
	t.Run("the id directory links to the models folder", func(t *testing.T) {
		root := t.TempDir()
		d := Dir{Root: root}
		os.Mkdir(filepath.Join(root, TrashDir), 0o755)
		write(t, filepath.Join(root, "a.gguf"), "TOP-GGUF-a")
		link(t, root, filepath.Join(root, TrashDir, rid))
		if err := d.RemoveTrashed(rid, "a.gguf", -1); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("id dir -> models folder: %v", err)
		}
		survivors(t, root)
	})
	t.Run(".trash is a link", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		d := Dir{Root: root}
		os.MkdirAll(filepath.Join(outside, rid), 0o755)
		write(t, filepath.Join(outside, rid, "a.gguf"), "OUT")
		write(t, filepath.Join(root, "a.gguf"), "TOP-GGUF-a")
		link(t, outside, filepath.Join(root, TrashDir))
		if err := d.RemoveTrashed(rid, "a.gguf", -1); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("link .trash: %v", err)
		}
		if !exists(filepath.Join(outside, rid, "a.gguf")) {
			t.Fatal("removed through a link")
		}
		survivors(t, root)
	})
	// swapped between the check and the unlink: the last check (by
	// descriptor on Linux) sees it and refuses
	t.Run("the file is swapped for a link after the checks", func(t *testing.T) {
		d, root := trashed(t, "a.gguf", "GGUF-a")
		p := filepath.Join(root, TrashDir, rid, "a.gguf")
		if err := os.Symlink(filepath.Join(root, "a.gguf"), filepath.Join(root, "probe")); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		os.Remove(filepath.Join(root, "probe"))
		afterCheck = func() {
			os.Rename(p, filepath.Join(root, "moved-away"))
			os.Symlink(filepath.Join(root, "a.gguf"), p)
		}
		defer func() { afterCheck = nil }()
		if err := d.RemoveTrashed(rid, "a.gguf", 6); err == nil {
			t.Fatal("removed a swapped path")
		}
		survivors(t, root)
		if !exists(filepath.Join(root, "moved-away")) {
			t.Fatal("the moved file went")
		}
	})
	t.Run("the id directory is swapped for a link after the checks", func(t *testing.T) {
		d, root := trashed(t, "a.gguf", "GGUF-a")
		outside := t.TempDir()
		write(t, filepath.Join(outside, "a.gguf"), "OUT")
		if err := os.Symlink(outside, filepath.Join(root, "probe")); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		os.Remove(filepath.Join(root, "probe"))
		idDir := filepath.Join(root, TrashDir, rid)
		afterCheck = func() {
			os.Rename(idDir, filepath.Join(root, TrashDir, "moved"))
			os.Symlink(outside, idDir)
		}
		defer func() { afterCheck = nil }()
		if err := d.RemoveTrashed(rid, "a.gguf", 6); err == nil {
			t.Fatal("removed through a swapped directory")
		}
		if !exists(filepath.Join(outside, "a.gguf")) {
			t.Fatal("removed outside the models folder")
		}
		survivors(t, root)
	})
}

// A real read-only folder: without write permission (a user that is not
// root, mode 0555), and, when MODELFILES_RO_ROOT names a models folder
// mounted read-only that holds .trash/0123456789ab/a.gguf (6 bytes), a real
// read-only mount (EROFS). Nothing is removed and the refusal is ErrReadOnly.
func TestRemoveTrashedRealReadOnly(t *testing.T) {
	if root := os.Getenv("MODELFILES_RO_ROOT"); root != "" {
		d := Dir{Root: root}
		if err := d.TrashWritable(rid); !errors.Is(err, ErrReadOnly) || !errors.Is(err, syscall.EROFS) {
			t.Fatalf("read-only mount: TrashWritable %v", err)
		}
		if err := d.RemoveTrashed(rid, "a.gguf", 6); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("read-only mount: %v", err)
		}
		if !exists(filepath.Join(root, TrashDir, rid, "a.gguf")) {
			t.Fatal("gone from a read-only mount")
		}
		t.Logf("read-only mount: refused with %v", d.RemoveTrashed(rid, "a.gguf", 6))
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a POSIX folder mode and a user that is not root")
	}
	d, root := trashed(t, "a.gguf", "GGUF-a")
	idDir := filepath.Join(root, TrashDir, rid)
	os.Chmod(idDir, 0o555)
	t.Cleanup(func() { os.Chmod(idDir, 0o755) })
	if err := d.RemoveTrashed(rid, "a.gguf", 6); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("no write permission: %v", err)
	}
	if !exists(filepath.Join(idDir, "a.gguf")) {
		t.Fatal("removed without write permission")
	}
}
