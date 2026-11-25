// Package filecache drops one model file from the kernel's page cache and
// measures how much of it was cached before and after, so a benchmark load
// can start cold. The agent serves it on its API (agent contract 6.13); the
// scheduler calls the same code for its own machine, which serves the model
// files to the nodes.
//
// Counting reads no data: the file is mapped in chunks and mincore says which
// pages are resident. Dropping is posix_fadvise(POSIX_FADV_DONTNEED) on the
// whole file. Neither needs a privilege. Both exist on Linux only: elsewhere
// Supported is false and an Evictor without a Sys answers "unsupported".
package filecache

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelpath"
)

// DefaultMinGap is the shortest time between two evictions on one machine.
const DefaultMinGap = 10 * time.Second

// ColdShare is the share of the file that may still be cached for the file to
// count as cold: under 1 %.
const ColdShare = 0.01

// Refusal reasons, the words of the agent contract (6.13).
const (
	ReasonInvalid     = "invalid_request"          // 400: a name that is empty, absolute, leaves the model folder or is not a regular file
	ReasonNotFound    = "model_not_found"          // 404
	ReasonBusy        = "busy"                     // 409: a launch is starting, running or stopping
	ReasonTooSoon     = "too_soon"                 // 429: the last eviction was less than MinGap ago, or one is under way
	ReasonUnreachable = "model_source_unreachable" // 503: the model folder cannot be read
	ReasonUnsupported = "unsupported"              // 501: no page cache calls on this system
	ReasonFailed      = "evict_failed"             // 500: a system call failed
)

// Sys is the two system calls, behind an interface so the logic can be tested
// anywhere with a fake.
type Sys interface {
	// Cached counts the bytes of f (size bytes long) that are in the page cache.
	Cached(f *os.File, size int64) (int64, error)
	// Drop asks the kernel to drop f's cached pages.
	Drop(f *os.File, size int64) error
}

// Supported reports whether this system has the calls (Linux).
func Supported() bool { return defaultSys() != nil }

// Default is this system's Sys, nil when it has none.
func Default() Sys { return defaultSys() }

// Result is what one eviction measured.
type Result struct {
	File         string  `json:"file"`
	FileBytes    int64   `json:"file_bytes"`
	CachedBefore int64   `json:"cached_before"`
	CachedAfter  int64   `json:"cached_after"`
	DurationS    float64 `json:"duration_s"`
}

// Cold reports whether a file of fileBytes with cached bytes in the page
// cache counts as cold: under ColdShare of it is cached.
func Cold(fileBytes, cached int64) bool {
	if cached <= 0 {
		return true
	}
	return float64(cached) < ColdShare*float64(fileBytes)
}

// CachedPct is cached as a percentage of fileBytes, 0 for an empty file.
func CachedPct(fileBytes, cached int64) float64 {
	if fileBytes <= 0 {
		return 0
	}
	return 100 * float64(cached) / float64(fileBytes)
}

// Error is a refusal: the HTTP status, the reason word and the words for a person.
type Error struct {
	Status  int
	Reason  string
	Message string
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

func refuse(status int, reason, f string, a ...any) *Error {
	return &Error{Status: status, Reason: reason, Message: fmt.Sprintf(f, a...)}
}

// Resolve turns a model file name relative to root into the real path of a
// regular file inside root. It refuses an empty or absolute name, a name that
// leaves root, a name whose symlinks resolve outside root, and anything that
// is not a regular file.
func Resolve(root, name string) (string, *Error) {
	if strings.TrimSpace(name) == "" {
		return "", refuse(400, ReasonInvalid, "no file named")
	}
	if strings.ContainsRune(name, 0) {
		return "", refuse(400, ReasonInvalid, "the file name has a NUL byte")
	}
	// the shared rule for a model path (internal/modelpath): relative, forward
	// slashes, no ".." and no part that starts with a dot (.trash, .incoming,
	// .potluck-uploads are not model files to evict)
	if err := modelpath.Check(name); err != nil {
		return "", refuse(400, ReasonInvalid, "%v", err)
	}
	clean := filepath.FromSlash(name)
	if root == "" {
		return "", refuse(503, ReasonUnreachable, "no model folder")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", refuse(503, ReasonUnreachable, "the model folder cannot be read: %v", err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(rootReal, clean))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", refuse(404, ReasonNotFound, "%q is not in the model folder", name)
		}
		return "", refuse(400, ReasonInvalid, "%q cannot be resolved: %v", name, err)
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(filepath.ToSlash(rel), "../") || filepath.IsAbs(rel) {
		return "", refuse(400, ReasonInvalid, "%q resolves outside the model folder", name)
	}
	if modelpath.Check(filepath.ToSlash(rel)) != nil {
		return "", refuse(400, ReasonInvalid, "%q resolves into a dot-folder of the model folder", name)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", refuse(404, ReasonNotFound, "%q cannot be read: %v", name, err)
	}
	if !fi.Mode().IsRegular() {
		return "", refuse(400, ReasonInvalid, "%q is not a regular file", name)
	}
	return real, nil
}

// Evictor drops model files from the page cache of one machine, one at a
// time and at most once every MinGap.
type Evictor struct {
	// Root is the model folder; names are relative to it.
	Root string
	// Sys does the system calls; nil answers 501 unsupported.
	Sys Sys
	// Busy says whether the machine runs a launch (and why); a busy machine
	// is refused with 409. Nil: never busy.
	Busy func() (bool, string)
	// MinGap is the shortest time between two evictions (0: DefaultMinGap,
	// negative: none).
	MinGap time.Duration
	// Now is the clock (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger

	mu      sync.Mutex
	running bool
	last    time.Time // when the last eviction that reached the file ended
}

func (e *Evictor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Evictor) gap() time.Duration {
	switch {
	case e.MinGap < 0:
		return 0
	case e.MinGap == 0:
		return DefaultMinGap
	}
	return e.MinGap
}

// begin marks an eviction running; it refuses while one runs or within the
// gap after the last.
func (e *Evictor) begin() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return refuse(429, ReasonTooSoon, "an eviction is under way")
	}
	if g := e.gap(); g > 0 && !e.last.IsZero() {
		if since := e.now().Sub(e.last); since < g {
			return refuse(429, ReasonTooSoon, "the last eviction was %.1f s ago; wait %.1f s more", since.Seconds(), (g - since).Seconds())
		}
	}
	e.running = true
	return nil
}

// Evict drops name (relative to Root) from the page cache and reports the
// cached bytes before and after. A refusal is an *Error.
func (e *Evictor) Evict(name string) (Result, error) {
	if e.Sys == nil {
		return Result{}, refuse(501, ReasonUnsupported, "this system has no page cache calls (mincore, posix_fadvise)")
	}
	path, rerr := Resolve(e.Root, name)
	if rerr != nil {
		return Result{}, rerr
	}
	if e.Busy != nil {
		if busy, why := e.Busy(); busy {
			if why == "" {
				why = "a launch is running"
			}
			return Result{}, refuse(409, ReasonBusy, "%s; the file cache is left alone while a model uses it", why)
		}
	}
	if err := e.begin(); err != nil {
		return Result{}, err
	}
	reached := false
	defer func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.running = false
		if reached {
			e.last = e.now()
		}
	}()

	start := e.now()
	f, err := os.Open(path)
	if err != nil {
		return Result{}, refuse(404, ReasonNotFound, "%q cannot be opened: %v", name, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return Result{}, refuse(400, ReasonInvalid, "%q is not a regular file", name)
	}
	if again, err := os.Stat(path); err != nil || !os.SameFile(fi, again) {
		return Result{}, refuse(400, ReasonInvalid, "%q changed while it was opened", name)
	}
	reached = true
	res := Result{File: filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))), FileBytes: fi.Size()}
	if res.CachedBefore, err = e.Sys.Cached(f, res.FileBytes); err != nil {
		return Result{}, refuse(500, ReasonFailed, "counting the cached pages of %q: %v", name, err)
	}
	if err := e.Sys.Drop(f, res.FileBytes); err != nil {
		return Result{}, refuse(500, ReasonFailed, "dropping %q from the file cache: %v", name, err)
	}
	if res.CachedAfter, err = e.Sys.Cached(f, res.FileBytes); err != nil {
		return Result{}, refuse(500, ReasonFailed, "counting the cached pages of %q again: %v", name, err)
	}
	res.DurationS = float64(e.now().Sub(start).Round(time.Millisecond)) / float64(time.Second)
	if e.Logger != nil {
		e.Logger.Info("model file dropped from the file cache", "file", res.File, "file_bytes", res.FileBytes,
			"cached_before", res.CachedBefore, "cached_after", res.CachedAfter,
			"cached_after_pct", fmt.Sprintf("%.2f", CachedPct(res.FileBytes, res.CachedAfter)), "duration_s", res.DurationS)
	}
	return res, nil
}
