//go:build !linux

package modelfiles

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// writable has no cheap check here: nil (a read-only folder still fails at
// the removal itself, classified as ErrReadOnly). The scheduler runs on Linux.
func writable(string) error { return nil }

// unlinkChecked removes .trash/<id>/<name> under root. Without the Linux
// descriptor calls it checks the directories and the file once more just
// before the removal (plain directories, the same regular file); the window
// left is the one between that check and os.Remove, which removes a link
// itself, never what it points at.
func unlinkChecked(root, id, name string, checked fs.FileInfo) error {
	d := Dir{Root: root}
	if err := d.plainDirs(id, false); err != nil {
		return err
	}
	p := filepath.Join(root, TrashDir, id, name)
	st, err := regular(p)
	if err != nil {
		return err
	}
	if !os.SameFile(st, checked) {
		return fmt.Errorf("%w: %s changed while it was being removed", ErrChanged, name)
	}
	got, err := filepath.EvalSymlinks(p) // the path resolves inside the models folder, at .trash/<id>/<name>
	if err != nil {
		return fmt.Errorf("%w: where %s resolves cannot be read: %v", ErrUnsafe, name, err)
	}
	if err := checkResolved(root, id, name, got); err != nil {
		return err
	}
	return os.Remove(p)
}
