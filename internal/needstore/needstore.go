// Package needstore keeps the memory needs the scheduler learned from loads
// in the scheduler database, so they survive a restart (design 7.2a).
//
// One row per shape of load (ShapeKey), so profiles of the same shape share
// one measurement and deleting or renaming a profile loses nothing. The row
// says what was measured (the total and the per-node figures) and what it
// was measured with: the model file and its size, the settings that change
// the need, the placement and split, when, on which release, and the
// profile that measured it (for display). The scheduler decides whether a
// row describes a profile; this package only stores and reads rows.
package needstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// DB is what the store needs from the scheduler database (bench.Store): the
// shared connection and its single writer. These tables are versioned on
// their own, as the start-up tables are.
type DB interface {
	DB() *sql.DB
	Write(fn func(*sql.Tx) error) error
}

// RecordSchema is the version of the record this build writes and reads. A
// row with another version is skipped (and replaced by the next measurement).
const RecordSchema = 1

// Causes of a stored figure.
const (
	CauseLoad = "load" // measured by a load that became ready
	// a watchdog or OOM kill raised it: the cause is the exit reason
)

// Node is one node's figure in the measured load, with its share of the split.
// PeakMiB is the highest memory the node was seen to hold for the profile
// while it stayed loaded (the context fills after the load ends, so it can
// be above MiB); 0 when none was recorded. A row written before peaks were
// kept has none: it reads as it was.
type Node struct {
	ID       string  `json:"node"`
	Fraction float64 `json:"fraction"`
	MiB      int     `json:"mib"`
	PeakMiB  int     `json:"peak_mib,omitempty"`
}

// Record is one stored measurement.
type Record struct {
	Schema  int    `json:"schema"`
	Profile string `json:"profile"`

	// What the need was measured with. A change to any of these makes the
	// record stale (the scheduler drops it).
	Kind       string   `json:"kind"`
	Model      string   `json:"model"`
	ModelBytes int64    `json:"model_bytes,omitempty"` // 0: not known
	Ctx        int      `json:"ctx"`
	Parallel   int      `json:"parallel"`
	KVQuant    string   `json:"kv_quant"`
	FlashAttn  bool     `json:"flash_attn"`
	ExtraArgs  []string `json:"extra_args,omitempty"`

	TotalMiB  int       `json:"total_mib"`
	Placement []string  `json:"placement,omitempty"` // node ids in device order
	Split     []float64 `json:"split,omitempty"`     // each node's share of the split
	Nodes     []Node    `json:"nodes,omitempty"`     // empty when the load log did not account for every node

	Cause      string    `json:"cause"`
	MeasuredAt time.Time `json:"measured_at"`
	Release    string    `json:"release"`
	// PeakAt is when the highest per-node peak was last raised; zero when
	// no peak is stored. The peaks live in Nodes, with the same placement
	// and split, and are dropped with the row.
	PeakAt time.Time `json:"peak_at,omitzero"`
}

// ShapeKey is the key of a stored measurement: what the memory need depends
// on besides the model file's content. An empty KV cache type is f16 and
// fewer than one slot is one, as the scheduler reads them. Placement (the
// member nodes and the split) is not in it: the total does not depend on it,
// and the per-node figures inside the record are only used for the same
// node set and split.
func ShapeKey(kind, model string, ctx, parallel int, kvQuant string, flashAttn bool, extraArgs []string) string {
	if kvQuant == "" {
		kvQuant = "f16"
	}
	if extraArgs == nil {
		extraArgs = []string{}
	}
	b, _ := json.Marshal([]any{kind, model, ctx, max(parallel, 1), kvQuant, flashAttn, extraArgs})
	return string(b)
}

// Shape is the record's ShapeKey.
func (r Record) Shape() string {
	return ShapeKey(r.Kind, r.Model, r.Ctx, r.Parallel, r.KVQuant, r.FlashAttn, r.ExtraArgs)
}

// Skipped is a row All could not use, and why: Profile is the profile the
// row names.
type Skipped struct {
	Profile string
	Reason  string
}

var migrations = []func(*sql.Tx) error{
	execMigration(`
CREATE TABLE learned_needs (
  profile    TEXT PRIMARY KEY,
  updated_at INTEGER NOT NULL,
  record     TEXT NOT NULL
);
`),
	migrateToShapes,
}

func execMigration(q string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error { _, err := tx.Exec(q); return err }
}

// migrateToShapes (version 2) keys the rows by shape: every usable row of
// learned_needs moves to needs_by_shape (when two profiles had the same
// shape, the newer row wins); a row that cannot be read or keyed stays
// under its profile in learned_needs, untouched.
func migrateToShapes(tx *sql.Tx) error {
	if _, err := tx.Exec(`
CREATE TABLE needs_by_shape (
  shape      TEXT PRIMARY KEY,
  profile    TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  record     TEXT NOT NULL
);`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT profile, updated_at, record FROM learned_needs ORDER BY updated_at, profile`)
	if err != nil {
		return err
	}
	type row struct {
		profile, doc string
		at           int64
		r            Record
	}
	var move []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.profile, &x.at, &x.doc); err != nil {
			rows.Close()
			return err
		}
		if usable(x.profile, x.doc, &x.r) == "" {
			move = append(move, x)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, x := range move { // oldest first: a newer row of the same shape replaces it
		if _, err := tx.Exec(`INSERT INTO needs_by_shape (shape, profile, updated_at, record) VALUES (?,?,?,?)
			ON CONFLICT(shape) DO UPDATE SET profile = excluded.profile, updated_at = excluded.updated_at, record = excluded.record`,
			x.r.Shape(), x.profile, x.at, x.doc); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM learned_needs WHERE profile = ?`, x.profile); err != nil {
			return err
		}
	}
	return nil
}

// usable reads doc into r and says why it cannot be used, or "".
func usable(profile, doc string, r *Record) string {
	switch err := json.Unmarshal([]byte(doc), r); {
	case err != nil:
		return "unreadable: " + err.Error()
	case r.Schema != RecordSchema:
		return fmt.Sprintf("record version %d, this build reads %d", r.Schema, RecordSchema)
	case r.Profile != profile || r.TotalMiB <= 0 || r.Kind == "" || r.Model == "":
		return "no usable figure"
	}
	return ""
}

// Store keeps the learned needs in the scheduler database.
type Store struct {
	db DB
}

// Open applies the migrations and returns the store.
func Open(db DB) (*Store, error) {
	err := db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS needs_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
			return err
		}
		var have int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM needs_migrations`).Scan(&have); err != nil {
			return err
		}
		if have > len(migrations) {
			return fmt.Errorf("the learned-need tables are at version %d but this build knows %d", have, len(migrations))
		}
		for i := have; i < len(migrations); i++ {
			if err := migrations[i](tx); err != nil {
				return fmt.Errorf("learned-need migration %d: %w", i+1, err)
			}
			if _, err := tx.Exec(`INSERT INTO needs_migrations (version, applied_at) VALUES (?,?)`, i+1, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Put stores r as its shape's row, replacing the one there (r.Profile is
// the profile that measured it, kept for display).
func (s *Store) Put(r Record) error {
	r.Schema = RecordSchema
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO needs_by_shape (shape, profile, updated_at, record) VALUES (?,?,?,?)
			ON CONFLICT(shape) DO UPDATE SET profile = excluded.profile, updated_at = excluded.updated_at, record = excluded.record`,
			r.Shape(), r.Profile, time.Now().UnixMilli(), string(b))
		return err
	})
}

// Delete removes the shape's row; no row is not an error.
func (s *Store) Delete(shape string) error {
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM needs_by_shape WHERE shape = ?`, shape)
		return err
	})
}

// All returns every usable row, ordered by shape, and the rows it skipped: a
// row that cannot be read, that has another schema, or that holds no figure,
// and every row still left under its profile. Skipped rows are left in place.
func (s *Store) All() ([]Record, []Skipped, error) {
	var out []Record
	var skipped []Skipped
	err := s.scan(`SELECT shape, profile, record FROM needs_by_shape ORDER BY shape`, func(shape, profile, doc string) {
		var r Record
		why := usable(profile, doc, &r)
		if why == "" && r.Shape() != shape {
			why = "stored under another shape"
		}
		if why != "" {
			skipped = append(skipped, Skipped{Profile: profile, Reason: why})
			return
		}
		out = append(out, r)
	})
	if err != nil {
		return nil, nil, err
	}
	err = s.scan(`SELECT '', profile, record FROM learned_needs ORDER BY profile`, func(_, profile, doc string) {
		var r Record
		why := usable(profile, doc, &r)
		if why == "" {
			why = "left under its profile"
		}
		skipped = append(skipped, Skipped{Profile: profile, Reason: why})
	})
	if err != nil {
		return nil, nil, err
	}
	return out, skipped, nil
}

func (s *Store) scan(q string, fn func(shape, profile, doc string)) error {
	rows, err := s.db.DB().Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var shape, profile, doc string
		if err := rows.Scan(&shape, &profile, &doc); err != nil {
			return err
		}
		fn(shape, profile, doc)
	}
	return rows.Err()
}
