//go:build !linux

package osx

import (
	"os"
	"time"
)

// NewBootClock returns ErrUnsupported: CLOCK_BOOTTIME exists only on Linux.
func NewBootClock() (Clock, error) { return nil, ErrUnsupported }

// ProcessClock is a monotonic clock since process start, for running the
// agent on a development machine only (it does not survive restarts).
type ProcessClock struct{ start time.Time }

// NewProcessClock returns a ProcessClock.
func NewProcessClock() *ProcessClock { return &ProcessClock{start: time.Now()} }

// Now implements Clock.
func (c *ProcessClock) Now() time.Duration { return time.Since(c.start) }

func signalPID(pid int, sig Sig) error { return ErrUnsupported }

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
