package modelfiles

import (
	"os"
	"path/filepath"
)

// mountReadOnlyOf reads /proc/self/mountinfo for dir (its links resolved).
func mountReadOnlyOf(dir string) (ro bool, mountPoint string, ok bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, "", false
	}
	defer f.Close()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, "", false
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return MountReadOnly(f, abs)
}
