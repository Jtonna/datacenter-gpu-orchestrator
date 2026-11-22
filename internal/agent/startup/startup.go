// Package startup is the agent's start-up report (agent contract, "Start-up
// reporting"): the marks of this agent process on the machine's own time
// axis (seconds since the kernel booted, monotonic), systemd's userspace
// start and soft-restart count, and the marks another component writes into
// the start-up marks file. Potluck turns them into one figure per start.
//
// Nothing here reads a wall clock (contract rule 2.1): every figure comes
// from the agent's monotonic clock or from the kernel's and systemd's
// monotonic figures.
package startup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// secs turns a duration on the agent's clock into seconds with millisecond
// decimals.
func secs(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Millisecond)) / 1000 }

// ---- systemd ----

// Facts are what systemd says about userspace. A field is nil when systemd
// did not give it: never guessed.
type Facts struct {
	// UserspaceStartS is UserspaceTimestampMonotonic in seconds: when
	// userspace last started, at boot or at the last soft restart.
	UserspaceStartS *float64
	// SoftReboots is SoftRebootsCount (systemd 254 or later).
	SoftReboots *int64
}

// Systemd asks systemd, once, about userspace.
type Systemd interface {
	Show(ctx context.Context) (Facts, error)
}

// ShowArgs are the arguments of the one systemctl call.
var ShowArgs = []string{"show", "--property=UserspaceTimestampMonotonic", "--property=SoftRebootsCount"}

// ExecSystemd runs `systemctl show` for the manager's two properties.
type ExecSystemd struct {
	// Run runs a command and returns its standard output; nil runs it.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Show implements Systemd.
func (e ExecSystemd) Show(ctx context.Context) (Facts, error) {
	run := e.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	out, err := run(ctx, "systemctl", ShowArgs...)
	if err != nil {
		return Facts{}, err
	}
	f := ParseShow(out)
	if f.UserspaceStartS == nil && f.SoftReboots == nil {
		return f, errors.New("systemctl show gave neither UserspaceTimestampMonotonic nor SoftRebootsCount")
	}
	return f, nil
}

// ParseShow reads `systemctl show` output (KEY=VALUE lines). A property
// that is missing, empty or not a number is left nil; a userspace start of 0
// means systemd does not know it and is left nil too.
func ParseShow(out []byte) Facts {
	var f Facts
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "UserspaceTimestampMonotonic":
			if us, err := strconv.ParseUint(v, 10, 64); err == nil && us > 0 {
				s := math.Round(float64(us)/1000) / 1000 // microseconds to seconds, millisecond decimals
				f.UserspaceStartS = &s
			}
		case "SoftRebootsCount":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				f.SoftReboots = &n
			}
		}
	}
	return f
}

// ---- the marks file ----

// ParseMarks reads the marks file: one "<name> <seconds>" per line. A line
// that is not exactly a valid name and a finite, non-negative number is
// skipped (a half-written last line included). A name given twice keeps its
// last value. At most agentproto.StartupMaxExternalMarks names are taken, in
// the order they first appear. An origin line (isOrigin) counts only when it
// ends with a newline: its value is exempt from the cut-off, and a cut value
// ("49" of "490.25") would still parse.
func ParseMarks(b []byte) map[string]float64 {
	out := map[string]float64{}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) != 2 || !agentproto.ValidStartupMarkName(f[0]) {
			continue
		}
		if i == len(lines)-1 && isOrigin(f[0]) {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		if _, seen := out[f[0]]; !seen && len(out) >= agentproto.StartupMaxExternalMarks {
			continue
		}
		out[f[0]] = v
	}
	return out
}

// errTooLarge: the marks file is larger than the cap and is ignored.
var errTooLarge = errors.New("the start-up marks file is larger than 64 KiB; it is ignored")

// readCapped reads a file of at most max bytes; a missing file is (nil, nil).
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errTooLarge
	}
	return b, nil
}

// Watcher re-reads the marks file for a while after the agent starts: marks
// keep arriving (the dashboard after the agent, for example). It stops after
// WatchFor, or when the file has not changed for QuietFor. It only reads:
// it never creates or deletes the file or its folder.
//
// /run survives a soft restart, so the file can still hold the marks of the
// previous userspace when its writer has not run again. With a cut-off (the
// current userspace start, Recorder.MarksCutoff) a mark earlier than it is
// dropped, and a file whose boot_progress_start is earlier is ignored whole
// (the writer starts a fresh file with that mark at each of its starts), until
// a fresh file appears.
type Watcher struct {
	Path string
	// Read reads the file (nil: a capped read of Path); tests replace it.
	Read func(path string) ([]byte, error)
	// UserspaceStart gives the cut-off in seconds since boot; ok false (or a
	// nil func) keeps every mark.
	UserspaceStart func() (float64, bool)
	// Logger gets one INFO line when a stale file is ignored; nil: none.
	Logger *slog.Logger

	cut       float64
	cutKnown  bool
	staleSaid bool

	started    bool
	start      time.Duration
	lastChange time.Duration
	last       []byte
	missing    bool
	done       bool
	tooLarge   bool
}

// The bounds of the re-reading.
const (
	WatchFor = 5 * time.Minute
	QuietFor = 60 * time.Second
)

// Poll reads the file once at agent clock now. It returns the marks when the
// file changed since the last poll (a file that went missing gives an empty
// map), changed false otherwise, and done once the watcher has stopped; a
// stopped watcher reads nothing. warn is a line worth logging once.
func (w *Watcher) Poll(now time.Duration) (marks map[string]float64, changed, done bool, warn string) {
	if w.done {
		return nil, false, true, ""
	}
	if !w.started {
		w.started, w.start, w.lastChange, w.missing = true, now, now, true
	}
	read := w.Read
	if read == nil {
		read = func(p string) ([]byte, error) { return readCapped(p, agentproto.StartupMarksFileMaxBytes) }
	}
	b, err := read(w.Path)
	switch {
	case errors.Is(err, errTooLarge) || (err == nil && len(b) > agentproto.StartupMarksFileMaxBytes):
		if !w.tooLarge {
			w.tooLarge = true
			warn = errTooLarge.Error()
		}
	case err != nil:
		// Unreadable now; tried again at the next poll.
	case b == nil:
		if !w.missing {
			w.missing, w.last, changed = true, nil, true
			marks = map[string]float64{}
			w.lastChange = now
		}
	default:
		w.tooLarge = false
		cut, known := 0.0, false
		if w.UserspaceStart != nil {
			cut, known = w.UserspaceStart()
		}
		cutChanged := known != w.cutKnown || cut != w.cut
		if w.missing || !bytes.Equal(b, w.last) || cutChanged {
			w.cut, w.cutKnown = cut, known
			w.missing, w.last, changed = false, append([]byte(nil), b...), true
			marks = w.current(ParseMarks(b))
			w.lastChange = now
		}
	}
	if now-w.start >= WatchFor || now-w.lastChange >= QuietFor {
		w.done = true
	}
	return marks, changed, w.done, warn
}

// markBootProgressStart is the first mark the console side writes at each of
// its starts, in a fresh file.
const markBootProgressStart = "boot_progress_start"

// isOrigin: boot_origin or boot_origin_partial, the file's start origin.
func isOrigin(name string) bool {
	return name == agentproto.StartupMarkBootOrigin || name == agentproto.StartupMarkBootOriginPartial
}

// current drops the marks of an earlier userspace (see Watcher).
//
// The cut-off (cut) is the current userspace start; it is known only after a
// soft restart (or with an unknown count), when /run may still hold the
// previous start's file. Which file is this start's:
//
//   - The origin lines are exempt from the cut-off: after a soft restart
//     boot_origin is the moment the restart began, always before the
//     current userspace start, and boot_origin_partial is the userspace
//     start itself (the writer's figure, possibly a little off ours). So an
//     origin's value cannot tell a left-over file from a fresh one: the
//     previous start's boot_origin (0, or an earlier restart's moment) is
//     also before the cut-off, and the agent does not know when this
//     start's shutdown began.
//   - The other marks can: the writer starts a fresh file at each of its
//     starts, so a mark at or after the cut-off was written in this
//     userspace. The file is this start's when its boot_progress_start is at
//     or after the cut-off; without boot_progress_start, when at least one
//     mark other than an origin is at or after the cut-off. Otherwise the
//     whole file, origin included, is ignored (one INFO line when it has
//     marks other than an origin; a file with only an origin so far waits,
//     silently, for the writer's next mark).
//   - In a file of this start, marks other than an origin earlier than the
//     cut-off are dropped as before, and a boot_origin after the cut-off is
//     dropped (a restart cannot begin after the userspace it started).
func (w *Watcher) current(m map[string]float64) map[string]float64 {
	if !w.cutKnown {
		return m
	}
	stale, other, ref, refName := true, false, 0.0, "boot_progress_start_s"
	if b, ok := m[markBootProgressStart]; ok {
		stale, other, ref = b < w.cut, true, b
	} else {
		refName = "latest_mark_s"
		for k, v := range m {
			if isOrigin(k) {
				continue
			}
			if !other || v > ref {
				ref = v
			}
			other = true
			if v >= w.cut {
				stale = false
			}
		}
	}
	if stale {
		if other && !w.staleSaid && w.Logger != nil {
			w.Logger.Info("start-up marks file is from an earlier start; ignored", "file", w.Path,
				refName, ref, "userspace_start_s", w.cut)
		}
		if other {
			w.staleSaid = true
		}
		return map[string]float64{}
	}
	for k, v := range m {
		switch {
		case k == agentproto.StartupMarkBootOrigin:
			if v > w.cut {
				delete(m, k)
			}
		case k == agentproto.StartupMarkBootOriginPartial:
		case v < w.cut:
			delete(m, k)
		}
	}
	return m
}

// ---- the report ----

// Recorder holds this agent process's start-up report. Safe for concurrent
// use. Every change of the report gives it a new revision; the report is
// sent in heartbeats until Potluck has acknowledged a revision that is the
// current one.
type Recorder struct {
	mu        sync.Mutex
	rep       agentproto.Startup
	rev       int  // revision of rep: bumped by every change
	sentRev   int  // revision of the report last sent
	sentCount int  // its number of marks
	ackedRev  int  // the last revision Potluck acknowledged
	noAck     bool // Potluck answered without startup_ack: it takes no reports
}

// NewRecorder starts a report: the boot id ("" when unknown) and the
// agent's own start, which is also the mark agent_start.
func NewRecorder(bootID string, agentStart time.Duration) *Recorder {
	r := &Recorder{rev: 1, rep: agentproto.Startup{BootID: bootID, AgentStartS: secs(agentStart),
		Marks: map[string]float64{}, ExternalMarks: map[string]float64{}}}
	r.rep.Marks[agentproto.StartupMarkAgentStart] = r.rep.AgentStartS
	return r
}

// SetSystemd keeps what systemd said.
func (r *Recorder) SetSystemd(f Facts) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.UserspaceStartS != nil {
		v := *f.UserspaceStartS
		r.rep.UserspaceStartS = &v
	}
	if f.SoftReboots != nil {
		v := *f.SoftReboots
		r.rep.SoftReboots = &v
	}
	r.rev++
}

// MarksCutoff is the time (seconds since boot) before which a mark of the
// marks file belongs to an earlier userspace: the userspace start, when
// systemd gave it and this boot may have had a soft restart (the count is
// above 0 or unknown). On a boot without a soft restart /run is new, so
// nothing is cut.
func (r *Recorder) MarksCutoff() (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rep.UserspaceStartS == nil || (r.rep.SoftReboots != nil && *r.rep.SoftReboots == 0) {
		return 0, false
	}
	return *r.rep.UserspaceStartS, true
}

// Mark records a mark the first time it happens; later calls are ignored.
// It reports whether this call recorded it.
func (r *Recorder) Mark(name string, at time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rep.Marks[name]; ok {
		return false
	}
	r.rep.Marks[name] = secs(at)
	r.rev++
	return true
}

// Has reports whether a mark of the agent's own has happened.
func (r *Recorder) Has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.rep.Marks[name]
	return ok
}

// SetExternal replaces the marks of the marks file.
func (r *Recorder) SetExternal(m map[string]float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	same := len(m) == len(r.rep.ExternalMarks)
	for k, v := range m {
		if old, ok := r.rep.ExternalMarks[k]; !ok || old != v {
			same = false
		}
	}
	if same {
		return
	}
	r.rep.ExternalMarks = map[string]float64{}
	for k, v := range m {
		r.rep.ExternalMarks[k] = v
	}
	r.rev++
}

// Report returns a copy of the report as it is now, for a registration, and
// notes it as sent.
func (r *Recorder) Report() *agentproto.Startup {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendLocked()
}

// ForHeartbeat returns the report when it holds something Potluck has not
// acknowledged, and notes it as sent; nil when everything is acknowledged
// or Potluck takes no reports.
func (r *Recorder) ForHeartbeat() *agentproto.Startup {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.noAck || r.ackedRev == r.rev {
		return nil
	}
	return r.sendLocked()
}

// Pending reports whether a heartbeat would carry the report.
func (r *Recorder) Pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.noAck && r.ackedRev != r.rev
}

func (r *Recorder) sendLocked() *agentproto.Startup {
	c := r.rep
	c.Marks, c.ExternalMarks = map[string]float64{}, map[string]float64{}
	for k, v := range r.rep.Marks {
		c.Marks[k] = v
	}
	for k, v := range r.rep.ExternalMarks {
		c.ExternalMarks[k] = v
	}
	if r.rep.UserspaceStartS != nil {
		v := *r.rep.UserspaceStartS
		c.UserspaceStartS = &v
	}
	if r.rep.SoftReboots != nil {
		v := *r.rep.SoftReboots
		c.SoftReboots = &v
	}
	r.sentRev, r.sentCount = r.rev, c.Count()
	return &c
}

// Answered takes Potluck's startup_ack for the report last sent: the number
// of its marks Potluck has taken. 0 means Potluck takes no start-up reports
// (an older Potluck): none in heartbeats until the next registration, whose
// answer (fromRegistration) decides again.
func (r *Recorder) Answered(ack int, fromRegistration bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fromRegistration {
		r.noAck = false
	}
	if ack <= 0 {
		r.noAck = true
		return
	}
	if ack == r.sentCount && r.sentRev > r.ackedRev {
		r.ackedRev = r.sentRev
	}
}
