package selflog

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
)

// helperEnv makes the test binary a child that writes output and exits.
const helperEnv = "SELFLOG_TEST_CHILD_MIB"

func TestMain(m *testing.M) {
	if v := os.Getenv(helperEnv); v != "" {
		var mib int
		fmt.Sscan(v, &mib)
		line := strings.Repeat("x", 199) + "\n" // 200 bytes a line
		w := bufio.NewWriterSize(os.Stdout, 64<<10)
		for i := 0; i < mib<<20/len(line); i++ {
			w.WriteString(line)
		}
		w.Flush()
		fmt.Fprintln(os.Stderr, "child done")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeStore records what reaches the store's intake; hang makes it never
// answer.
type fakeStore struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []post
	hang atomic.Bool
	n    atomic.Int32
}

type post struct {
	path  string
	extra map[string]string
	query map[string]string
	lines []string
}

func newFakeStore(t *testing.T) *fakeStore {
	s := &fakeStore{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.n.Add(1)
		if s.hang.Load() {
			<-r.Context().Done()
			return
		}
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			z, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			body = z
		}
		b, _ := io.ReadAll(body)
		p := post{path: r.URL.Path, extra: map[string]string{}, query: map[string]string{}}
		for k := range r.URL.Query() {
			p.query[k] = r.URL.Query().Get(k)
		}
		for _, kv := range strings.Split(r.URL.Query().Get("extra_fields"), ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				p.extra[k] = v
			}
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				p.lines = append(p.lines, l)
			}
		}
		s.mu.Lock()
		s.got = append(s.got, p)
		s.mu.Unlock()
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeStore) posts() []post {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]post(nil), s.got...)
}

// waitFor polls cond until it holds or 15 s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type setup struct {
	store *fakeStore
	relay *logrelay.Relay
	ship  *Shipper
	on    atomic.Bool
	log   *slog.Logger
	print *strings.Builder
}

func newSetup(t *testing.T, mod ...func(*Options)) *setup {
	t.Helper()
	st := newFakeStore(t)
	r, err := logrelay.New(logrelay.Options{StoreURL: st.srv.URL + "/logs", Config: func() config.Logs { return config.DefaultLogs() }})
	if err != nil {
		t.Fatal(err)
	}
	s := &setup{store: st, relay: r, print: &strings.Builder{}}
	s.on.Store(true)
	o := Options{On: func() bool { return s.on.Load() && r.StoreOn() }, InsertURL: r.InsertURL,
		Node: func() (string, string) { return "host-a", "host-a" }, Release: "0.1.2", Platform: "linux-amd64",
		CheckEvery: 20 * time.Millisecond, FirstPause: 50 * time.Millisecond, MaxPause: 200 * time.Millisecond,
		HTTP: &http.Client{Timeout: 2 * time.Second}}
	for _, m := range mod {
		m(&o)
	}
	s.ship = New(o)
	var mu sync.Mutex
	s.log = slog.New(s.ship.Handler(slog.NewTextHandler(lockedWriter{&mu, s.print}, nil)))
	return s
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (s *setup) run(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.ship.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func allLines(ps []post) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.lines...)
	}
	return out
}

// Potluck's own lines reach the store's intake with source scheduler and the
// local node's fields, set for the whole request; the printed log is
// unchanged; each line carries Potluck's time.
func TestOwnLinesReachTheStore(t *testing.T) {
	s := newSetup(t)
	s.run(t)
	before := time.Now()
	s.log.Info("llm-scheduler started", "listen", "0.0.0.0:8080")
	s.log.Warn("a warning")
	waitFor(t, "two lines at the store", func() bool { return len(allLines(s.store.posts())) >= 2 })
	p := s.store.posts()[0]
	if p.path != "/logs/insert/jsonline" {
		t.Errorf("path %q", p.path)
	}
	want := map[string]string{"node_id": "host-a", "node_name": "host-a", "platform": "linux-amd64", "release": "0.1.2", "source": "scheduler"}
	for k, v := range want {
		if p.extra[k] != v {
			t.Errorf("extra_fields %s = %q, want %q (%v)", k, p.extra[k], v, p.extra)
		}
	}
	if !strings.Contains(p.query["ignore_fields"], "profile") || p.query["_stream_fields"] != "source,node_id,unit" {
		t.Errorf("store arguments: %v", p.query)
	}
	l := allLines(s.store.posts())
	if !strings.Contains(l[0], `"_msg":"llm-scheduler started listen=0.0.0.0:8080"`) || !strings.Contains(l[0], `"level":"info"`) || !strings.Contains(l[1], `"level":"warn"`) {
		t.Errorf("lines: %q", l)
	}
	i := strings.Index(l[0], `"_time":"`)
	if i < 0 {
		t.Fatalf("no _time: %s", l[0])
	}
	at, err := time.Parse(time.RFC3339Nano, l[0][i+9:i+9+strings.Index(l[0][i+9:], `"`)])
	if err != nil || at.Before(before.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("_time %v (%v), want about %v", at, err, before)
	}
	if !strings.Contains(s.print.String(), "llm-scheduler started") {
		t.Errorf("the printed log lost the line: %q", s.print.String())
	}
}

// A child's output reaches the store with source host, the profile and the
// instance id, and its stream.
func TestChildOutputReachesTheStore(t *testing.T) {
	s := newSetup(t)
	s.run(t)
	waitFor(t, "shipping on", s.ship.ship.On)
	tap := s.ship.ChildTap("gemma-4-12b:48k", 7)
	tap.Line("stderr", "load_tensors: loading model")
	tap.Line("stdout", "server is listening")
	tap.Close()
	waitFor(t, "two child lines", func() bool { return len(allLines(s.store.posts())) >= 2 })
	p := s.store.posts()[0]
	for k, v := range map[string]string{"source": "host", "profile": "gemma-4-12b:48k", "instance_id": "7", "node_id": "host-a"} {
		if p.extra[k] != v {
			t.Errorf("extra_fields %s = %q, want %q", k, p.extra[k], v)
		}
	}
	l := allLines(s.store.posts())
	if !strings.Contains(l[0], `"stream":"stderr"`) || !strings.Contains(l[1], `"stream":"stdout"`) {
		t.Errorf("lines: %q", l)
	}
}

// When the ring overflows, the next batch of that source starts with one
// line that says how many lines were dropped.
func TestDroppedLinesNotice(t *testing.T) {
	s := newSetup(t, func(o *Options) { o.BufferBytes = 8 << 10 })
	// Before Run the lines are kept (the start of the log); the small ring
	// drops the oldest.
	for i := range 500 {
		s.log.Info(fmt.Sprintf("line %d", i))
	}
	if s.ship.Ring().DroppedTotal() == 0 {
		t.Fatal("nothing was dropped")
	}
	s.run(t)
	waitFor(t, "a batch", func() bool { return len(allLines(s.store.posts())) > 0 })
	first := allLines(s.store.posts())[0]
	if !strings.Contains(first, "potluck: ") || !strings.Contains(first, "lines of this source were dropped") || !strings.Contains(first, `"level":"warn"`) {
		t.Errorf("the first line is not the drop notice: %s", first)
	}
}

// A child that writes faster than the rate: the part over it is skipped and
// one line says how many bytes.
func TestChildRateLimit(t *testing.T) {
	s := newSetup(t, func(o *Options) { o.ChildBytesPerS = 2000 })
	s.run(t)
	waitFor(t, "shipping on", s.ship.ship.On)
	tap := s.ship.ChildTap("p", 1)
	line := strings.Repeat("y", 99)
	for range 200 { // 20 000 bytes at once, ten times the rate
		tap.Line("stdout", line)
	}
	tap.Close()
	waitFor(t, "the skip notice", func() bool {
		for _, l := range allLines(s.store.posts()) {
			if strings.Contains(l, "bytes of this program's output were skipped") {
				return true
			}
		}
		return false
	})
	kept := 0
	for _, l := range allLines(s.store.posts()) {
		if strings.Contains(l, line) {
			kept++
		}
	}
	if kept == 0 || kept > 40 {
		t.Errorf("%d lines kept of 200 at a rate of 2000 bytes/s", kept)
	}
}

// Logging off: nothing is kept or sent, and what was kept is dropped; on
// again, lines flow.
func TestOffShipsNothing(t *testing.T) {
	s := newSetup(t)
	s.on.Store(false)
	s.log.Info("kept until the first check")
	s.run(t)
	waitFor(t, "the ring emptied", func() bool { return s.ship.Ring().Len() == 0 })
	s.log.Info("not kept")
	s.ship.ChildTap("p", 1).Line("stdout", "not kept either")
	time.Sleep(100 * time.Millisecond)
	if n := s.store.n.Load(); n != 0 || s.ship.Ring().Len() != 0 {
		t.Fatalf("logging off: the store got %d requests, the ring holds %d lines", n, s.ship.Ring().Len())
	}
	s.on.Store(true)
	waitFor(t, "shipping on", s.ship.ship.On)
	s.log.Info("now it goes")
	waitFor(t, "the line", func() bool { return len(allLines(s.store.posts())) == 1 })
	if l := allLines(s.store.posts())[0]; !strings.Contains(l, "now it goes") {
		t.Errorf("line: %s", l)
	}
}

// The store hangs and the ring is full: a real child that writes 16 MiB as
// fast as it can still runs to its end, its log file holds every byte, and
// Potluck's own logging returns at once. If any producer waited on the store,
// the child would block on its full pipe and never exit.
func TestHungStoreSlowsNeitherChildNorLogging(t *testing.T) {
	const mib = 16
	s := newSetup(t, func(o *Options) { o.BufferBytes = 64 << 10; o.ChildBytesPerS = 1 << 40 })
	s.store.hang.Store(true)
	s.run(t)
	waitFor(t, "shipping on", s.ship.ship.On)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ports, err := instance.NewPortAllocator(5900, 5999)
	if err != nil {
		t.Fatal(err)
	}
	runChild := func(dir string, tap bool) time.Duration {
		l := &instance.ProcessLauncher{Ports: ports, LogDir: dir, Logger: slog.New(slog.DiscardHandler)}
		if tap {
			l.Tap = func(p string, id uint64) instance.ChildTap { return s.ship.ChildTap(p, id) }
		}
		t0 := time.Now()
		c, err := l.Start(instance.StartSpec{Build: func(int) ([]string, error) { return []string{exe, "-test.run=^$"}, nil },
			Env: []string{helperEnv + "=" + fmt.Sprint(mib)}, Profile: "flood", InstanceID: 3})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case st := <-c.Exited():
			if st.Code != 0 {
				t.Fatalf("child: %+v", st)
			}
		case <-time.After(120 * time.Second):
			c.Stop(instance.StopKill)
			t.Fatal("the child did not finish within 120 s: its output is held up")
		}
		return time.Since(t0)
	}
	base := runChild(t.TempDir(), false)

	// Potluck logs as fast as it can meanwhile, through the same ring.
	stop := make(chan struct{})
	var worstLog time.Duration
	var logged atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			t0 := time.Now()
			s.log.Info("flood", "n", logged.Add(1))
			worstLog = max(worstLog, time.Since(t0))
		}
	}()
	dir := t.TempDir()
	with := runChild(dir, true)
	close(stop)
	wg.Wait()

	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if len(files) != 1 {
		t.Fatalf("log files: %v", files)
	}
	fi, err := os.Stat(files[0])
	want := int64(mib<<20/200*200) + int64(len("child done\n"))
	if err != nil || fi.Size() != want {
		t.Errorf("the child log file has %v bytes (%v), want %d", fi.Size(), err, want)
	}
	if s.ship.Ring().Bytes() > 64<<10 || s.ship.Ring().DroppedTotal() == 0 {
		t.Errorf("the ring holds %d bytes and dropped %d lines: it was not full", s.ship.Ring().Bytes(), s.ship.Ring().DroppedTotal())
	}
	t.Logf("child of %d MiB: %v without the tap, %v with the tap, a hung store and a full ring; %d log calls meanwhile, the slowest %v; %d lines dropped",
		mib, base, with, logged.Load(), worstLog, s.ship.Ring().DroppedTotal())
	if worstLog > time.Second {
		t.Errorf("a log call took %v with the store hung", worstLog)
	}
}
