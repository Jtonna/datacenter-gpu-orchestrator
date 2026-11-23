// Package needstore keeps the memory needs the scheduler learned from loads
// in the scheduler database, so they survive a restart (design 7.2a).
//
// One row per shape of load (ShapeKey) and device count, so profiles of the
// same shape share their measurements, a measurement on one node never
// stands for one on three (each device holds its own compute buffer), and
// deleting or renaming a profile loses nothing. The row says what was
// measured (the total and the per-node figures) and what it was measured
// with: the model file and its size, the settings that change the need, the
// placement and split, when, on which release and llama build, and the
// profile that measured it (for display). The scheduler decides whether a
// row describes a profile; this package only stores and reads rows.
//
// Rollback: the table of a build before shape keys (learned_needs, one row
// per profile, version 1) is left as it was, at version 1, and every write
// also goes there under the measuring profile, so an older build opens the
// database and reads, for each profile, the latest figure that profile
// measured. The shape rows live in tables of their own with their own
// version (needs_shape_migrations), which an older build does not look at.
package needstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
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

// MaxRowsPerShape is how many device counts one shape keeps; past it the
// oldest row goes.
const MaxRowsPerShape = 8

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
	// Host is "node" for a load whose host process ran on a node (phase 6,
	// release 0.1.9); empty for the scheduler's machine. Part of the shape.
	Host string `json:"host,omitempty"`

	TotalMiB int `json:"total_mib"`
	// Devices is the number of devices (nodes) the load was spread over; 0
	// in a row written before it was kept (DeviceCount).
	Devices   int       `json:"devices,omitempty"`
	Placement []string  `json:"placement,omitempty"` // node ids in device order
	Split     []float64 `json:"split,omitempty"`     // each node's share of the split
	Nodes     []Node    `json:"nodes,omitempty"`     // empty when the load log did not account for every node

	Cause      string    `json:"cause"`
	MeasuredAt time.Time `json:"measured_at"`
	Release    string    `json:"release"`
	// LlamaBuild is the llama.cpp commit the load ran on; empty when not
	// known (and in rows written before it was kept).
	LlamaBuild string `json:"llama_build,omitempty"`
	// PeakAt is when the per-node peaks were last set (at the end of a
	// load-to-unload cycle); zero when no peak is stored. The peaks live in Nodes, with the same placement
	// and split, and are dropped with the row.
	PeakAt time.Time `json:"peak_at,omitzero"`
	// WriteThrough marks a row this build wrote into the profile table for
	// an older build (see the package comment); it is not read back from
	// there. An older build rewrites the row without it.
	WriteThrough bool `json:"write_through,omitempty"`
}

// ShapeKey is the key of a stored measurement: what the memory need depends
// on besides the model file's content and the placement. An empty KV cache
// type is f16 and fewer than one slot is one, as the scheduler reads them.
// The device count is the second part of a row's key (DeviceCount); the
// member nodes and the split are not keys: the per-node figures inside the
// record are only used for the same node set and split.
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

// ShapeKeyHost is ShapeKey for a load whose host process runs on host:
// "" (the scheduler's machine) gives ShapeKey itself, so stored shapes stay
// what they were; any other value is a shape of its own.
func ShapeKeyHost(kind, model string, ctx, parallel int, kvQuant string, flashAttn bool, extraArgs []string, host string) string {
	if host == "" {
		return ShapeKey(kind, model, ctx, parallel, kvQuant, flashAttn, extraArgs)
	}
	if kvQuant == "" {
		kvQuant = "f16"
	}
	if extraArgs == nil {
		extraArgs = []string{}
	}
	b, _ := json.Marshal([]any{kind, model, ctx, max(parallel, 1), kvQuant, flashAttn, extraArgs, "host=" + host})
	return string(b)
}

// Shape is the record's ShapeKey.
func (r Record) Shape() string {
	return ShapeKeyHost(r.Kind, r.Model, r.Ctx, r.Parallel, r.KVQuant, r.FlashAttn, r.ExtraArgs, r.Host)
}

// DeviceCount is the number of devices the load was spread over: Devices,
// else the placement's length, else 1 (a row of an older build that kept no
// placement counts as one device: a figure scaled up from one device is the
// safe side).
func (r Record) DeviceCount() int {
	switch {
	case r.Devices > 0:
		return r.Devices
	case len(r.Placement) > 0:
		return len(r.Placement)
	}
	return 1
}

// Skipped is a row All could not use, and why: Profile is the profile the
// row names.
type Skipped struct {
	Profile string
	Reason  string
}

// migrations are the profile table's (learned_needs). It stays at version 1
// so that an older build can open the database (see the package comment).
var migrations = []func(*sql.Tx) error{
	execMigration(`
CREATE TABLE learned_needs (
  profile    TEXT PRIMARY KEY,
  updated_at INTEGER NOT NULL,
  record     TEXT NOT NULL
);
`),
}

// shapeMigrations are the shape tables', versioned in needs_shape_migrations.
var shapeMigrations = []func(*sql.Tx) error{
	execMigration(`
CREATE TABLE needs_by_placement (
  shape      TEXT NOT NULL,
  devices    INTEGER NOT NULL,
  profile    TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  record     TEXT NOT NULL,
  PRIMARY KEY (shape, devices)
);
CREATE TABLE needs_meta (
  key   TEXT PRIMARY KEY,
  value INTEGER NOT NULL
);
`),
}

func execMigration(q string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error { _, err := tx.Exec(q); return err }
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

// Open applies the migrations of both tracks, then reads into the shape
// table every usable row of the profile table that an older build wrote
// since the last start (all of them the first time): for one shape and
// device count the higher total wins.
func Open(db DB) (*Store, error) {
	err := db.Write(func(tx *sql.Tx) error {
		if err := migrate(tx, "needs_migrations", migrations); err != nil {
			return err
		}
		if err := migrate(tx, "needs_shape_migrations", shapeMigrations); err != nil {
			return err
		}
		if err := undoDevelopmentV2(tx); err != nil {
			return err
		}
		return importProfileRows(tx)
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate applies list on the version table named table. A development
// build of the shape keys left learned_needs at version 2, which
// undoDevelopmentV2 undoes, so it is allowed here.
func migrate(tx *sql.Tx, table string, list []func(*sql.Tx) error) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS ` + table + ` (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var have int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM ` + table).Scan(&have); err != nil {
		return err
	}
	if table == "needs_migrations" && have == 2 {
		have = 1
	}
	if have > len(list) {
		return fmt.Errorf("the learned-need tables (%s) are at version %d but this build knows %d", table, have, len(list))
	}
	for i := have; i < len(list); i++ {
		if err := list[i](tx); err != nil {
			return fmt.Errorf("learned-need migration %s %d: %w", table, i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO `+table+` (version, applied_at) VALUES (?,?)`, i+1, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// undoDevelopmentV2 moves the rows of a development build's needs_by_shape
// table into the shape table (the higher total wins) and puts learned_needs
// back at version 1. No release had it.
func undoDevelopmentV2(tx *sql.Tx) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM needs_migrations WHERE version = 2`).Scan(&n); err != nil || n == 0 {
		return err
	}
	keep, err := readRows(tx, `SELECT profile, updated_at, record FROM needs_by_shape`)
	if err != nil {
		return err
	}
	for _, x := range keep {
		if err := putHigher(tx, x.r, x.at); err != nil {
			return err
		}
	}
	for _, q := range []string{`DROP TABLE needs_by_shape`, `DELETE FROM needs_migrations WHERE version = 2`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

type timedRecord struct {
	at int64
	r  Record
}

// readRows reads the usable rows of q (profile, updated_at, record).
func readRows(tx *sql.Tx, q string, args ...any) ([]timedRecord, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []timedRecord
	for rows.Next() {
		var profile, doc string
		var x timedRecord
		if err := rows.Scan(&profile, &x.at, &doc); err != nil {
			return nil, err
		}
		if usable(profile, doc, &x.r) == "" {
			out = append(out, x)
		}
	}
	return out, rows.Err()
}

// importProfileRows reads the profile table's rows written since the last
// import, except this build's own write-through rows, into the shape table
// (the higher total wins), and moves the import mark on.
func importProfileRows(tx *sql.Tx) error {
	var since, mark int64
	if err := tx.QueryRow(`SELECT COALESCE((SELECT value FROM needs_meta WHERE key = 'imported_at'), -1)`).Scan(&since); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(updated_at), 0) FROM learned_needs`).Scan(&mark); err != nil {
		return err
	}
	rows, err := readRows(tx, `SELECT profile, updated_at, record FROM learned_needs WHERE updated_at > ? ORDER BY updated_at, profile`, since)
	if err != nil {
		return err
	}
	for _, x := range rows {
		if x.r.WriteThrough {
			continue
		}
		if err := putHigher(tx, x.r, x.at); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO needs_meta (key, value) VALUES ('imported_at', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		max(mark, since, 0))
	return err
}

// putHigher stores r unless its shape and device count hold a total at least
// as high.
func putHigher(tx *sql.Tx, r Record, at int64) error {
	var doc string
	switch err := tx.QueryRow(`SELECT record FROM needs_by_placement WHERE shape = ? AND devices = ?`, r.Shape(), r.DeviceCount()).Scan(&doc); {
	case err == sql.ErrNoRows:
	case err != nil:
		return err
	default:
		var have Record
		if json.Unmarshal([]byte(doc), &have) == nil && have.TotalMiB >= r.TotalMiB {
			return nil
		}
	}
	r.WriteThrough = false
	return upsert(tx, r, at)
}

func upsert(tx *sql.Tx, r Record, at int64) error {
	r.Schema = RecordSchema
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO needs_by_placement (shape, devices, profile, updated_at, record) VALUES (?,?,?,?,?)
		ON CONFLICT(shape, devices) DO UPDATE SET profile = excluded.profile, updated_at = excluded.updated_at, record = excluded.record`,
		r.Shape(), r.DeviceCount(), r.Profile, at, string(b))
	return err
}

// Put stores r as its shape's row for its device count, replacing the one
// there (r.Profile is the profile that measured it, kept for display), and
// writes it under r.Profile into the profile table for an older build. A
// shape past MaxRowsPerShape loses its oldest rows.
func (s *Store) Put(r Record) error {
	r.Schema, r.WriteThrough = RecordSchema, false
	now := time.Now().UnixMilli()
	return s.db.Write(func(tx *sql.Tx) error {
		if err := upsert(tx, r, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM needs_by_placement WHERE shape = ? AND devices NOT IN
			(SELECT devices FROM needs_by_placement WHERE shape = ? ORDER BY updated_at DESC, devices LIMIT ?)`,
			r.Shape(), r.Shape(), MaxRowsPerShape); err != nil {
			return err
		}
		// The row an older build reads (it has one figure for every device
		// count): the shape's highest total, so that it never places on less
		// than either build would (third review, TS2).
		w := r
		var doc string
		err := tx.QueryRow(`SELECT record FROM needs_by_placement WHERE shape = ? ORDER BY json_extract(record, '$.total_mib') DESC, devices LIMIT 1`,
			r.Shape()).Scan(&doc)
		var top Record
		if err == nil && json.Unmarshal([]byte(doc), &top) == nil && top.TotalMiB > w.TotalMiB {
			w = top
			w.Profile = r.Profile
		}
		w.WriteThrough = true
		b, err := json.Marshal(w)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO learned_needs (profile, updated_at, record) VALUES (?,?,?)
			ON CONFLICT(profile) DO UPDATE SET updated_at = excluded.updated_at, record = excluded.record`, r.Profile, now, string(b))
		return err
	})
}

// Delete removes every row of the shape, in both tables (so it cannot come
// back at the next start); no row is not an error.
func (s *Store) Delete(shape string) error {
	var left []string
	err := s.scan(`SELECT '', profile, updated_at, record FROM learned_needs`, func(_, profile string, _ int64, doc string) {
		var r Record
		if usable(profile, doc, &r) == "" && r.Shape() == shape {
			left = append(left, profile)
		}
	})
	if err != nil {
		return err
	}
	return s.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM needs_by_placement WHERE shape = ?`, shape); err != nil {
			return err
		}
		for _, p := range left {
			if _, err := tx.Exec(`DELETE FROM learned_needs WHERE profile = ?`, p); err != nil {
				return err
			}
		}
		return nil
	})
}

// All returns every usable measurement, one per shape and device count,
// ordered by shape and device count, and the rows it skipped (a row that
// cannot be read, that has another schema, or that holds no figure, in
// either table; skipped rows are left in place).
func (s *Store) All() ([]Record, []Skipped, error) {
	var out []Record
	var skipped []Skipped
	err := s.scan(`SELECT shape, profile, updated_at, record FROM needs_by_placement`, func(shape, profile string, _ int64, doc string) {
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
	err = s.scan(`SELECT '', profile, updated_at, record FROM learned_needs ORDER BY profile`, func(_, profile string, _ int64, doc string) {
		var r Record
		if why := usable(profile, doc, &r); why != "" {
			skipped = append(skipped, Skipped{Profile: profile, Reason: why})
		}
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].Shape(), out[j].Shape(); a != b {
			return a < b
		}
		return out[i].DeviceCount() < out[j].DeviceCount()
	})
	return out, skipped, nil
}

func (s *Store) scan(q string, fn func(shape, profile string, at int64, doc string)) error {
	rows, err := s.db.DB().Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var shape, profile, doc string
		var at int64
		if err := rows.Scan(&shape, &profile, &at, &doc); err != nil {
			return err
		}
		fn(shape, profile, at, doc)
	}
	return rows.Err()
}
