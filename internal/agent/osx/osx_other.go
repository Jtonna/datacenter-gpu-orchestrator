//go:build !linux

package osx

import (
	"os"
	"time"
)

// NewBootClock returns ErrUnsupported: CLOCK_BOOTTIME exists only on Linux.
func NewBootClock() (Clock, error) { return nil, ErrUnsupported }

func signalPID(pid int, sig Sig) error { return ErrUnsupported }

func pinPID(pid int) (send func(Sig) error, done func(), err error) {
	return func(sig Sig) error { return signalPID(pid, sig) }, func() {}, nil
}

// StatfsFree returns ErrUnsupported.
func StatfsFree(path string) (uint64, error) { return 0, ErrUnsupported }

// PunchHole returns ErrUnsupported; callers fall back to zeroing.
func PunchHole(f *os.File, off, n int64) error { return ErrUnsupported }

// OpenDirect opens without O_DIRECT (which does not exist here).
func OpenDirect(path string) (*os.File, error) { return os.Open(path) }

// Kmsg is the kernel log reader; it never finds anything here.
type Kmsg struct{ Path string }

// OOMKilled implements KernelLog.
func (Kmsg) OOMKilled(pid int) bool { return false }

// MonotonicNow returns ErrUnsupported: the journal exists only on Linux.
func MonotonicNow() (time.Duration, error) { return 0, ErrUnsupported }
