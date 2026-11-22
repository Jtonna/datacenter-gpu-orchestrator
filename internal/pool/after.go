package pool

import (
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Work that must not run under p.mu: writes to the start-up store and
// history events. Code holding p.mu queues it with afterLocked; unlock
// releases p.mu and then runs it. Batches run one at a time in the order
// their locks were released, so the store and the history see the same
// order as before, and a store or sink may read the pool. Queued work must
// not itself queue work and wait for it (a sink that records into the pool
// from inside Record would wait for its own batch).
type afterQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	next   uint64 // ticket of the next batch (under p.mu)
	done   uint64 // tickets finished (under mu)
	queued []func()
}

// afterLocked queues f to run once p.mu is released. The caller holds p.mu.
func (p *Pool) afterLocked(f func()) { p.after.queued = append(p.after.queued, f) }

// unlock releases p.mu and runs the work queued under it. Every unlock of
// p.mu in this package goes through here, so nothing queued is left behind.
func (p *Pool) unlock() {
	work := p.after.queued
	if len(work) == 0 {
		p.mu.Unlock()
		return
	}
	p.after.queued = nil
	ticket := p.after.next
	p.after.next++
	p.mu.Unlock()

	q := &p.after
	q.mu.Lock()
	if q.cond == nil {
		q.cond = sync.NewCond(&q.mu)
	}
	for q.done != ticket {
		q.cond.Wait()
	}
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.done++
		q.cond.Broadcast()
		q.mu.Unlock()
	}()
	for _, f := range work {
		// The work is store writes and history events, run by whoever
		// unlocks (the scheduler loop among them): one that panics is
		// reported and lost, the rest of the batch still runs.
		supervise.Guard("pool.after", f, nil)
	}
}
