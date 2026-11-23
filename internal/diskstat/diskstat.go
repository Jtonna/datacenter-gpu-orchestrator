// Package diskstat is the scheduler's one reader of disk capacity and free
// space: statfs on the folders Potluck writes to or reads models from, done
// off every request path by a supervised task, cached, and read by the
// routes (GET /api/system, GET /api/health, the model catalog).
//
// A folder may sit on a network share whose statfs hangs. Each read runs on
// its own goroutine with a timeout: a read that has not come back is marked
// as such and the folder keeps its last figures; no second read of a path
// starts while one is still out, so a hung mount costs one goroutine, not
// one per tick.
package diskstat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// ErrUnsupported is what Stat returns on a build without statfs (the
// scheduler runs on Linux; other builds report the figures as unknown).
var ErrUnsupported = errors.New("disk figures are not read on this platform")

// FS is one statfs answer: bytes of the filesystem, bytes free for an
// unprivileged writer, bytes used, and the device it is on (folders with the
// same device share one filesystem).
type FS struct {
	Total, Free, Used uint64
	Dev               uint64
}

// Folder is one folder to watch. Name is its role: data, history, logs or
// models.
type Folder struct {
	Name, Path string
}

// The folder states.
const (
	StateOK      = "ok"      // read on the last pass
	StateWaiting = "waiting" // not read yet (the first pass is out)
	StateTimeout = "timeout" // the last read did not answer within the timeout
	StateError   = "error"   // the last read failed (the folder is missing, say)
	StateUnknown = "unknown" // this platform reads no disk figures
)

// Entry is one folder as the routes show it. The byte figures are those of
// the last read that answered (zero when none has); State says how current
// they are.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
	// The filesystem holding the folder.
	TotalBytes  int64   `json:"total_bytes"`
	FreeBytes   int64   `json:"free_bytes"`
	UsedBytes   int64   `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
	// PotluckBytes is the size of the files directly in the folder (what
	// Potluck keeps there; for models, the model files), FileCount their
	// number.
	PotluckBytes int64 `json:"potluck_bytes"`
	FileCount    int   `json:"files"`
	// SameDiskAs names the folders read before this one that are on the
	// same filesystem (empty when it is a disk of its own).
	SameDiskAs []string `json:"same_disk_as"`
	Error      string   `json:"error,omitempty"`
	// CheckedAt is when the figures were read; nil before the first answer.
	CheckedAt *time.Time `json:"checked_at"`
	// LateSince is when the read that has not come back started.
	LateSince *time.Time `json:"late_since,omitempty"`

	dev  uint64
	read bool // figures from a read that answered
}

// Known reports whether the entry has figures (from this pass or an earlier
// one).
func (e Entry) Known() bool { return e.read }

// Options configures a Reader. Zero values take the defaults.
type Options struct {
	Folders []Folder
	Every   time.Duration // between passes; default 30 s
	Timeout time.Duration // a read that takes longer is late; default 5 s
	// Stat reads one path's filesystem; default Stat (statfs). Files lists
	// the bytes and count of the files directly in a folder; default
	// DirFiles. Both run on the read's own goroutine.
	Stat  func(path string) (FS, error)
	Files func(path string) (int64, int)
	Now   func() time.Time
}

// Reader keeps the folders' last figures. The zero Reader and a nil one
// answer with nothing.
type Reader struct {
	opt Options

	mu      sync.Mutex
	entries map[string]*Entry // by folder name
	out     map[string]bool   // paths with a read still out
	passes  int
	onPass  []func()
}

// New makes a reader of the folders with a path; Start begins the reads.
func New(o Options) *Reader {
	if o.Every <= 0 {
		o.Every = 30 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.Stat == nil {
		o.Stat = Stat
	}
	if o.Files == nil {
		o.Files = DirFiles
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	r := &Reader{entries: map[string]*Entry{}, out: map[string]bool{}}
	for _, f := range o.Folders {
		if f.Path == "" || f.Name == "" {
			continue
		}
		r.opt.Folders = append(r.opt.Folders, f)
		r.entries[f.Name] = &Entry{Name: f.Name, Path: f.Path, State: StateWaiting}
	}
	r.opt.Every, r.opt.Timeout, r.opt.Stat, r.opt.Files, r.opt.Now = o.Every, o.Timeout, o.Stat, o.Files, o.Now
	return r
}

// OnPass registers fn to run after each pass (the health summary's push).
func (r *Reader) OnPass(fn func()) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onPass = append(r.onPass, fn)
}

// Start runs a pass now and then every Every until ctx ends, as a
// supervised task; done (when set) runs once it has stopped.
func (r *Reader) Start(ctx context.Context, done func()) {
	if r == nil || len(r.opt.Folders) == 0 {
		if done != nil {
			done()
		}
		return
	}
	supervise.Go(ctx, supervise.Spec{Name: "diskstat.reader", Policy: supervise.Restart, Done: done}, func(ctx context.Context) {
		t := time.NewTicker(r.opt.Every)
		defer t.Stop()
		for {
			r.Pass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

type answer struct {
	fs    FS
	err   error
	bytes int64
	files int
	at    time.Time
}

// Pass reads every folder once, waiting at most Timeout (or until ctx ends)
// for the answers. A folder whose read is still out from an earlier pass is
// not read again; it stays late until that read returns.
func (r *Reader) Pass(ctx context.Context) {
	if r == nil {
		return
	}
	type job struct {
		f  Folder
		ch chan answer
	}
	var jobs []job
	start := r.opt.Now()
	for _, f := range r.opt.Folders {
		if !r.begin(f.Path) {
			r.markLate(f, start)
			continue
		}
		ch := make(chan answer, 1)
		jobs = append(jobs, job{f, ch})
		go supervise.Once("diskstat.read", func() {
			a := answer{}
			a.fs, a.err = r.opt.Stat(f.Path)
			if a.err == nil {
				a.bytes, a.files = r.opt.Files(f.Path)
			}
			a.at = r.opt.Now()
			r.finish(f, a)
			ch <- a
		}, func(p any) {
			a := answer{err: errors.New("the disk read failed"), at: r.opt.Now()}
			r.finish(f, a)
			ch <- a
		})
	}
	timer := time.NewTimer(r.opt.Timeout)
	defer timer.Stop()
	for _, j := range jobs {
		select {
		case <-j.ch: // finish has stored it
		case <-timer.C:
			r.markLate(j.f, start)
			timer.Reset(0)
		case <-ctx.Done():
			r.markLate(j.f, start)
		}
	}
	r.endPass()
}

func (r *Reader) begin(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out[path] {
		return false
	}
	r.out[path] = true
	return true
}

// finish stores an answer, whenever it comes (a late read that comes back
// after its pass still updates the folder).
func (r *Reader) finish(f Folder, a answer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.out, f.Path)
	e := r.entries[f.Name]
	if e == nil {
		return
	}
	e.LateSince = nil
	if errors.Is(a.err, ErrUnsupported) {
		e.State, e.Error = StateUnknown, ""
		return
	}
	if a.err != nil {
		e.State, e.Error = StateError, a.err.Error()
		return
	}
	at := a.at
	e.State, e.Error, e.CheckedAt, e.read = StateOK, "", &at, true
	e.TotalBytes, e.FreeBytes, e.UsedBytes, e.dev = clamp(a.fs.Total), clamp(a.fs.Free), clamp(a.fs.Used), a.fs.Dev
	e.UsedPercent = 0
	if a.fs.Total > 0 {
		e.UsedPercent = float64(int64(float64(a.fs.Used)/float64(a.fs.Total)*1000+0.5)) / 10
	}
	e.PotluckBytes, e.FileCount = a.bytes, a.files
}

func (r *Reader) markLate(f Folder, since time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[f.Name]
	if e == nil || !r.out[f.Path] {
		return // it answered meanwhile
	}
	e.State, e.Error = StateTimeout, "the disk did not answer within "+r.opt.Timeout.String()
	if e.LateSince == nil {
		s := since
		e.LateSince = &s
	}
}

func (r *Reader) endPass() {
	fns := func() []func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.passes++
		return append([]func(){}, r.onPass...)
	}()
	for _, fn := range fns {
		supervise.Guard("diskstat.onpass", fn, nil)
	}
}

// Passes is how many passes have ended (tests).
func (r *Reader) Passes() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.passes
}

// Folders returns every folder's entry, in the order they were given, with
// SameDiskAs filled from the devices of the folders read so far.
func (r *Reader) Folders() []Entry {
	if r == nil {
		return []Entry{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.opt.Folders))
	for _, f := range r.opt.Folders {
		e := *r.entries[f.Name]
		e.SameDiskAs = []string{}
		for _, p := range out {
			if e.read && p.read && p.dev == e.dev && e.TotalBytes == p.TotalBytes {
				e.SameDiskAs = append(e.SameDiskAs, p.Name)
			}
		}
		out = append(out, e)
	}
	return out
}

// Folder is one folder's entry by name.
func (r *Reader) Folder(name string) (Entry, bool) {
	for _, e := range r.Folders() {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Disks is one entry per filesystem among the folders with figures (the
// first folder on it names it; Names lists them all), fullest first.
func (r *Reader) Disks() []Disk {
	var out []Disk
	for _, e := range r.Folders() {
		if !e.read {
			continue
		}
		if len(e.SameDiskAs) > 0 {
			for i := range out {
				if out[i].Folders[0] == e.SameDiskAs[0] {
					out[i].Folders = append(out[i].Folders, e.Name)
				}
			}
			continue
		}
		out = append(out, Disk{Entry: e, Folders: []string{e.Name}})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UsedPercent > out[j].UsedPercent })
	return out
}

// Disk is one filesystem with the folders on it.
type Disk struct {
	Entry
	Folders []string
}

func clamp(v uint64) int64 {
	if v > 1<<62 {
		return 1 << 62
	}
	return int64(v)
}

// DirFiles is the bytes and number of the files directly in dir, following
// links (a models folder of links to a share counts the files they name).
func DirFiles(dir string) (int64, int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	var total int64
	n := 0
	for _, e := range ents {
		info, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		n++
	}
	return total, n
}
