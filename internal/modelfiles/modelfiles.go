// Package modelfiles is the only code that moves model files (models slice
// 3). Its rules:
//
//   - a model name is one path part ending .gguf, with no leading dot and no
//     separator: no request can name a path outside the models folder;
//   - it renames only inside the models folder, between the folder itself and
//     <models>/.trash/<id>/, which is on the same file system, so every move
//     is one atomic rename (never a copy);
//   - it never removes a file. The only removal is of an EMPTY trash
//     directory it made itself (os.Remove refuses a directory with anything
//     in it);
//   - it refuses a model file that is a link, and a .trash or .trash/<id>
//     that is a link or not a directory, so nothing is written through a
//     link to a place outside the folder.
//
// A models folder mounted read-only gives ErrReadOnly on the first write,
// which is always the making of the trash directory, before anything else
// changed.
package modelfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// TrashDir is the trash folder's name inside the models folder. Dot-names
// are not in the catalog (it lists top-level files only) and the agents'
// share probe skips them.
const TrashDir = ".trash"

// Errors.
var (
	// ErrReadOnly: the models folder cannot be written here (read-only
	// mount, or no permission).
	ErrReadOnly = errors.New("the models folder is read-only here")
	// ErrBadName: not one path part ending .gguf without a leading dot.
	ErrBadName = errors.New("not a model file name")
	// ErrNotFound: no such file.
	ErrNotFound = errors.New("no such file")
	// ErrExists: the destination already exists.
	ErrExists = errors.New("a file of that name exists")
	// ErrNotRegular: a link, a directory or a device.
	ErrNotRegular = errors.New("not a regular file")
	// ErrUnsafe: the trash folder is a link or not a directory.
	ErrUnsafe = errors.New("the trash folder is not a plain directory inside the models folder")
)

var trashID = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidID reports whether id is a trash id this package makes.
func ValidID(id string) bool { return trashID.MatchString(id) }

// ValidName checks a model file name: one path part, ending .gguf (any
// case), no leading dot, no separator, no NUL. The rule is its own and the
// same on every OS: it does not use path/filepath, whose idea of a drive or
// a separator differs between Windows and Linux. Refused: empty; any '/',
// '\', ':' or NUL; a leading dot (so "." and ".." too); not ending .gguf
// after at least one other character.
func ValidName(name string) error {
	const ext = ".gguf"
	switch {
	case name == "":
		return fmt.Errorf("%w: empty", ErrBadName)
	case strings.ContainsAny(name, "/\\:\x00"), strings.HasPrefix(name, "."):
		return fmt.Errorf("%w: %q must be one file name in the models folder, without a folder, a drive, a colon or a leading dot", ErrBadName, name)
	case len(name) <= len(ext) || !strings.EqualFold(name[len(name)-len(ext):], ext):
		return fmt.Errorf("%w: %q does not end in .gguf", ErrBadName, name)
	}
	return nil
}

// Ops are the file system calls that write; tests replace them to act out a
// read-only folder.
type Ops struct {
	Mkdir  func(path string, perm fs.FileMode) error
	Rename func(oldpath, newpath string) error
	Remove func(path string) error // only ever an empty trash directory
}

// Dir is the models folder.
type Dir struct {
	Root string
	Ops  *Ops // nil: the os package
}

func (d Dir) ops() Ops {
	o := Ops{Mkdir: os.Mkdir, Rename: os.Rename, Remove: os.Remove}
	if d.Ops != nil {
		if d.Ops.Mkdir != nil {
			o.Mkdir = d.Ops.Mkdir
		}
		if d.Ops.Rename != nil {
			o.Rename = d.Ops.Rename
		}
		if d.Ops.Remove != nil {
			o.Remove = d.Ops.Remove
		}
	}
	return o
}

// FilePath is where a model file is in the folder.
func (d Dir) FilePath(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return d.inside(name)
}

// TrashPath is where a model file is in the trash.
func (d Dir) TrashPath(id, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	if !ValidID(id) {
		return "", fmt.Errorf("%w: trash id %q", ErrBadName, id)
	}
	return d.inside(TrashDir, id, name)
}

// inside joins parts under Root and checks the result stays under it.
func (d Dir) inside(parts ...string) (string, error) {
	if d.Root == "" {
		return "", errors.New("no models folder is set")
	}
	root := filepath.Clean(d.Root)
	p := filepath.Join(append([]string{root}, parts...)...)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q leaves the models folder", ErrBadName, filepath.Join(parts...))
	}
	return p, nil
}

// Stat describes the model file name in the folder: ErrNotFound when
// there is none, ErrNotRegular for a link or anything not a plain file.
func (d Dir) Stat(name string) (fs.FileInfo, error) {
	p, err := d.FilePath(name)
	if err != nil {
		return nil, err
	}
	return regular(p)
}

// TrashStat is Stat of the file in the trash (the trash folders are checked
// to be plain directories first).
func (d Dir) TrashStat(id, name string) (fs.FileInfo, error) {
	p, err := d.TrashPath(id, name)
	if err != nil {
		return nil, err
	}
	if err := d.plainDirs(id, false); err != nil {
		return nil, err
	}
	return regular(p)
}

func regular(p string) (fs.FileInfo, error) {
	st, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, filepath.Base(p))
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		what := "not a regular file"
		if st.Mode()&fs.ModeSymlink != 0 {
			what = "a link"
		} else if st.IsDir() {
			what = "a directory"
		}
		return st, fmt.Errorf("%w: %s is %s", ErrNotRegular, filepath.Base(p), what)
	}
	return st, nil
}

// plainDirs checks that .trash (and .trash/<id> when id is not "") are
// directories and not links. missingOK: a directory that is not there yet
// is fine.
func (d Dir) plainDirs(id string, missingOK bool) error {
	parts := []string{TrashDir}
	if id != "" {
		parts = append(parts, id)
	}
	for i := range parts {
		p, err := d.inside(parts[:i+1]...)
		if err != nil {
			return err
		}
		st, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			if missingOK {
				return nil
			}
			return fmt.Errorf("%w: %s", ErrNotFound, filepath.Join(parts[:i+1]...))
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
			return fmt.Errorf("%w (%s)", ErrUnsafe, filepath.Join(parts[:i+1]...))
		}
	}
	return nil
}

// PrepareTrash makes .trash/<id>/ (and .trash when missing). It is the
// first write of a delete: on a read-only folder it fails with ErrReadOnly
// before anything else has changed. The id directory must be new.
func (d Dir) PrepareTrash(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%w: trash id %q", ErrBadName, id)
	}
	o := d.ops()
	if err := d.plainDirs("", true); err != nil {
		return err
	}
	top, _ := d.inside(TrashDir)
	if _, err := os.Lstat(top); errors.Is(err, fs.ErrNotExist) {
		if err := o.Mkdir(top, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return classify(err)
		}
	}
	if err := d.plainDirs("", false); err != nil {
		return err
	}
	p, _ := d.inside(TrashDir, id)
	if err := o.Mkdir(p, 0o755); err != nil {
		return classify(err)
	}
	return d.plainDirs(id, false)
}

// MoveToTrash renames <models>/<name> to <models>/.trash/<id>/<name>.
// PrepareTrash(id) must have made the directory.
func (d Dir) MoveToTrash(id, name string) error {
	from, err := d.FilePath(name)
	if err != nil {
		return err
	}
	to, err := d.TrashPath(id, name)
	if err != nil {
		return err
	}
	if _, err := regular(from); err != nil {
		return err
	}
	if err := d.plainDirs(id, false); err != nil {
		return err
	}
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%w: %s in the trash", ErrExists, name)
	}
	return classify(d.ops().Rename(from, to))
}

// RestoreFromTrash renames <models>/.trash/<id>/<name> back to
// <models>/<name>; ErrExists when a file (or anything) of that name is in
// the folder.
func (d Dir) RestoreFromTrash(id, name string) error {
	from, err := d.TrashPath(id, name)
	if err != nil {
		return err
	}
	to, err := d.FilePath(name)
	if err != nil {
		return err
	}
	if err := d.plainDirs(id, false); err != nil {
		return err
	}
	if _, err := regular(from); err != nil {
		return err
	}
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%w: %s is in the models folder again", ErrExists, name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return classify(d.ops().Rename(from, to))
}

// RemoveEmptyTrashDir removes .trash/<id>/ when it is empty (os.Remove
// refuses a directory that holds anything). Nothing else is ever removed.
func (d Dir) RemoveEmptyTrashDir(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%w: trash id %q", ErrBadName, id)
	}
	if err := d.plainDirs(id, false); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	p, _ := d.inside(TrashDir, id)
	return classify(d.ops().Remove(p))
}

// ReadOnlyError is a write refused because the folder is read-only (or
// not writable); Cause is the OS error. It matches ErrReadOnly.
type ReadOnlyError struct{ Cause error }

func (e *ReadOnlyError) Error() string        { return fmt.Sprintf("%v (%v)", ErrReadOnly, e.Cause) }
func (e *ReadOnlyError) Is(target error) bool { return target == ErrReadOnly }
func (e *ReadOnlyError) Unwrap() error        { return e.Cause }

// ReadOnlyCause is the OS error behind a read-only refusal (err itself when
// it is not one), so a message can say "read-only" once.
func ReadOnlyCause(err error) error {
	var ro *ReadOnlyError
	if errors.As(err, &ro) {
		return ro.Cause
	}
	return err
}

// classify turns a read-only or permission error into a *ReadOnlyError.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var ro *ReadOnlyError
	if errors.As(err, &ro) {
		return err
	}
	if IsReadOnly(err) {
		return &ReadOnlyError{Cause: err}
	}
	return err
}

// IsReadOnly reports a write refused by a read-only file system or by
// permissions.
func IsReadOnly(err error) bool {
	return errors.Is(err, ErrReadOnly) || errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission)
}
