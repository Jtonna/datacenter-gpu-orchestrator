//go:build linux

package diskstat

import "syscall"

// Stat reads the filesystem holding path (statfs) and its device (stat).
// Free is what an unprivileged writer may still use; Used is what is taken.
func Stat(path string) (FS, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FS{}, err
	}
	var fi syscall.Stat_t
	if err := syscall.Stat(path, &fi); err != nil {
		return FS{}, err
	}
	bs := uint64(st.Bsize)
	return FS{Total: st.Blocks * bs, Free: st.Bavail * bs, Used: (st.Blocks - st.Bfree) * bs, Dev: uint64(fi.Dev)}, nil
}
