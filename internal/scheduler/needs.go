package scheduler

import (
	"context"
	"errors"
	"fmt"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Learned needs across a restart (design 7.2a). What a load measured, or what
// a memory kill raised the need to, is written to the scheduler database and
// read back by New, before Run plans any request. A figure is kept by the
// shape of the load (needKey, as needstore.ShapeKey), not by profile: every
// profile of that shape uses it, and deleting, renaming or changing a profile
// removes nothing (a changed profile simply has another shape). A figure is
// dropped when the model file no longer has the size it had, or by
// forget-need.
//
// Writes never wait on the database inside a step: the core only appends to
// an outbox (needOps); the runner hands it to needWriter after the step, and
// the writer's own goroutine does the database work. The writer's lock guards
// only its pending map and is never held across a database call.

// NeedStore keeps learned needs across restarts (needstore.Store).
type NeedStore interface {
	All() ([]needstore.Record, []needstore.Skipped, error)
	Put(needstore.Record) error
	Delete(shape string) error
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
	// host is core.HostNode for a host on a node (phase 6): its
	// measurements are kept apart from those of a host on the scheduler's
	// machine, whose host node figures differ.
	host string
}

func needKeyOf(p *core.Profile) needKey {
	k := makeNeedKey(p.Kind, p.Model, p.Ctx, p.Parallel, p.KVQuant, p.FlashAttn, p.ExtraArgs)
	k.host = p.Host
	return k
}

func needKeyOfRecord(r needstore.Record) needKey {
	k := makeNeedKey(core.Kind(r.Kind), r.Model, r.Ctx, r.Parallel, r.KVQuant, r.FlashAttn, r.ExtraArgs)
	k.host = r.Host
	return k
}

// shape is the store key of k (needstore.ShapeKey).
func (k needKey) shape() string {
	var extra []string
	if k.extraArgs != "" {
		extra = strings.Split(k.extraArgs, "\x00")
	}
	return needstore.ShapeKeyHost(string(k.kind), k.model, k.ctx, k.parallel, k.kvQuant, k.flashAttn, extra, k.host)
}

// shapeOf is the key of p's learned need in c.learned and the store.
func shapeOf(p *core.Profile) string { return needKeyOf(p).shape() }

// ProfilesOfShape lists, sorted, the profiles of cfg whose load has the
// shape of profile name's (they share one learned need); nil when there is
// no such profile.
func ProfilesOfShape(cfg *config.Config, name string) []string {
	p := cfg.Profiles[name]
	if p == nil {
		return nil
	}
	shape := shapeOf(p)
	var out []string
	for n, q := range cfg.Profiles {
		if shapeOf(q) == shape {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func makeNeedKey(kind core.Kind, model string, ctx, parallel int, kv string, fa bool, extra []string) needKey {
	if kv == "" {
		kv = "f16"
	}
	return needKey{kind: kind, model: model, ctx: ctx, parallel: max(parallel, 1), kvQuant: kv, flashAttn: fa,
		extraArgs: strings.Join(extra, "\x00")}
}

// needOp is one queued store write: rec nil and host nil delete every row
// of the shape (its host overheads with it); host writes one node's measured
// host overhead (hostoverhead.go).
type needOp struct {
	shape string
	rec   *needstore.Record
	host  *needstore.HostOverhead
}

// key is what the writer merges ops by: the shape, and the device count of
// a write.
func (op needOp) key() string {
	if op.host != nil {
		return fmt.Sprintf("%s\x00host\x00%s\x00%d", op.shape, op.host.Node, op.host.Devices)
	}
	if op.rec == nil {
		return op.shape
	}
	return fmt.Sprintf("%s\x00%d", op.shape, op.rec.DeviceCount())
}

// takeNeedOps returns and clears the store outbox.
func (c *schedCore) takeNeedOps() []needOp {
	out := c.needOps
	c.needOps = nil
	return out
}

// storeLearned queues the write of measurement l, when there is a store.
func (c *schedCore) storeLearned(l *learnedNeed) {
	if !c.persist || l == nil {
		return
	}
	var extra []string
	if l.key.extraArgs != "" {
		extra = strings.Split(l.key.extraArgs, "\x00")
	}
	r := needstore.Record{Schema: needstore.RecordSchema, Profile: l.profile, Kind: string(l.key.kind), Model: l.key.model,
		ModelBytes: l.modelBytes, Ctx: l.key.ctx, Parallel: l.key.parallel, KVQuant: l.key.kvQuant, FlashAttn: l.key.flashAttn,
		ExtraArgs: extra, Host: l.key.host, TotalMiB: l.TotalMiB, Devices: l.count(), Split: slices.Clone(l.split), Cause: l.cause,
		MeasuredAt: l.at, Release: l.release, LlamaBuild: l.build, PeakAt: l.peakAt}
	for _, id := range l.placement {
		r.Placement = append(r.Placement, string(id))
	}
	for _, n := range l.Nodes {
		r.Nodes = append(r.Nodes, needstore.Node{ID: string(n.ID), Fraction: n.Fraction, MiB: n.MiB, PeakMiB: n.PeakMiB})
	}
	c.needOps = append(c.needOps, needOp{shape: l.key.shape(), rec: &r})
}

// dropLearned forgets every learned need of a shape (all device counts) and
// queues the removal of its rows; name is the profile it is dropped for
// (for the log).
func (c *schedCore) dropLearned(shape, name, why string) {
	ls := c.learned[shape]
	delete(c.learned, shape)
	c.forgetHostOverheads(shape) // hostoverhead.go
	if c.persist {
		c.needOps = append(c.needOps, needOp{shape: shape})
	}
	for _, l := range ls {
		c.log.Info("learned memory need dropped", "profile", name, "measured_by", l.profile, "measured_mib", l.TotalMiB, "devices", l.count(), "reason", why)
		c.changed = true
	}
}

// restoreNeeds reads the stored learned needs (New calls it before Run). A
// store that cannot be read, or rows that cannot be used, only warn: the
// scheduler then starts from the estimates, as without a store. Every row is
// kept by its shape, whether a profile of that shape exists now or not.
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
	c.restoreHostOverheads(st) // hostoverhead.go
	restored, used := 0, 0
	for _, r := range recs {
		c.putLearned(learnedFromRecord(r))
		restored++
	}
	c.servedBuild = ""
	if c.releaseBuild != nil && c.buildRelease != "" {
		c.servedBuild = c.releaseBuild(c.buildRelease)
	}
	switch {
	case len(recs) == 0:
	case c.llamaBuild == "":
		c.log.Warn("the llama build of the host program is not known: stored memory needs only raise the estimates", "rows", len(recs))
	case c.servedBuild == "":
		c.log.Warn("the llama build of the served release is not known (release.json): stored memory needs only raise the estimates", "release", c.buildRelease, "rows", len(recs))
	case !release.SameCommit(c.llamaBuild, c.servedBuild):
		c.log.Warn("the host program's llama build is not the served release's: stored memory needs only raise the estimates",
			"host_build", c.llamaBuild, "release", c.buildRelease, "release_build", c.servedBuild, "rows", len(recs))
	}
	c.stampBuilds()
	for _, p := range c.cfg.Profiles {
		if len(c.learnedOf(p)) > 0 {
			used++
		}
	}
	if restored > 0 {
		c.log.Info("learned memory needs restored", "restored", restored, "profiles_using_them", used)
	}
}

func learnedFromRecord(r needstore.Record) *learnedNeed {
	l := &learnedNeed{TotalMiB: r.TotalMiB, key: needKeyOfRecord(r), profile: r.Profile, at: r.MeasuredAt, restored: true, release: r.Release,
		cause: r.Cause, split: slices.Clone(r.Split), modelBytes: r.ModelBytes, peakAt: r.PeakAt, devices: r.DeviceCount(), build: r.LlamaBuild}
	for _, id := range r.Placement {
		l.placement = append(l.placement, core.NodeID(id))
	}
	for _, n := range r.Nodes {
		l.Nodes = append(l.Nodes, learnedNode{ID: core.NodeID(n.ID), Fraction: n.Fraction, MiB: n.MiB, PeakMiB: n.PeakMiB})
	}
	return l
}

// checkModelBytes drops a learned need measured on a model file of another
// size (the file was replaced under the same name). Called when the
// provider's figure is (re)computed.
func (c *schedCore) checkModelBytes(p *core.Profile, base NeedInfo) {
	for _, l := range c.learnedOf(p) {
		if l.modelBytes > 0 && base.FileBytes > 0 && l.modelBytes != base.FileBytes {
			c.dropLearned(shapeOf(p), p.Name, fmt.Sprintf("the model file is %d bytes, it was %d when measured", base.FileBytes, l.modelBytes))
			return
		}
	}
}

// ErrNoProfile is returned by ForgetNeed for a name no profile has.
var ErrNoProfile = errors.New("scheduler: no such profile")

// forgetNeed drops the learned need of profile name's shape, in memory and
// in the store (for when the hardware changed); every profile of that shape
// loses it. A name no profile has may still name the profile that measured a
// kept figure. It reports whether there was one.
func (c *schedCore) forgetNeed(name string) (bool, error) {
	shape := ""
	if p := c.cfg.Profiles[name]; p != nil {
		shape = shapeOf(p)
	} else {
		for s, ls := range c.learned {
			for _, l := range ls {
				if l.profile == name && (shape == "" || s < shape) {
					shape = s
				}
			}
		}
		if shape == "" {
			return false, fmt.Errorf("%w: %s", ErrNoProfile, name)
		}
	}
	ls := c.learned[shape]
	delete(c.learned, shape)
	delete(c.warnedLow, name)
	hadHost := c.forgetHostOverheads(shape) // hostoverhead.go
	if c.persist {
		c.needOps = append(c.needOps, needOp{shape: shape})
	}
	if len(ls) == 0 {
		return hadHost, nil
	}
	if len(ls) == 0 {
		return false, nil
	}
	for _, l := range ls {
		c.log.Info("learned memory need forgotten", "profile", name, "measured_mib", l.TotalMiB, "devices", l.count())
	}
	c.changed = true
	return true, nil
}

// applyNeedOps performs queued store writes in order. A failure only warns:
// the figure stays in memory for this run.
func applyNeedOps(st NeedStore, ops []needOp, log *slog.Logger) {
	for _, op := range ops {
		var err error
		switch {
		case op.host != nil:
			if hs, ok := st.(HostOverheadStore); ok {
				err = hs.PutHost(*op.host)
			}
		case op.rec == nil:
			err = st.Delete(op.shape)
		default:
			err = st.Put(*op.rec)
		}
		if err != nil && log != nil {
			log.Warn("a learned memory need was not saved; it is kept for this run only", "shape", op.shape, "err", err)
		}
	}
}

// needWriter writes the queued ops on its own goroutine. Ops for the same
// shape are merged (the last one wins), so the queue never grows past one
// entry per shape.
type needWriter struct {
	st  NeedStore
	log *slog.Logger

	mu      sync.Mutex
	pending map[string]needOp
	order   []string

	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
}

// start runs the writer. A failure loses only the batch flush had taken (the
// store keeps the last good figures and the core's are untouched); the
// writer runs again.
func (w *needWriter) start() {
	var ctx context.Context
	ctx, w.cancel = context.WithCancel(context.Background())
	supervise.Go(ctx, supervise.Spec{Name: "scheduler.needs", Policy: supervise.Restart, Done: func() { close(w.done) }},
		func(context.Context) { w.run() })
}

func newNeedWriter(st NeedStore, log *slog.Logger) *needWriter {
	return &needWriter{st: st, log: log, pending: map[string]needOp{}, wake: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{})}
}

// queue adds ops and wakes the writer. It never blocks on the database.
func (w *needWriter) queue(ops []needOp) {
	func() {
		w.mu.Lock()
		defer supervise.Unlocked("scheduler", w.mu.Unlock)
		for _, op := range ops {
			if op.rec == nil && op.host == nil { // a delete of the shape makes its queued writes moot
				for k, q := range w.pending {
					if q.shape == op.shape && (q.rec != nil || q.host != nil) {
						delete(w.pending, k)
						w.order = slices.DeleteFunc(w.order, func(o string) bool { return o == k })
					}
				}
			}
			k := op.key()
			if _, ok := w.pending[k]; !ok {
				w.order = append(w.order, k)
			}
			w.pending[k] = op
		}
	}()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *needWriter) flush() {
	var ops []needOp
	func() {
		w.mu.Lock()
		defer supervise.Unlocked("scheduler", w.mu.Unlock)
		ops = make([]needOp, 0, len(w.order))
		for _, name := range w.order {
			ops = append(ops, w.pending[name])
		}
		supervise.Point("scheduler.needs_flush", w) // test hook, under w.mu
		w.pending, w.order = map[string]needOp{}, nil
	}()
	applyNeedOps(w.st, ops, w.log)
}

// run writes until close; what is queued when close is called is written
// before run returns.
func (w *needWriter) run() {
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
	if w.cancel != nil {
		w.cancel() // a writer waiting to restart does not hold the stop up
	}
	<-w.done
}

// age is how long ago l was measured, 0 when not known.
func (l *learnedNeed) age(now time.Time) time.Duration {
	if l.at.IsZero() || now.Before(l.at) {
		return 0
	}
	return now.Sub(l.at)
}
