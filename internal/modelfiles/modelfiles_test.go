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

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a.gguf", "gemma-4-12b-it-Q4_K_M.gguf", "X.GGUF"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	// The rule is its own, the same on every OS (the scheduler runs on
	// Linux; filepath's rules differ): no '/', '\', ':' or NUL anywhere, no
	// leading dot, ends .gguf after at least one other character.
	for _, bad := range []string{"", ".", "..", "../a.gguf", "..\\a.gguf", "sub/a.gguf", `sub\a.gguf`, "/etc/a.gguf", `\a.gguf`,
		`C:\a.gguf`, "C:a.gguf", "a:b.gguf", ":a.gguf", `\\host\share\a.gguf`, `a\.gguf`, "a/", `a\`,
		".trash", ".a.gguf", "a.txt", ".gguf", "gguf", "a.gguf/..", "a\x00.gguf", "a.gguf:x", "/models/a.gguf"} {
		if err := ValidName(bad); !errors.Is(err, ErrBadName) {
			t.Errorf("%q: %v, want ErrBadName", bad, err)
		}
	}
}

// Trash and restore are renames inside the folder; the file's bytes stay
// the same file.
func TestTrashAndRestore(t *testing.T) {
	root := t.TempDir()
	d := Dir{Root: root}
	write(t, filepath.Join(root, "a.gguf"), "GGUF-a")
	id := "0123456789ab"
	if err := d.PrepareTrash(id); err != nil {
		t.Fatal(err)
	}
	if err := d.PrepareTrash(id); err == nil {
		t.Fatal("the same trash id twice")
	}
	if err := d.MoveToTrash(id, "a.gguf"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat("a.gguf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("still in the folder: %v", err)
	}
	if st, err := d.TrashStat(id, "a.gguf"); err != nil || st.Size() != 6 {
		t.Fatalf("in the trash: %v %v", st, err)
	}
	// restore is refused while a file of that name is back
	write(t, filepath.Join(root, "a.gguf"), "other")
	if err := d.RestoreFromTrash(id, "a.gguf"); !errors.Is(err, ErrExists) {
		t.Fatalf("restore over a file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.gguf")); string(b) != "other" {
		t.Fatal("the other file was touched")
	}
	os.Rename(filepath.Join(root, "a.gguf"), filepath.Join(t.TempDir(), "x")) // the test moves it away
	if err := d.RestoreFromTrash(id, "a.gguf"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.gguf")); string(b) != "GGUF-a" {
		t.Fatalf("restored bytes %q", b)
	}
	// the empty trash folder may go; one with anything in it never does
	other := "ba9876543210"
	d.PrepareTrash(other)
	write(t, filepath.Join(root, TrashDir, other, "keep.gguf"), "k")
	if err := d.RemoveEmptyTrashDir(other); err == nil {
		t.Fatal("removed a trash folder that holds a file")
	}
	if err := d.RemoveEmptyTrashDir(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, TrashDir, other, "keep.gguf")); err != nil {
		t.Fatal(err)
	}
}

// Nothing a request names leaves the folder: bad names and ids are
// refused before any call, a link is not moved, a .trash that is a link
// (to a place outside) is refused.
func TestNoEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	d := Dir{Root: root}
	write(t, filepath.Join(outside, "o.gguf"), "outside")
	for _, n := range []string{"../o.gguf", filepath.Join(outside, "o.gguf"), "..", ".trash"} {
		if err := d.MoveToTrash("0123456789ab", n); !errors.Is(err, ErrBadName) {
			t.Errorf("%q: %v", n, err)
		}
		if err := d.RestoreFromTrash("0123456789ab", n); !errors.Is(err, ErrBadName) {
			t.Errorf("restore %q: %v", n, err)
		}
	}
	for _, id := range []string{"..", "../x", "ABCDEF012345", "0123"} {
		if err := d.PrepareTrash(id); !errors.Is(err, ErrBadName) {
			t.Errorf("id %q: %v", id, err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "o.gguf"), filepath.Join(root, "l.gguf")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if _, err := d.Stat("l.gguf"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("a link: %v", err)
	}
	d.PrepareTrash("0123456789ab")
	if err := d.MoveToTrash("0123456789ab", "l.gguf"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("moved a link: %v", err)
	}
	// .trash a link to outside
	root2 := t.TempDir()
	d2 := Dir{Root: root2}
	write(t, filepath.Join(root2, "a.gguf"), "a")
	if err := os.Symlink(outside, filepath.Join(root2, TrashDir)); err != nil {
		t.Skip(err)
	}
	if err := d2.PrepareTrash("0123456789ab"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf(".trash a link: %v", err)
	}
	if err := d2.MoveToTrash("0123456789ab", "a.gguf"); err == nil {
		t.Fatal("moved through a linked .trash")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatalf("something was written outside: %v", ents)
	}
}

// A read-only folder: the first write (the trash directory) fails with
// ErrReadOnly; nothing moved.
func TestReadOnlyInjected(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.gguf"), "a")
	rofs := func(string) error { return &fs.PathError{Op: "mkdir", Path: root, Err: syscall.EROFS} }
	d := Dir{Root: root, Ops: &Ops{Mkdir: func(p string, _ fs.FileMode) error { return rofs(p) },
		Rename: func(a, b string) error { return &os.LinkError{Op: "rename", Old: a, New: b, Err: syscall.EROFS} }}}
	if err := d.PrepareTrash("0123456789ab"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("prepare: %v", err)
	}
	os.MkdirAll(filepath.Join(root, TrashDir, "0123456789ab"), 0o755)
	if err := d.MoveToTrash("0123456789ab", "a.gguf"); !errors.Is(err, ErrReadOnly) || err.Error()[:len(ErrReadOnly.Error())] != ErrReadOnly.Error() {
		t.Fatalf("move: %v", err)
	}
	if _, err := d.Stat("a.gguf"); err != nil {
		t.Fatalf("the file moved: %v", err)
	}
}

// The same on a real folder without write permission (Linux, not root).
func TestReadOnlyRealFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a POSIX folder mode and a user that is not root")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "a.gguf"), "a")
	os.Chmod(root, 0o555)
	t.Cleanup(func() { os.Chmod(root, 0o755) })
	d := Dir{Root: root}
	if err := d.PrepareTrash("0123456789ab"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := d.Stat("a.gguf"); err != nil {
		t.Fatal(err)
	}
}
