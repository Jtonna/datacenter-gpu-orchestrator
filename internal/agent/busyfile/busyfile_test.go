package busyfile

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration)     { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func newClock() *clock                   { return &clock{t: time.Unix(1_800_000_000, 0)} }
func read(t *testing.T, p string) string { b, _ := os.ReadFile(p); return string(b) }

func TestFileFollowsTheLaunches(t *testing.T) {
	c := newClock()
	p := filepath.Join(t.TempDir(), "busy")
	f := &File{Path: p, Now: c.now, PID: 4242}
	f.Set(0)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("idle node has a busy file")
	}
	f.Set(1)
	if got := read(t, p); got != "launches 1\nsince 1800000000\nupdated 1800000000\nagent_pid 4242\n" {
		t.Fatalf("%q", got)
	}
	if runtime.GOOS == "linux" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	c.add(5 * time.Second)
	f.Set(2)
	if got := read(t, p); got != "launches 2\nsince 1800000000\nupdated 1800000005\nagent_pid 4242\n" {
		t.Fatalf("%q", got)
	}
	c.add(5 * time.Second)
	f.Set(2) // unchanged and fresh: not rewritten
	if !strings.Contains(read(t, p), "updated 1800000005\n") {
		t.Fatal("rewritten without a change")
	}
	c.add(Refresh)
	f.Set(2) // unchanged but old: refreshed
	if !strings.Contains(read(t, p), "updated 1800000025\n") {
		t.Fatalf("not refreshed: %q", read(t, p))
	}
	if n, since := f.State(); n != 2 || since.Unix() != 1800000000 {
		t.Fatal(n, since)
	}
	f.Set(0)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("not removed after the last launch")
	}
	c.add(time.Minute)
	f.Set(1) // busy again: a new since
	if !strings.Contains(read(t, p), "since 1800000085\n") {
		t.Fatalf("%q", read(t, p))
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".busy-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// A reader that reads in a loop while the count changes never sees a
// partial file.
func TestReaderNeverSeesAPartialFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "busy")
	f := &File{Path: p}
	var stop atomic.Bool
	var bad atomic.Value
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			b, err := os.ReadFile(p)
			if err != nil {
				continue // absent while idle, or (Windows) the rename in progress
			}
			s := string(b)
			lines := strings.Split(s, "\n")
			if len(lines) != 5 || lines[4] != "" || !strings.HasPrefix(lines[0], "launches ") || !strings.HasPrefix(lines[3], "agent_pid ") {
				bad.Store(s)
				return
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		f.Set(1 + i%3)
		if i%50 == 49 {
			f.Set(0)
		}
	}
	stop.Store(true)
	wg.Wait()
	if s := bad.Load(); s != nil {
		t.Fatalf("partial file read: %q", s)
	}
}

func TestLeftoverRemovedAndProblemsLoggedOnce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "busy")
	os.WriteFile(p, []byte("launches 1\n"), 0o644)
	(&File{Path: p}).RemoveLeftover()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("left-over file kept")
	}

	var logs bytes.Buffer
	f := &File{Path: filepath.Join(dir, "missing", "busy"), Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	for i := 0; i < 5; i++ {
		f.Set(1 + i)
		f.Set(0)
	}
	if n := strings.Count(logs.String(), "cannot write the busy file"); n != 1 {
		t.Fatalf("logged %d times: %s", n, logs.String())
	}
	if n, _ := f.State(); n != 0 {
		t.Fatal("state not tracked")
	}
	g := &File{} // no path: tracks only
	g.Set(1)
	if n, since := g.State(); n != 1 || since.IsZero() {
		t.Fatal(n, since)
	}
}
