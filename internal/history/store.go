package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Defaults of the history settings.
const (
	DefaultMaxFileMiB   = 16
	DefaultKeepFiles    = 20
	DefaultMemoryEvents = 5000
	defaultQueue        = 4096
	fileNameLayout      = "20060102T150405Z"
	// nameSlack is how far an event time may lie before the creation time in
	// the name of the file it is in (the name is taken from the wall clock when
	// the file is opened, the event time when it happened).
	nameSlack = time.Minute
)

// Options configure a Store. Dir empty means history is off.
type Options struct {
	Dir          string
	MaxFileBytes int64 // rotate when a file would grow past this (default 16 MiB)
	KeepFiles    int   // files kept (default 20); the oldest are deleted
	MemoryEvents int   // events kept in memory for queries (default 5000)
	QueueSize    int   // buffered events waiting for the writer (default 4096)
	Logger       *slog.Logger
	// Now names new files (default time.Now); tests set it.
	Now func() time.Time
}

// fileName matches the files this package creates, so retention never
// touches anything else in the directory.
var fileName = regexp.MustCompile(`^events-(\d{8}T\d{6}Z)\.jsonl$`)

// Store is the event history. All methods are safe for concurrent use.
type Store struct {
	opt Options
	log *slog.Logger
	on  bool // a usable directory was given

	mu        sync.Mutex
	ring      []Event
	seq       uint64
	coversAll bool // the ring holds every event that is on disk
	closed    bool
	ch        chan item
	done      chan struct{}

	dropped atomic.Uint64

	subs   map[uint64]func(Event) // live listeners (Subscribe), under mu
	subSeq uint64

	// Owned by the writer goroutine (and by Open before it starts).
	f      *os.File
	size   int64
	failed bool
}

// Open prepares the store: it creates the directory, opens (or reuses) the
// newest file and loads the newest events into memory. It never fails: an
// unusable directory is warned about once and history stays off.
func Open(o Options) *Store {
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileMiB << 20
	}
	if o.KeepFiles <= 0 {
		o.KeepFiles = DefaultKeepFiles
	}
	if o.MemoryEvents <= 0 {
		o.MemoryEvents = DefaultMemoryEvents
	}
	if o.QueueSize <= 0 {
		o.QueueSize = defaultQueue
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Store{opt: o, log: o.Logger, done: make(chan struct{})}
	if o.Dir == "" {
		close(s.done)
		s.closed = true
		return s
	}
	if err := s.prepare(); err != nil {
		s.log.Warn("history_dir is not usable; the event history is off", "dir", o.Dir, "err", err)
		close(s.done)
		s.closed = true
		return s
	}
	s.on = true
	s.loadRecent()
	s.ch = make(chan item, o.QueueSize)
	// A failure loses the event being written; the writer continues in a
	// fresh file (a torn line stays in the old one), with the queue kept.
	supervise.Go(context.Background(), supervise.Spec{Name: "history.writer", Policy: supervise.Restart,
		Rebuild: func(any) { s.reopen() }, Done: s.writerDone}, func(context.Context) { s.writer() })
	s.log.Info("event history on", "dir", o.Dir, "max_file_mib", o.MaxFileBytes>>20, "keep_files", o.KeepFiles,
		"in_memory", len(s.ring))
	return s
}

// On says whether history is recording.
func (s *Store) On() bool { return s.on }

// Dropped is how many events the writer queue could not take.
func (s *Store) Dropped() uint64 { return s.dropped.Load() }

type fileRef struct {
	path  string
	start int64 // creation time from the name, unix ms
}

func (s *Store) files() []fileRef {
	ents, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return nil
	}
	var out []fileRef
	for _, e := range ents {
		m := fileName.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			continue
		}
		t, err := time.Parse(fileNameLayout, m[1])
		if err != nil {
			continue
		}
		out = append(out, fileRef{path: filepath.Join(s.opt.Dir, e.Name()), start: t.UnixMilli()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path }) // the name is the time
	return out
}

// prepare creates the directory, opens the file to append to and applies
// retention. Called once, before the writer starts.
func (s *Store) prepare() error {
	if err := os.MkdirAll(s.opt.Dir, 0o755); err != nil {
		return err
	}
	if fs := s.files(); len(fs) > 0 {
		last := fs[len(fs)-1]
		if st, err := os.Stat(last.path); err == nil && st.Size() < s.opt.MaxFileBytes {
			f, err := os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				s.f, s.size = f, st.Size()
				if s.size > 0 && !endsWithNewline(last.path, s.size) {
					if n, err := f.Write([]byte{'\n'}); err == nil {
						s.size += int64(n)
					}
				}
				s.prune()
				return nil
			}
		}
	}
	return s.openNew()
}

func endsWithNewline(path string, size int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, size-1); err != nil {
		return true
	}
	return b[0] == '\n'
}

// openNew starts a new file named for the current time (moved forward by
// seconds until the name is free and later than every existing name, so names
// stay strictly increasing) and prunes.
func (s *Store) openNew() error {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	at := s.opt.Now().UTC().Truncate(time.Second)
	if fs := s.files(); len(fs) > 0 {
		if m := fileName.FindStringSubmatch(filepath.Base(fs[len(fs)-1].path)); m != nil {
			if t, err := time.Parse(fileNameLayout, m[1]); err == nil && !at.After(t) {
				at = t.Add(time.Second)
			}
		}
	}
	for i := 0; ; i++ {
		p := filepath.Join(s.opt.Dir, "events-"+at.Format(fileNameLayout)+".jsonl")
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			s.f, s.size = f, 0
			s.prune()
			return nil
		}
		if !os.IsExist(err) || i >= 100 {
			return err
		}
		at = at.Add(time.Second)
	}
}

// prune deletes the oldest files beyond KeepFiles. The current file is never
// deleted. Failures are ignored: retention is best effort.
func (s *Store) prune() {
	fs := s.files()
	cur := ""
	if s.f != nil {
		cur = filepath.Clean(s.f.Name())
	}
	for i := 0; len(fs)-i > s.opt.KeepFiles; i++ {
		if filepath.Clean(fs[i].path) == cur {
			continue
		}
		os.Remove(fs[i].path)
	}
}

// loadRecent reads the newest events from the files, newest file first, until
// MemoryEvents are held or the files run out.
func (s *Store) loadRecent() {
	fs := s.files()
	var chunks [][]Event
	have, consumed := 0, 0
	for i := len(fs) - 1; i >= 0 && have < s.opt.MemoryEvents; i-- {
		var evs []Event
		scanFile(fs[i].path, func(e *Event) bool { evs = append(evs, *e); return true })
		chunks = append(chunks, evs)
		have += len(evs)
		consumed++
	}
	var all []Event
	for i := len(chunks) - 1; i >= 0; i-- {
		all = append(all, chunks[i]...)
	}
	s.coversAll = consumed == len(fs) && len(all) <= s.opt.MemoryEvents
	if len(all) > s.opt.MemoryEvents {
		all = all[len(all)-s.opt.MemoryEvents:]
	}
	s.ring = all
}

// scanFile calls fn for every parseable event of a file, in file order, until
// fn returns false. Unparseable lines (a torn last line) are skipped.
func scanFile(path string, fn func(*Event) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var e Event
			if json.Unmarshal(line, &e) == nil && e.Type != "" {
				if !fn(&e) {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// Record adds an event. It never blocks and never fails: the event goes into
// the in-memory ring and, if the writer queue has room, to the file writer.
// The store sets Seq. With history off it does nothing.
func (s *Store) Record(ev Event) {
	if !s.on {
		return
	}
	if ev.Nodes == nil {
		ev.Nodes = []string{}
	}
	if ev.Data == nil {
		ev.Data = map[string]any{}
	}
	s.mu.Lock()
	defer supervise.Unlocked("history", s.mu.Unlock)
	if s.closed {
		return
	}
	s.seq++
	supervise.Point("history.record", s) // test hook, under s.mu: the sequence is taken, the event not kept
	ev.Seq = s.seq
	s.ring = append(s.ring, ev)
	for _, fn := range s.subs {
		// A subscriber runs on the recording goroutine, which may be the
		// scheduler's event loop: its panic must not end that loop. The
		// subscriber stays subscribed (background-task isolation).
		supervise.Guard("history.subscriber", func() { fn(ev) }, nil)
	}
	if len(s.ring) > s.opt.MemoryEvents {
		s.ring = s.ring[len(s.ring)-s.opt.MemoryEvents:]
		s.coversAll = false
	}
	select {
	case s.ch <- item{ev: ev}:
	default:
		s.dropped.Add(1)
	}
}

// item is one unit of work for the writer: an event, or a flush marker.
type item struct {
	ev    Event
	flush chan struct{}
}

// Flush waits until every event recorded before the call has been handed to
// the file (it is for tests and for a clean shutdown; Record never waits).
func (s *Store) Flush() {
	if !s.on {
		return
	}
	done := make(chan struct{})
	if s.queueFlush(done) {
		<-done
	}
}

// queueFlush queues a flush marker; false once the store is closed.
func (s *Store) queueFlush(done chan struct{}) bool {
	s.mu.Lock()
	defer supervise.Unlocked("history", s.mu.Unlock)
	if s.closed {
		return false
	}
	s.ch <- item{flush: done} // may wait for room; only Flush callers wait
	return true
}

// Close stops accepting events, writes what is queued and closes the file.
func (s *Store) Close() {
	func() {
		s.mu.Lock()
		defer supervise.Unlocked("history", s.mu.Unlock)
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}()
	<-s.done
}

// writerDone is the writer's final return: the file is closed.
func (s *Store) writerDone() {
	if s.f != nil {
		s.f.Close()
	}
	close(s.done)
}

// reopen is the writer's rebuild after a failure: a fresh file.
func (s *Store) reopen() {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	s.size = 0
	if err := s.openNew(); err != nil {
		s.fail(err)
		return
	}
	s.failed = false
}

func (s *Store) writer() {
	for it := range s.ch {
		if it.flush != nil {
			close(it.flush)
			continue
		}
		if s.failed {
			continue
		}
		supervise.Point("history.writer", s)
		line, err := json.Marshal(it.ev)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		if s.size > 0 && s.size+int64(len(line)) > s.opt.MaxFileBytes {
			if err := s.openNew(); err != nil {
				s.fail(err)
				continue
			}
		}
		n, err := s.f.Write(line)
		s.size += int64(n)
		if err != nil {
			s.fail(err)
		}
	}
}

func (s *Store) fail(err error) {
	s.failed = true
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	s.log.Warn("writing the event history failed; events are kept in memory only until restart", "dir", s.opt.Dir, "err", err)
}

// snapshot copies the ring.
func (s *Store) snapshot() ([]Event, bool) {
	s.mu.Lock()
	defer supervise.Unlocked("history", s.mu.Unlock)
	return slices.Clone(s.ring), s.coversAll
}

// OldestTs is the time of the oldest event known (0 when there is none).
func (s *Store) OldestTs() int64 {
	if !s.on {
		return 0
	}
	mem, covers := s.snapshot()
	if !covers {
		var ts int64
		for _, f := range s.files() {
			scanFile(f.path, func(e *Event) bool { ts = e.Ts; return false })
			if ts != 0 {
				return ts
			}
		}
	}
	if len(mem) > 0 {
		return mem[0].Ts
	}
	return 0
}

// fileMayHold says whether file i (of fs, ascending) can hold events in
// [since, until], going by the file creation times in the names.
func fileMayHold(fs []fileRef, i int, since, until int64) bool {
	slack := nameSlack.Milliseconds()
	if until > 0 && fs[i].start-slack > until {
		return false
	}
	if since > 0 && i+1 < len(fs) && fs[i+1].start+slack <= since {
		return false
	}
	return true
}

// Query returns the events matching q. The newest events come from memory;
// when the query reaches further back than the memory window, the files are
// read, newest (or oldest, for Asc) file first, and reading stops as soon as
// the limit is reached.
func (s *Store) Query(q Query) Result {
	res := Result{Events: []Event{}, History: "off"}
	if !s.on {
		return res
	}
	res.History = "on"
	res.OldestTs = s.OldestTs()
	limit := q.limit()
	mem, covers := s.snapshot()
	memOldest := int64(math.MaxInt64)
	if len(mem) > 0 {
		memOldest = mem[0].Ts
	}
	needFiles := !covers && (q.Since == 0 || q.Since < memOldest)

	var memHits []Event
	for i := range mem {
		if q.Match(&mem[i]) {
			memHits = append(memHits, mem[i])
		}
	}
	res.TotalMatching = len(memHits)

	if q.Asc {
		var out []Event
		if needFiles {
			var n int
			var more bool
			out, n, more = s.readFiles(q, memOldest, limit, true)
			res.TotalMatching += n
			if more {
				res.Truncated = true
			}
		}
		for _, e := range memHits {
			if len(out) >= limit {
				res.Truncated = true
				break
			}
			out = append(out, e)
		}
		res.Events = append(res.Events, out...)
		return res
	}
	// newest first
	for i := len(memHits) - 1; i >= 0 && len(res.Events) < limit; i-- {
		res.Events = append(res.Events, memHits[i])
	}
	if len(memHits) > limit {
		res.Truncated = true
	}
	if needFiles {
		if need := limit - len(res.Events); need > 0 {
			out, n, more := s.readFiles(q, memOldest, need, false)
			res.TotalMatching += n
			res.Events = append(res.Events, out...)
			if more {
				res.Truncated = true
			}
		} else {
			res.Truncated = true // older events may exist on disk
		}
	}
	return res
}

// readFiles reads events older than cutoff (those the memory window does not
// hold) that match q. Asc: from the oldest file, the first `need`. Desc: from
// the newest file, the newest `need`, newest first. It returns the events and
// how many matches it saw.
func (s *Store) readFiles(q Query, cutoff int64, need int, asc bool) (out []Event, seen int, more bool) {
	fs := s.files()
	if asc {
		for i := range fs {
			if !fileMayHold(fs, i, q.Since, q.Until) {
				continue
			}
			stopped := false
			scanFile(fs[i].path, func(e *Event) bool {
				if e.Ts >= cutoff || !q.Match(e) {
					return true
				}
				if len(out) == need {
					stopped = true // one more match exists
					return false
				}
				seen++
				out = append(out, *e)
				return true
			})
			if stopped {
				return out, seen, true
			}
			if len(out) >= need {
				return out, seen, i+1 < len(fs)
			}
		}
		return out, seen, false
	}
	i := len(fs) - 1
	for ; i >= 0 && len(out) < need; i-- {
		if !fileMayHold(fs, i, q.Since, q.Until) {
			continue
		}
		// Forward through the file, keeping only the newest `want` matches.
		want := need - len(out)
		tail := make([]Event, 0, min(want, 256))
		scanFile(fs[i].path, func(e *Event) bool {
			if e.Ts >= cutoff || !q.Match(e) {
				return true
			}
			seen++
			if len(tail) == want {
				copy(tail, tail[1:])
				tail = tail[:want-1]
			}
			tail = append(tail, *e)
			return true
		})
		for j := len(tail) - 1; j >= 0; j-- {
			out = append(out, tail[j])
		}
	}
	return out, seen, seen > len(out) || i >= 0
}

// scanRange calls fn for every event in [since, until] in time order: the
// files first (events older than the memory window), then the window.
func (s *Store) scanRange(since, until int64, fn func(*Event)) {
	if !s.on {
		return
	}
	mem, covers := s.snapshot()
	memOldest := int64(math.MaxInt64)
	if len(mem) > 0 {
		memOldest = mem[0].Ts
	}
	q := Query{Since: since, Until: until}
	if !covers && (since == 0 || since < memOldest) {
		fs := s.files()
		for i := range fs {
			if !fileMayHold(fs, i, since, until) {
				continue
			}
			scanFile(fs[i].path, func(e *Event) bool {
				if e.Ts < memOldest && q.Match(e) {
					fn(e)
				}
				return true
			})
		}
	}
	for i := range mem {
		if q.Match(&mem[i]) {
			fn(&mem[i])
		}
	}
}

// Export writes the events in [since, until] as JSON Lines, oldest first.
func (s *Store) Export(w io.Writer, since, until int64) error {
	var werr error
	enc := json.NewEncoder(w) // one compact line per event
	s.scanRange(since, until, func(e *Event) {
		if werr == nil {
			werr = enc.Encode(e)
		}
	})
	return werr
}

// ---- summary ----

// Summary is the answer to GET /api/history/summary.
type Summary struct {
	Since      int64            `json:"since"`
	Until      int64            `json:"until"`
	History    string           `json:"history"`
	Events     int              `json:"events"`
	ByType     map[string]int   `json:"by_type"`
	BySeverity map[string]int   `json:"by_severity"`
	Loads      LoadCounts       `json:"loads"`
	Profiles   []ProfileSummary `json:"profiles"`
	Nodes      []NodeSummary    `json:"nodes"`
	Incidents  []Event          `json:"incidents"` // every error-severity event, newest first
	// IncidentsTruncated is true when more than MaxIncidents were found.
	IncidentsTruncated bool `json:"incidents_truncated"`
}

// MaxIncidents bounds the incident list of a summary.
const MaxIncidents = 500

// LoadCounts counts finished loads by outcome.
type LoadCounts struct {
	Ready     int `json:"ready"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// ProfileSummary is one profile's finished loads. The load-time and speed
// figures are over the loads that became ready.
type ProfileSummary struct {
	Profile   string  `json:"profile"`
	Loads     int     `json:"loads"` // all finished loads
	Ready     int     `json:"ready"`
	Failed    int     `json:"failed"`
	Cancelled int     `json:"cancelled"`
	MinS      float64 `json:"min_s"`
	MedianS   float64 `json:"median_s"`
	MaxS      float64 `json:"max_s"`
	AvgMBs    float64 `json:"avg_mb_s"`
}

// NodeSummary counts the events that name a node as the subject of trouble.
type NodeSummary struct {
	Node      string `json:"node"`
	Hung      int    `json:"hung"`
	Lost      int    `json:"lost"`
	LowMemory int    `json:"low_memory"`
}

// Num reads a number from an event data value (a freshly built event holds Go
// numbers, one read back from a file holds float64).
func Num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func str(v any) string { s, _ := v.(string); return s }

// Summarize aggregates the events in [since, until].
func (s *Store) Summarize(since, until int64) Summary {
	sum := Summary{Since: since, Until: until, History: "off", ByType: map[string]int{}, BySeverity: map[string]int{},
		Profiles: []ProfileSummary{}, Nodes: []NodeSummary{}, Incidents: []Event{}}
	if !s.on {
		return sum
	}
	sum.History = "on"
	type prof struct {
		ProfileSummary
		secs  []float64
		rates []float64
	}
	profs := map[string]*prof{}
	nodes := map[string]*NodeSummary{}
	node := func(n string) *NodeSummary {
		if nodes[n] == nil {
			nodes[n] = &NodeSummary{Node: n}
		}
		return nodes[n]
	}
	var incidents []Event
	s.scanRange(since, until, func(e *Event) {
		sum.Events++
		sum.ByType[e.Type]++
		sum.BySeverity[e.Severity]++
		if e.Severity == Error {
			incidents = append(incidents, *e)
		}
		switch e.Type {
		case TLoadFinished:
			outcome := str(e.Data["outcome"])
			p := profs[e.Profile]
			if p == nil {
				p = &prof{ProfileSummary: ProfileSummary{Profile: e.Profile}}
				profs[e.Profile] = p
			}
			p.Loads++
			switch outcome {
			case "ready":
				sum.Loads.Ready++
				p.Ready++
				if v, ok := Num(e.Data["load_s"]); ok {
					p.secs = append(p.secs, v)
				}
				if v, ok := Num(e.Data["avg_mb_s"]); ok && v > 0 {
					p.rates = append(p.rates, v)
				}
			case "failed":
				sum.Loads.Failed++
				p.Failed++
			case "cancelled":
				sum.Loads.Cancelled++
				p.Cancelled++
			}
		case TWorkerHealth:
			if str(e.Data["state"]) == "hung" {
				for _, n := range e.Nodes {
					node(n).Hung++
				}
			}
		case TNodeLost:
			for _, n := range e.Nodes {
				node(n).Lost++
			}
		case TLoadLowMemory:
			for _, n := range e.Nodes {
				node(n).LowMemory++
			}
		}
	})
	for _, p := range profs {
		if len(p.secs) > 0 {
			sort.Float64s(p.secs)
			p.MinS, p.MaxS = p.secs[0], p.secs[len(p.secs)-1]
			p.MedianS = median(p.secs)
		}
		if len(p.rates) > 0 {
			t := 0.0
			for _, r := range p.rates {
				t += r
			}
			p.AvgMBs = math.Round(t/float64(len(p.rates))*10) / 10
		}
		sum.Profiles = append(sum.Profiles, p.ProfileSummary)
	}
	sort.Slice(sum.Profiles, func(i, j int) bool { return sum.Profiles[i].Profile < sum.Profiles[j].Profile })
	for _, n := range nodes {
		sum.Nodes = append(sum.Nodes, *n)
	}
	sort.Slice(sum.Nodes, func(i, j int) bool { return sum.Nodes[i].Node < sum.Nodes[j].Node })
	for i := len(incidents) - 1; i >= 0; i-- {
		if len(sum.Incidents) == MaxIncidents {
			sum.IncidentsTruncated = true
			break
		}
		sum.Incidents = append(sum.Incidents, incidents[i])
	}
	return sum
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return math.Round((sorted[n/2-1]+sorted[n/2])/2*10) / 10
}

// Subscribe registers fn to be called with every event Record accepts, in
// order, after it has its Seq. fn runs on the recording goroutine (the
// scheduler loop's hand-off) and so must never block. The returned function
// removes the subscription. An off store never calls fn.
func (s *Store) Subscribe(fn func(Event)) (cancel func()) {
	s.mu.Lock()
	defer supervise.Unlocked("history", s.mu.Unlock)
	s.subSeq++
	id := s.subSeq
	if s.subs == nil {
		s.subs = map[uint64]func(Event){}
	}
	s.subs[id] = fn
	return func() {
		s.mu.Lock()
		defer supervise.Unlocked("history", s.mu.Unlock)
		delete(s.subs, id)
	}
}

// ScanType calls fn, oldest first, for every kept event of type typ: the
// files, then the in-memory window. Used to rebuild totals from history at
// start (api occupied_only).
func (s *Store) ScanType(typ string, fn func(Event)) {
	s.scanRange(0, math.MaxInt64, func(e *Event) {
		if e.Type == typ {
			fn(*e)
		}
	})
}
