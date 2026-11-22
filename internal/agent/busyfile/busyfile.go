// Package busyfile keeps the node's busy file (agent contract 6.14): a small
// text file that exists exactly while a launch exists on the node, so other
// software on the machine can leave it alone without asking the agent over
// the network.
package busyfile

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultPath is the busy file on a bare Linux install.
const DefaultPath = "/run/potluck/busy"

// Refresh is how often the file is rewritten while busy, so a reader can
// also judge it by the age of its updated line.
const Refresh = 15 * time.Second

// File tracks whether the node is busy and mirrors it into Path. With Path
// "" it only tracks (GET /status still reports it). Problems with the file
// are logged once and never fail the caller.
type File struct {
	Path   string
	Logger *slog.Logger
	Now    func() time.Time // default time.Now
	PID    int              // default os.Getpid()

	mu      sync.Mutex
	n       int
	since   time.Time
	written time.Time
	warned  bool
}

func (f *File) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *File) warn(msg string, err error) {
	if f.warned {
		return
	}
	f.warned = true
	if f.Logger != nil {
		f.Logger.Warn(msg+"; the agent carries on without it (logged once)", "file", f.Path, "err", err)
	}
}

// RemoveLeftover removes a busy file left by an earlier agent process. It is
// called before launches are re-adopted; Set writes it again if one is.
func (f *File) RemoveLeftover() {
	if f.Path == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
		f.warn("cannot remove the left-over busy file", err)
	}
}

// Set records how many launches exist now (starting, running or stopping)
// and brings the file up to date: written when the count changes or the
// last write is older than Refresh, removed when the count is 0.
func (f *File) Set(launches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	switch {
	case launches <= 0:
		if f.n > 0 && f.Path != "" {
			if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
				f.warn("cannot remove the busy file", err)
			}
		}
		f.n, f.since = 0, time.Time{}
		return
	case f.n == 0:
		f.since = now
	case f.n == launches && now.Sub(f.written) < Refresh:
		return
	}
	f.n = launches
	f.written = now
	if f.Path != "" {
		if err := f.write(now); err != nil {
			f.warn("cannot write the busy file", err)
		}
	}
}

// State is the current count and when the node became busy (zero when idle).
func (f *File) State() (launches int, since time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n, f.since
}

// write replaces the file atomically: a temporary file in the same folder,
// renamed over it, so a reader sees the old or the new content, never part.
func (f *File) write(now time.Time) error {
	pid := f.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	body := fmt.Sprintf("launches %d\nsince %d\nupdated %d\nagent_pid %d\n", f.n, f.since.Unix(), now.Unix(), pid)
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".busy-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.WriteString(body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o644)
	}
	if err == nil {
		err = os.Rename(name, f.Path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}
