// Package cfgstore keeps the scheduler configuration revisions and the audit
// log in the scheduler database. It shares the connection and the single
// writer of the benchmark store; its own tables are versioned separately.
package cfgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// DB is what the store needs from the shared database.
type DB interface {
	DB() *sql.DB
	Write(fn func(*sql.Tx) error) error
}

var migrations = []string{`
CREATE TABLE config_revisions (
  revision INTEGER PRIMARY KEY,
  ts       INTEGER NOT NULL,
  source   TEXT NOT NULL,
  actor    TEXT NOT NULL,
  comment  TEXT NOT NULL DEFAULT '',
  doc      TEXT NOT NULL CHECK (json_valid(doc))
);
CREATE TABLE audit_log (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  ts          INTEGER NOT NULL,
  source      TEXT NOT NULL,
  actor       TEXT NOT NULL,
  remote_addr TEXT NOT NULL DEFAULT '',
  user_agent  TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL,
  section     TEXT NOT NULL,
  target      TEXT NOT NULL DEFAULT '',
  before      TEXT,
  after       TEXT,
  diff        TEXT NOT NULL DEFAULT '[]',
  revision    INTEGER NOT NULL DEFAULT 0,
  comment     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_ts ON audit_log (ts);
CREATE INDEX audit_section ON audit_log (section, target);
CREATE INDEX audit_revision ON audit_log (revision);
CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log BEGIN SELECT RAISE(ABORT, 'the audit log is append-only'); END;
`, `
CREATE TABLE agents (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,
  description    TEXT NOT NULL DEFAULT '',
  key_hash       TEXT NOT NULL,
  key_prefix     TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  created_source TEXT NOT NULL DEFAULT '',
  created_actor  TEXT NOT NULL DEFAULT '',
  rotated_at     INTEGER NOT NULL DEFAULT 0,
  last_used_at   INTEGER NOT NULL DEFAULT 0,
  revoked_at     INTEGER NOT NULL DEFAULT 0,
  request_count  INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX agents_key ON agents (key_hash);
ALTER TABLE audit_log ADD COLUMN agent_id TEXT NOT NULL DEFAULT '';
CREATE INDEX audit_agent ON audit_log (agent_id);
CREATE TABLE model_icons (
  key          TEXT PRIMARY KEY,
  content_type TEXT NOT NULL,
  data         BLOB NOT NULL,
  etag         TEXT NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE TABLE model_file_meta (
  path    TEXT PRIMARY KEY,
  size    INTEGER NOT NULL,
  mtime   INTEGER NOT NULL,
  meta    TEXT NOT NULL CHECK (json_valid(meta)),
  read_at INTEGER NOT NULL
);
`, `
CREATE TABLE node_admin (
  id         TEXT PRIMARY KEY,
  paused     INTEGER NOT NULL DEFAULT 0,
  removed    INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
`}

// Store is the revision and audit store.
type Store struct {
	db  DB
	now func() time.Time
}

// Open applies the migrations and returns the store.
func Open(db DB) (*Store, error) {
	s := &Store{db: db, now: time.Now}
	err := db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS cfg_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
			return err
		}
		var have int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM cfg_migrations`).Scan(&have); err != nil {
			return err
		}
		if have > len(migrations) {
			return fmt.Errorf("the configuration tables are at version %d but this build knows %d", have, len(migrations))
		}
		for i := have; i < len(migrations); i++ {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return fmt.Errorf("config migration %d: %w", i+1, err)
			}
			if _, err := tx.Exec(`INSERT INTO cfg_migrations (version, applied_at) VALUES (?,?)`, i+1, s.now().UnixMilli()); err != nil {
				return err
			}
		}
		return migrateAccess(tx, s.now().UnixMilli()) // access.go: its own ledger
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Revision is a stored configuration revision.
type Revision struct {
	Revision int64
	TS       int64
	Source   string
	Actor    string
	Comment  string
	Doc      *config.Document
	// Unknown names the fields of the stored document this version does
	// not know (a newer version wrote them): they are not in Doc
	// (config.ParseStoredDocument).
	Unknown []string
}

// ErrStale reports that the current revision is not the one the caller expected.
type ErrStale struct{ Current int64 }

func (e *ErrStale) Error() string {
	return fmt.Sprintf("the configuration is at revision %d", e.Current)
}

// ErrNotEmpty is returned when a change requires an empty database.
var ErrNotEmpty = errors.New("the database already holds a configuration")

// ErrNotFound is returned for an unknown revision or audit id.
var ErrNotFound = errors.New("not found")

// Latest returns the newest revision; revision 0 with an empty document when there is none.
func (s *Store) Latest(ctx context.Context) (*Revision, error) {
	return s.revision(ctx, 0)
}

// Get returns revision n (0 means the latest).
func (s *Store) Get(ctx context.Context, n int64) (*Revision, error) {
	r, err := s.revision(ctx, n)
	if err == nil && n > 0 && r.Revision == 0 {
		return nil, ErrNotFound
	}
	return r, err
}

func (s *Store) revision(ctx context.Context, n int64) (*Revision, error) {
	q := `SELECT revision, ts, source, actor, comment, doc FROM config_revisions `
	args := []any{}
	if n > 0 {
		q += `WHERE revision = ?`
		args = append(args, n)
	} else {
		q += `ORDER BY revision DESC LIMIT 1`
	}
	var r Revision
	var doc string
	err := s.db.DB().QueryRowContext(ctx, q, args...).Scan(&r.Revision, &r.TS, &r.Source, &r.Actor, &r.Comment, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return &Revision{Doc: config.NewDocument()}, nil
	}
	if err != nil {
		return nil, err
	}
	d, unknown, err := config.ParseStoredDocument([]byte(doc))
	if err != nil {
		return nil, fmt.Errorf("revision %d: %w", r.Revision, err)
	}
	r.Doc, r.Unknown = d, unknown
	return &r, nil
}

// Revisions lists revisions newest first, without their documents.
func (s *Store) Revisions(ctx context.Context, limit int) ([]Revision, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := s.db.DB().QueryContext(ctx, `SELECT revision, ts, source, actor, comment FROM config_revisions ORDER BY revision DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.Revision, &r.TS, &r.Source, &r.Actor, &r.Comment); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Change is one configuration commit: the new document and its audit entries.
type Change struct {
	Doc     *config.Document
	Origin  audit.Origin
	Entries []audit.Entry // Revision, TS and origin are filled in
	// IfMatch, when not nil, must equal the current revision.
	IfMatch *int64
	// RequireEmpty refuses the change when a revision already exists.
	RequireEmpty bool
}

// Commit writes the next revision and its audit entries in one transaction.
func (s *Store) Commit(ctx context.Context, ch Change) (rev int64, ids []int64, err error) {
	doc, err := json.Marshal(ch.Doc)
	if err != nil {
		return 0, nil, err
	}
	ts := s.now().UnixMilli()
	err = s.db.Write(func(tx *sql.Tx) error {
		var cur int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(revision),0) FROM config_revisions`).Scan(&cur); err != nil {
			return err
		}
		if ch.RequireEmpty && cur != 0 {
			return ErrNotEmpty
		}
		if ch.IfMatch != nil && *ch.IfMatch != cur {
			return &ErrStale{Current: cur}
		}
		rev = cur + 1
		if _, err := tx.Exec(`INSERT INTO config_revisions (revision, ts, source, actor, comment, doc) VALUES (?,?,?,?,?,?)`,
			rev, ts, ch.Origin.Source, ch.Origin.Actor, ch.Origin.Comment, string(doc)); err != nil {
			return err
		}
		ids = nil
		for _, e := range ch.Entries {
			e.Revision, e.TS = rev, ts
			ch.Origin.Apply(&e)
			id, err := insertEntry(tx, e)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return rev, ids, nil
}

func nullable(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return string(b)
}

func insertEntry(tx *sql.Tx, e audit.Entry) (int64, error) {
	diff, _ := json.Marshal(e.Diff)
	if e.Diff == nil {
		diff = []byte("[]")
	}
	res, err := tx.Exec(`INSERT INTO audit_log (ts, source, actor, remote_addr, user_agent, action, section, target, before, after, diff, revision, comment, agent_id)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.TS, e.Source, e.Actor, e.RemoteAddr, e.UserAgent, e.Action, e.Section, e.Target,
		nullable(e.Before), nullable(e.After), string(diff), e.Revision, e.Comment, e.AgentID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Record appends an entry that created no configuration revision (it implements audit.Recorder).
func (s *Store) Record(ctx context.Context, e audit.Entry) (int64, error) {
	if e.TS == 0 {
		e.TS = s.now().UnixMilli()
	}
	if e.Source == "" {
		e.Source = audit.SourceSystem
	}
	if e.Actor == "" {
		e.Actor = audit.DefaultActor
	}
	var id int64
	err := s.db.Write(func(tx *sql.Tx) (err error) { id, err = insertEntry(tx, e); return })
	return id, err
}

// Filter selects audit entries.
type Filter struct {
	Since, Until                                  int64 // unix ms, inclusive; 0 = unbounded
	Source, Actor, Section, Target, Action, Query string
	Agent                                         string // an agent id (audit_log.agent_id)
	Limit                                         int
	Ascending                                     bool
}

// Query returns the matching entries and how many match in total.
func (s *Store) Query(ctx context.Context, f Filter) ([]audit.Entry, int, error) {
	var where []string
	var args []any
	add := func(cond string, v any) { where = append(where, cond); args = append(args, v) }
	if f.Since > 0 {
		add("ts >= ?", f.Since)
	}
	if f.Until > 0 {
		add("ts <= ?", f.Until)
	}
	for col, v := range map[string]string{"source": f.Source, "actor": f.Actor, "section": f.Section, "target": f.Target, "action": f.Action, "agent_id": f.Agent} {
		if v != "" {
			add(col+" = ?", v)
		}
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		where = append(where, `(comment LIKE ? ESCAPE '\' OR target LIKE ? ESCAPE '\' OR actor LIKE ? ESCAPE '\' OR section LIKE ? ESCAPE '\' OR before LIKE ? ESCAPE '\' OR after LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like, like, like)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	order := "DESC"
	if f.Ascending {
		order = "ASC"
	}
	rows, err := s.db.DB().QueryContext(ctx, `SELECT id, ts, source, actor, remote_addr, user_agent, action, section, target, before, after, diff, revision, comment, agent_id
	  FROM audit_log`+w+` ORDER BY id `+order+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []audit.Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func scanEntry(r interface{ Scan(...any) error }) (audit.Entry, error) {
	var e audit.Entry
	var before, after sql.NullString
	var diff string
	if err := r.Scan(&e.ID, &e.TS, &e.Source, &e.Actor, &e.RemoteAddr, &e.UserAgent, &e.Action, &e.Section, &e.Target, &before, &after, &diff, &e.Revision, &e.Comment, &e.AgentID); err != nil {
		return e, err
	}
	e.Before, e.After = json.RawMessage("null"), json.RawMessage("null")
	if before.Valid {
		e.Before = json.RawMessage(before.String)
	}
	if after.Valid {
		e.After = json.RawMessage(after.String)
	}
	_ = json.Unmarshal([]byte(diff), &e.Diff)
	if e.Diff == nil {
		e.Diff = []audit.DiffItem{}
	}
	return e, nil
}

// GetEntry returns one audit entry.
func (s *Store) GetEntry(ctx context.Context, id int64) (*audit.Entry, error) {
	row := s.db.DB().QueryRowContext(ctx, `SELECT id, ts, source, actor, remote_addr, user_agent, action, section, target, before, after, diff, revision, comment, agent_id FROM audit_log WHERE id = ?`, id)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// PruneAudit deletes entries older than days. It is the only delete in the
// application and runs only when the audit_retention_days setting is above 0.
func (s *Store) PruneAudit(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cut := s.now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	var n int64
	err := s.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM audit_log WHERE ts < ?`, cut)
		if err == nil {
			n, _ = res.RowsAffected()
		}
		return err
	})
	return n, err
}
