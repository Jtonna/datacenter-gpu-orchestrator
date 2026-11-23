//go:build !linux

package diskstat

// Stat returns ErrUnsupported: disk figures are read on Linux only.
func Stat(path string) (FS, error) { return FS{}, ErrUnsupported }
