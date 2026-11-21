package programs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

type fakePotluck struct {
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
	srv   *httptest.Server
}

func newFakePotluck(t *testing.T) *fakePotluck {
	f := &fakePotluck{files: map[string][]byte{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		b, ok := f.files[r.URL.Path]
		f.hits[r.URL.Path]++
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePotluck) add(name string, body string) (path, sum string) {
	path = "/api/pool/programs/0.1.0/linux-amd64-vulkan/" + name
	s := sha256.Sum256([]byte(body))
	f.mu.Lock()
	f.files[path] = []byte(body)
	f.mu.Unlock()
	return path, hex.EncodeToString(s[:])
}

func (f *fakePotluck) hitCount(p string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.hits[p] }

func newMgr(t *testing.T, base string, free uint64) *Manager {
	return New(Config{Root: filepath.Join(t.TempDir(), "programs"), Release: "0.1.0", BaseURL: base, RetryWait: 1,
		DiskFree: func(string) (uint64, error) { return free, nil }})
}

func TestDownloadVerifyAndRestart(t *testing.T) {
	pl := newFakePotluck(t)
	p1, s1 := pl.add("rpc-server", "RPC")
	p2, s2 := pl.add("node_exporter", "NODE")
	m := newMgr(t, pl.srv.URL, 1<<30)
	if held, _ := m.Prepare(); len(held) != 0 {
		t.Fatal(held)
	}
	// A stale .part from a crash is deleted at start.
	os.WriteFile(filepath.Join(m.Dir(), "rpc-server.part"), []byte("junk"), 0o644)
	m.Prepare()
	if _, err := os.Stat(filepath.Join(m.Dir(), "rpc-server.part")); !os.IsNotExist(err) {
		t.Fatal(".part kept")
	}
	entries := []agentEntry{{"rpc-server", p1, s1, 3}, {"node_exporter", p2, s2, 4}}
	if err := m.Sync(context.Background(), toEntries(entries), nil); err != nil {
		t.Fatal(err)
	}
	if !m.Ready() || m.Error() != nil {
		t.Fatal("not ready")
	}
	path, ok := m.Path("rpc-server")
	if !ok {
		t.Fatal("not verified")
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		t.Fatal("not executable")
	}
	// A restarted agent re-hashes and does not fetch again.
	m2 := newMgr(t, pl.srv.URL, 1<<30)
	m2.cfg.Root = m.cfg.Root
	held, _ := m2.Prepare()
	if len(held) != 2 || held[1].Name != "rpc-server" || held[1].SHA256 != s1 {
		t.Fatalf("%+v", held)
	}
	m2.Sync(context.Background(), toEntries(entries), nil)
	if pl.hitCount(p1) != 1 {
		t.Fatalf("fetched again: %d", pl.hitCount(p1))
	}
}

type agentEntry struct {
	name, path, sum string
	bytes           int64
}

func toEntries(a []agentEntry) []entry {
	var out []entry
	for _, e := range a {
		out = append(out, entry{Name: e.name, Path: e.path, SHA256: e.sum, Bytes: e.bytes})
	}
	return out
}

func TestWrongChecksumRetriedThenReported(t *testing.T) {
	pl := newFakePotluck(t)
	p1, _ := pl.add("rpc-server", "RPC")
	m := newMgr(t, pl.srv.URL, 1<<30)
	m.Prepare()
	err := m.Sync(context.Background(), []entry{{Name: "rpc-server", Path: p1, SHA256: strings.Repeat("0", 64), Bytes: 3}}, nil)
	if err == nil || m.Ready() || m.Error() == nil || !strings.Contains(*m.Error(), "sha256 mismatch") {
		t.Fatalf("%v %v", err, m.Error())
	}
	if pl.hitCount(p1) != 3 {
		t.Fatalf("attempts %d, want 3", pl.hitCount(p1))
	}
	if _, ok := m.Path("rpc-server"); ok {
		t.Fatal("a file whose check failed must never be runnable")
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), "rpc-server")); !os.IsNotExist(err) {
		t.Fatal("bad file renamed into place")
	}
	if ents, _ := os.ReadDir(m.Dir()); len(ents) != 0 {
		t.Fatal("left files behind")
	}
}

func TestTooLittleSpace(t *testing.T) {
	pl := newFakePotluck(t)
	p1, s1 := pl.add("rpc-server", "RPC")
	m := newMgr(t, pl.srv.URL, 2)
	m.Prepare()
	err := m.Sync(context.Background(), []entry{{Name: "rpc-server", Path: p1, SHA256: s1, Bytes: 3 << 20}}, nil)
	if err == nil || !strings.Contains(*m.Error(), "not enough free space") || pl.hitCount(p1) != 0 {
		t.Fatalf("%v hits=%d", m.Error(), pl.hitCount(p1))
	}
}

func TestFetchOnlyWhileIdle(t *testing.T) {
	pl := newFakePotluck(t)
	p1, s1 := pl.add("rpc-server", "RPC")
	m := newMgr(t, pl.srv.URL, 1<<30)
	m.Prepare()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	waitIdle := func(c context.Context) bool { calls++; cancel(); <-c.Done(); return false }
	if err := m.Sync(ctx, []entry{{Name: "rpc-server", Path: p1, SHA256: s1}}, waitIdle); err == nil {
		t.Fatal("must not fetch while busy")
	}
	if calls != 1 || pl.hitCount(p1) != 0 {
		t.Fatal("fetched while busy")
	}
}

func TestMissingLibraries(t *testing.T) {
	pl := newFakePotluck(t)
	p1, s1 := pl.add("rpc-server", "RPC")
	m := newMgr(t, pl.srv.URL, 1<<30)
	m.cfg.LibChecked = []string{"rpc-server", "llama-server"}
	m.cfg.MissingLibs = func(path string) ([]string, error) { return []string{"libgomp.so.1", "libvulkan.so.1"}, nil }
	m.Prepare()
	m.Sync(context.Background(), []entry{{Name: "rpc-server", Path: p1, SHA256: s1}}, nil)
	if m.Ready() || m.Error() == nil || *m.Error() != "missing libraries: rpc-server needs libgomp.so.1, libvulkan.so.1" {
		t.Fatal(m.Error())
	}
}

func TestBadNameAndOldReleases(t *testing.T) {
	pl := newFakePotluck(t)
	m := newMgr(t, pl.srv.URL, 1<<30)
	m.Prepare()
	if err := m.Sync(context.Background(), []entry{{Name: "../evil", Path: "/x", SHA256: "x"}}, nil); err == nil {
		t.Fatal("path name accepted")
	}
	old := filepath.Join(m.cfg.Root, "0.0.9")
	os.MkdirAll(old, 0o755)
	if r := m.DeleteOldReleases(false); r != nil {
		t.Fatal("deleted before the new set is verified")
	}
	m.Sync(context.Background(), nil, nil)
	if r := m.DeleteOldReleases(true); r != nil {
		t.Fatal("deleted while a launch exists")
	}
	if r := m.DeleteOldReleases(false); len(r) != 1 || r[0] != "0.0.9" {
		t.Fatal(r)
	}
}

type entry = agentproto.ProgramEntry

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// A registration again cancels the download of the registration before it;
// the next one starts at once. That is the agent's own doing, not a fault:
// nothing is logged above DEBUG and programs_error stays empty.
func TestCancelledDownloadIsNotAnError(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	body := "NODE"
	sum := sha256.Sum256([]byte(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.Write([]byte("NO"))
		w.(http.Flusher).Flush()
		first := false
		once.Do(func() { first = true; close(started) })
		if first {
			select { // the first download hangs half way until it is cancelled
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Write([]byte("DE"))
	}))
	defer srv.Close()
	defer close(release)
	var logs lockedBuf
	m := New(Config{Root: filepath.Join(t.TempDir(), "programs"), Release: "0.1.0", BaseURL: srv.URL, RetryWait: 1,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	m.Prepare()
	entries := []entry{{Name: "node_exporter", Path: "/p/node_exporter", SHA256: hex.EncodeToString(sum[:]), Bytes: 4}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Sync(ctx, entries, nil) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync after its cancellation: %v", err)
	}
	if out := logs.String(); strings.Contains(out, "level=ERROR") || strings.Contains(out, "level=WARN") {
		t.Fatalf("a cancelled download was logged as a fault:\n%s", out)
	}
	if out := logs.String(); !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "node_exporter") {
		t.Fatalf("the cancellation is not in the debug log:\n%s", out)
	}
	if e := m.Error(); e != nil {
		t.Fatalf("programs_error %q after a cancellation", *e)
	}
	if err := m.Sync(context.Background(), entries, nil); err != nil || !m.Ready() {
		t.Fatalf("the next download: %v", err)
	}
}
