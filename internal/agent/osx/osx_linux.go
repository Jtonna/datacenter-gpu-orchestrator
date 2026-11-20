//go:build linux

package osx

import (
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const clockBoottime = 7 // CLOCK_BOOTTIME

// BootClock reads CLOCK_BOOTTIME, which keeps counting across agent restarts
// and through suspend (contract 2.1).
type BootClock struct{}

// NewBootClock returns the real clock.
func NewBootClock() (Clock, error) {
	var c BootClock
	if _, err := c.read(); err != nil {
		return nil, err
	}
	return c, nil
}

func (BootClock) read() (time.Duration, error) {
	var ts syscall.Timespec
	_, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockBoottime, uintptr(unsafe.Pointer(&ts)), 0)
	if e != 0 {
		return 0, e
	}
	return time.Duration(ts.Nano()), nil
}

// Now implements Clock.
func (c BootClock) Now() time.Duration {
	d, err := c.read()
	if err != nil {
		panic("CLOCK_BOOTTIME: " + err.Error()) // checked once in NewBootClock
	}
	return d
}

func signalPID(pid int, sig Sig) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	return syscall.Kill(pid, syscall.Signal(sig))
}

// StatfsFree returns the bytes available to unprivileged users on the
// filesystem holding path.
func StatfsFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// PunchHole frees [off, off+n) of f while keeping its size, so byte offsets
// of later data do not move (contract 6.6). tmpfs and ext4 support it.
func PunchHole(f *os.File, off, n int64) error {
	const punchHole, keepSize = 0x02, 0x01
	return syscall.Fallocate(int(f.Fd()), punchHole|keepSize, off, n)
}

// OpenDirect opens a file for reading with O_DIRECT, so the read goes to the
// server and not to the page or attribute cache (contract 8, share probe).
func OpenDirect(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
}

// Kmsg reads the kernel log from /dev/kmsg.
type Kmsg struct{ Path string }

// OOMKilled implements KernelLog: it reads every record currently in the
// kernel ring buffer (non-blocking) and looks for an OOM kill of pid.
func (k Kmsg) OOMKilled(pid int) bool {
	p := k.Path
	if p == "" {
		p = "/dev/kmsg"
	}
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	// /dev/kmsg returns one record per read; EAGAIN at the end.
	buf := make([]byte, 8192)
	for i := 0; i < 100000; i++ {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			if err == syscall.EPIPE { // record overwritten; continue
				continue
			}
			break
		}
		if n <= 0 {
			break
		}
		if OOMLineNames(string(buf[:n]), pid) {
			return true
		}
	}
	return false
}
