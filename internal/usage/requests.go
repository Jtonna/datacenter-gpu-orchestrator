package usage

import (
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelname"

	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Statuses of a request. Queued and running are live (in memory only until
// the request ends); the others are final.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"    // reason says why (stall, cut, upstream_error, upstream_status, aborted, scheduler_restart)
	StatusRefused   = "refused"   // turned away before it ran; reason is the scheduler's reason
	StatusCancelled = "cancelled" // the app that sent it left (reason client_left, or client_left_queued)
)

// Reasons the recorder itself gives.
const (
	ReasonClientLeft       = "client_left"
	ReasonClientLeftQueued = "client_left_queued"
	ReasonSchedulerRestart = "scheduler_restart"
	ReasonUpstreamStatus   = "upstream_status"
)

// Kinds of request.
const (
	KindV1       = "v1"       // /v1/..., queued by the scheduler
	KindUpstream = "upstream" // /upstream/... passthrough, never queued
)

// Node is one machine that served a request, with its role at the start.
type Node struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"` // host or helper; the host is listed first
	// Share is the machine's fraction of the model's layers; absent when
	// it holds none (the scheduler's machine hosting, a command profile).
	Share float64 `json:"share,omitempty"`
}

// Request is one row of the request log, stored or live. Times are unix
// ms; a token count or rate that is not known is nil.
type Request struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
	Stream     bool   `json:"stream"`

	QueuedAt     int64 `json:"queued_at"`
	StartedAt    int64 `json:"started_at,omitempty"`
	FirstTokenAt int64 `json:"first_token_at,omitempty"`
	EndedAt      int64 `json:"ended_at,omitempty"`
	// WaitMS is the time queued (to the start, or to the end when it never
	// started); DurationMS the time running (start to end, or to now while
	// it runs). Live rows compute both from now.
	WaitMS     int64 `json:"wait_ms"`
	DurationMS int64 `json:"duration_ms"`

	Profile   string `json:"profile"`             // never an alias
	Requested string `json:"requested,omitempty"` // the model name the client sent
	ModelFile string `json:"model_file,omitempty"`
	// ShortName is the model file's name as the page shows it
	// ("qwen3.5-4b"): not stored, derived from ModelFile whenever a row is
	// read, with the namer the API sets (SetNamer: the catalog as it is
	// then, so it follows the same rule as /api/models). Ctx is the
	// profile's ctx when the request came in (0 when unknown: a row of an
	// older build, or a profile without one). The page draws
	// "qwen3.5-4b:8k" from the two, even after the profile is deleted.
	ShortName string `json:"short_name,omitempty"`
	Ctx       int    `json:"ctx,omitempty"`

	TokensIn     *int     `json:"tokens_in"`     // every prompt token, reused ones included
	TokensReused *int     `json:"tokens_reused"` // prompt tokens reused from the cache
	TokensOut    *int     `json:"tokens_out"`
	PrefillTPS   *float64 `json:"prefill_tps"`
	DecodeTPS    *float64 `json:"decode_tps"`
	// Live: tokens_out is an estimate counted from the stream, replaced by
	// the server's own figure at the end. While a running request is still
	// reading its prompt, PrefillProcessed is how many of tokens_in are done
	// (only when the model server reports prompt progress; never stored).
	Live             bool `json:"live,omitempty"`
	PrefillProcessed *int `json:"prefill_processed,omitempty"`

	SchedID  uint64 `json:"sched_id,omitempty"` // the scheduler's request id (feed, events)
	Instance uint64 `json:"instance,omitempty"`
	Session  string `json:"session_id,omitempty"` // the load session
	HostNode string `json:"host_node,omitempty"`  // "" when the scheduler's machine hosts
	Nodes    []Node `json:"nodes"`

	Caller     string `json:"caller"`      // the agent key's name, else the default user
	CallerKind string `json:"caller_kind"` // key or default
	SourceAddr string `json:"source_addr,omitempty"`
	BenchRun   string `json:"bench_run,omitempty"`

	prefillMS, decodeMS float64 // for the roll-ups (not shown)
}

// Begin describes a request as it arrives.
type Begin struct {
	Kind       string
	Path       string
	Stream     bool
	Profile    string
	Requested  string
	ModelFile  string
	Ctx        int // the profile's ctx now (kept on the row)
	Caller     string
	CallerKind string
	SourceAddr string
	BenchRun   string
}

// Placement is where an admitted request runs.
type Placement struct {
	SchedID  uint64
	Instance uint64
	Session  string
	HostNode string
	Nodes    []Node
	// Ctx is the context per slot the instance runs with (the model
	// server's own figure once read); 0 keeps the row's ctx.
	Ctx int
}

// End is how a request ended. Counts and rates below 0 are unknown.
type End struct {
	Status     string
	Reason     string
	Message    string
	HTTPStatus int
	TokensIn   int
	Reused     int
	TokensOut  int
	PrefillTPS float64
	DecodeTPS  float64
	PrefillMS  float64
	DecodeMS   float64
	// Stream, when set, marks the answer as an event stream (known only once
	// it began).
	Stream bool
}

// LiveSource is what a running request's live figures come from (the
// proxy's Live): first-byte time and the streamed event count.
type LiveSource interface {
	FirstByte() time.Time
	Chunks() int64
}

// PrefillSource is a LiveSource that may also know the prompt while it is
// read: every prompt token (reused ones included), the reused ones, how many
// are done, and the prefill rate (0 when not known yet). ok is false until
// the model server said anything about it.
type PrefillSource interface {
	Prefill() (total, reused, processed int, tps float64, ok bool)
}

// Change kinds a Watcher hears of.
const (
	ChangeQueued  = "queued"
	ChangeStarted = "started"
	ChangeEnded   = "ended"
)

// Watcher hears of every change to a request row, on the goroutine that
// made it (a request's own). Watching is asked first, so a watcher with no
// one listening costs one call and no row copy. Changed must not block.
type Watcher interface {
	Watching() bool
	Changed(kind string, row Request)
}

// recentEnded is how many just-finished rows the recorder keeps in memory,
// so a list shows them before the writer has stored them.
const recentEnded = 64

// Recorder keeps the live requests in memory and hands every change to the
// store's writer. It never touches the disk on the caller's goroutine. A
// recorder on an off store still lists live requests.
type Recorder struct {
	st  *Store
	now func() time.Time

	mu     sync.Mutex
	live   map[int64]*Tracker
	lastID int64
	recent []Request // the last recentEnded final rows, oldest first

	ended atomic.Uint64
	watch atomic.Pointer[watcherBox]
	namer atomic.Pointer[func(file string) string]
}

// SetNamer sets how a row's model file is named (short_name); nil: the
// suffix rule without a catalog (modelname.Namer nil).
func (r *Recorder) SetNamer(f func(file string) string) {
	if r == nil {
		return
	}
	if f == nil {
		r.namer.Store(nil)
		return
	}
	r.namer.Store(&f)
}

// named fills short_name on rows leaving the recorder.
func (r *Recorder) named(rows []Request) []Request {
	f := r.namer.Load()
	for i := range rows {
		if rows[i].ModelFile == "" {
			rows[i].ShortName = ""
		} else if f != nil {
			rows[i].ShortName = (*f)(rows[i].ModelFile)
		} else {
			rows[i].ShortName = (*modelname.Namer)(nil).Name(rows[i].ModelFile)
		}
	}
	return rows
}

func (r *Recorder) namedOne(row Request) Request { return r.named([]Request{row})[0] }

type watcherBox struct{ w Watcher }

// SetWatcher sets the one watcher of the recorder's changes (nil: none).
func (r *Recorder) SetWatcher(w Watcher) {
	if r == nil {
		return
	}
	if w == nil {
		r.watch.Store(nil)
		return
	}
	r.watch.Store(&watcherBox{w})
}

// notify tells the watcher, when anyone listens, of a change; row builds
// the row only then.
func (r *Recorder) notify(kind string, row func() Request) {
	b := r.watch.Load()
	if b == nil || !b.w.Watching() {
		return
	}
	b.w.Changed(kind, r.namedOne(row()))
}

// NewRecorder starts the request log on st (which may be off): rows that a
// previous process left queued or running are closed as failed with reason
// scheduler_restart (through the writer, before any new row).
func NewRecorder(st *Store) *Recorder {
	r := &Recorder{st: st, now: time.Now, live: map[int64]*Tracker{}}
	if st != nil {
		r.now = st.Now
	}
	if db := st.DB(); db != nil {
		db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM client_requests`).Scan(&r.lastID)
		st.Submit(Op{Terminal: true, Apply: closeStale})
	}
	return r
}

// Store is the recorder's store.
func (r *Recorder) Store() *Store {
	if r == nil {
		return nil
	}
	return r.st
}

// Tracker follows one request from arrival to end. Its methods are safe on
// a nil Tracker.
type Tracker struct {
	r       *Recorder
	mu      sync.Mutex
	row     Request
	src     LiveSource
	stopped bool
}

// nextID is time-ordered and unique: unix ms times 1000 plus a counter
// (stays exact in a JSON number until the year 2255).
func (r *Recorder) nextID(at time.Time) int64 {
	id := at.UnixMilli() * 1000
	if id <= r.lastID {
		id = r.lastID + 1
	}
	r.lastID = id
	return id
}

// Queued records a request that has arrived and waits for admission.
func (r *Recorder) Queued(b Begin) *Tracker {
	if r == nil {
		return nil
	}
	now := r.now()
	t := &Tracker{r: r, row: Request{Status: StatusQueued, Kind: b.Kind, Path: b.Path, Stream: b.Stream,
		QueuedAt: now.UnixMilli(), Profile: b.Profile, Requested: b.Requested, ModelFile: b.ModelFile,
		Ctx:    max(b.Ctx, 0),
		Caller: b.Caller, CallerKind: b.CallerKind, SourceAddr: b.SourceAddr, BenchRun: b.BenchRun}}
	r.add(t, now)
	row := t.row
	r.st.Submit(Op{Apply: func(tx *sql.Tx) error { return upsertRow(tx, &row, false) }})
	r.notify(ChangeQueued, func() Request { return t.snapshot(r.now().UnixMilli()) })
	return t
}

func (r *Recorder) add(t *Tracker, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t.row.ID = r.nextID(now)
	r.live[t.row.ID] = t
}

// remove takes a finished row out of the live set and keeps it among the
// recent ones.
func (r *Recorder) remove(final Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, final.ID)
	if len(r.recent) >= recentEnded {
		r.recent = append(r.recent[:0], r.recent[1:]...)
	}
	r.recent = append(r.recent, final)
}

// recentRows is a copy of the recent final rows, oldest first.
func (r *Recorder) recentRows() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.recent...)
}

// ID is the request's id in the log (0 for a nil tracker).
func (t *Tracker) ID() int64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.row.ID
}

// Started records the admission: where the request runs. src gives the
// live figures while it streams (nil: none).
func (t *Tracker) Started(p Placement, src LiveSource) {
	if t == nil {
		return
	}
	if t.start(p, src) {
		t.r.notify(ChangeStarted, func() Request { return t.snapshot(t.r.now().UnixMilli()) })
	}
}

func (t *Tracker) start(p Placement, src LiveSource) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return false
	}
	t.row.Status = StatusRunning
	t.row.StartedAt = t.r.now().UnixMilli()
	t.row.SchedID, t.row.Instance, t.row.Session, t.row.HostNode = p.SchedID, p.Instance, p.Session, p.HostNode
	t.row.Nodes = append([]Node(nil), p.Nodes...)
	if p.Ctx > 0 {
		t.row.Ctx = p.Ctx
	}
	t.src = src
	row := t.row
	t.r.st.Submit(Op{Apply: func(tx *sql.Tx) error { return upsertRow(tx, &row, false) }})
	return true
}

// Ended records the end and hands the final row (and its roll-ups) to the
// writer as a terminal operation. Only the first call counts.
func (t *Tracker) Ended(e End) {
	if t == nil {
		return
	}
	if final, ok := t.end(e); ok {
		t.r.notify(ChangeEnded, func() Request { return final })
	}
}

func (t *Tracker) end(e End) (Request, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return Request{}, false
	}
	t.stopped = true
	now := t.r.now().UnixMilli()
	row := &t.row
	row.Status, row.Reason, row.Message, row.HTTPStatus = e.Status, e.Reason, e.Message, e.HTTPStatus
	row.EndedAt = now
	row.Stream = row.Stream || e.Stream
	if t.src != nil {
		if fb := t.src.FirstByte(); !fb.IsZero() {
			row.FirstTokenAt = fb.UnixMilli()
		}
	}
	row.TokensIn, row.TokensReused, row.TokensOut = intOrNil(e.TokensIn), intOrNil(e.Reused), intOrNil(e.TokensOut)
	row.PrefillTPS, row.DecodeTPS = floatOrNil(e.PrefillTPS), floatOrNil(e.DecodeTPS)
	row.prefillMS, row.decodeMS = e.PrefillMS, e.DecodeMS
	if e.Status == StatusCancelled || e.Status == StatusFailed {
		// Cut short: the server's closing figures never came. Keep what
		// the stream said so far, as the live row showed it; the message
		// says tokens_out was counted.
		if streamFigures(t.src, row, now, false) {
			row.Message = joinMessage(row.Message, MessageTokensCounted)
		}
	}
	row.WaitMS, row.DurationMS = spans(row, now)
	final := *row
	final.Nodes = nonNil(append([]Node(nil), row.Nodes...))
	t.src = nil
	t.r.remove(final)
	t.r.ended.Add(1)
	t.r.st.Submit(Op{Terminal: true, Apply: func(tx *sql.Tx) error {
		if err := upsertRow(tx, &final, true); err != nil {
			return err
		}
		return addRollups(tx, &final)
	}})
	return final, true
}

// spans is (wait, duration) for a row at now (ms).
func spans(row *Request, now int64) (wait, dur int64) {
	end := now
	if row.EndedAt > 0 {
		end = row.EndedAt
	}
	if row.StartedAt > 0 {
		return max(row.StartedAt-row.QueuedAt, 0), max(end-row.StartedAt, 0)
	}
	return max(end-row.QueuedAt, 0), 0
}

// snapshot is the live row now: durations from now, tokens out estimated
// from the stream.
func (t *Tracker) snapshot(now int64) Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	row := t.row
	row.Nodes = nonNil(append([]Node(nil), t.row.Nodes...))
	row.WaitMS, row.DurationMS = spans(&row, now)
	if streamFigures(t.src, &row, now, true) {
		row.Live = true
	}
	return row
}

// MessageTokensCounted is added to the message of a row cut short whose
// tokens_out was counted from the stream (one event, about one token), the
// server's own count having never come.
const MessageTokensCounted = "tokens_out counted from the stream"

func joinMessage(m, more string) string {
	if m == "" {
		return more
	}
	return m + "; " + more
}

// streamFigures fills row from what the stream said so far (src): the
// prompt (every token, the reused ones, the prefill rate) when the request
// asked llama-server for it, the events streamed as tokens_out and their
// rate since the first one. live (a running row) sets every figure known
// and the prompt progress; at the end (live false) only figures still
// unknown are set. It reports whether tokens_out was counted.
func streamFigures(src LiveSource, row *Request, now int64, live bool) (counted bool) {
	if src == nil {
		return false
	}
	if ps, ok := src.(PrefillSource); ok {
		if total, reused, done, tps, ok := ps.Prefill(); ok {
			if live || row.TokensIn == nil {
				row.TokensIn, row.TokensReused = &total, &reused
				if live && done < total {
					row.PrefillProcessed = &done
				}
			}
			if tps > 0 && (live || row.PrefillTPS == nil) {
				row.PrefillTPS = &tps
			}
			counted = live
		}
	}
	if fb := src.FirstByte(); !fb.IsZero() {
		row.FirstTokenAt = fb.UnixMilli()
	}
	if n := src.Chunks(); n > 0 && (live || row.TokensOut == nil) {
		v := int(n)
		row.TokensOut, counted = &v, true
		if row.FirstTokenAt > 0 && now > row.FirstTokenAt && (live || row.DecodeTPS == nil) {
			rate := float64(n) / (float64(now-row.FirstTokenAt) / 1000)
			row.DecodeTPS = &rate
		}
	}
	return counted
}

// Live lists the queued and running requests, oldest first.
func (r *Recorder) Live() []Request {
	if r == nil {
		return []Request{}
	}
	ts := r.liveTrackers()
	now := r.now().UnixMilli()
	out := make([]Request, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.snapshot(now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return r.named(out)
}

func (r *Recorder) liveTrackers() []*Tracker {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Tracker, 0, len(r.live))
	for _, t := range r.live {
		out = append(out, t)
	}
	return out
}

// LiveByID is one live request, if it is live.
func (r *Recorder) LiveByID(id int64) (Request, bool) {
	if r == nil {
		return Request{}, false
	}
	t := r.tracker(id)
	if t == nil {
		return Request{}, false
	}
	return r.namedOne(t.snapshot(r.now().UnixMilli())), true
}

func (r *Recorder) tracker(id int64) *Tracker {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[id]
}

// Ended is how many requests ended since the start (tests).
func (r *Recorder) Ended() uint64 {
	if r == nil {
		return 0
	}
	return r.ended.Load()
}

// Flush waits until every change so far is written (tests).
func (r *Recorder) Flush(ctx context.Context) error { return r.Store().Flush(ctx) }

func intOrNil(v int) *int {
	if v < 0 {
		return nil
	}
	return &v
}

func floatOrNil(v float64) *float64 {
	if v < 0 {
		return nil
	}
	return &v
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullMS(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullCtx is a ctx, NULL when unknown.
func nullCtx(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullPos(v float64) any {
	if v < 0 {
		return nil
	}
	return v
}

const rowColumns = `id, status, reason, message, http_status, kind, path, stream, queued_at, started_at, first_token_at, ended_at,
  profile, requested, model_file, tokens_in, tokens_reused, tokens_out, prefill_tps, decode_tps, prefill_ms, decode_ms,
  sched_id, instance_id, session_id, host_node, nodes, caller, caller_kind, source_addr, bench_run`

// readColumns is rowColumns and the columns of extMigrations (scanRows reads
// either list).
const readColumns = rowColumns + `, ctx`

// upsertRow writes the whole row. A non-final write never replaces a final
// one (an update that arrives late).
func upsertRow(tx *sql.Tx, r *Request, final bool) error {
	nodes, err := json.Marshal(nonNil(r.Nodes))
	if err != nil {
		return err
	}
	q := `INSERT INTO client_requests (` + readColumns + `) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET status=excluded.status, reason=excluded.reason, message=excluded.message, http_status=excluded.http_status,
  stream=excluded.stream, started_at=excluded.started_at, first_token_at=excluded.first_token_at, ended_at=excluded.ended_at,
  tokens_in=excluded.tokens_in, tokens_reused=excluded.tokens_reused, tokens_out=excluded.tokens_out,
  prefill_tps=excluded.prefill_tps, decode_tps=excluded.decode_tps, prefill_ms=excluded.prefill_ms, decode_ms=excluded.decode_ms,
  sched_id=excluded.sched_id, instance_id=excluded.instance_id, session_id=excluded.session_id, host_node=excluded.host_node, nodes=excluded.nodes,
  ctx=COALESCE(excluded.ctx, client_requests.ctx)`
	if !final {
		q += ` WHERE client_requests.status IN ('queued','running')`
	}
	stream := 0
	if r.Stream {
		stream = 1
	}
	_, err = tx.Exec(q, r.ID, r.Status, nullStr(r.Reason), nullStr(r.Message), r.HTTPStatus, r.Kind, nullStr(r.Path), stream,
		r.QueuedAt, nullMS(r.StartedAt), nullMS(r.FirstTokenAt), nullMS(r.EndedAt),
		r.Profile, nullStr(r.Requested), nullStr(r.ModelFile), nullInt(r.TokensIn), nullInt(r.TokensReused), nullInt(r.TokensOut),
		nullFloat(r.PrefillTPS), nullFloat(r.DecodeTPS), nullPos(r.prefillMS), nullPos(r.decodeMS),
		int64(r.SchedID), int64(r.Instance), nullStr(r.Session), nullStr(r.HostNode), string(nodes),
		r.Caller, r.CallerKind, nullStr(r.SourceAddr), nullStr(r.BenchRun), nullCtx(r.Ctx))
	return err
}

func nonNil(n []Node) []Node {
	if n == nil {
		return []Node{}
	}
	return n
}

// closeStale ends the rows a previous process left queued or running.
func closeStale(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT ` + readColumns + ` FROM client_requests WHERE status IN ('queued','running')`)
	if err != nil {
		return err
	}
	stale, err := scanRows(rows)
	if err != nil {
		return err
	}
	for i := range stale {
		r := &stale[i]
		r.EndedAt = max(r.QueuedAt, r.StartedAt, r.FirstTokenAt)
		r.Status, r.Reason = StatusFailed, ReasonSchedulerRestart
		r.Message = "the scheduler stopped while this request was " + map[bool]string{true: "running", false: "queued"}[r.StartedAt > 0]
		r.WaitMS, r.DurationMS = spans(r, r.EndedAt)
		if err := upsertRow(tx, r, true); err != nil {
			return err
		}
		if err := addRollups(tx, r); err != nil {
			return err
		}
	}
	return nil
}
