package modelsrc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
)

func TestParseSpec(t *testing.T) {
	ok := map[string]Spec{
		"share:192.0.2.1:/": {KindShare, "192.0.2.1:/"},
		"dir:/srv/models":   {KindDir, "/srv/models"},
		"push":              {Kind: KindPush},
	}
	for in, want := range ok {
		got, err := ParseSpec(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "share:", "share:host:/", "share:192.0.2.1", "dir:rel", "nfs:x"} {
		if _, err := ParseSpec(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if w := (Spec{KindDir, "/srv/models"}).Wire(); w.Kind != "share" || w.Target != "/srv/models" {
		t.Fatal(w)
	}
}

func TestMountArgsExact(t *testing.T) {
	got := "mount " + strings.Join(MountArgs("192.0.2.1:/", "192.0.2.101", "/run/potluck/models"), " ")
	want := "mount -t nfs4 -o ro,hard,vers=4.2,addr=192.0.2.1,clientaddr=192.0.2.101,rsize=1048576 192.0.2.1:/ /run/potluck/models"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

type fakeMounter struct {
	mu      sync.Mutex
	mounted bool
	calls   []string
	failMnt bool
}

func (m *fakeMounter) IsMounted(string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted, nil
}
func (m *fakeMounter) Mount(ctx context.Context, args []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "mount "+strings.Join(args, " "))
	if m.failMnt {
		return errors.New("mount failed")
	}
	m.mounted = true
	return nil
}
func (m *fakeMounter) Unmount(ctx context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "umount "+p)
	m.mounted = false
	return nil
}
func (m *fakeMounter) MkdirAll(string) error { return nil }
func (m *fakeMounter) callList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// fakeProber hands out probes whose outcome the test controls.
type fakeProber struct {
	mu      sync.Mutex
	mode    string // "ok", "fail", "hang"
	started int
	hung    []*fakeProbe
}

type fakeProbe struct {
	done chan struct{}
	err  error
	once sync.Once
}

func (p *fakeProbe) Done() <-chan struct{} { return p.done }
func (p *fakeProbe) Err() error            { <-p.done; return p.err }
func (p *fakeProbe) Kill()                 {} // a probe stuck in D state ignores the kill
func (p *fakeProbe) release()              { p.once.Do(func() { close(p.done) }) }

func (f *fakeProber) Start(dir string) (ProbeHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	p := &fakeProbe{done: make(chan struct{})}
	switch f.mode {
	case "ok":
		p.release()
	case "fail":
		p.err = errors.New("EIO")
		p.release()
	case "hang":
		f.hung = append(f.hung, p)
	}
	return p, nil
}

func (f *fakeProber) set(mode string) { f.mu.Lock(); f.mode = mode; f.mu.Unlock() }
func (f *fakeProber) count() int      { f.mu.Lock(); defer f.mu.Unlock(); return f.started }

func newSrc(mnt *fakeMounter, pr *fakeProber, launch *bool) *Source {
	return New(Config{Spec: Spec{KindShare, "192.0.2.1:/"}, Dir: "/run/potluck/models", Mount: true,
		NodeIP: func() string { return "192.0.2.101" }, Timeout: 20 * time.Millisecond,
		Clock: osx.NewFakeClock(time.Hour), Mounter: mnt, Prober: pr, LaunchExists: func() bool { return *launch }})
}

func TestNeverMountsTwice(t *testing.T) {
	mnt := &fakeMounter{mounted: true}
	launch := false
	s := newSrc(mnt, &fakeProber{mode: "ok"}, &launch)
	if err := s.Start(context.Background()); err != nil || len(mnt.callList()) != 0 {
		t.Fatalf("existing mount must be used: %v", mnt.callList())
	}
	mnt.mounted = false
	s.Start(context.Background())
	s.Start(context.Background())
	if c := mnt.callList(); len(c) != 1 || !strings.HasPrefix(c[0], "mount -t nfs4 -o ro,hard,vers=4.2,addr=192.0.2.1,clientaddr=192.0.2.101,") {
		t.Fatal(c)
	}
}

func TestProbeAgainstHungMount(t *testing.T) {
	mnt := &fakeMounter{mounted: true}
	pr := &fakeProber{mode: "ok"}
	launch := true
	s := newSrc(mnt, pr, &launch)
	ctx := context.Background()
	st := s.State()
	if *st.Reachable || st.CheckedAgeS != nil {
		t.Fatal("before the first probe: unreachable, no age")
	}
	s.ProbeOnce(ctx)
	if !s.Reachable() || s.State().CheckedAgeS == nil {
		t.Fatal("ok probe")
	}
	// The share hangs: the probe times out, the result is unreachable, and
	// while it hangs no second probe is started.
	pr.set("hang")
	s.ProbeOnce(ctx)
	if s.Reachable() || pr.count() != 2 {
		t.Fatal("hung probe")
	}
	for i := 0; i < 6; i++ {
		s.ProbeOnce(ctx)
	}
	if pr.count() != 2 {
		t.Fatalf("started %d probes; at most one may be outstanding", pr.count())
	}
	// No remount while a launch exists, even after many failures.
	if m, u := s.Counts(); m != 0 || u != 0 {
		t.Fatal("remounted during a launch")
	}
	// The hung probe finally returns; the next round starts a new one.
	pr.mu.Lock()
	pr.hung[0].release()
	pr.mu.Unlock()
	pr.set("ok")
	s.ProbeOnce(ctx)
	if !s.Reachable() || pr.count() != 3 {
		t.Fatal("recovered")
	}
}

func TestRemountAfterFiveFailures(t *testing.T) {
	mnt := &fakeMounter{mounted: true}
	pr := &fakeProber{mode: "fail"}
	launch := false
	s := newSrc(mnt, pr, &launch)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		s.ProbeOnce(ctx)
	}
	if m, u := s.Counts(); m != 0 || u != 0 {
		t.Fatal("remounted before five failures")
	}
	s.ProbeOnce(ctx)
	c := mnt.callList()
	if len(c) != 2 || c[0] != "umount /run/potluck/models" || !strings.HasPrefix(c[1], "mount ") {
		t.Fatalf("calls %v", c)
	}
	for _, x := range c {
		if strings.Contains(x, "-l") {
			t.Fatal("lazy unmount")
		}
	}
}

func TestPushHasNoProbe(t *testing.T) {
	s := New(Config{Spec: Spec{Kind: KindPush}, Clock: osx.NewFakeClock(0)})
	st := s.State()
	if st.Kind != "push" || st.Reachable != nil || st.CheckedAgeS != nil {
		t.Fatal(st)
	}
}

func TestProbeRead(t *testing.T) {
	dir := t.TempDir()
	if err := ProbeRead(dir); !errors.Is(err, ErrNoModelFile) {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "m.gguf"), make([]byte, 10000), 0o644)
	if err := ProbeRead(dir); err != nil {
		t.Fatal(err)
	}
	if err := ProbeRead(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing folder")
	}
	if b := alignedBuf(4096); len(b) != 4096 {
		t.Fatal(len(b))
	}
}

// One log line for the share probe at its first result and at each change of
// result, with the reason when it failed; a repeated result says nothing.
func TestProbeResultIsLoggedOnChangeOnly(t *testing.T) {
	var logs strings.Builder
	pr := &fakeProber{mode: "ok"}
	launch := true
	s := New(Config{Spec: Spec{KindShare, "192.0.2.1:/"}, Dir: "/run/potluck/models", Mount: true,
		NodeIP: func() string { return "192.0.2.101" }, Timeout: 20 * time.Millisecond,
		Clock: osx.NewFakeClock(time.Hour), Mounter: &fakeMounter{mounted: true}, Prober: pr,
		LaunchExists: func() bool { return launch }, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	ctx := context.Background()
	ok := func() int { return strings.Count(logs.String(), `level=INFO msg="share probe ok`) }
	failed := func() int { return strings.Count(logs.String(), `msg="share probe failed`) }
	for i := 0; i < 3; i++ {
		s.ProbeOnce(ctx)
	}
	if ok() != 1 || failed() != 0 {
		t.Fatalf("three good probes: %d ok lines, %d failed\n%s", ok(), failed(), logs.String())
	}
	pr.set("hang")
	for i := 0; i < 4; i++ {
		s.ProbeOnce(ctx)
	}
	if ok() != 1 || failed() != 1 || !strings.Contains(logs.String(), "why=") {
		t.Fatalf("four failed probes: %d ok lines, %d failed\n%s", ok(), failed(), logs.String())
	}
	pr.mu.Lock()
	pr.hung[0].release()
	pr.mu.Unlock()
	pr.set("ok")
	s.ProbeOnce(ctx)
	s.ProbeOnce(ctx)
	if ok() != 2 || failed() != 1 {
		t.Fatalf("back: %d ok lines, %d failed\n%s", ok(), failed(), logs.String())
	}
}
