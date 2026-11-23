package bench

import (
	"database/sql"
	"fmt"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// Background-task isolation: the manager's loop restarts after a failure
// (manager.go, Start). Before it does, the run it was executing is failed
// in the store, as a restart of the process would (RecoverInterrupted) but
// for that run only. Its models were unloaded while the panic unwound (the
// cleanup in execute is deferred), the queue is kept, and the loop starts
// again with a schedule check.

// recoverRun is the loop's rebuild. It runs on the loop's goroutine, before
// the restart.
func (m *Manager) recoverRun(p any) {
	q := m.running
	m.running = nil
	if q == nil {
		return
	}
	r := q.run
	why := fmt.Sprintf("internal error: %v", p)
	now := m.o.Now().UnixMilli()
	if err := m.o.Store.FailRun(r.ID, now, why); err != nil {
		m.log.Warn("cannot mark the failed benchmark run", "run", r.ID, "err", err)
	}
	if got, err := m.o.Store.GetRun(r.ID); err == nil && got != nil {
		r = got
	} else {
		r.State, r.FinishedAt, r.Error = RunFailed, now, why
	}
	m.publishRun(r)
	m.record(history.TBenchRunFinished, history.Error, "", fmt.Sprintf("Benchmark run %s failed: %s.", r.ID, why),
		map[string]any{"bench_run": r.ID, "suite": r.SuiteID, "state": RunFailed, "error": why})
}

// FailRun marks a run that is still running (or paused) failed with why,
// and its unfinished steps failed as interrupted. A run that already
// finished is left as it is.
func (s *Store) FailRun(id string, now int64, why string) error {
	return s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE run_steps SET state='failed', outcome='error', error='interrupted', finished_at=?
		  WHERE state IN ('running','pending') AND run_id=? AND run_id IN (SELECT id FROM runs WHERE state IN ('running','paused'))`, now, id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE runs SET state='failed', error=?, finished_at=? WHERE id=? AND state IN ('running','paused')`, why, now, id)
		return err
	})
}
