package instance

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a goroutine-safe buffer for capturing log output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func logListing(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// runFakeToExit starts the fake llama, waits until it serves, stops it and
// returns once it has exited (so its log file is complete and closed).
func runFakeToExit(t *testing.T, l *ProcessLauncher, spec StartSpec) Child {
	t.Helper()
	c := start(t, l, spec)
	waitHealthy(t, c)
	c.Stop(StopGraceful)
	recvExit(t, c)
	return c
}

func TestChildLog_WritesFullOutputAndExposesPath(t *testing.T) {
	dir := t.TempDir()
	l := newLauncher(t)
	l.LogDir = dir
	l.TailLines = 1 // the in-memory tail keeps one line; the file must keep them all
	spec := fakeSpec(t)
	spec.Profile, spec.InstanceID = "gemma-4-12b:48k", 7
	c := runFakeToExit(t, l, spec)

	lf, ok := c.(interface{ LogFile() string })
	if !ok || lf.LogFile() == "" {
		t.Fatalf("child has no LogFile: %T", c)
	}
	path := lf.LogFile()
	if filepath.Dir(path) != dir {
		t.Fatalf("log file %q is not in %q", path, dir)
	}
	name := filepath.Base(path)
	if !regexp.MustCompile(`^\d{8}T\d{6}Z-gemma-4-12b_48k-7\.log$`).MatchString(name) {
		t.Fatalf("file name %q", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "fakellama: listening on") {
		t.Fatalf("file lacks the child's output:\n%s", got)
	}
	if len(c.Tail(10)) != 1 {
		t.Fatalf("in-memory tail should still work and keep 1 line, got %q", c.Tail(10))
	}
}

func TestChildLog_OffWhenDirUnset(t *testing.T) {
	l := newLauncher(t)
	c := runFakeToExit(t, l, fakeSpec(t))
	if lf := c.(interface{ LogFile() string }).LogFile(); lf != "" {
		t.Fatalf("LogFile = %q with the feature off", lf)
	}
	if len(c.Tail(5)) == 0 {
		t.Fatal("tail empty")
	}
}

func TestChildLog_RetentionDeletesOldest(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("2020010%dT000000Z-old-%d.log", i, i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "notes.txt") // not ours: never deleted
	if err := os.WriteFile(other, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := newLauncher(t)
	l.LogDir, l.LogKeep = dir, 3
	c := runFakeToExit(t, l, fakeSpec(t))

	var logs []string
	for _, n := range logListing(t, dir) {
		if strings.HasSuffix(n, ".log") {
			logs = append(logs, n)
		}
	}
	want := []string{"20200104T000000Z-old-4.log", "20200105T000000Z-old-5.log", filepath.Base(c.(interface{ LogFile() string }).LogFile())}
	if strings.Join(logs, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v, want %v", logs, want)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("an unrelated file was deleted: %v", err)
	}
}

func TestChildLog_BadDirNeverFailsStartAndWarnsOnce(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf syncBuf
	l := newLauncher(t)
	l.LogDir = filepath.Join(notDir, "sub") // cannot be created: its parent is a file
	l.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	for i := 0; i < 2; i++ {
		c := runFakeToExit(t, l, fakeSpec(t))
		if lf := c.(interface{ LogFile() string }).LogFile(); lf != "" {
			t.Fatalf("LogFile = %q for an unusable directory", lf)
		}
		if len(c.Tail(5)) == 0 {
			t.Fatal("tail empty: the child did not run normally")
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings, want exactly 1:\n%s", n, buf.String())
	}
}

func TestChildLog_PrepareLogDir(t *testing.T) {
	l := &ProcessLauncher{}
	if err := l.PrepareLogDir(); err != nil {
		t.Fatalf("unset dir: %v", err)
	}
	l.LogDir = filepath.Join(t.TempDir(), "a", "b") // created on demand
	if err := l.PrepareLogDir(); err != nil {
		t.Fatalf("creatable dir: %v", err)
	}
	if st, err := os.Stat(l.LogDir); err != nil || !st.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	if left := logListing(t, l.LogDir); len(left) != 0 {
		t.Fatalf("probe file left behind: %v", left)
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, []byte("x"), 0o644)
	l.LogDir = filepath.Join(f, "sub")
	if err := l.PrepareLogDir(); err == nil {
		t.Fatal("expected an error for an uncreatable dir")
	}
}

func TestChildLogFileName(t *testing.T) {
	at := time.Date(2026, 11, 18, 6, 45, 12, 0, time.UTC)
	for _, tc := range []struct {
		profile string
		id      uint64
		want    string
	}{
		{"gemma-4-12b:48k", 3, "20261118T064512Z-gemma-4-12b_48k-3.log"},
		{"a/b c", 9, "20261118T064512Z-a_b_c-9.log"},
		{"", 1, "20261118T064512Z-child-1.log"},
	} {
		if got := childLogFileName(at, tc.profile, tc.id); got != tc.want {
			t.Errorf("childLogFileName(%q) = %q, want %q", tc.profile, got, tc.want)
		}
	}
}

func TestChildLog_WriteFailureDoesNotStopChild(t *testing.T) {
	var buf syncBuf
	s := &fileSink{f: nil, log: slog.New(slog.NewTextHandler(&buf, nil))}
	// A closed file makes every write fail.
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	s.f, s.path = f, f.Name()
	for i := 0; i < 3; i++ {
		if n, err := s.Write([]byte("hello")); n != 5 || err != nil {
			t.Fatalf("Write = %d, %v; must never fail", n, err)
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, buf.String())
	}
}
