package usage

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuf) warns() int { return strings.Count(l.String(), "level=WARN") }

func newLog() (*slog.Logger, *logBuf) {
	b := &logBuf{}
	return slog.New(slog.NewTextHandler(b, nil)), b
}

// gate parks the writer (Options.BeforeBatch) while blocked: a stuck disk.
type gate struct {
	mu      sync.Mutex
	release chan struct{}
	entered chan struct{}
}

func newGate() *gate { return &gate{} }

// block makes the next batches wait until the returned channel is closed.
func (g *gate) block() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.release, g.entered = make(chan struct{}), make(chan struct{})
	return g.release
}

func (g *gate) pass() {
	g.mu.Lock()
	rel, ent := g.release, g.entered
	g.mu.Unlock() // lockscan: manual (tests are not scanned)
	if rel == nil {
		return
	}
	select {
	case <-ent:
	default:
		close(ent)
	}
	<-rel
}

func (g *gate) waitEntered(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	ent := g.entered
	g.mu.Unlock()
	select {
	case <-ent:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never reached the gate")
	}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func openT(t *testing.T, o Options) *Store {
	t.Helper()
	if o.Logger == nil {
		o.Logger, _ = newLog()
	}
	s := Open(o)
	t.Cleanup(func() { s.Close() })
	return s
}

// exec submits a terminal statement and waits for it.
func exec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if !s.Submit(Op{Terminal: true, Apply: func(tx *sql.Tx) error { _, err := tx.Exec(q, args...); return err }}) {
		t.Fatal("submit refused")
	}
	if err := s.Flush(ctxT(t)); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpen_MissingFileStartsEmptyWithOneWarn(t *testing.T) {
	dir := t.TempDir()
	log, buf := newLog()
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s := openT(t, Options{Dir: dir, Logger: log, Now: func() time.Time { return at }})
	if !s.On() {
		t.Fatal("store is off")
	}
	if buf.warns() != 1 || !strings.Contains(buf.String(), "begins now") {
		t.Fatalf("want one WARN saying the history begins now, log:\n%s", buf)
	}
	if !s.HistoryBeginsAt().Equal(at) {
		t.Fatalf("history_begins_at %v, want %v", s.HistoryBeginsAt(), at)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
}

func TestOpen_RestartKeepsRowsAndBeginning(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s := Open(Options{Dir: dir, Now: func() time.Time { return at }})
	exec(t, s, `CREATE TABLE t (x INTEGER)`)
	exec(t, s, `INSERT INTO t VALUES (1)`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	log, buf := newLog()
	s2 := openT(t, Options{Dir: dir, Logger: log, Now: func() time.Time { return at.Add(time.Hour) }})
	if count(t, s2, "t") != 1 {
		t.Fatal("row lost across a restart")
	}
	if !s2.HistoryBeginsAt().Equal(at) {
		t.Fatalf("history_begins_at moved to %v", s2.HistoryBeginsAt())
	}
	if buf.warns() != 0 {
		t.Fatalf("a healthy reopen warned:\n%s", buf)
	}
}

func TestOpen_DamagedFileIsMovedAsideAndStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	garbage := bytes.Repeat([]byte("not a database "), 400)
	if err := os.WriteFile(path, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	log, buf := newLog()
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s := openT(t, Options{Dir: dir, Logger: log, Now: func() time.Time { return at }})
	if !s.On() {
		t.Fatalf("store is off after a damaged file; log:\n%s", buf)
	}
	if buf.warns() != 1 || !strings.Contains(buf.String(), "damaged") {
		t.Fatalf("want one WARN about the damaged file, log:\n%s", buf)
	}
	aside := path + ".damaged-20261123T100000Z"
	if b, err := os.ReadFile(aside); err != nil || !bytes.Equal(b, garbage) {
		t.Fatalf("damaged file not kept aside unchanged: %v", err)
	}
	if s.Stats().MovedAside != aside || !s.HistoryBeginsAt().Equal(at) {
		t.Fatalf("stats %+v", s.Stats())
	}
	exec(t, s, `CREATE TABLE t (x INTEGER)`) // the new file works
}

// A file that opens but fails the integrity check (a torn page) is moved
// aside too.
func TestOpen_CorruptPagesAreMovedAside(t *testing.T) {
	dir := t.TempDir()
	s := Open(Options{Dir: dir})
	exec(t, s, `CREATE TABLE t (x TEXT)`)
	for i := 0; i < 50; i++ {
		exec(t, s, `INSERT INTO t VALUES (?)`, strings.Repeat("x", 2000))
	}
	exec(t, s, `CREATE INDEX t_x ON t (x)`)
	s.Close()
	path := filepath.Join(dir, FileName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 4096 * 3; i < len(b) && i < 4096*12; i++ { // scribble over pages past the header
		b[i] = 0xA5
	}
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	log, buf := newLog()
	s2 := openT(t, Options{Dir: dir, Logger: log})
	if !s2.On() || s2.Stats().MovedAside == "" {
		t.Fatalf("not moved aside: %+v\n%s", s2.Stats(), buf)
	}
	if buf.warns() != 1 {
		t.Fatalf("want one WARN:\n%s", buf)
	}
}

func TestOpen_UnusableFolderLeavesItOffAndSafe(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain-file")
	os.WriteFile(file, []byte("x"), 0o644)
	log, buf := newLog()
	s := openT(t, Options{Dir: file, Logger: log})
	if s.On() || buf.warns() != 1 {
		t.Fatalf("on=%v log:\n%s", s.On(), buf)
	}
	if s.Submit(Op{Apply: func(*sql.Tx) error { return nil }}) {
		t.Fatal("an off store accepted a write")
	}
	if err := s.Flush(ctxT(t)); err != nil || s.DB() != nil || !s.HistoryBeginsAt().IsZero() {
		t.Fatal("off store misbehaves")
	}
	var nilStore *Store
	if nilStore.On() || nilStore.Submit(Op{}) || nilStore.Close() != nil {
		t.Fatal("nil store misbehaves")
	}
	off := openT(t, Options{})
	if off.On() {
		t.Fatal("no folder, yet on")
	}
}

func TestOpen_NewerSchemaIsKeptAndOff(t *testing.T) {
	dir := t.TempDir()
	s := Open(Options{Dir: dir})
	exec(t, s, `INSERT INTO usage_migrations (version, name, applied_at) VALUES (?, 'future', 0)`, SchemaVersion()+5)
	s.Close()
	log, buf := newLog()
	s2 := openT(t, Options{Dir: dir, Logger: log})
	if s2.On() || s2.Stats().MovedAside != "" || buf.warns() != 1 || !strings.Contains(buf.String(), "newer scheduler") {
		t.Fatalf("on=%v stats=%+v log:\n%s", s2.On(), s2.Stats(), buf)
	}
}

// A full queue drops the oldest non-terminal operations, counts them, keeps
// every terminal one, and Submit never blocks while the writer is stuck.
func TestQueue_OverflowDropsOldestNonTerminalKeepsTerminal(t *testing.T) {
	gate := newGate()
	s := openT(t, Options{Dir: t.TempDir(), QueueSize: 10, BatchSize: 1000, BeforeBatch: gate.pass})
	exec0 := func(tag string, terminal bool) bool {
		return s.Submit(Op{Terminal: terminal, Apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO t VALUES (?)`, tag)
			return err
		}})
	}
	// The table first, then park the writer on a batch of its own.
	exec(t, s, `CREATE TABLE t (tag TEXT)`)
	release := gate.block()
	s.Submit(Op{Terminal: true, Apply: func(*sql.Tx) error { return nil }})
	gate.waitEntered(t)

	start := time.Now()
	for i := 0; i < 30; i++ {
		exec0("update", false)
		if i%3 == 0 {
			exec0("final", true)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Submit blocked for %v", d)
	}
	st := s.Stats()
	if st.Queued != 10 || st.Dropped != 30 || st.DroppedTerminal != 0 {
		t.Fatalf("stats %+v: want 10 queued, 30 dropped", st)
	}
	close(release)
	if err := s.Flush(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	var finals, updates int
	s.DB().QueryRow(`SELECT COUNT(*) FROM t WHERE tag='final'`).Scan(&finals)
	s.DB().QueryRow(`SELECT COUNT(*) FROM t WHERE tag='update'`).Scan(&updates)
	if finals != 10 || updates != 0 {
		t.Fatalf("finals %d updates %d: every terminal op must be kept", finals, updates)
	}
}

func TestQueue_TerminalOnlyQueueHasAHardCap(t *testing.T) {
	gate := newGate()
	s := openT(t, Options{Dir: t.TempDir(), QueueSize: 4, BeforeBatch: gate.pass})
	release := gate.block()
	defer close(release)
	s.Submit(Op{Terminal: true, Apply: func(*sql.Tx) error { return nil }})
	gate.waitEntered(t)
	for i := 0; i < 20; i++ {
		s.Submit(Op{Terminal: true, Apply: func(*sql.Tx) error { return nil }})
	}
	if st := s.Stats(); st.Queued != 8 || st.DroppedTerminal != 12 {
		t.Fatalf("stats %+v", st)
	}
}

func TestClose_WritesWhatIsQueued(t *testing.T) {
	dir := t.TempDir()
	s := Open(Options{Dir: dir})
	exec(t, s, `CREATE TABLE t (x INTEGER)`)
	for i := 0; i < 100; i++ {
		s.Submit(Op{Terminal: true, Apply: func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO t VALUES (1)`); return err }})
	}
	s.Close()
	s.Close()
	s2 := openT(t, Options{Dir: dir})
	if n := count(t, s2, "t"); n != 100 {
		t.Fatalf("%d rows after Close, want 100", n)
	}
}

func TestWriter_AFailingOpCountsAndOthersCommit(t *testing.T) {
	s := openT(t, Options{Dir: t.TempDir()})
	exec(t, s, `CREATE TABLE t (x INTEGER)`)
	s.Submit(Op{Terminal: true, Apply: func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO nope VALUES (1)`); return err }})
	exec(t, s, `INSERT INTO t VALUES (1)`)
	if st := s.Stats(); st.Failed != 1 || st.LastError == "" || count(t, s, "t") != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestTrim_DeletesInBatches(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	old := trimRules
	trimRules = []TrimRule{{Table: "t", Column: "at", Keep: func(r Retention) time.Duration { return r.KeepRequests }}}
	t.Cleanup(func() { trimRules = old })
	s := openT(t, Options{Dir: t.TempDir(), Now: func() time.Time { return now },
		Retention: Retention{KeepRequests: 24 * time.Hour, TrimBatch: 7, TrimEvery: time.Hour}})
	exec(t, s, `CREATE TABLE t (at INTEGER)`)
	for i := 0; i < 30; i++ {
		exec(t, s, `INSERT INTO t VALUES (?)`, now.Add(-48*time.Hour).UnixMilli())
	}
	for i := 0; i < 5; i++ {
		exec(t, s, `INSERT INTO t VALUES (?)`, now.Add(-time.Hour).UnixMilli())
	}
	b0 := s.Stats().Batches
	if n := s.Trim(ctxT(t)); n != 30 {
		t.Fatalf("trimmed %d, want 30", n)
	}
	if count(t, s, "t") != 5 {
		t.Fatal("kept rows wrong")
	}
	if b := s.Stats().Batches - b0; b < 5 { // 30 rows at 7 per batch: 5 transactions
		t.Fatalf("%d transactions: not trimmed in batches", b)
	}
}
