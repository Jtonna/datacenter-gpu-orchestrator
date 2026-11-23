package usage

import (
	"database/sql"
	"log/slog"
	"strings"
)

// upstreamTail is the path a passthrough row sent to the model server:
// what follows /upstream/<model> in its stored path ("/" when nothing).
func upstreamTail(path string) string {
	rest, ok := strings.CutPrefix(path, "/upstream/")
	if !ok {
		return path
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return "/"
}

// sentPath is the path a stored row sent to the model server, as
// IsInference reads it: the /v1 path itself, or a passthrough's tail.
func sentPath(r *Request) string {
	if r.Kind == KindUpstream {
		return upstreamTail(r.Path)
	}
	return r.Path
}

// dropNonInferenceRows (migration 4) deletes the rows that did not run the
// model: the first image recorded every /upstream call (the /slots and
// /props polls of a monitoring client) and every POST under /v1 (tokenize
// and the like). The method is not stored; /v1 rows were POSTs only, and a
// row is kept exactly when its path is on the inference allow-list
// (routes.go), the same table both entry points use. The hourly totals of
// every hour that held a deleted row, and the daily totals of every such
// UTC day, are then rebuilt from the rows that remain, so they equal a
// fresh roll-up of the kept rows. (The rows of those hours and days are
// all still stored: they are younger than the 90-day row retention, the
// first image having shipped days ago.) One INFO line gives the counts.
func dropNonInferenceRows(tx *sql.Tx, log *slog.Logger) error {
	var before int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM client_requests`).Scan(&before); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT ` + rowColumns + ` FROM client_requests`)
	if err != nil {
		return err
	}
	all, err := scanRows(rows)
	if err != nil {
		return err
	}
	hours, days := map[int64]bool{}, map[int64]bool{}
	deleted := 0
	for i := range all {
		r := &all[i]
		if inferenceRoutes[sentPath(r)] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM client_requests WHERE id = ?`, r.ID); err != nil {
			return err
		}
		hours[hourOf(r.QueuedAt)], days[dayOf(r.QueuedAt)] = true, true
		deleted++
	}
	for _, t := range []struct {
		table   string
		buckets map[int64]bool
	}{{"request_hours", hours}, {"request_days", days}} {
		for b := range t.buckets {
			if _, err := tx.Exec(`DELETE FROM `+t.table+` WHERE bucket = ?`, b); err != nil {
				return err
			}
		}
	}
	for i := range all {
		r := &all[i]
		if r.Status == StatusQueued || r.Status == StatusRunning || !inferenceRoutes[sentPath(r)] {
			continue // not in the totals yet, or deleted
		}
		if h := hourOf(r.QueuedAt); hours[h] {
			if err := addRollup(tx, "request_hours", h, r); err != nil {
				return err
			}
		}
		if d := dayOf(r.QueuedAt); days[d] {
			if err := addRollup(tx, "request_days", d, r); err != nil {
				return err
			}
		}
	}
	if log != nil {
		log.Info("usage migration 4: removed the request rows that were not inference",
			"rows_before", before, "rows_deleted", deleted, "rows_after", before-deleted,
			"hours_rebuilt", len(hours), "days_rebuilt", len(days))
	}
	return nil
}
