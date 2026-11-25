//go:build linux

package modelfiles

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// writable is access(2) with W_OK: EROFS on a read-only mount, EACCES
// without permission; nothing is written.
func writable(path string) error {
	if err := unix.Access(path, unix.W_OK); err != nil {
		return &fs.PathError{Op: "access", Path: path, Err: err}
	}
	return nil
}

// unlinkChecked removes .trash/<id>/<name> under root by descriptor: .trash
// and .trash/<id> are opened without following a link (a link there fails
// with ELOOP or ENOTDIR), the file is opened the same way and must be the
// regular file that was checked (same device and inode), and the unlink is
// made relative to the id directory's descriptor, so a path swapped after
// the checks cannot send it anywhere else. The id directory is synced so the
// removal is on disk before the caller records it.
func unlinkChecked(root, id, name string, checked fs.FileInfo) error {
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: root, Err: err}
	}
	defer unix.Close(rootFD)
	trashFD, err := unix.Openat(rootFD, TrashDir, dirFlags, 0)
	if err != nil {
		return fmt.Errorf("%w (%s: %v)", ErrUnsafe, TrashDir, err)
	}
	defer unix.Close(trashFD)
	idFD, err := unix.Openat(trashFD, id, dirFlags, 0)
	if err != nil {
		return fmt.Errorf("%w (%s/%s: %v)", ErrUnsafe, TrashDir, id, err)
	}
	defer unix.Close(idFD)
	fd, err := unix.Openat(idFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: %s could not be opened without following a link: %v", ErrNotRegular, name, err)
	}
	// the file open now resolves inside the models folder, at .trash/<id>/<name>
	got, rerr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if rerr != nil {
		unix.Close(fd)
		return fmt.Errorf("%w: where %s resolves cannot be read: %v", ErrUnsafe, name, rerr)
	}
	if err := checkResolved(root, id, name, got); err != nil {
		unix.Close(fd)
		return err
	}
	var fst unix.Stat_t
	err = unix.Fstat(fd, &fst)
	unix.Close(fd)
	if err != nil {
		return &fs.PathError{Op: "fstat", Path: name, Err: err}
	}
	want, ok := checked.Sys().(*syscall.Stat_t)
	if fst.Mode&unix.S_IFMT != unix.S_IFREG || !ok || uint64(fst.Dev) != uint64(want.Dev) || uint64(fst.Ino) != uint64(want.Ino) {
		return fmt.Errorf("%w: %s changed while it was being removed", ErrChanged, name)
	}
	// the name, looked up once more in the same directory, is still that file
	var lst unix.Stat_t
	if err := unix.Fstatat(idFD, name, &lst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "fstatat", Path: name, Err: err}
	}
	if lst.Mode&unix.S_IFMT != unix.S_IFREG || lst.Dev != fst.Dev || lst.Ino != fst.Ino {
		return fmt.Errorf("%w: %s changed while it was being removed", ErrChanged, name)
	}
	if err := unix.Unlinkat(idFD, name, 0); err != nil {
		return &fs.PathError{Op: "unlink", Path: name, Err: err}
	}
	if err := unix.Fsync(idFD); err != nil {
		return &fs.PathError{Op: "fsync", Path: TrashDir + "/" + id, Err: err}
	}
	return nil
}
