package filecache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSys keeps a cached figure per file name and drops it to rest.
type fakeSys struct {
	mu     sync.Mutex
	cached map[string]int64 // by base name
	rest   int64            // what Drop leaves cached
	drops  int
	fail   string // "count" or "drop"
	block  chan struct{}
}

func (s *fakeSys) Cached(f *os.File, size int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == "count" {
		return 0, errors.New("mincore: no")
	}
	return s.cached[filepath.Base(f.Name())], nil
}

func (s *fakeSys) Drop(f *os.File, size int64) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == "drop" {
		return errors.New("fadvise: no")
	}
	s.drops++
	s.cached[filepath.Base(f.Name())] = s.rest
	return nil
}

func modelDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "m.gguf"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(d, "sub"), 0o755)
	os.WriteFile(filepath.Join(d, "sub", "n.gguf"), make([]byte, 10), 0o644)
	return d
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func reasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

func statusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestEvictMeasuresBeforeAndAfter(t *testing.T) {
	d := modelDir(t)
	sys := &fakeSys{cached: map[string]int64{"m.gguf": 900}, rest: 4}
	c := &clock{t: time.Unix(1000, 0)}
	e := &Evictor{Root: d, Sys: sys, Now: c.now}
	r, err := e.Evict("m.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if r.File != "m.gguf" || r.FileBytes != 1000 || r.CachedBefore != 900 || r.CachedAfter != 4 || sys.drops != 1 {
		t.Fatalf("%+v drops=%d", r, sys.drops)
	}
	if r2, err := e.Evict("sub/n.gguf"); err == nil {
		t.Fatalf("a second eviction right away was not refused: %+v", r2)
	} else if statusOf(err) != 429 || reasonOf(err) != ReasonTooSoon || !strings.Contains(err.Error(), "wait") {
		t.Fatalf("%v", err)
	}
	c.t = c.t.Add(DefaultMinGap)
	if r, err := e.Evict("sub/n.gguf"); err != nil || r.File != "sub/n.gguf" || r.FileBytes != 10 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestPathSafety(t *testing.T) {
	d := modelDir(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.gguf"), []byte("x"), 0o644)
	symlinks := os.Symlink(filepath.Join(outside, "secret.gguf"), filepath.Join(d, "escape.gguf")) == nil
	os.MkdirAll(filepath.Join(d, ".trash", "0123"), 0o755)
	os.WriteFile(filepath.Join(d, ".trash", "0123", "t.gguf"), make([]byte, 10), 0o644)
	if symlinks {
		os.Symlink(filepath.Join(d, "m.gguf"), filepath.Join(d, "inside.gguf"))
		os.Symlink(outside, filepath.Join(d, "outdir"))
		os.Symlink(filepath.Join(d, ".trash", "0123", "t.gguf"), filepath.Join(d, "totrash.gguf"))
	}
	cases := []struct {
		name   string
		status int
		reason string
	}{
		{"", 400, ReasonInvalid},
		{"../secret.gguf", 400, ReasonInvalid},
		{"sub/../../secret.gguf", 400, ReasonInvalid},
		{"/etc/passwd", 400, ReasonInvalid},
		{"sub", 400, ReasonInvalid}, // a directory
		{"missing.gguf", 404, ReasonNotFound},
		{"sub/n.gguf", 0, ""},                      // a plain sub-folder: allowed
		{"sub/../m.gguf", 400, ReasonInvalid},      // the shared path rule (internal/modelpath) refuses any ".." part
		{".trash/0123/t.gguf", 400, ReasonInvalid}, // a dot-folder of the models folder, though the file is there
		{"sub/.x/t.gguf", 400, ReasonInvalid},
		{`sub
.gguf`, 400, ReasonInvalid},
	}
	if symlinks {
		cases = append(cases,
			struct {
				name   string
				status int
				reason string
			}{"escape.gguf", 400, ReasonInvalid},
			struct {
				name   string
				status int
				reason string
			}{"outdir/secret.gguf", 400, ReasonInvalid},
			struct {
				name   string
				status int
				reason string
			}{"inside.gguf", 0, ""},
			struct {
				name   string
				status int
				reason string
			}{"totrash.gguf", 400, ReasonInvalid}) // resolves into a dot-folder
	} else {
		t.Log("symlinks cannot be made here; the symlink cases are left to the Linux run")
	}
	for _, c := range cases {
		e := &Evictor{Root: d, Sys: &fakeSys{cached: map[string]int64{}}, MinGap: -1}
		_, err := e.Evict(c.name)
		if c.status == 0 {
			if err != nil {
				t.Errorf("%q: %v", c.name, err)
			}
			continue
		}
		if statusOf(err) != c.status || reasonOf(err) != c.reason {
			t.Errorf("%q: got %v, want %d %s", c.name, err, c.status, c.reason)
		}
	}
}

func TestRefusedWhileALaunchRuns(t *testing.T) {
	d := modelDir(t)
	sys := &fakeSys{cached: map[string]int64{"m.gguf": 500}}
	busy := true
	e := &Evictor{Root: d, Sys: sys, Busy: func() (bool, string) { return busy, "launch ln-1 is ready" }}
	_, err := e.Evict("m.gguf")
	if statusOf(err) != 409 || reasonOf(err) != ReasonBusy || !strings.Contains(err.Error(), "ln-1") || sys.drops != 0 {
		t.Fatalf("%v drops=%d", err, sys.drops)
	}
	// a refusal does not count against the rate limit
	busy = false
	if _, err := e.Evict("m.gguf"); err != nil {
		t.Fatal(err)
	}
}

func TestOneAtATime(t *testing.T) {
	d := modelDir(t)
	sys := &fakeSys{cached: map[string]int64{}, block: make(chan struct{})}
	e := &Evictor{Root: d, Sys: sys, MinGap: -1}
	done := make(chan error)
	go func() { _, err := e.Evict("m.gguf"); done <- err }()
	for {
		e.mu.Lock()
		r := e.running
		e.mu.Unlock()
		if r {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := e.Evict("sub/n.gguf"); statusOf(err) != 429 {
		t.Fatalf("%v", err)
	}
	close(sys.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSystemCallFailures(t *testing.T) {
	d := modelDir(t)
	for _, f := range []string{"count", "drop"} {
		e := &Evictor{Root: d, Sys: &fakeSys{cached: map[string]int64{}, fail: f}, MinGap: -1}
		if _, err := e.Evict("m.gguf"); statusOf(err) != 500 || reasonOf(err) != ReasonFailed {
			t.Errorf("%s: %v", f, err)
		}
	}
	e := &Evictor{Root: d}
	if _, err := e.Evict("m.gguf"); statusOf(err) != 501 || reasonOf(err) != ReasonUnsupported {
		t.Fatalf("%v", err)
	}
	e = &Evictor{Root: filepath.Join(d, "nope"), Sys: &fakeSys{}}
	if _, err := e.Evict("m.gguf"); statusOf(err) != 503 || reasonOf(err) != ReasonUnreachable {
		t.Fatalf("%v", err)
	}
}

func TestTheOnePercentRule(t *testing.T) {
	for _, c := range []struct {
		size, cached int64
		cold         bool
		pct          float64
	}{
		{1000, 0, true, 0},
		{1000, 9, true, 0.9},
		{1000, 10, false, 1},
		{1000, 1000, false, 100},
		{0, 0, true, 0},
		{0, 5, false, 0},
	} {
		if Cold(c.size, c.cached) != c.cold || CachedPct(c.size, c.cached) != c.pct {
			t.Errorf("%+v: cold=%v pct=%v", c, Cold(c.size, c.cached), CachedPct(c.size, c.cached))
		}
	}
}
