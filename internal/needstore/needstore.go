// Package needstore keeps the memory needs the scheduler learned from loads
// in the scheduler database, so they survive a restart (design 7.2a).
//
// One row per profile. The row says what was measured (the total and the
// per-node figures) and what it was measured with: the model file, the
// settings that change the need, the placement and split, when, and on which
// release. The scheduler decides whether a row still describes its profile;
// this package only stores and reads rows.
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

// Record is one profile's stored measurement.
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

// Skipped is a row All could not use, and why.
type Skipped struct {
	Profile string
	Reason  string
}

var migrations = []string{`
CREATE TABLE learned_needs (
  profile    TEXT PRIMARY KEY,
  updated_at INTEGER NOT NULL,
  record     TEXT NOT NULL
);
`}

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
			if _, err := tx.Exec(migrations[i]); err != nil {
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

// Put stores r as its profile's row, replacing the one there.
func (s *Store) Put(r Record) error {
	r.Schema = RecordSchema
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO learned_needs (profile, updated_at, record) VALUES (?,?,?)
			ON CONFLICT(profile) DO UPDATE SET updated_at = excluded.updated_at, record = excluded.record`,
			r.Profile, time.Now().UnixMilli(), string(b))
		return err
	})
}

// Delete removes the profile's row; no row is not an error.
func (s *Store) Delete(profile string) error {
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM learned_needs WHERE profile = ?`, profile)
		return err
	})
}

// All returns every usable row, by profile name, and the rows it skipped: a
// row that cannot be read, that has another schema, or that holds no figure.
// Skipped rows are left in place.
func (s *Store) All() ([]Record, []Skipped, error) {
	rows, err := s.db.DB().Query(`SELECT profile, record FROM learned_needs ORDER BY profile`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Record
	var skipped []Skipped
	for rows.Next() {
		var profile, doc string
		if err := rows.Scan(&profile, &doc); err != nil {
			return nil, nil, err
		}
		var r Record
		switch err := json.Unmarshal([]byte(doc), &r); {
		case err != nil:
			skipped = append(skipped, Skipped{Profile: profile, Reason: "unreadable: " + err.Error()})
		case r.Schema != RecordSchema:
			skipped = append(skipped, Skipped{Profile: profile, Reason: fmt.Sprintf("record version %d, this build reads %d", r.Schema, RecordSchema)})
		case r.Profile != profile || r.TotalMiB <= 0:
			skipped = append(skipped, Skipped{Profile: profile, Reason: "no usable figure"})
		default:
			out = append(out, r)
		}
	}
	return out, skipped, rows.Err()
}
