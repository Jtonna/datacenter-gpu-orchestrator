//go:build unix

package modelfiles

import "syscall"

// accessWrite is access(W_OK) on dir: it writes nothing. EROFS and EACCES
// both answer false.
func accessWrite(dir string) (writable, ok bool) {
	const wOK = 2
	return syscall.Access(dir, wOK) == nil, true
}
