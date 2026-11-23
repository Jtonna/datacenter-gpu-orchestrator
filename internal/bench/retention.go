package bench

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// Retention (data model step 4) is off unless the owner sets
// bench_retention_days (default 0: off, nothing is ever removed). When set, a
// finished, unpinned run older than that is trimmed: its request rows and its
// transcript files are removed; the run, its steps and phases, its task
// answers and scores and its measurements (the compact record) stay, so its
// history and trends are unchanged. A pinned run is never trimmed, nor is a
// run that has not finished. Every trim is logged in run_removals.
// GET /api/bench/removals/preview?days=N lists what a setting would trim
// without trimming anything.

// DefaultRetentionEvery is how often the manager looks for expired runs.
const DefaultRetentionEvery = time.Hour

// Removal is the log entry of a trimmed run.
type Removal struct {
	ID           int64  `json:"id"`
	RunID        string `json:"run_id"`
	Kind         string `json:"kind"`
	TargetID     string `json:"target_id,omitempty"`
	Name         string `json:"name,omitempty"`
	State        string `json:"state"`
	QueuedAt     int64  `json:"queued_at"`
	FinishedAt   int64  `json:"finished_at,omitempty"`
	RemovedAt    int64  `json:"removed_at"`
	Reason       string `json:"reason"`
	Actor        string `json:"actor"`
	Steps        int    `json:"steps"`
	Requests     int    `json:"requests"`     // request rows removed
	Measurements int    `json:"measurements"` // measurements kept
	FileBytes    int64  `json:"file_bytes"`
	FilesError   string `json:"files_error,omitempty"`
}

// SetPinned pins or unpins a run. A pinned run is never trimmed by retention.
func (s *Store) SetPinned(id string, pinned bool) (*Run, error) {
	err := s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE runs SET pinned=? WHERE id=?`, pinned, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetRun(id)
}

// ExpiredRun is a run a retention setting would trim.
type ExpiredRun struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name,omitempty"`
	State      string `json:"state"`
	FinishedAt int64  `json:"finished_at"`
}

// ExpiredRuns are the finished, unpinned, untrimmed runs that ended before
// cutoff (unix ms), oldest first.
func (s *Store) ExpiredRuns(cutoff int64, limit int) ([]ExpiredRun, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT id, kind, suite_name, state, COALESCE(NULLIF(finished_at,0), queued_at) AS t FROM runs
	  WHERE pinned=0 AND trimmed_at=0 AND state IN ('completed','failed','cancelled','skipped') AND COALESCE(NULLIF(finished_at,0), queued_at) < ?
	  ORDER BY t, id LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExpiredRun{}
	for rows.Next() {
		var e ExpiredRun
		if err := rows.Scan(&e.ID, &e.Kind, &e.Name, &e.State, &e.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// RemoveRun trims a finished, unpinned run: it removes the run's request rows
// and transcript files, keeps the run, its steps, phases, task rows and
// measurements, marks the run trimmed and logs it. A pinned, unfinished or
// already trimmed run is ErrConflict.
func (s *Store) RemoveRun(id, reason, actor string, now int64) (*Removal, error) {
	dir, err := s.RunDir(id)
	if err != nil {
		return nil, err
	}
	rm := &Removal{RunID: id, RemovedAt: now, Reason: reason, Actor: actor, FileBytes: dirSize(dir)}
	err = s.write(func(tx *sql.Tx) error {
		var pinned bool
		var trimmed int64
		err := tx.QueryRow(`SELECT kind, target_id, suite_name, state, queued_at, finished_at, pinned, trimmed_at FROM runs WHERE id=?`, id).
			Scan(&rm.Kind, &rm.TargetID, &rm.Name, &rm.State, &rm.QueuedAt, &rm.FinishedAt, &pinned, &trimmed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case pinned:
			return fmt.Errorf("%w: run %s is pinned", ErrConflict, id)
		case trimmed != 0:
			return fmt.Errorf("%w: run %s was already trimmed", ErrConflict, id)
		case rm.State == RunQueued || rm.State == RunRunning || rm.State == RunPaused:
			return fmt.Errorf("%w: run %s is %s", ErrConflict, id, rm.State)
		}
		tx.QueryRow(`SELECT COUNT(*) FROM run_steps WHERE run_id=?`, id).Scan(&rm.Steps)
		tx.QueryRow(`SELECT COUNT(*) FROM measurements WHERE run_id=?`, id).Scan(&rm.Measurements)
		// Only the request rows go; the measurements mapped from them stay.
		res, err := tx.Exec(`DELETE FROM requests WHERE step_id IN (SELECT id FROM run_steps WHERE run_id=?)`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		rm.Requests = int(n)
		if _, err := tx.Exec(`UPDATE runs SET trimmed_at=? WHERE id=?`, now, id); err != nil {
			return err
		}
		res, err = tx.Exec(`INSERT INTO run_removals (run_id, kind, target_id, name, state, queued_at, finished_at, removed_at, reason, actor, steps, measurements, file_bytes)
		  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, rm.RunID, rm.Kind, rm.TargetID, rm.Name, rm.State, rm.QueuedAt, rm.FinishedAt, rm.RemovedAt, rm.Reason, rm.Actor,
			rm.Steps, rm.Measurements, rm.FileBytes)
		if err != nil {
			return err
		}
		rm.ID, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		rm.FilesError = err.Error()
		s.write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE run_removals SET files_error=? WHERE id=?`, rm.FilesError, rm.ID)
			return err
		})
	}
	return rm, nil
}

// Removals is the trim log, newest first.
func (s *Store) Removals(limit int) ([]Removal, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, run_id, kind, target_id, name, state, queued_at, finished_at, removed_at, reason, actor, steps, measurements, file_bytes, files_error
	  FROM run_removals ORDER BY removed_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Removal{}
	for rows.Next() {
		var r Removal
		if err := rows.Scan(&r.ID, &r.RunID, &r.Kind, &r.TargetID, &r.Name, &r.State, &r.QueuedAt, &r.FinishedAt, &r.RemovedAt, &r.Reason, &r.Actor,
			&r.Steps, &r.Measurements, &r.FileBytes, &r.FilesError); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// leftoverRunDirs are the folders of trimmed runs (a trim whose files could
// not be deleted at the time). A folder without a run row is left alone.
func (s *Store) leftoverRunDirs() []string {
	des, err := os.ReadDir(filepath.Join(s.DataDir(), RunsDirName))
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		if !de.IsDir() || !safeRunID.MatchString(de.Name()) {
			continue
		}
		var n int
		if s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE id=? AND trimmed_at>0`, de.Name()).Scan(&n) == nil && n == 1 {
			out = append(out, de.Name())
		}
	}
	return out
}

// sweepRetention trims the runs older than the retention setting; it does
// nothing while the setting is 0 (off, the default).
func (m *Manager) sweepRetention() {
	if m.o.Retention == nil {
		return
	}
	keep := m.o.Retention()
	if keep <= 0 {
		return // off
	}
	now := m.o.Now()
	runs, err := m.o.Store.ExpiredRuns(now.Add(-keep).UnixMilli(), 500)
	if err != nil {
		m.log.Warn("cannot list the benchmark runs past their retention", "err", err)
		return
	}
	days := int(keep / (24 * time.Hour))
	reason := fmt.Sprintf("retention: finished more than %d days ago (bench_retention_days); requests and transcripts removed, measurements kept", days)
	for _, e := range runs {
		id := e.ID
		rm, err := m.o.Store.RemoveRun(id, reason, "system", now.UnixMilli())
		if err != nil {
			m.log.Warn("cannot trim a benchmark run past its retention", "run", id, "err", err)
			continue
		}
		m.log.Info("benchmark run trimmed (retention)", "run", id, "steps", rm.Steps, "requests", rm.Requests, "measurements_kept", rm.Measurements, "bytes", rm.FileBytes)
		m.emit(map[string]any{"kind": "run_removed", "run_id": id, "reason": reason})
		m.record("benchRunRemoved", history.Info, "", fmt.Sprintf("Benchmark run %s trimmed: %s", id, reason),
			map[string]any{"bench_run": id, "reason": reason, "steps": rm.Steps, "requests": rm.Requests, "measurements_kept": rm.Measurements, "file_bytes": rm.FileBytes})
	}
	for _, id := range m.o.Store.leftoverRunDirs() {
		if dir, err := m.o.Store.RunDir(id); err == nil {
			if err := os.RemoveAll(dir); err != nil {
				m.log.Warn("cannot remove the files of a trimmed benchmark run", "run", id, "err", err)
			}
		}
	}
}
