package launch

import (
	"io"
	"os"
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
)

// DefaultLogCap is the launch log cap (contract 6.1): when the kept part is
// larger, its oldest half is dropped.
const DefaultLogCap = 8 << 20

// LogFile is a launch log written by the launch process itself (appended to
// by systemd or by the child's stdout). Offsets are absolute from the start
// of the launch (contract 6.6): dropping the oldest half punches a hole in
// the file instead of moving data, so every offset stays valid and the
// memory is freed (tmpfs and ext4 support hole punching). Dropped is the
// offset below which nothing is kept.
type LogFile struct {
	Path string
	Cap  int64
	// Punch frees a byte range; defaults to osx.PunchHole, falling back to
	// writing zeros where punching is unsupported (development machines).
	Punch func(f *os.File, off, n int64) error

	mu      sync.Mutex
	dropped int64
}

const punchAlign = 4096

func (l *LogFile) cap() int64 {
	if l.Cap > 0 {
		return l.Cap
	}
	return DefaultLogCap
}

// Dropped returns the first kept offset.
func (l *LogFile) Dropped() int64 { l.mu.Lock(); defer l.mu.Unlock(); return l.dropped }

// SetDropped restores the dropped offset (after an agent restart).
func (l *LogFile) SetDropped(n int64) { l.mu.Lock(); l.dropped = n; l.mu.Unlock() }

// Size returns the absolute end offset of the log.
func (l *LogFile) Size() int64 {
	st, err := os.Stat(l.Path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// Enforce drops the oldest half of the kept part when it exceeds the cap.
// It returns true when something was dropped.
func (l *LogFile) Enforce() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	size := l.Size()
	kept := size - l.dropped
	if kept <= l.cap() {
		return false, nil
	}
	newDropped := l.dropped + kept/2
	newDropped -= newDropped % punchAlign
	if newDropped <= l.dropped {
		return false, nil
	}
	f, err := os.OpenFile(l.Path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	punch := l.Punch
	if punch == nil {
		punch = osx.PunchHole
	}
	if err := punch(f, l.dropped, newDropped-l.dropped); err != nil {
		// Fallback: zero the range (keeps offsets; frees nothing).
		if err := zeroRange(f, l.dropped, newDropped-l.dropped); err != nil {
			return false, err
		}
	}
	l.dropped = newDropped
	return true, nil
}

func zeroRange(f *os.File, off, n int64) error {
	buf := make([]byte, 64<<10)
	for n > 0 {
		k := int64(len(buf))
		if k > n {
			k = n
		}
		if _, err := f.WriteAt(buf[:k], off); err != nil {
			return err
		}
		off += k
		n -= k
	}
	return nil
}

// Read returns up to max bytes from offset since, the next offset, and how
// many requested bytes were already dropped (contract 6.6).
func (l *LogFile) Read(since int64, max int) (data []byte, next int64, droppedBytes int64, err error) {
	l.mu.Lock()
	dropped := l.dropped
	l.mu.Unlock()
	if since < 0 {
		since = 0
	}
	if since < dropped {
		droppedBytes = dropped - since
		since = dropped
	}
	f, err := os.Open(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, since, droppedBytes, nil
		}
		return nil, since, droppedBytes, err
	}
	defer f.Close()
	buf := make([]byte, max)
	n, err := f.ReadAt(buf, since)
	if err == io.EOF {
		err = nil
	}
	return buf[:n], since + int64(n), droppedBytes, err
}
