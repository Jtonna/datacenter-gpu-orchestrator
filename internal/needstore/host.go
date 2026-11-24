package needstore

import (
	"database/sql"
	"fmt"
	"time"
)

// The measured host overhead (phase 6, profile host: node): what a host
// process on a node took there besides its device share, per shape of load
// and per node. One row per (shape, node); each measurement replaces it.
//
// The table has a ledger of its own, needs_host_migrations, which an older
// build never looks at: the two ledgers it checks (needs_migrations,
// needs_shape_migrations) stay where they were, so an older build opens the
// file, ignores this table and plans with host_overhead_mib as before. Going
// forward again, the rows are where they were left.

// MaxHostOverheadMiB is the largest figure kept (host_overhead_mib's range).
const MaxHostOverheadMiB = 16384

// HostOverhead is one stored measurement. Shape is the profile's shape
// (ShapeKeyHost with host "node"); Node the node id that hosted. MiB is the
// figure plans use: the memory the node gave up for the load less what the
// load log put on that node's own devices. LogHostMiB is the log's host
// buffers (CPU, CPU_Mapped, *_Host) at the time, shown beside it and never
// used. ModelBytes and LlamaBuild say what it was measured with.
type HostOverhead struct {
	Shape      string
	Node       string
	MiB        int
	LogHostMiB int
	MeasuredAt time.Time
	Release    string
	LlamaBuild string
	ModelBytes int64
	Profile    string // the profile that measured it (for display)
	Cause      string // CauseLoad, or the exit reason that raised it
}

var hostMigrations = []func(*sql.Tx) error{
	execMigration(`
CREATE TABLE needs_host_overhead (
  shape        TEXT NOT NULL,
  node         TEXT NOT NULL,
  mib          INTEGER NOT NULL,
  log_host_mib INTEGER NOT NULL,
  measured_at  INTEGER NOT NULL,
  release      TEXT NOT NULL,
  llama_build  TEXT NOT NULL,
  model_bytes  INTEGER NOT NULL,
  profile      TEXT NOT NULL,
  cause        TEXT NOT NULL,
  PRIMARY KEY (shape, node)
);
`),
}

func usableHost(mib int) bool { return mib >= 0 && mib <= MaxHostOverheadMiB }

// PutHost stores h, replacing the row of its shape and node.
func (s *Store) PutHost(h HostOverhead) error {
	if h.Shape == "" || h.Node == "" || !usableHost(h.MiB) {
		return fmt.Errorf("host overhead %d MiB for node %q: not storable (0 to %d MiB, with a shape and a node)", h.MiB, h.Node, MaxHostOverheadMiB)
	}
	var at int64
	if !h.MeasuredAt.IsZero() {
		at = h.MeasuredAt.UnixMilli()
	}
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO needs_host_overhead (shape, node, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause)
			VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(shape, node) DO UPDATE SET mib = excluded.mib, log_host_mib = excluded.log_host_mib, measured_at = excluded.measured_at,
			release = excluded.release, llama_build = excluded.llama_build, model_bytes = excluded.model_bytes, profile = excluded.profile, cause = excluded.cause`,
			h.Shape, h.Node, h.MiB, h.LogHostMiB, at, h.Release, h.LlamaBuild, h.ModelBytes, h.Profile, h.Cause)
		return err
	})
}

// AllHost returns every usable row, ordered by shape and node. A row whose
// figure is out of range is skipped (and left in place).
func (s *Store) AllHost() ([]HostOverhead, error) {
	rows, err := s.db.DB().Query(`SELECT shape, node, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause
		FROM needs_host_overhead ORDER BY shape, node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostOverhead
	for rows.Next() {
		var h HostOverhead
		var at int64
		if err := rows.Scan(&h.Shape, &h.Node, &h.MiB, &h.LogHostMiB, &at, &h.Release, &h.LlamaBuild, &h.ModelBytes, &h.Profile, &h.Cause); err != nil {
			return nil, err
		}
		if !usableHost(h.MiB) {
			continue
		}
		if at > 0 {
			h.MeasuredAt = time.UnixMilli(at).UTC()
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
