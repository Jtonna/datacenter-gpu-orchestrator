package usage

import (
	"context"
	"database/sql"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Defaults of the retention settings.
const (
	DefaultKeepRequests = 90 * 24 * time.Hour  // request rows
	DefaultKeepHourly   = 396 * 24 * time.Hour // hourly roll-ups: 13 months
	DefaultTrimBatch    = 500
	DefaultTrimEvery    = time.Hour
)

// Retention says how long each kind of row is kept. Daily roll-ups are
// kept for ever. Zero takes the default.
type Retention struct {
	KeepRequests time.Duration
	KeepHourly   time.Duration
	TrimBatch    int           // rows deleted per transaction
	TrimEvery    time.Duration // how often the retention task runs
}

func (r Retention) withDefaults() Retention {
	if r.KeepRequests <= 0 {
		r.KeepRequests = DefaultKeepRequests
	}
	if r.KeepHourly <= 0 {
		r.KeepHourly = DefaultKeepHourly
	}
	if r.TrimBatch <= 0 {
		r.TrimBatch = DefaultTrimBatch
	}
	if r.TrimEvery <= 0 {
		r.TrimEvery = DefaultTrimEvery
	}
	return r
}

// TrimRule is one table that the retention task trims: rows whose Column
// (unix ms) is older than now minus Keep(retention) are deleted, TrimBatch
// rows per transaction. Another feature's tables add their rule to
// trimRules.
type TrimRule struct {
	Table, Column string
	Keep          func(Retention) time.Duration
}

var trimRules []TrimRule

// startTasks starts the supervised retention task.
func (s *Store) startTasks(ctx context.Context) {
	s.tasks.Add(1)
	supervise.Go(ctx, supervise.Spec{Name: "usage.retention", Policy: supervise.Restart, Done: s.tasks.Done}, func(ctx context.Context) {
		// The first pass a minute after the start (not during it), then every TrimEvery.
		t := time.NewTimer(min(time.Minute, s.opt.Retention.TrimEvery))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.Trim(ctx)
			t.Reset(s.opt.Retention.TrimEvery)
		}
	})
}

// Trim deletes the rows past their retention, in batches through the
// writer, and returns how many it deleted. The retention task calls it.
func (s *Store) Trim(ctx context.Context) int {
	if !s.On() {
		return 0
	}
	total := 0
	now := s.opt.Now()
	for _, r := range trimRules {
		cutoff := now.Add(-r.Keep(s.opt.Retention)).UnixMilli()
		q := `DELETE FROM ` + r.Table + ` WHERE rowid IN (SELECT rowid FROM ` + r.Table + ` WHERE ` + r.Column + ` < ? LIMIT ?)`
		for ctx.Err() == nil {
			var n int64
			ok := s.Submit(Op{Terminal: true, Apply: func(tx *sql.Tx) error {
				res, err := tx.Exec(q, cutoff, s.opt.Retention.TrimBatch)
				if err != nil {
					return err
				}
				n, err = res.RowsAffected()
				return err
			}})
			if !ok || s.Flush(ctx) != nil {
				return total
			}
			total += int(n)
			if n < int64(s.opt.Retention.TrimBatch) {
				break
			}
		}
	}
	return total
}
