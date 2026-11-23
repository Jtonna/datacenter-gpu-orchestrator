package diskstat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const gb = 1_000_000_000

type fakeDisk struct {
	mu    sync.Mutex
	fs    map[string]FS
	errs  map[string]error
	hang  map[string]chan struct{}
	calls map[string]int
}

func newFake() *fakeDisk {
	return &fakeDisk{fs: map[string]FS{}, errs: map[string]error{}, hang: map[string]chan struct{}{}, calls: map[string]int{}}
}

func (f *fakeDisk) stat(p string) (FS, error) {
	f.mu.Lock()
	f.calls[p]++
	h, fs, err := f.hang[p], f.fs[p], f.errs[p]
	f.mu.Unlock()
	if h != nil {
		<-h
		f.mu.Lock()
		fs, err = f.fs[p], f.errs[p]
		f.mu.Unlock()
	}
	return fs, err
}

func (f *fakeDisk) count(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[p]
}

func files(string) (int64, int) { return 3 * gb, 2 }

func folders() []Folder {
	return []Folder{{"data", "/srv/a/data"}, {"history", "/srv/a/history"}, {"logs", ""}, {"models", "/mnt/share"}}
}

func TestDiskstat_ReadsGroupsAndOrdersFullestFirst(t *testing.T) {
	f := newFake()
	f.fs["/srv/a/data"] = FS{Total: 240 * gb, Free: 189 * gb, Used: 51 * gb, Dev: 1}
	f.fs["/srv/a/history"] = FS{Total: 240 * gb, Free: 189 * gb, Used: 51 * gb, Dev: 1}
	f.fs["/mnt/share"] = FS{Total: 1000 * gb, Free: 50 * gb, Used: 950 * gb, Dev: 9}
	r := New(Options{Folders: folders(), Stat: f.stat, Files: files, Timeout: time.Second})
	if got := r.Folders(); len(got) != 3 || got[0].State != StateWaiting || got[0].Known() {
		t.Fatalf("before the first pass: %+v", got)
	}
	r.Pass(context.Background())
	got := r.Folders()
	if len(got) != 3 {
		t.Fatalf("a folder without a path is not read: %+v", got)
	}
	d := got[0]
	if d.State != StateOK || d.TotalBytes != 240*gb || d.FreeBytes != 189*gb || d.UsedBytes != 51*gb || d.UsedPercent != 21.3 || d.PotluckBytes != 3*gb || d.FileCount != 2 || d.CheckedAt == nil {
		t.Fatalf("data: %+v", d)
	}
	if len(got[1].SameDiskAs) != 1 || got[1].SameDiskAs[0] != "data" || len(got[2].SameDiskAs) != 0 {
		t.Fatalf("same disk: %+v / %+v", got[1].SameDiskAs, got[2].SameDiskAs)
	}
	disks := r.Disks()
	if len(disks) != 2 || disks[0].Name != "models" || disks[1].Name != "data" || len(disks[1].Folders) != 2 {
		t.Fatalf("disks: %+v", disks)
	}
	if r.Passes() != 1 {
		t.Fatalf("passes %d", r.Passes())
	}
}

// A hung statfs (a network share that does not answer) never blocks a pass
// past the timeout, keeps the last figures, is not read again while its read
// is out, and updates the folder when it finally comes back.
func TestDiskstat_HungReadTimesOutKeepsFiguresAndIsNotPiledUp(t *testing.T) {
	f := newFake()
	f.fs["/mnt/share"] = FS{Total: 100 * gb, Free: 40 * gb, Used: 60 * gb, Dev: 2}
	r := New(Options{Folders: []Folder{{"models", "/mnt/share"}}, Stat: f.stat, Files: files, Timeout: 50 * time.Millisecond})
	r.Pass(context.Background())
	release := make(chan struct{})
	f.mu.Lock()
	f.hang["/mnt/share"] = release
	f.fs["/mnt/share"] = FS{Total: 100 * gb, Free: 30 * gb, Used: 70 * gb, Dev: 2}
	f.mu.Unlock()
	t0 := time.Now()
	r.Pass(context.Background())
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("a hung read held the pass %v", d)
	}
	e, _ := r.Folder("models")
	if e.State != StateTimeout || !e.Known() || e.UsedBytes != 60*gb || e.LateSince == nil || e.Error == "" {
		t.Fatalf("late folder: %+v", e)
	}
	for i := 0; i < 3; i++ {
		r.Pass(context.Background())
	}
	if n := f.count("/mnt/share"); n != 2 {
		t.Fatalf("reads started while one was out: %d calls, want 2", n)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if e, _ = r.Folder("models"); e.State == StateOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the late answer was not stored: %+v", e)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e.UsedBytes != 70*gb || e.LateSince != nil {
		t.Fatalf("after the late answer: %+v", e)
	}
}

func TestDiskstat_ErrorsAndUnsupported(t *testing.T) {
	f := newFake()
	f.errs["/srv/a/data"] = errors.New("no such file or directory")
	f.errs["/srv/a/history"] = ErrUnsupported
	r := New(Options{Folders: folders()[:2], Stat: f.stat, Files: files})
	r.Pass(context.Background())
	d, _ := r.Folder("data")
	h, _ := r.Folder("history")
	if d.State != StateError || d.Error == "" || d.Known() || h.State != StateUnknown || h.Known() {
		t.Fatalf("data %+v history %+v", d, h)
	}
	if len(r.Disks()) != 0 {
		t.Fatalf("disks without figures: %+v", r.Disks())
	}
}

func TestDiskstat_PanickingReadIsAnError(t *testing.T) {
	r := New(Options{Folders: []Folder{{"data", "/x"}}, Stat: func(string) (FS, error) { panic("boom") }, Files: files})
	r.Pass(context.Background())
	if e, _ := r.Folder("data"); e.State != StateError || e.Known() {
		t.Fatalf("%+v", e)
	}
}

func TestDiskstat_StartPassesAndCallsOnPass(t *testing.T) {
	f := newFake()
	f.fs["/srv/a/data"] = FS{Total: 10 * gb, Free: 5 * gb, Used: 5 * gb}
	r := New(Options{Folders: folders()[:1], Stat: f.stat, Files: files, Every: 10 * time.Millisecond})
	var n atomic.Int64
	r.OnPass(func() { n.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	r.Start(ctx, func() { close(stopped) })
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("passes %d", n.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-stopped
	var nilReader *Reader
	nilReader.Pass(ctx)
	if len(nilReader.Folders()) != 0 || len(nilReader.Disks()) != 0 {
		t.Fatal("a nil reader has folders")
	}
	done := false
	New(Options{}).Start(ctx, func() { done = true })
	if !done {
		t.Fatal("a reader with no folders did not report done")
	}
}

func TestDiskstat_DirFilesFollowsLinksAndSkipsFolders(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.gguf"), make([]byte, 100), 0o644)
	os.WriteFile(filepath.Join(dir, "b.gguf"), make([]byte, 50), 0o644)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	if b, n := DirFiles(dir); b != 150 || n != 2 {
		t.Fatalf("%d bytes, %d files", b, n)
	}
	if b, n := DirFiles(filepath.Join(dir, "missing")); b != 0 || n != 0 {
		t.Fatal("a missing folder has files")
	}
}
