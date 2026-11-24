package cfgstore

import (
	"context"
	"database/sql"
	"fmt"
)

// The models page's tables (pins now; the trash and upload jobs later) have
// their own ledger, model_migrations, never cfg_migrations: an older image
// refuses a database whose cfg_migrations is ahead of it, and it never reads
// these tables, so going back to it stays a plain image swap (its catalog
// shows no pins; they come back with this image).
var modelMigrations = []string{`
CREATE TABLE model_pins (
  key       TEXT PRIMARY KEY,
  pinned_at INTEGER NOT NULL
) WITHOUT ROWID;
`}

// ModelVersion is the version of the models page's tables this build writes.
func ModelVersion() int { return len(modelMigrations) }

func migrateModels(tx *sql.Tx, now int64) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var have int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM model_migrations`).Scan(&have); err != nil {
		return err
	}
	if have > len(modelMigrations) {
		return nil // a newer image wrote them: the pins still read, the rest is left alone
	}
	for i := have; i < len(modelMigrations); i++ {
		if _, err := tx.Exec(modelMigrations[i]); err != nil {
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
