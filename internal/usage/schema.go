package usage

import (
	"database/sql"
	"fmt"
	"time"
)

// Migration is one forward step of the usage file's schema. Add to the end
// of migrations (another feature's tables go in their own entries); never
// edit one that has shipped.
type Migration struct {
	Name string
	SQL  string
}

// migrations of usage.db, applied in order, once each, in their own
// transaction. Version n is migrations[n-1]. Keep this the one list (no
// init-time appends: their order would follow file names).
var migrations = []Migration{
	{Name: "meta", SQL: `
CREATE TABLE usage_meta (
  key   TEXT PRIMARY KEY,
  value INTEGER NOT NULL
);`},
	{Name: "requests", SQL: requestsSQL},
}

// NewerSchemaError: a newer scheduler migrated the file. It is kept as it
// is and the store stays off (going back to an older image never loses the
// newer history).
type NewerSchemaError struct{ Have, Know int }

func (e *NewerSchemaError) Error() string {
	return fmt.Sprintf("usage.db is at schema version %d but this scheduler knows %d (a newer scheduler wrote it)", e.Have, e.Know)
}

// SchemaVersion is the number of migrations this build knows.
func SchemaVersion() int { return len(migrations) }

func migrate(db *sql.DB, now time.Time) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS usage_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var have int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM usage_migrations`).Scan(&have); err != nil {
		return err
	}
	if have > len(migrations) {
		return &NewerSchemaError{Have: have, Know: len(migrations)}
	}
	for i := have; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i].SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("usage migration %d (%s): %w", i+1, migrations[i].Name, err)
		}
		if _, err := tx.Exec(`INSERT INTO usage_migrations (version, name, applied_at) VALUES (?,?,?)`, i+1, migrations[i].Name, now.UnixMilli()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO usage_meta (key, value) VALUES ('history_begins_at', ?)`, now.UnixMilli())
	return err
}
