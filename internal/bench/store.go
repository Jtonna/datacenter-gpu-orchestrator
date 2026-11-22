package bench

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// DBFile is the database file name inside data_dir.
const DBFile = "scheduler.db"

// migrations are applied in order, once each, and recorded in the migrations
// table. Add to the end; never edit one that has shipped.
var migrations = []struct {
	name string
	sql  string
}{
	{"initial schema", `
CREATE TABLE benchmarks (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  source     TEXT NOT NULL CHECK (source IN ('config','api')),
  spec       TEXT NOT NULL CHECK (json_valid(spec)),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE suites (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  source     TEXT NOT NULL CHECK (source IN ('config','api')),
  spec       TEXT NOT NULL CHECK (json_valid(spec)),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE runs (
  id              TEXT PRIMARY KEY,
  suite_id        TEXT NOT NULL,
  suite_name      TEXT NOT NULL,
  state           TEXT NOT NULL,
  note            TEXT NOT NULL DEFAULT '',
  triggered_by    TEXT NOT NULL DEFAULT 'manual',
  baseline_run_id TEXT NOT NULL DEFAULT '',
  queued_at       INTEGER NOT NULL,
  started_at      INTEGER NOT NULL DEFAULT 0,
  finished_at     INTEGER NOT NULL DEFAULT 0,
  error           TEXT NOT NULL DEFAULT '',
  version         TEXT NOT NULL DEFAULT '',
  suite           TEXT NOT NULL CHECK (json_valid(suite)),
  summary         TEXT CHECK (summary IS NULL OR json_valid(summary))
);
CREATE INDEX runs_suite ON runs (suite_id, queued_at);
CREATE INDEX runs_state ON runs (state);
CREATE TABLE run_steps (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id       TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
  idx          INTEGER NOT NULL,
  grp          TEXT NOT NULL DEFAULT '',
  benchmark_id TEXT NOT NULL,
  name         TEXT NOT NULL DEFAULT '',
  profile      TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL,
  outcome      TEXT NOT NULL DEFAULT '',
  reason       TEXT NOT NULL DEFAULT '',
  http_status  INTEGER NOT NULL DEFAULT 0,
  log_file     TEXT NOT NULL DEFAULT '',
  started_at   INTEGER NOT NULL DEFAULT 0,
  finished_at  INTEGER NOT NULL DEFAULT 0,
  error        TEXT NOT NULL DEFAULT '',
  spec         TEXT CHECK (spec IS NULL OR json_valid(spec)),
  plan         TEXT CHECK (plan IS NULL OR json_valid(plan)),
  load         TEXT CHECK (load IS NULL OR json_valid(load)),
  stats        TEXT CHECK (stats IS NULL OR json_valid(stats)),
  expect       TEXT CHECK (expect IS NULL OR json_valid(expect)),
  comparison   TEXT CHECK (comparison IS NULL OR json_valid(comparison))
);
CREATE INDEX run_steps_run ON run_steps (run_id, idx);
CREATE INDEX run_steps_benchmark ON run_steps (benchmark_id);
CREATE TABLE requests (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  step_id       INTEGER NOT NULL REFERENCES run_steps (id) ON DELETE CASCADE,
  run_id        TEXT NOT NULL,
  seq           INTEGER NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  phase         TEXT NOT NULL CHECK (phase IN ('load','warmup','measured')),
  status        INTEGER NOT NULL DEFAULT 0,
  wall_ms       REAL NOT NULL DEFAULT 0,
  ttft_ms       REAL,
  prompt_n      INTEGER NOT NULL DEFAULT 0,
  prompt_ms     REAL NOT NULL DEFAULT 0,
  prompt_tps    REAL NOT NULL DEFAULT 0,
  predicted_n   INTEGER NOT NULL DEFAULT 0,
  predicted_ms  REAL NOT NULL DEFAULT 0,
  decode_tps    REAL NOT NULL DEFAULT 0,
  cache_n       INTEGER NOT NULL DEFAULT 0,
  timings       TEXT CHECK (timings IS NULL OR json_valid(timings)),
  output_sha256 TEXT NOT NULL DEFAULT '',
  output_head   TEXT NOT NULL DEFAULT '',
  deterministic INTEGER NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  started_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX requests_step ON requests (step_id, seq);
`},
	{"benchmark phases, run environment, aggregates", `
ALTER TABLE runs ADD COLUMN env TEXT CHECK (env IS NULL OR json_valid(env));
ALTER TABLE run_steps ADD COLUMN model_file TEXT NOT NULL DEFAULT '';
ALTER TABLE run_steps ADD COLUMN node_count INTEGER NOT NULL DEFAULT 0;
CREATE INDEX run_steps_model ON run_steps (model_file);
CREATE TABLE run_phases (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  step_id     INTEGER NOT NULL REFERENCES run_steps (id) ON DELETE CASCADE,
  run_id      TEXT NOT NULL,
  idx         INTEGER NOT NULL,
  type        TEXT NOT NULL,
  label       TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL,
  outcome     TEXT NOT NULL DEFAULT '',
  error       TEXT NOT NULL DEFAULT '',
  started_at  INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  result      TEXT CHECK (result IS NULL OR json_valid(result))
);
CREATE INDEX run_phases_step ON run_phases (step_id, idx);
CREATE TABLE requests_v2 (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  step_id       INTEGER NOT NULL REFERENCES run_steps (id) ON DELETE CASCADE,
  run_id        TEXT NOT NULL,
  seq           INTEGER NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  phase         TEXT NOT NULL CHECK (phase IN ('load','warmup','measured','conversation','hold','concurrent')),
  status        INTEGER NOT NULL DEFAULT 0,
  wall_ms       REAL NOT NULL DEFAULT 0,
  ttft_ms       REAL,
  prompt_n      INTEGER NOT NULL DEFAULT 0,
  prompt_ms     REAL NOT NULL DEFAULT 0,
  prompt_tps    REAL NOT NULL DEFAULT 0,
  predicted_n   INTEGER NOT NULL DEFAULT 0,
  predicted_ms  REAL NOT NULL DEFAULT 0,
  decode_tps    REAL NOT NULL DEFAULT 0,
  cache_n       INTEGER NOT NULL DEFAULT 0,
  timings       TEXT CHECK (timings IS NULL OR json_valid(timings)),
  output_sha256 TEXT NOT NULL DEFAULT '',
  output_head   TEXT NOT NULL DEFAULT '',
  deterministic INTEGER NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  started_at    INTEGER NOT NULL DEFAULT 0,
  phase_idx     INTEGER NOT NULL DEFAULT 0,
  turn          INTEGER NOT NULL DEFAULT 0,
  wait_ms       REAL
);
INSERT INTO requests_v2 (id, step_id, run_id, seq, name, phase, status, wall_ms, ttft_ms, prompt_n, prompt_ms, prompt_tps, predicted_n, predicted_ms, decode_tps,
  cache_n, timings, output_sha256, output_head, deterministic, error, started_at)
  SELECT id, step_id, run_id, seq, name, phase, status, wall_ms, ttft_ms, prompt_n, prompt_ms, prompt_tps, predicted_n, predicted_ms, decode_tps,
  cache_n, timings, output_sha256, output_head, deterministic, error, started_at FROM requests;
DROP TABLE requests;
ALTER TABLE requests_v2 RENAME TO requests;
CREATE INDEX requests_step ON requests (step_id, seq);
`},
	{"lowest available memory, roles and reload from cache per step", `
ALTER TABLE run_steps ADD COLUMN mem_low TEXT CHECK (mem_low IS NULL OR json_valid(mem_low));
ALTER TABLE run_steps ADD COLUMN roles TEXT CHECK (roles IS NULL OR json_valid(roles));
ALTER TABLE run_steps ADD COLUMN reload TEXT CHECK (reload IS NULL OR json_valid(reload));
`},
}

type job struct {
	fn   func(*sql.Tx) error
	done chan error
}

// Store is the SQLite database. Writes go through one goroutine (one
// transaction per job); reads use the connection pool (WAL lets them run
// beside the writer).
type Store struct {
	db   *sql.DB
	path string

	jobs   chan job
	mu     sync.RWMutex // guards closed against senders
	closed bool
	wg     sync.WaitGroup
}

// OpenStore creates dir if needed, opens dir/scheduler.db in WAL mode and
// applies the migrations. An error means the benchmark feature stays off.
func OpenStore(dir string) (*Store, error) { return openStore(dir, len(migrations)) }

// openStore applies at most upTo migrations (tests use it to build an older database).
func openStore(dir string, upTo int) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("no data directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, DBFile)
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path, jobs: make(chan job, 64)}
	if err := s.migrate(upTo); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

func (s *Store) migrate(upTo int) error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var have int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM migrations`).Scan(&have); err != nil {
		return err
	}
	if have > len(migrations) {
		return fmt.Errorf("the database is at schema version %d but this build knows %d: it was written by a newer scheduler", have, len(migrations))
	}
	for i := have; i < upTo && i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i].sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (%s): %w", i+1, migrations[i].name, err)
		}
		if _, err := tx.Exec(`INSERT INTO migrations (version, name, applied_at) VALUES (?,?,?)`, i+1, migrations[i].name, time.Now().UnixMilli()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion is the highest applied migration.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM migrations`).Scan(&v)
	return v, err
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

// SizeBytes is the size of the database file plus its WAL.
func (s *Store) SizeBytes() (int64, error) {
	var total int64
	for _, p := range []string{s.path, s.path + "-wal"} {
		if st, err := os.Stat(p); err == nil {
			total += st.Size()
		} else if p == s.path {
			return 0, err
		}
	}
	return total, nil
}

func (s *Store) writer() {
	defer s.wg.Done()
	for j := range s.jobs {
		j.done <- s.runJob(j.fn)
	}
}

func (s *Store) runJob(fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			err = fmt.Errorf("store: %v", r)
		}
	}()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// write runs fn in a transaction on the writer goroutine and waits for it.
func (s *Store) write(fn func(*sql.Tx) error) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return errors.New("store is closed")
	}
	j := job{fn: fn, done: make(chan error, 1)}
	s.jobs <- j
	s.mu.RUnlock()
	return <-j.done
}

// Close finishes queued writes and closes the database. Safe to call twice.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.jobs)
	s.mu.Unlock()
	s.wg.Wait()
	return s.db.Close()
}

// ---- benchmarks and suites ----

func scanItem(spec string, source string, created, updated int64, into any) error {
	switch v := into.(type) {
	case *StoredBenchmark:
		var b benchspec.Benchmark
		if err := json.Unmarshal([]byte(spec), &b); err != nil {
			return err
		}
		*v = StoredBenchmark{Spec: &b, Source: source, CreatedAt: created, UpdatedAt: updated}
	case *StoredSuite:
		var su benchspec.Suite
		if err := json.Unmarshal([]byte(spec), &su); err != nil {
			return err
		}
		*v = StoredSuite{Spec: &su, Source: source, CreatedAt: created, UpdatedAt: updated}
	}
	return nil
}

func (s *Store) saveItem(table, id, name string, spec any, source string, now int64, seed bool) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	q := `INSERT INTO ` + table + ` (id, name, source, spec, created_at, updated_at) VALUES (?,?,?,?,?,?)
	      ON CONFLICT(id) DO UPDATE SET name=excluded.name, source=excluded.source, spec=excluded.spec, updated_at=excluded.updated_at`
	if seed { // config seeding only ever updates what config itself seeded
		q += ` WHERE ` + table + `.source = 'config'`
	}
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(q, id, name, source, string(b), now, now)
		return err
	})
}

// SaveBenchmark inserts or replaces a benchmark created or edited through the API.
func (s *Store) SaveBenchmark(b *benchspec.Benchmark, source string, now int64) error {
	return s.saveItem("benchmarks", b.ID, b.Title(), b, source, now, false)
}

// SeedBenchmark upserts a benchmark from the config: it updates an item that
// config seeded and never touches one created through the API.
func (s *Store) SeedBenchmark(b *benchspec.Benchmark, now int64) error {
	return s.saveItem("benchmarks", b.ID, b.Title(), b, SourceConfig, now, true)
}

// SaveSuite inserts or replaces a suite created or edited through the API.
func (s *Store) SaveSuite(su *benchspec.Suite, source string, now int64) error {
	return s.saveItem("suites", su.ID, su.Title(), su, source, now, false)
}

// SeedSuite is SeedBenchmark for suites.
func (s *Store) SeedSuite(su *benchspec.Suite, now int64) error {
	return s.saveItem("suites", su.ID, su.Title(), su, SourceConfig, now, true)
}

// GetBenchmark returns one benchmark or ErrNotFound.
func (s *Store) GetBenchmark(id string) (*StoredBenchmark, error) {
	var spec, source string
	var c, u int64
	err := s.db.QueryRow(`SELECT spec, source, created_at, updated_at FROM benchmarks WHERE id=?`, id).Scan(&spec, &source, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out StoredBenchmark
	return &out, scanItem(spec, source, c, u, &out)
}

// ListBenchmarks returns every benchmark ordered by id.
func (s *Store) ListBenchmarks() ([]StoredBenchmark, error) {
	rows, err := s.db.Query(`SELECT spec, source, created_at, updated_at FROM benchmarks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredBenchmark
	for rows.Next() {
		var spec, source string
		var c, u int64
		if err := rows.Scan(&spec, &source, &c, &u); err != nil {
			return nil, err
		}
		var it StoredBenchmark
		if err := scanItem(spec, source, c, u, &it); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetSuite returns one suite or ErrNotFound.
func (s *Store) GetSuite(id string) (*StoredSuite, error) {
	var spec, source string
	var c, u int64
	err := s.db.QueryRow(`SELECT spec, source, created_at, updated_at FROM suites WHERE id=?`, id).Scan(&spec, &source, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out StoredSuite
	return &out, scanItem(spec, source, c, u, &out)
}

// ListSuites returns every suite ordered by id.
func (s *Store) ListSuites() ([]StoredSuite, error) {
	rows, err := s.db.Query(`SELECT spec, source, created_at, updated_at FROM suites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredSuite
	for rows.Next() {
		var spec, source string
		var c, u int64
		if err := rows.Scan(&spec, &source, &c, &u); err != nil {
			return nil, err
		}
		var it StoredSuite
		if err := scanItem(spec, source, c, u, &it); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) deleteItem(table, id string) error {
	var n int64
	err := s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM `+table+` WHERE id=?`, id)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteBenchmark removes a benchmark (runs that used it keep their snapshot).
func (s *Store) DeleteBenchmark(id string) error { return s.deleteItem("benchmarks", id) }

// DeleteSuite removes a suite (its runs stay).
func (s *Store) DeleteSuite(id string) error { return s.deleteItem("suites", id) }

// SuitesUsing lists the suites that name benchmark id.
func (s *Store) SuitesUsing(id string) ([]string, error) {
	all, err := s.ListSuites()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, su := range all {
		for _, it := range su.Spec.Benchmarks {
			if it.Inline == nil && it.Ref == id {
				out = append(out, su.Spec.ID)
				break
			}
		}
	}
	return out, nil
}

// BenchmarksUsing lists the stored parallel steps that name benchmark id.
func (s *Store) BenchmarksUsing(id string) ([]string, error) {
	all, err := s.ListBenchmarks()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range all {
		for _, m := range b.Spec.Parallel {
			if m == id {
				out = append(out, b.Spec.ID)
				break
			}
		}
	}
	return out, nil
}

// ---- runs ----

func nullJSON(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func rawOrNull(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// CreateRun inserts a run (normally queued).
func (s *Store) CreateRun(r *Run) error {
	sum, err := nullJSON(r.Summary)
	if err != nil {
		return err
	}
	suite := string(r.Suite)
	if suite == "" {
		suite = "{}"
	}
	trig := r.Trigger
	if trig == "" {
		trig = "manual"
	}
	var env any
	if r.Env != nil {
		var err error
		if env, err = nullJSON(r.Env); err != nil {
			return err
		}
	}
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO runs (id, suite_id, suite_name, state, note, triggered_by, baseline_run_id, queued_at, started_at, finished_at, error, version, suite, summary, env)
		  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.SuiteID, r.SuiteName, r.State, r.Note, trig, r.BaselineRunID, r.QueuedAt,
			r.StartedAt, r.FinishedAt, r.Error, r.Version, suite, sum, env)
		return err
	})
}

// SetRunState moves a run to a state. startedAt and finishedAt are written
// when above 0; errText replaces the run's error when not empty.
func (s *Store) SetRunState(id, state string, startedAt, finishedAt int64, errText string) error {
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE runs SET state=?, started_at=CASE WHEN ?>0 THEN ? ELSE started_at END,
		  finished_at=CASE WHEN ?>0 THEN ? ELSE finished_at END, error=CASE WHEN ?<>'' THEN ? ELSE error END WHERE id=?`,
			state, startedAt, startedAt, finishedAt, finishedAt, errText, errText, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetRunEnv stores the environment a run ran in.
func (s *Store) SetRunEnv(id string, env *Environment) error {
	var v any
	if env != nil {
		var err error
		if v, err = nullJSON(env); err != nil {
			return err
		}
	}
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE runs SET env=? WHERE id=?`, v, id)
		return err
	})
}

// SetRunSummary stores the step counts and the baseline used.
func (s *Store) SetRunSummary(id string, sum *RunSummary, baseline string) error {
	v, err := nullJSON(sum)
	if err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE runs SET summary=?, baseline_run_id=CASE WHEN ?<>'' THEN ? ELSE baseline_run_id END WHERE id=?`, v, baseline, baseline, id)
		return err
	})
}

const runCols = `id, suite_id, suite_name, state, note, triggered_by, baseline_run_id, queued_at, started_at, finished_at, error, version, suite, summary, env`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var suite string
	var sum, env sql.NullString
	if err := sc.Scan(&r.ID, &r.SuiteID, &r.SuiteName, &r.State, &r.Note, &r.Trigger, &r.BaselineRunID, &r.QueuedAt,
		&r.StartedAt, &r.FinishedAt, &r.Error, &r.Version, &suite, &sum, &env); err != nil {
		return nil, err
	}
	r.Suite = json.RawMessage(suite)
	unmarshalNull(env, &r.Env)
	if sum.Valid {
		var rs RunSummary
		if err := json.Unmarshal([]byte(sum.String), &rs); err == nil {
			r.Summary = &rs
		}
	}
	return &r, nil
}

// ListRuns lists runs newest first, without their steps.
func (s *Store) ListRuns(f RunFilter) ([]Run, error) {
	q := `SELECT ` + runCols + ` FROM runs WHERE 1=1`
	var args []any
	if f.Suite != "" {
		q += ` AND suite_id=?`
		args = append(args, f.Suite)
	}
	if f.State != "" {
		q += ` AND state=?`
		args = append(args, f.State)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q += ` ORDER BY queued_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		r.Suite = nil // the list stays small; GET one run for the snapshot
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetRun returns a run with its steps and their requests, or ErrNotFound.
func (s *Store) GetRun(id string) (*Run, error) {
	r, err := scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	steps, err := s.steps(id)
	if err != nil {
		return nil, err
	}
	r.Steps = steps
	return r, nil
}

// PreviousCompleted is the newest completed run of a suite other than
// exclude, started before it: the default baseline. nil when there is none.
func (s *Store) PreviousCompleted(suiteID, exclude string) (*Run, error) {
	r, err := scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE suite_id=? AND state='completed' AND id<>?
	  AND queued_at < COALESCE((SELECT queued_at FROM runs WHERE id=?), 9223372036854775807)
	  ORDER BY queued_at DESC, id DESC LIMIT 1`, suiteID, exclude, exclude))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	steps, err := s.steps(r.ID)
	if err != nil {
		return nil, err
	}
	r.Steps = steps
	return r, nil
}

// LastScheduledStart is when a suite's last scheduled run was queued (0 when none).
func (s *Store) LastScheduledStart(suiteID string) (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(queued_at) FROM runs WHERE suite_id=? AND triggered_by='schedule'`, suiteID).Scan(&v)
	return v.Int64, err
}

// RecoverInterrupted runs at start: a run that was running or paused when the
// scheduler stopped is marked failed (interrupted), a run that was only queued
// is cancelled. It returns how many runs changed.
func (s *Store) RecoverInterrupted(now int64) (int, error) {
	var n int
	err := s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE run_steps SET state='failed', outcome='error', error='interrupted', finished_at=?
		  WHERE state IN ('running','pending') AND run_id IN (SELECT id FROM runs WHERE state IN ('running','paused'))`, now); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE runs SET state='failed', error='failed (interrupted)', finished_at=? WHERE state IN ('running','paused')`, now)
		if err != nil {
			return err
		}
		a, _ := res.RowsAffected()
		res, err = tx.Exec(`UPDATE runs SET state='cancelled', error='the scheduler stopped before the run started', finished_at=? WHERE state='queued'`, now)
		if err != nil {
			return err
		}
		b, _ := res.RowsAffected()
		n = int(a + b)
		return nil
	})
	return n, err
}

// ---- steps and requests ----

// AddStep inserts a step and sets its ID.
func (s *Store) AddStep(st *Step) error {
	vals, err := stepJSON(st)
	if err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO run_steps (run_id, idx, grp, benchmark_id, name, profile, state, outcome, reason, http_status, log_file, started_at, finished_at, error, spec, plan, load, stats, expect, comparison, model_file, node_count, mem_low, roles, reload)
		  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, st.RunID, st.Idx, st.Group, st.BenchmarkID, st.Name, st.Profile, st.State, st.Outcome,
			st.Reason, st.HTTPStatus, st.LogFile, st.StartedAt, st.FinishedAt, st.Error, vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], st.ModelFile, st.NodeCount,
			vals[6], vals[7], vals[8])
		if err != nil {
			return err
		}
		st.ID, err = res.LastInsertId()
		return err
	})
}

func stepJSON(st *Step) ([9]any, error) {
	var out [9]any
	var err error
	if len(st.MemLow) > 0 {
		if out[6], err = nullJSON(st.MemLow); err != nil {
			return out, err
		}
	}
	if len(st.Roles) > 0 {
		if out[7], err = nullJSON(st.Roles); err != nil {
			return out, err
		}
	}
	if len(st.Reload) > 0 {
		if out[8], err = nullJSON(st.Reload); err != nil {
			return out, err
		}
	}
	if st.Spec != nil {
		if out[0], err = nullJSON(st.Spec); err != nil {
			return out, err
		}
	}
	out[1] = rawOrNull(st.Plan)
	if st.Load != nil {
		if out[2], err = nullJSON(st.Load); err != nil {
			return out, err
		}
	}
	if st.Stats != nil {
		if out[3], err = nullJSON(st.Stats); err != nil {
			return out, err
		}
	}
	if st.Expect != nil {
		if out[4], err = nullJSON(st.Expect); err != nil {
			return out, err
		}
	}
	if st.Comparison != nil {
		if out[5], err = nullJSON(st.Comparison); err != nil {
			return out, err
		}
	}
	return out, nil
}

// UpdateStep rewrites every column of a step that has an ID.
func (s *Store) UpdateStep(st *Step) error {
	vals, err := stepJSON(st)
	if err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE run_steps SET grp=?, name=?, profile=?, state=?, outcome=?, reason=?, http_status=?, log_file=?, started_at=?, finished_at=?, error=?,
		  spec=?, plan=?, load=?, stats=?, expect=?, comparison=?, model_file=?, node_count=?, mem_low=?, roles=?, reload=? WHERE id=?`, st.Group, st.Name, st.Profile, st.State, st.Outcome, st.Reason, st.HTTPStatus,
			st.LogFile, st.StartedAt, st.FinishedAt, st.Error, vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], st.ModelFile, st.NodeCount, vals[6], vals[7], vals[8], st.ID)
		return err
	})
}

// AddRequest inserts one request result and sets its ID.
func (s *Store) AddRequest(r *RequestResult) error {
	var ttft, wait any
	if r.TTFTMS != nil {
		ttft = *r.TTFTMS
	}
	if r.WaitMS != nil {
		wait = *r.WaitMS
	}
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO requests (step_id, run_id, seq, name, phase, status, wall_ms, ttft_ms, prompt_n, prompt_ms, prompt_tps, predicted_n, predicted_ms, decode_tps,
		  cache_n, timings, output_sha256, output_head, deterministic, error, started_at, phase_idx, turn, wait_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.StepID, r.RunID, r.Seq, r.Name, r.Phase, r.Status, r.WallMS, ttft, r.PromptN, r.PromptMS, r.PromptTPS, r.PredictedN, r.PredictedMS, r.DecodeTPS,
			r.CacheN, rawOrNull(r.Timings), r.OutputSHA256, r.OutputHead, r.Deterministic, r.Error, r.StartedAt, r.PhaseIdx, r.Turn, wait)
		if err != nil {
			return err
		}
		r.ID, err = res.LastInsertId()
		return err
	})
}

func (s *Store) steps(runID string) ([]Step, error) { return s.stepsOpt(runID, true) }

// stepsOpt loads a run's steps with their phases, and their request rows when requests is set.
func (s *Store) stepsOpt(runID string, requests bool) ([]Step, error) {
	rows, err := s.db.Query(`SELECT id, run_id, idx, grp, benchmark_id, name, profile, state, outcome, reason, http_status, log_file, started_at, finished_at, error,
	  spec, plan, load, stats, expect, comparison, model_file, node_count, mem_low, roles, reload FROM run_steps WHERE run_id=? ORDER BY idx, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		var spec, plan, load, stats, expect, cmp, memLow, roles, reload sql.NullString
		if err := rows.Scan(&st.ID, &st.RunID, &st.Idx, &st.Group, &st.BenchmarkID, &st.Name, &st.Profile, &st.State, &st.Outcome, &st.Reason,
			&st.HTTPStatus, &st.LogFile, &st.StartedAt, &st.FinishedAt, &st.Error, &spec, &plan, &load, &stats, &expect, &cmp, &st.ModelFile, &st.NodeCount,
			&memLow, &roles, &reload); err != nil {
			return nil, err
		}
		unmarshalSlice(memLow, &st.MemLow)
		unmarshalSlice(roles, &st.Roles)
		unmarshalSlice(reload, &st.Reload)
		unmarshalNull(spec, &st.Spec)
		if plan.Valid {
			st.Plan = json.RawMessage(plan.String)
		}
		unmarshalNull(load, &st.Load)
		unmarshalNull(stats, &st.Stats)
		unmarshalNull(expect, &st.Expect)
		unmarshalNull(cmp, &st.Comparison)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if requests {
			reqs, err := s.requests(out[i].ID)
			if err != nil {
				return nil, err
			}
			out[i].Requests = reqs
		}
		phases, err := s.phases(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Phases = phases
	}
	return out, nil
}

// unmarshalSlice reads a JSON array column; NULL (a row written before the
// column existed) leaves the slice nil.
func unmarshalSlice[T any](s sql.NullString, into *[]T) {
	if !s.Valid {
		return
	}
	var v []T
	if err := json.Unmarshal([]byte(s.String), &v); err == nil {
		*into = v
	}
}

func unmarshalNull[T any](s sql.NullString, into **T) {
	if !s.Valid {
		return
	}
	var v T
	if err := json.Unmarshal([]byte(s.String), &v); err == nil {
		*into = &v
	}
}

func (s *Store) requests(stepID int64) ([]RequestResult, error) {
	rows, err := s.db.Query(`SELECT id, step_id, run_id, seq, name, phase, status, wall_ms, ttft_ms, prompt_n, prompt_ms, prompt_tps, predicted_n, predicted_ms, decode_tps,
	  cache_n, timings, output_sha256, output_head, deterministic, error, started_at, phase_idx, turn, wait_ms FROM requests WHERE step_id=? ORDER BY seq, id`, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestResult
	for rows.Next() {
		var r RequestResult
		var ttft, wait sql.NullFloat64
		var timings sql.NullString
		if err := rows.Scan(&r.ID, &r.StepID, &r.RunID, &r.Seq, &r.Name, &r.Phase, &r.Status, &r.WallMS, &ttft, &r.PromptN, &r.PromptMS, &r.PromptTPS,
			&r.PredictedN, &r.PredictedMS, &r.DecodeTPS, &r.CacheN, &timings, &r.OutputSHA256, &r.OutputHead, &r.Deterministic, &r.Error, &r.StartedAt,
			&r.PhaseIdx, &r.Turn, &wait); err != nil {
			return nil, err
		}
		if wait.Valid {
			v := wait.Float64
			r.WaitMS = &v
		}
		if ttft.Valid {
			v := ttft.Float64
			r.TTFTMS = &v
		}
		if timings.Valid {
			r.Timings = json.RawMessage(timings.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- phases ----

func phaseBody(p *PhaseResult) (string, error) {
	b, err := json.Marshal(p)
	return string(b), err
}

// AddPhase inserts a phase result of a step and sets its ID.
func (s *Store) AddPhase(p *PhaseResult) error {
	body, err := phaseBody(p)
	if err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO run_phases (step_id, run_id, idx, type, label, state, outcome, error, started_at, finished_at, result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			p.StepID, p.RunID, p.Idx, p.Type, p.Label, p.State, p.Outcome, p.Error, p.StartedAt, p.FinishedAt, body)
		if err != nil {
			return err
		}
		p.ID, err = res.LastInsertId()
		return err
	})
}

// UpdatePhase rewrites a phase result that has an ID.
func (s *Store) UpdatePhase(p *PhaseResult) error {
	body, err := phaseBody(p)
	if err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE run_phases SET label=?, state=?, outcome=?, error=?, started_at=?, finished_at=?, result=? WHERE id=?`,
			p.Label, p.State, p.Outcome, p.Error, p.StartedAt, p.FinishedAt, body, p.ID)
		return err
	})
}

func (s *Store) phases(stepID int64) ([]PhaseResult, error) {
	rows, err := s.db.Query(`SELECT id, step_id, run_id, idx, type, label, state, outcome, error, started_at, finished_at, result FROM run_phases WHERE step_id=? ORDER BY idx, id`, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PhaseResult
	for rows.Next() {
		var p PhaseResult
		var body sql.NullString
		if err := rows.Scan(&p.ID, &p.StepID, &p.RunID, &p.Idx, &p.Type, &p.Label, &p.State, &p.Outcome, &p.Error, &p.StartedAt, &p.FinishedAt, &body); err != nil {
			return nil, err
		}
		if body.Valid {
			var full PhaseResult // the JSON holds the measurements; the columns are authoritative for the rest
			if json.Unmarshal([]byte(body.String), &full) == nil {
				full.ID, full.StepID, full.RunID, full.Idx, full.Type, full.Label = p.ID, p.StepID, p.RunID, p.Idx, p.Type, p.Label
				full.State, full.Outcome, full.Error, full.StartedAt, full.FinishedAt = p.State, p.Outcome, p.Error, p.StartedAt, p.FinishedAt
				p = full
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- trends ----

// TrendMetrics are the metrics /api/bench/trends accepts.
var TrendMetrics = []string{"load_s", "load_mb_s", "memory_mib", "decode_tps", "prefill_tps", "ttft_ms", "unload_s", "concurrent_wall_s", "lowest_available_mib", "lowest_available_gpu_mib"}

// Trend returns up to limit points of a metric for a benchmark over past
// runs, oldest first (the newest limit points).
func (s *Store) Trend(benchmark, metric string, limit int) ([]TrendPoint, error) {
	var expr string
	switch metric {
	case "load_s":
		expr = `json_extract(s.load,'$.load_s')`
	case "decode_tps":
		expr = `json_extract(s.stats,'$.decode_tps.mean')`
	case "prefill_tps":
		expr = `json_extract(s.stats,'$.prefill_tps.mean')`
	case "ttft_ms":
		expr = `json_extract(s.stats,'$.ttft_ms.mean')`
	case "load_mb_s":
		expr = `json_extract(s.load,'$.avg_mb_s')`
	case "memory_mib":
		expr = `json_extract(s.load,'$.memory_mib')`
	case "unload_s": // from the phase results
		expr = `(SELECT json_extract(p.result,'$.unload.unload_s') FROM run_phases p WHERE p.step_id=s.id AND p.type='unload' AND p.state='done' ORDER BY p.idx LIMIT 1)`
	case "concurrent_wall_s":
		expr = `(SELECT json_extract(p.result,'$.concurrent.wall_ms')/1000.0 FROM run_phases p WHERE p.step_id=s.id AND p.type='concurrent_requests' AND p.state='done' ORDER BY p.idx LIMIT 1)`
	case "lowest_available_mib": // the lowest system-memory node of the step (no kind: system); NULL for a step stored before
		expr = `(SELECT MIN(json_extract(m.value,'$.step_mib')) FROM json_each(s.mem_low) m WHERE COALESCE(json_extract(m.value,'$.kind'),'system')='system')`
	case "lowest_available_gpu_mib": // the lowest free GPU memory of a dedicated-GPU node of the step
		expr = `(SELECT MIN(json_extract(m.value,'$.step_mib')) FROM json_each(s.mem_low) m WHERE json_extract(m.value,'$.kind')='gpu')`
	default:
		return nil, fmt.Errorf("metric must be one of %s", strings.Join(TrendMetrics, ", "))
	}
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT s.run_id, CASE WHEN r.started_at>0 THEN r.started_at ELSE r.queued_at END, `+expr+`
	  FROM run_steps s JOIN runs r ON r.id=s.run_id
	  WHERE s.benchmark_id=? AND s.outcome='ready' AND r.state IN ('completed','failed') AND `+expr+` IS NOT NULL
	  ORDER BY r.queued_at DESC, s.id DESC LIMIT ?`, benchmark, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrendPoint
	for rows.Next() {
		var p TrendPoint
		if err := rows.Scan(&p.RunID, &p.StartedAt, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
