// Package usage keeps the scheduler's usage history: the request log, its
// hourly and daily roll-ups and (later) the per-machine availability hours.
//
// It lives in its own SQLite file beside scheduler.db (FileName, in the data
// folder), never in scheduler.db: an older scheduler image starts on the same
// data folder and simply ignores the file. The file is optional in every
// sense. A missing, unreadable or damaged file never stops the scheduler or
// makes it unhealthy: a damaged file is moved aside under a timestamped name,
// the store starts empty, one WARN is logged and HistoryBeginsAt says from
// when the history is complete.
//
// Nothing is written on the caller's goroutine. Callers (the core loop, the
// proxy) only Submit operations into a bounded in-memory queue; one writer
// goroutine, supervised, drains it in batches, one transaction per batch.
// Submit never blocks: when the queue is full the oldest droppable operation
// (a non-terminal update) is dropped and counted.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo), as scheduler.db uses
)

// FileName is the usage database in the data folder.
const FileName = "usage.db"

// Defaults of the options.
const (
	DefaultQueueSize = 4096
	DefaultBatchSize = 256
	// hardCapFactor bounds the queue when it holds only terminal operations:
	// up to QueueSize*hardCapFactor, beyond that the oldest operation is
	// dropped whatever it is (and counted in DroppedTerminal).
	hardCapFactor = 2
	damagedLayout = "20060102T150405Z"
)

// Options configure a Store. Dir empty means the store is off.
type Options struct {
	Dir       string // the data folder (beside scheduler.db)
	QueueSize int    // operations waiting for the writer (default 4096)
	BatchSize int    // operations per transaction (default 256)
	Retention Retention
	Logger    *slog.Logger
	// Now is the clock (default time.Now); tests set it.
	Now func() time.Time
	// BeforeBatch, when set, runs on the writer goroutine before each batch
	// is written (tests: a slow or blocked disk).
	BeforeBatch func()
	// CloseWait bounds how long Close waits for the writer to write what is
	// queued (default 10 s): a disk that does not answer never holds a
	// shutdown up. What was not written then is lost.
	CloseWait time.Duration
}

// Op is one write. Apply runs on the writer goroutine inside the batch's
// transaction; it must only touch the usage tables.
type Op struct {
	// Terminal marks an operation that must survive a full queue (a
	// request's final row, a closed hour). Non-terminal operations (a row
	// written at admission, an update at start) may be dropped when the
	// queue is full: a later terminal operation writes the whole row again.
	Terminal bool
	Apply    func(tx *sql.Tx) error

	flush chan struct{} // Flush's marker: closed once everything before it is written
}

// Stats is the store's state, as GET /api/system can show it.
type Stats struct {
	On              bool   `json:"on"`
	Path            string `json:"path,omitempty"`
	HistoryBeginsAt int64  `json:"history_begins_at,omitempty"` // unix ms
	Queued          int    `json:"queued"`
	Written         uint64 `json:"written"`          // operations applied
	Batches         uint64 `json:"batches"`          // transactions committed
	Dropped         uint64 `json:"dropped"`          // non-terminal operations dropped (full queue)
	DroppedTerminal uint64 `json:"dropped_terminal"` // terminal operations dropped (queue at its hard cap)
	Failed          uint64 `json:"failed"`           // operations that returned an error, or whose batch failed
	LastError       string `json:"last_error,omitempty"`
	MovedAside      string `json:"moved_aside,omitempty"` // a damaged file moved aside at open
}

// Store is the usage database. All methods are safe for concurrent use and
// safe on a nil or off store.
type Store struct {
	opt  Options
	log  *slog.Logger
	db   *sql.DB
	path string

	beginsAt time.Time
	aside    string

	mu      sync.Mutex
	queue   []Op
	closing bool
	wake    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	tasks   sync.WaitGroup
	lastErr string

	written, batches, dropped, droppedTerminal, failed atomic.Uint64
	tx                                                 *sql.Tx // the batch in progress (writer only)
}

// Open opens dir/usage.db, checks it and applies the migrations, then
// starts the writer (and the retention task). It never fails: an unusable
// folder leaves the store off, a damaged file is moved aside and replaced
// by an empty one. Each of these logs one WARN.
func Open(o Options) *Store {
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CloseWait <= 0 {
		o.CloseWait = 10 * time.Second
	}
	o.Retention = o.Retention.withDefaults()
	s := &Store{opt: o, log: o.Logger, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if strings.TrimSpace(o.Dir) == "" {
		close(s.done)
		return s
	}
	s.path = filepath.Join(o.Dir, FileName)
	if err := s.open(); err != nil {
		s.log.Warn("the usage history is off: its file cannot be used; the scheduler runs without it", "path", s.path, "err", err)
		s.db = nil
		close(s.done)
		return s
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	supervise.Go(ctx, supervise.Spec{Name: "usage.writer", Policy: supervise.Restart,
		Rebuild: func(any) { s.rollback() }, Done: func() { close(s.done) }}, s.writer)
	s.startTasks(ctx)
	return s
}

// open prepares s.db; a damaged file is moved aside once and replaced.
func (s *Store) open() error {
	if err := os.MkdirAll(s.opt.Dir, 0o755); err != nil {
		return err
	}
	_, statErr := os.Stat(s.path)
	missing := errors.Is(statErr, os.ErrNotExist)
	err := s.openFile()
	if err == nil {
		if missing {
			s.log.Warn("no usage history file yet: the usage history begins now", "path", s.path)
		}
		return nil
	}
	var newer *NewerSchemaError
	if errors.As(err, &newer) {
		return err // a newer scheduler wrote it: kept as it is, the store stays off
	}
	if missing {
		return err // nothing to move aside: the folder itself is the problem
	}
	aside, mvErr := s.moveAside()
	if mvErr != nil {
		return fmt.Errorf("%w; moving it aside failed: %v", err, mvErr)
	}
	s.aside = aside
	if err2 := s.openFile(); err2 != nil {
		return err2
	}
	s.log.Warn("the usage history file was damaged: moved aside, the usage history begins now",
		"path", s.path, "moved_to", aside, "err", err)
	return nil
}

func (s *Store) openFile() error {
	dsn := "file:" + filepath.ToSlash(s.path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(4)
	fail := func(err error) error {
		db.Close()
		return err
	}
	if err := db.Ping(); err != nil {
		return fail(err)
	}
	var check string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil {
		return fail(fmt.Errorf("integrity check: %w", err))
	}
	if check != "ok" {
		return fail(fmt.Errorf("integrity check: %s", check))
	}
	if err := migrate(db, s.opt.Now()); err != nil {
		return fail(err)
	}
	var begins int64
	if err := db.QueryRow(`SELECT value FROM usage_meta WHERE key = 'history_begins_at'`).Scan(&begins); err != nil {
		return fail(fmt.Errorf("reading history_begins_at: %w", err))
	}
	s.db, s.beginsAt = db, time.UnixMilli(begins).UTC()
	return nil
}

// moveAside renames the file and its WAL and shared-memory files to
// usage.db.damaged-<UTC time>[-n].
func (s *Store) moveAside() (string, error) {
	base := s.path + ".damaged-" + s.opt.Now().UTC().Format(damagedLayout)
	dst := base
	for n := 1; ; n++ {
		if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
			break
		}
		dst = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(s.path, dst); err != nil {
		return "", err
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(s.path + sfx); err == nil {
			if err := os.Rename(s.path+sfx, dst+sfx); err != nil {
				os.Remove(s.path + sfx) // a stale WAL must not be replayed into the new file
			}
		}
	}
	return dst, nil
}

// On says whether the store records.
func (s *Store) On() bool { return s != nil && s.db != nil }

// DB is the read connection (WAL: reads run beside the writer). Nil when
// the store is off. Never write through it: writes go through Submit.
func (s *Store) DB() *sql.DB {
	if !s.On() {
		return nil
	}
	return s.db
}

// Path is the database file ("" when no folder was given).
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// HistoryBeginsAt is when the history in this file begins: when the file was
// created (after a damaged one was moved aside, that is now). Zero when off.
func (s *Store) HistoryBeginsAt() time.Time {
	if !s.On() {
		return time.Time{}
	}
	return s.beginsAt
}

// Now is the store's clock.
func (s *Store) Now() time.Time { return s.opt.Now() }

// Submit queues op for the writer and returns at once; false when the
// store is off or closing, or op itself was dropped. It never blocks and
// never touches the disk.
func (s *Store) Submit(op Op) bool {
	if !s.On() || op.Apply == nil && op.flush == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	kept := true
	if len(s.queue) >= s.opt.QueueSize {
		kept = s.makeRoomLocked(op)
	}
	if kept {
		s.queue = append(s.queue, op)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return kept
}

// makeRoomLocked frees a place for op in a full queue: the oldest
// droppable operation goes. With none queued, a droppable op is itself
// dropped; a terminal one is kept up to the hard cap, then the oldest
// operation goes whatever it is.
func (s *Store) makeRoomLocked(op Op) bool {
	for i, q := range s.queue {
		if !q.Terminal && q.flush == nil {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.dropped.Add(1)
			return true
		}
	}
	if !op.Terminal && op.flush == nil {
		s.dropped.Add(1)
		return false
	}
	if len(s.queue) < s.opt.QueueSize*hardCapFactor {
		return true
	}
	for i, q := range s.queue {
		if q.flush == nil {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.droppedTerminal.Add(1)
			return true
		}
	}
	return true
}

// Flush waits until every operation submitted before it is written (or
// dropped), or ctx ends. Tests and shutdown use it.
func (s *Store) Flush(ctx context.Context) error {
	if !s.On() {
		return nil
	}
	ch := make(chan struct{})
	if !s.Submit(Op{Terminal: true, flush: ch}) {
		return errors.New("usage: the store is closing")
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("usage: the writer stopped")
	}
}

// Close writes what is queued, stops the tasks and closes the file. Safe
// to call twice and on an off store.
func (s *Store) Close() error {
	if !s.On() {
		return nil
	}
	if !s.markClosing() {
		return nil
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	t := time.NewTimer(s.opt.CloseWait)
	defer t.Stop()
	select {
	case <-s.done:
	case <-t.C:
		s.cancel()
		s.log.Warn("usage history: the writer did not finish in time; the queued changes are lost", "queued", s.Stats().Queued)
		return errors.New("usage: the writer did not finish in time")
	}
	s.cancel()
	s.tasks.Wait()
	return s.db.Close()
}

// markClosing stops Submit; false when Close already ran.
func (s *Store) markClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.closing = true
	return true
}

// Stats reports the queue and the writer's counts.
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	st := Stats{On: s.On(), Path: s.path, Written: s.written.Load(), Batches: s.batches.Load(),
		Dropped: s.dropped.Load(), DroppedTerminal: s.droppedTerminal.Load(), Failed: s.failed.Load(), MovedAside: s.aside}
	if st.On {
		st.HistoryBeginsAt = s.beginsAt.UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Queued, st.LastError = len(s.queue), s.lastErr
	return st
}

// take removes up to BatchSize operations; done once closing with nothing left.
func (s *Store) take() (batch []Op, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := min(len(s.queue), s.opt.BatchSize)
	batch = append(batch, s.queue[:n]...)
	s.queue = append(s.queue[:0], s.queue[n:]...)
	return batch, n == 0 && s.closing
}

func (s *Store) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err.Error()
}

// writer drains the queue until Close; ctx only ends after the drain.
func (s *Store) writer(ctx context.Context) {
	for {
		batch, done := s.take()
		if done {
			return
		}
		if len(batch) == 0 {
			select {
			case <-s.wake:
			case <-ctx.Done():
				return
			}
			continue
		}
		s.writeBatch(batch)
	}
}

var errLogEvery = time.Minute

func (s *Store) writeBatch(batch []Op) {
	if f := s.opt.BeforeBatch; f != nil {
		f()
	}
	var flushes []chan struct{}
	defer func() {
		for _, ch := range flushes {
			close(ch)
		}
	}()
	work := 0
	for _, op := range batch {
		if op.flush != nil {
			flushes = append(flushes, op.flush)
		} else {
			work++
		}
	}
	if work == 0 {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		s.failBatch(work, err)
		return
	}
	s.tx = tx
	applied := 0
	for _, op := range batch {
		if op.flush != nil {
			continue
		}
		if err := op.Apply(tx); err != nil {
			s.failed.Add(1)
			s.noteErr(err)
			continue
		}
		applied++
	}
	s.tx = nil
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		s.failBatch(applied, err)
		return
	}
	s.written.Add(uint64(applied))
	s.batches.Add(1)
}

func (s *Store) failBatch(n int, err error) {
	s.failed.Add(uint64(n))
	s.noteErr(err)
}

var lastErrLog atomic.Int64

// noteErr keeps the error for Stats and logs at most one WARN a minute.
func (s *Store) noteErr(err error) {
	s.setErr(err)
	now := time.Now().UnixNano()
	last := lastErrLog.Load()
	if now-last >= errLogEvery.Nanoseconds() && lastErrLog.CompareAndSwap(last, now) {
		s.log.Warn("usage history: a write failed; the scheduler goes on without it", "err", err, "failed_total", s.failed.Load())
	}
}

// rollback ends a batch that a panic interrupted (the writer's Rebuild).
func (s *Store) rollback() {
	if s.tx != nil {
		s.tx.Rollback()
		s.tx = nil
	}
}
