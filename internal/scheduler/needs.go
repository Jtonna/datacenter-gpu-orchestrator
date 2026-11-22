package scheduler

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
)

// Learned needs across a restart (design 7.2a). What a load measured, or what
// a memory kill raised the need to, is written to the scheduler database and
// read back by New, before Run plans any request. A stored figure is used
// only while it still describes its profile: the settings that change the
// need (needKey) must be the ones it was measured with, the profile must
// exist, and the model file must have the size it had.
//
// Writes never wait on the database inside a step: the core only appends to
// an outbox (needOps); the runner hands it to needWriter after the step, and
// the writer's own goroutine does the database work. The writer's lock guards
// only its pending map and is never held across a database call.

// NeedStore keeps learned needs across restarts (needstore.Store).
type NeedStore interface {
	All() ([]needstore.Record, []needstore.Skipped, error)
	Put(needstore.Record) error
	Delete(profile string) error
}

// needKey is what a profile's memory need depends on, besides the model
// file's content: the kind, the model file, the context per slot, the slots,
// the KV cache type, flash attention and the extra arguments (which can
// change offload, batch sizes or the cache). Placement settings (force_nodes,
// tensor_split) are not in it: the total does not depend on them and the
// per-node figures are only used for the same node set and split.
type needKey struct {
	kind      core.Kind
	model     string
	ctx       int
	parallel  int
	kvQuant   string
	flashAttn bool
	extraArgs string
}

func needKeyOf(p *core.Profile) needKey {
	return makeNeedKey(p.Kind, p.Model, p.Ctx, p.Parallel, p.KVQuant, p.FlashAttn, p.ExtraArgs)
}

func needKeyOfRecord(r needstore.Record) needKey {
	return makeNeedKey(core.Kind(r.Kind), r.Model, r.Ctx, r.Parallel, r.KVQuant, r.FlashAttn, r.ExtraArgs)
}

func makeNeedKey(kind core.Kind, model string, ctx, parallel int, kv string, fa bool, extra []string) needKey {
	if kv == "" {
		kv = "f16"
	}
	return needKey{kind: kind, model: model, ctx: ctx, parallel: max(parallel, 1), kvQuant: kv, flashAttn: fa,
		extraArgs: strings.Join(extra, "\x00")}
}

// needOp is one queued store write: rec nil deletes the profile's row.
type needOp struct {
	profile string
	rec     *needstore.Record
}

// takeNeedOps returns and clears the store outbox.
func (c *schedCore) takeNeedOps() []needOp {
	out := c.needOps
	c.needOps = nil
	return out
}

// learnedFor is p's learned need when it was measured with p's current
// need-relevant settings, else nil.
func (c *schedCore) learnedFor(p *core.Profile) *learnedNeed {
	l := c.learned[p.Name]
	if l == nil || l.key != needKeyOf(p) {
		return nil
	}
	return l
}

// storeLearned queues the write of name's learned need, when there is a
// store and the measurement describes the profile as configured now.
func (c *schedCore) storeLearned(name string) {
	l := c.learned[name]
	p := c.cfg.Profiles[name]
	if !c.persist || l == nil || p == nil || l.key != needKeyOf(p) {
		return
	}
	r := needstore.Record{Schema: needstore.RecordSchema, Profile: name, Kind: string(p.Kind), Model: p.Model,
		ModelBytes: l.modelBytes, Ctx: p.Ctx, Parallel: max(p.Parallel, 1), KVQuant: l.key.kvQuant, FlashAttn: p.FlashAttn,
		ExtraArgs: slices.Clone(p.ExtraArgs), TotalMiB: l.TotalMiB, Split: slices.Clone(l.split), Cause: l.cause,
		MeasuredAt: l.at, Release: l.release}
	for _, id := range l.placement {
		r.Placement = append(r.Placement, string(id))
	}
	for _, n := range l.Nodes {
		r.Nodes = append(r.Nodes, needstore.Node{ID: string(n.ID), Fraction: n.Fraction, MiB: n.MiB})
	}
	c.needOps = append(c.needOps, needOp{profile: name, rec: &r})
}

// dropLearned forgets name's learned need and queues the removal of its row.
func (c *schedCore) dropLearned(name, why string) {
	l := c.learned[name]
	delete(c.learned, name)
	if c.persist {
		c.needOps = append(c.needOps, needOp{profile: name})
	}
	if l != nil {
		c.log.Info("learned memory need dropped", "profile", name, "measured_mib", l.TotalMiB, "reason", why)
		c.changed = true
	}
}

// restoreNeeds reads the stored learned needs (New calls it before Run). A
// store that cannot be read, or rows that cannot be used, only warn: the
// scheduler then starts from the estimates, as without a store. Rows of a
// deleted profile, or measured with other need-relevant settings, are
// removed; with no configuration at all (the built-in defaults) nothing is
// removed.
func (c *schedCore) restoreNeeds(st NeedStore) {
	if st == nil {
		return
	}
	c.persist = true
	recs, skipped, err := st.All()
	if err != nil {
		c.log.Warn("stored learned needs cannot be read; starting from the estimates", "err", err)
		return
	}
	if len(skipped) > 0 {
		var parts []string
		for _, s := range skipped {
			parts = append(parts, s.Profile+": "+s.Reason)
		}
		c.log.Warn("some stored learned needs were ignored", "count", len(skipped), "rows", strings.Join(parts, "; "))
	}
	restored, removed := 0, 0
	for _, r := range recs {
		p := c.cfg.Profiles[r.Profile]
		switch {
		case p == nil && !c.cfg.Configured:
			continue
		case p == nil:
			c.needOps = append(c.needOps, needOp{profile: r.Profile})
			c.log.Info("stored learned need removed", "profile", r.Profile, "reason", "the profile no longer exists")
			removed++
		case needKeyOf(p) != needKeyOfRecord(r):
			c.needOps = append(c.needOps, needOp{profile: r.Profile})
			c.log.Info("stored learned need removed", "profile", r.Profile,
				"reason", "a setting that changes the memory need is not the one it was measured with")
			removed++
		default:
			c.learned[r.Profile] = learnedFromRecord(r)
			restored++
		}
	}
	if restored+removed > 0 {
		c.log.Info("learned memory needs restored", "restored", restored, "removed", removed)
	}
}

func learnedFromRecord(r needstore.Record) *learnedNeed {
	l := &learnedNeed{TotalMiB: r.TotalMiB, key: needKeyOfRecord(r), at: r.MeasuredAt, restored: true, release: r.Release,
		cause: r.Cause, split: slices.Clone(r.Split), modelBytes: r.ModelBytes}
	for _, id := range r.Placement {
		l.placement = append(l.placement, core.NodeID(id))
	}
	for _, n := range r.Nodes {
		l.Nodes = append(l.Nodes, learnedNode{ID: core.NodeID(n.ID), Fraction: n.Fraction, MiB: n.MiB})
	}
	return l
}

// checkModelBytes drops a learned need measured on a model file of another
// size (the file was replaced under the same name). Called when the
// provider's figure is (re)computed.
func (c *schedCore) checkModelBytes(p *core.Profile, base NeedInfo) {
	if l := c.learned[p.Name]; l != nil && l.modelBytes > 0 && base.FileBytes > 0 && l.modelBytes != base.FileBytes {
		c.dropLearned(p.Name, fmt.Sprintf("the model file is %d bytes, it was %d when measured", base.FileBytes, l.modelBytes))
	}
}

// ErrNoProfile is returned by ForgetNeed for a name no profile has.
var ErrNoProfile = errors.New("scheduler: no such profile")

// forgetNeed drops profile name's learned need, in memory and in the store
// (for when the hardware changed). It reports whether there was one.
func (c *schedCore) forgetNeed(name string) (bool, error) {
	if c.cfg.Profiles[name] == nil {
		if _, ok := c.learned[name]; !ok {
			return false, fmt.Errorf("%w: %s", ErrNoProfile, name)
		}
	}
	l := c.learned[name]
	delete(c.learned, name)
	delete(c.warnedLow, name)
	if c.persist {
		c.needOps = append(c.needOps, needOp{profile: name})
	}
	if l == nil {
		return false, nil
	}
	c.log.Info("learned memory need forgotten", "profile", name, "measured_mib", l.TotalMiB)
	c.changed = true
	return true, nil
}

// applyNeedOps performs queued store writes in order. A failure only warns:
// the figure stays in memory for this run.
func applyNeedOps(st NeedStore, ops []needOp, log *slog.Logger) {
	for _, op := range ops {
		var err error
		if op.rec == nil {
			err = st.Delete(op.profile)
		} else {
			err = st.Put(*op.rec)
		}
		if err != nil && log != nil {
			log.Warn("a learned memory need was not saved; it is kept for this run only", "profile", op.profile, "err", err)
		}
	}
}

// needWriter writes the queued ops on its own goroutine. Ops for the same
// profile are merged (the last one wins), so the queue never grows past one
// entry per profile.
type needWriter struct {
	st  NeedStore
	log *slog.Logger

	mu      sync.Mutex
	pending map[string]needOp
	order   []string

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

func newNeedWriter(st NeedStore, log *slog.Logger) *needWriter {
	return &needWriter{st: st, log: log, pending: map[string]needOp{}, wake: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{})}
}

// queue adds ops and wakes the writer. It never blocks on the database.
func (w *needWriter) queue(ops []needOp) {
	w.mu.Lock()
	for _, op := range ops {
		if _, ok := w.pending[op.profile]; !ok {
			w.order = append(w.order, op.profile)
		}
		w.pending[op.profile] = op
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *needWriter) flush() {
	w.mu.Lock()
	ops := make([]needOp, 0, len(w.order))
	for _, name := range w.order {
		ops = append(ops, w.pending[name])
	}
	w.pending, w.order = map[string]needOp{}, nil
	w.mu.Unlock()
	applyNeedOps(w.st, ops, w.log)
}

// run writes until close; what is queued when close is called is written
// before run returns.
func (w *needWriter) run() {
	defer close(w.done)
	for {
		select {
		case <-w.wake:
			w.flush()
		case <-w.stop:
			w.flush()
			return
		}
	}
}

// close stops the writer after a last flush and waits for it.
func (w *needWriter) close() {
	close(w.stop)
	<-w.done
}

// age is how long ago l was measured, 0 when not known.
func (l *learnedNeed) age(now time.Time) time.Duration {
	if l.at.IsZero() || now.Before(l.at) {
		return 0
	}
	return now.Sub(l.at)
}
