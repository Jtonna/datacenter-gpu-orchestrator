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

// Retention (data model step 4): one setting, bench_retention_days (default
// 183, about 6 months; 0 keeps runs for ever), applied to everything of a run
// together: its rows (steps, requests, phases, measurements, task answers and
// scores) and its files (transcripts). A pinned run is never removed, nor is
// a run that has not finished. Every removal is logged in run_removals. A
// removed run disappears from the lists as a whole.

// DefaultRetentionEvery is how often the manager looks for expired runs.
const DefaultRetentionEvery = time.Hour

// Removal is the log entry of a removed run.
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
	Measurements int    `json:"measurements"`
	FileBytes    int64  `json:"file_bytes"`
	FilesError   string `json:"files_error,omitempty"`
}

// SetPinned pins or unpins a run. A pinned run is never removed by retention.
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

// ExpiredRuns are the finished, unpinned runs that ended before cutoff (unix
// ms), oldest first.
func (s *Store) ExpiredRuns(cutoff int64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT id FROM runs WHERE pinned=0 AND state IN ('completed','failed','cancelled','skipped')
	  AND COALESCE(NULLIF(finished_at,0), queued_at) < ? ORDER BY COALESCE(NULLIF(finished_at,0), queued_at), id LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
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

// RemoveRun removes a finished, unpinned run with its rows and files, and
// logs the removal. A pinned or unfinished run is ErrConflict.
func (s *Store) RemoveRun(id, reason, actor string, now int64) (*Removal, error) {
	dir, err := s.RunDir(id)
	if err != nil {
		return nil, err
	}
	rm := &Removal{RunID: id, RemovedAt: now, Reason: reason, Actor: actor, FileBytes: dirSize(dir)}
	err = s.write(func(tx *sql.Tx) error {
		var pinned bool
		err := tx.QueryRow(`SELECT kind, target_id, suite_name, state, queued_at, finished_at, pinned FROM runs WHERE id=?`, id).
			Scan(&rm.Kind, &rm.TargetID, &rm.Name, &rm.State, &rm.QueuedAt, &rm.FinishedAt, &pinned)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case pinned:
			return fmt.Errorf("%w: run %s is pinned", ErrConflict, id)
		case rm.State == RunQueued || rm.State == RunRunning || rm.State == RunPaused:
			return fmt.Errorf("%w: run %s is %s", ErrConflict, id, rm.State)
		}
		tx.QueryRow(`SELECT COUNT(*) FROM run_steps WHERE run_id=?`, id).Scan(&rm.Steps)
		tx.QueryRow(`SELECT COUNT(*) FROM measurements WHERE run_id=?`, id).Scan(&rm.Measurements)
		// The rows of the steps go with them (ON DELETE CASCADE).
		if _, err := tx.Exec(`DELETE FROM runs WHERE id=?`, id); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO run_removals (run_id, kind, target_id, name, state, queued_at, finished_at, removed_at, reason, actor, steps, measurements, file_bytes)
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

// Removals is the removal log, newest first.
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

// orphanRunDirs are run folders whose run is gone (a removal whose files
// could not be deleted at the time).
func (s *Store) orphanRunDirs() []string {
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
		if s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE id=?`, de.Name()).Scan(&n) == nil && n == 0 {
			out = append(out, de.Name())
		}
	}
	return out
}

// sweepRetention removes the runs older than the retention setting.
func (m *Manager) sweepRetention() {
	if m.o.Retention == nil {
		return
	}
	keep := m.o.Retention()
	if keep <= 0 {
		return // keep for ever
	}
	now := m.o.Now()
	ids, err := m.o.Store.ExpiredRuns(now.Add(-keep).UnixMilli(), 500)
	if err != nil {
		m.log.Warn("cannot list the benchmark runs past their retention", "err", err)
		return
	}
	days := int(keep / (24 * time.Hour))
	reason := fmt.Sprintf("retention: finished more than %d days ago (bench_retention_days)", days)
	for _, id := range ids {
		rm, err := m.o.Store.RemoveRun(id, reason, "system", now.UnixMilli())
		if err != nil {
			m.log.Warn("cannot remove a benchmark run past its retention", "run", id, "err", err)
			continue
		}
		m.log.Info("benchmark run removed (retention)", "run", id, "steps", rm.Steps, "bytes", rm.FileBytes)
		m.emit(map[string]any{"kind": "run_removed", "run_id": id, "reason": reason})
		m.record("benchRunRemoved", history.Info, "", fmt.Sprintf("Benchmark run %s removed: %s", id, reason),
			map[string]any{"bench_run": id, "reason": reason, "steps": rm.Steps, "measurements": rm.Measurements, "file_bytes": rm.FileBytes})
	}
	for _, id := range m.o.Store.orphanRunDirs() {
		if dir, err := m.o.Store.RunDir(id); err == nil {
			if err := os.RemoveAll(dir); err != nil {
				m.log.Warn("cannot remove the files of a removed benchmark run", "run", id, "err", err)
			}
		}
	}
}
