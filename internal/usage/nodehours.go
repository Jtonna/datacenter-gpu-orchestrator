package usage

import (
	"context"
	"database/sql"
	"time"
)

// The per-machine availability hours (UI slice 1a, S3): for each machine
// and UTC hour, the milliseconds the availability sampler saw it in each
// state, the in-use time weighted by how busy it was, and the time it was
// reachable. Time the sampler did not see (Potluck not running, a node not
// yet known, a gap after a restart) has no row and no milliseconds: the
// reader counts it unknown. Rows are written as additions, so a restart in
// the middle of an hour adds to what the earlier run wrote for that hour.

// nodeHoursSQL is the migration "node_hours" (schema.go).
const nodeHoursSQL = `
CREATE TABLE node_hours (
  node_id      TEXT    NOT NULL,
  hour         INTEGER NOT NULL, -- unix ms of the UTC hour's start
  name         TEXT    NOT NULL DEFAULT '',
  free_ms      INTEGER NOT NULL DEFAULT 0,
  in_use_ms    INTEGER NOT NULL DEFAULT 0,
  loading_ms   INTEGER NOT NULL DEFAULT 0,
  lost_ms      INTEGER NOT NULL DEFAULT 0, -- down: lost or gone (membership)
  set_aside_ms INTEGER NOT NULL DEFAULT 0, -- down: set aside (quarantined)
  off_ms       INTEGER NOT NULL DEFAULT 0, -- paused, excluded or removed
  busy_ms      INTEGER NOT NULL DEFAULT 0, -- in-use time weighted by in flight over parallel (0..1)
  reachable_ms INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (node_id, hour)
);
CREATE INDEX node_hours_hour ON node_hours (hour);`

func init() {
	// Hourly availability rows go with the hourly retention. (An append:
	// the order of trim rules does not matter, unlike the migrations.)
	trimRules = append(trimRules, TrimRule{Table: "node_hours", Column: "hour", Keep: func(r Retention) time.Duration { return r.KeepHourly }})
}

// NodeHour is one machine's hour, or what to add to it.
type NodeHour struct {
	Node        string
	Name        string
	Hour        time.Time // the UTC hour's start
	FreeMs      int64
	InUseMs     int64
	LoadingMs   int64
	LostMs      int64
	SetAsideMs  int64
	OffMs       int64
	BusyMs      int64
	ReachableMs int64
}

// KnownMs is the time the hour's states account for.
func (h NodeHour) KnownMs() int64 {
	return h.FreeMs + h.InUseMs + h.LoadingMs + h.LostMs + h.SetAsideMs + h.OffMs
}

// Add adds o's milliseconds to h.
func (h *NodeHour) Add(o NodeHour) {
	h.FreeMs += o.FreeMs
	h.InUseMs += o.InUseMs
	h.LoadingMs += o.LoadingMs
	h.LostMs += o.LostMs
	h.SetAsideMs += o.SetAsideMs
	h.OffMs += o.OffMs
	h.BusyMs += o.BusyMs
	h.ReachableMs += o.ReachableMs
}

// AddNodeHours queues one operation that adds each row's milliseconds to
// its (machine, hour) row, creating it when missing; the name is the newest.
// It never blocks (Submit); false when the store is off or the operation
// was dropped. The operation is terminal: added time is never replaced by a
// later write, so it must not be dropped for one.
func (s *Store) AddNodeHours(rows []NodeHour) bool {
	if len(rows) == 0 || !s.On() {
		return false
	}
	rows = append([]NodeHour(nil), rows...)
	now := s.opt.Now().UnixMilli()
	return s.Submit(Op{Terminal: true, Apply: func(tx *sql.Tx) error {
		st, err := tx.Prepare(`INSERT INTO node_hours (node_id, hour, name, free_ms, in_use_ms, loading_ms, lost_ms, set_aside_ms, off_ms, busy_ms, reachable_ms, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (node_id, hour) DO UPDATE SET
  name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE name END,
  free_ms = free_ms + excluded.free_ms, in_use_ms = in_use_ms + excluded.in_use_ms,
  loading_ms = loading_ms + excluded.loading_ms, lost_ms = lost_ms + excluded.lost_ms,
  set_aside_ms = set_aside_ms + excluded.set_aside_ms, off_ms = off_ms + excluded.off_ms,
  busy_ms = busy_ms + excluded.busy_ms, reachable_ms = reachable_ms + excluded.reachable_ms,
  updated_at = excluded.updated_at`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, r := range rows {
			if _, err := st.Exec(r.Node, r.Hour.UTC().Truncate(time.Hour).UnixMilli(), r.Name, r.FreeMs, r.InUseMs, r.LoadingMs,
				r.LostMs, r.SetAsideMs, r.OffMs, r.BusyMs, r.ReachableMs, now); err != nil {
				return err
			}
		}
		return nil
	}})
}

// NodeHours reads the rows whose hour starts in [from, to), ordered by
// machine and hour. Nil and no error when the store is off.
func (s *Store) NodeHours(ctx context.Context, from, to time.Time) ([]NodeHour, error) {
	if !s.On() {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, hour, name, free_ms, in_use_ms, loading_ms, lost_ms, set_aside_ms, off_ms, busy_ms, reachable_ms
FROM node_hours WHERE hour >= ? AND hour < ? ORDER BY node_id, hour`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeHour
	for rows.Next() {
		var h NodeHour
		var hour int64
		if err := rows.Scan(&h.Node, &hour, &h.Name, &h.FreeMs, &h.InUseMs, &h.LoadingMs, &h.LostMs, &h.SetAsideMs, &h.OffMs, &h.BusyMs, &h.ReachableMs); err != nil {
			return nil, err
		}
		h.Hour = time.UnixMilli(hour).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}
