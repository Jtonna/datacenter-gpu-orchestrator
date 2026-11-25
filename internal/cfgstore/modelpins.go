package cfgstore

import (
	"context"
	"database/sql"
	"fmt"
)

// The models page's tables (v1 pins, v2 the trash, v3 the tombstones of
// files removed for good; upload jobs later) have
// their own ledger, model_migrations, never cfg_migrations: an older image
// refuses a database whose cfg_migrations is ahead of it, and it never reads
// these tables, so going back to it stays a plain image swap (its catalog
// shows no pins; they come back with this image).
var modelMigrations = []string{`
CREATE TABLE model_pins (
  key       TEXT PRIMARY KEY,
  pinned_at INTEGER NOT NULL
) WITHOUT ROWID;
`, `
CREATE TABLE model_trash (
  id               TEXT PRIMARY KEY,
  file             TEXT NOT NULL,
  size_bytes       INTEGER NOT NULL,
  deleted_at       INTEGER NOT NULL,
  purge_at         INTEGER NOT NULL,
  state            TEXT NOT NULL,
  profiles         TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(profiles)),
  restore_profiles TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(restore_profiles)),
  revision         INTEGER NOT NULL DEFAULT 0,
  actor            TEXT NOT NULL DEFAULT '',
  comment          TEXT NOT NULL DEFAULT '',
  problem          TEXT NOT NULL DEFAULT '',
  updated_at       INTEGER NOT NULL
);
CREATE INDEX model_trash_file ON model_trash (file);
`, `
CREATE TABLE model_trash_removed (
  id              TEXT PRIMARY KEY,
  file            TEXT NOT NULL,
  size_bytes      INTEGER NOT NULL,
  deleted_at      INTEGER NOT NULL,
  purge_at        INTEGER NOT NULL,
  removed_at      INTEGER NOT NULL DEFAULT 0,
  state           TEXT NOT NULL,
  reason          TEXT NOT NULL,
  profile_ids     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(profile_ids)),
  revision        INTEGER NOT NULL DEFAULT 0,
  deleted_by      TEXT NOT NULL DEFAULT '',
  deleted_comment TEXT NOT NULL DEFAULT '',
  removed_by      TEXT NOT NULL DEFAULT '',
  comment         TEXT NOT NULL DEFAULT '',
  updated_at      INTEGER NOT NULL,
  audited         INTEGER NOT NULL DEFAULT 0 -- 1 once the audit entry and the modelRemoved event are written
);
CREATE INDEX model_trash_removed_file ON model_trash_removed (file);
`}

// ModelVersion is the version of the models page's tables this build writes.
func ModelVersion() int { return len(modelMigrations) }

func migrateModels(tx *sql.Tx, now int64) error { return migrateModelsList(tx, now, modelMigrations) }

// migrateModelsList applies list (an older image's list in the way-back
// tests). A ledger ahead of list is left alone.
func migrateModelsList(tx *sql.Tx, now int64, list []string) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var have int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM model_migrations`).Scan(&have); err != nil {
		return err
	}
	if have > len(list) {
		return nil // a newer image wrote them: the pins still read, the rest is left alone
	}
	for i := have; i < len(list); i++ {
		if _, err := tx.Exec(list[i]); err != nil {
			return fmt.Errorf("model migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO model_migrations (version, applied_at) VALUES (?,?)`, i+1, now); err != nil {
			return err
		}
	}
	return nil
}

// ModelPins returns catalog key -> pinned_at (unix ms) of every pinned model.
func (s *Store) ModelPins(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.DB().QueryContext(ctx, `SELECT key, pinned_at FROM model_pins`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var at int64
		if err := rows.Scan(&k, &at); err != nil {
			return nil, err
		}
		out[k] = at
	}
	return out, rows.Err()
}

// PinModel pins a model by its catalog key. It reports the time it is
// pinned since, and whether it was pinned already (then nothing changed).
func (s *Store) PinModel(key string, at int64) (since int64, already bool, err error) {
	err = s.db.Write(func(tx *sql.Tx) error {
		switch e := tx.QueryRow(`SELECT pinned_at FROM model_pins WHERE key = ?`, key).Scan(&since); {
		case e == nil:
			already = true
			return nil
		case e != sql.ErrNoRows:
			return e
		}
		since = at
		_, e := tx.Exec(`INSERT INTO model_pins (key, pinned_at) VALUES (?,?)`, key, at)
		return e
	})
	return since, already, err
}

// UnpinModel removes a pin; ErrNotFound when the model was not pinned.
func (s *Store) UnpinModel(key string) error {
	return s.execOne(`DELETE FROM model_pins WHERE key = ?`, key)
}
