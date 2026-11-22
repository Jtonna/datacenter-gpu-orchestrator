//go:build linux

package filecache

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// chunk is how much of the file is mapped at a time to count its pages: a
// mapping costs address space, not memory, and mincore reads no data.
const chunk = 256 << 20

type linuxSys struct{}

func defaultSys() Sys { return linuxSys{} }

// Cached maps the file read-only in chunks and counts the resident pages.
func (linuxSys) Cached(f *os.File, size int64) (int64, error) {
	page := int64(os.Getpagesize())
	vec := make([]byte, chunk/page)
	var total int64
	for off := int64(0); off < size; off += chunk {
		n := min(int64(chunk), size-off)
		data, err := unix.Mmap(int(f.Fd()), off, int(n), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return 0, err
		}
		pages := (n + page - 1) / page
		err = mincore(data, vec[:pages])
		unix.Munmap(data)
		if err != nil {
			return 0, err
		}
		for i, v := range vec[:pages] {
			if v&1 == 0 {
				continue
			}
			b := page
			if rest := n - int64(i)*page; rest < page {
				b = rest
			}
			total += b
		}
	}
	return total, nil
}

// Drop is posix_fadvise(POSIX_FADV_DONTNEED) over the whole file. Pages a
// process has mapped, and dirty pages, stay: the second count shows them.
func (linuxSys) Drop(f *os.File, size int64) error {
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}

// mincore is mincore(2); x/sys/unix has no wrapper for it.
func mincore(b, vec []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, _, e := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&vec[0])))
	if e != 0 {
		return e
	}
	return nil
}
