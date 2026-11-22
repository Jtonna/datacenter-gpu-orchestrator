package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// Options are the scheduler's collaborators. View, Prober and Launcher are
// required.
type Options struct {
	Clock  Clock           // default RealClock
	View   fleet.View      // the node view (fleet.Client)
	Prober instance.Prober // pre-flight (instance.ExecProber)
	// WorkerProber is the worker health connect probe; default TCPConnectProber.
	WorkerProber WorkerProber
	// RecoveryPoster sends recovery requests to the fleet manager; default
	// HTTPRecoveryPoster. Used only when recovery.enabled.
	RecoveryPoster RecoveryPoster
	Launcher       instance.Launcher      // children (instance.ProcessLauncher)
	Needs          NeedProvider           // default &ConfigNeeds{Cfg: cfg}
	Build          *instance.BuildOptions // default instance.OptionsFromConfig(cfg)
	Logger         *slog.Logger           // default slog.Default()
	// History receives the safety and lifecycle events (design 9.1); nil drops
	// them. Record must never block.
	History HistorySink
	// Workers starts and stops the worker launches on agents. Required with
	// defaults.membership agents, unused otherwise (agents.go).
	Workers WorkerLauncher
	// LoadGate, with agents, is asked before every load; an error refuses
	// the load with its text (no verified release, or the host program is
	// not the release's build, agent contract 5.1).
	LoadGate func() error
	// NeedStore keeps learned needs across restarts (needs.go); nil keeps
	// them for the life of the process only. Release is stored with each
	// measurement.
	NeedStore NeedStore
	Release   string
	// LlamaBuild is the llama.cpp commit the loads run on (the host
	// program's); stored with each measurement, and a restored measurement
	// of another build is not trusted (needcount.go). "" when not known.
	LlamaBuild string
	// ReleaseBuild gives the llama build an older program release shipped
	// ("" when not known): a stored need without a build, measured on a
	// release that shipped the build served now, is stamped with it at
	// start (needcount.go). nil: only the release served now is known.
	ReleaseBuild func(release string) string
}

// Scheduler is the scheduler core plus the one goroutine (Run) that owns
// it. Every exported method is safe for concurrent use. Methods that need
// the core (Acquire, Load, Unload, UnloadAllIdle, Status, Plan, Shutdown)
// wait for Run to process them; Upstream, Running and Profiles read a
// snapshot published after every step and never wait.
type Scheduler struct {
	cfg  atomic.Pointer[config.Config]
	opts Options
	core *schedCore

	events   chan event
	tick     chan struct{}
	started  atomic.Bool
	loopDone chan struct{} // closed when the loop stops taking events
	runDone  chan struct{} // closed when Run has returned
	snap     atomic.Pointer[snapshot]

	bgCtx    context.Context
	bgCancel context.CancelFunc
	wg       sync.WaitGroup

	healthMu     sync.Mutex
	healthCancel map[uint64]context.CancelFunc

	subsMu sync.Mutex
	subs   map[*subscriber]struct{}

	needs *needWriter // nil without a NeedStore
}

type snapshot struct {
	running  []RunningInstance
	recent   []LoadOutcome
	profiles []ProfileState
	upstream map[string]string
}

// New builds a scheduler for a validated config. Call Run to start it.
func New(cfg *config.Config, o Options) (*Scheduler, error) {
	if cfg == nil || o.View == nil || o.Prober == nil || o.Launcher == nil {
		return nil, errors.New("scheduler: config, View, Prober and Launcher are required")
	}
	if o.Clock == nil {
		o.Clock = RealClock{}
	}
	if o.WorkerProber == nil {
		o.WorkerProber = TCPConnectProber{}
	}
	if o.RecoveryPoster == nil {
		o.RecoveryPoster = HTTPRecoveryPoster{}
	}
	if o.Needs == nil {
		o.Needs = &ConfigNeeds{Cfg: cfg}
	}
	if o.Build == nil {
		b := instance.OptionsFromConfig(cfg)
		o.Build = &b
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if cfg.AgentsMode() && o.Workers == nil {
		return nil, errors.New("scheduler: membership agents needs a WorkerLauncher")
	}
	s := &Scheduler{
		opts:   o,
		core:   newCore(cfg, o.Clock, o.View, o.Needs, *o.Build, o.Logger),
		events: make(chan event, 256), tick: make(chan struct{}, 1),
		loopDone: make(chan struct{}), runDone: make(chan struct{}),
		healthCancel: map[uint64]context.CancelFunc{}, subs: map[*subscriber]struct{}{},
	}
	s.core.agents = cfg.AgentsMode()
	s.core.loadGate = o.LoadGate
	s.core.buildRelease = o.Release
	s.core.llamaBuild, s.core.releaseBuild = o.LlamaBuild, o.ReleaseBuild
	if o.NeedStore != nil {
		// Read before Run, so the first request is planned with them.
		s.core.restoreNeeds(o.NeedStore)
		s.needs = newNeedWriter(o.NeedStore, o.Logger)
		s.needs.queue(s.core.takeNeedOps())
	}
	s.cfg.Store(cfg)
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.snap.Store(&snapshot{profiles: s.core.profileStates(), upstream: map[string]string{}})
	return s, nil
}

// Run owns the core until shutdown completes (Shutdown, or ctx ending,
// which shuts down at once with SIGKILL). It returns after every child has
// exited and every helper goroutine has finished. Call it once.
func (s *Scheduler) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("scheduler: Run called twice")
	}
	defer close(s.runDone)
	if s.needs != nil {
		go s.needs.run()
		defer s.needs.close() // what is queued is written before Run returns
	}
	var stopTimer func() bool
	var timerAt time.Time
	ctxDone := ctx.Done()

	s.step(evTick{})
	for !s.core.finished {
		if dl, ok := s.core.nextDeadline(); ok && !dl.Equal(timerAt) {
			if stopTimer != nil {
				stopTimer()
			}
			timerAt = dl
			stopTimer = s.opts.Clock.AfterFunc(dl.Sub(s.opts.Clock.Now()), func() {
				select {
				case s.tick <- struct{}{}:
				default:
				}
			})
		}
		var ev event
		select {
		case ev = <-s.events:
		case <-s.tick:
			ev = evTick{}
		case <-s.opts.View.Changed():
			ev = evViewChanged{}
		case <-ctxDone:
			ev, ctxDone = evShutdown{force: true}, nil
		}
		s.step(ev)
	}
	if stopTimer != nil {
		stopTimer()
	}
	close(s.loopDone)
	s.bgCancel()
	s.wg.Wait()
	s.subsMu.Lock()
	for sub := range s.subs {
		delete(s.subs, sub)
		close(sub.ch)
	}
	s.subsMu.Unlock()
	return nil
}

// step runs the step function, refreshes the snapshot, then performs the
// effects and publishes the feed. The snapshot comes first so that a caller
// woken by a reply (say, a new lease) already sees the state it implies.
func (s *Scheduler) step(ev event) {
	s.core.handle(ev)
	up := map[string]string{}
	for name := range s.core.insts {
		if a, ok := s.core.upstream(name); ok {
			up[name] = a
		}
	}
	s.snap.Store(&snapshot{running: s.core.runningList(), recent: s.core.recentLoads(), profiles: s.core.profileStates(), upstream: up})
	for _, ef := range s.core.takeEffects() {
		s.exec(ef)
	}
	// The history hand-off only appends to a bounded in-memory ring and a
	// buffered channel: it never blocks the loop (file writes are off the loop).
	if evs := s.core.takeHistory(); len(evs) > 0 && s.opts.History != nil {
		for _, e := range evs {
			s.opts.History.Record(e)
		}
	}
	if feed := s.core.takeFeed(); len(feed) > 0 {
		s.publish(feed)
	}
	// Learned-need writes go to the writer's goroutine; the loop never waits
	// on the database.
	if ops := s.core.takeNeedOps(); len(ops) > 0 && s.needs != nil {
		s.needs.queue(ops)
	}
}

// post hands an event to the loop. It fails once the loop has stopped.
func (s *Scheduler) post(ctx context.Context, ev event) bool {
	select {
	case s.events <- ev:
		return true
	case <-s.loopDone:
		return false
	case <-ctx.Done():
		return false
	}
}

func (s *Scheduler) goBg(f func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f(s.bgCtx)
	}()
}

// exec performs one effect. Only effReply, effStop and effCancelHealth run
// inline; they never block.
func (s *Scheduler) exec(ef effect) {
	switch ef := ef.(type) {
	case effReply:
		ef.w.reply <- ef.a // room for exactly one answer
	case effStop:
		ef.child.Stop(ef.mode)
	case effCancelHealth:
		s.healthMu.Lock()
		if cancel := s.healthCancel[ef.iid]; cancel != nil {
			cancel()
			delete(s.healthCancel, ef.iid)
		}
		s.healthMu.Unlock()
	case effProbe:
		s.goBg(func(ctx context.Context) {
			out := make([]probeOutcome, len(ef.targets))
			var wg sync.WaitGroup
			for i, tg := range ef.targets {
				wg.Add(1)
				go func() {
					defer wg.Done()
					began := s.opts.Clock.Now()
					r, err := s.opts.Prober.Probe(ctx, tg.addr)
					out[i] = probeOutcome{id: tg.id, addr: tg.addr, result: r, err: err, took: s.opts.Clock.Now().Sub(began)}
				}()
			}
			wg.Wait()
			s.post(context.Background(), evPreflight{iid: ef.iid, results: out})
		})
	case effStart:
		s.goBg(func(ctx context.Context) { s.startChild(ctx, ef) })
	case effSample:
		// Reading the kernel's socket table is off the event loop; the
		// result comes back as an event.
		s.goBg(func(context.Context) {
			sc, err := ef.counter.SentBytes(ef.peers)
			s.post(context.Background(), evLoadSample{iid: ef.iid, bytes: sc.Bytes, sockets: sc.Sockets, peers: sc.Peers, err: err})
		})
	case effWorkerProbe:
		// A plain connect with a timeout, off the event loop.
		s.goBg(func(ctx context.Context) {
			err := s.opts.WorkerProber.Connect(ctx, ef.addr, workerProbeTimeout)
			s.post(context.Background(), evWorkerProbe{id: ef.id, gen: ef.gen, err: err})
		})
	case effRecoveryPost:
		// The POST to the fleet manager, off the event loop.
		s.goBg(func(ctx context.Context) {
			r, err := s.opts.RecoveryPoster.Post(ctx, ef.url, ef.body, ef.timeout)
			s.post(context.Background(), evRecoveryPosted{id: ef.id, epoch: ef.epoch, action: ef.action, nonce: ef.nonce,
				status: r.Status, tail: r.BodyTail, err: err})
		})
	case effFetch:
		s.goBg(func(ctx context.Context) {
			n, err := s.opts.View.FetchNode(ctx, ef.id)
			s.post(context.Background(), evFetched{id: ef.id, gen: ef.gen, node: n, err: err})
		})
	default:
		s.execAgents(ef)
	}
}

func (s *Scheduler) startChild(ctx context.Context, ef effStart) {
	ch, err := s.opts.Launcher.Start(ef.spec)
	if err != nil {
		s.post(context.Background(), evChildStarted{iid: ef.iid, err: err})
		return
	}
	hctx, cancel := context.WithCancel(ctx)
	s.healthMu.Lock()
	s.healthCancel[ef.iid] = cancel
	s.healthMu.Unlock()
	if !s.post(context.Background(), evChildStarted{iid: ef.iid, child: ch}) {
		// The loop is gone (shutdown raced the start): end the child here.
		cancel()
		ch.Stop(instance.StopKill)
		<-ch.Exited()
		return
	}
	s.goBg(func(context.Context) {
		st, ok := <-ch.Exited()
		if ok {
			s.post(context.Background(), evExited{iid: ef.iid, status: st, workers: s.workerExits(ef.iid, st)})
		}
	})
	s.goBg(func(context.Context) {
		err := ch.WaitHealthy(hctx)
		s.healthMu.Lock()
		delete(s.healthCancel, ef.iid)
		s.healthMu.Unlock()
		cancel()
		s.post(context.Background(), evHealthy{iid: ef.iid, err: err})
	})
}

// ---- public API ----

// Lease is an admitted request. Call Release exactly once when the
// response has ended (extra calls are ignored).
type Lease struct {
	ID      uint64 // request id, also used in the feed
	Addr    string // upstream address, 127.0.0.1:port
	Profile string // profile name, never an alias
	Alias   bool   // the request named an alias (log it with the caller's address)
	// StallTimeout is the profile's stall_timeout for the proxy (8.4): the
	// longest gap between streamed chunks after the first; 0 disables it.
	StallTimeout time.Duration

	once sync.Once
	s    *Scheduler
}

// Release ends the request: in_flight--, last_used = now.
func (l *Lease) Release() {
	l.once.Do(func() { l.s.post(context.Background(), evRelease{id: l.ID}) })
}

func errShutting() *Error {
	return err503(ReasonShuttingDown, "the scheduler is shutting down", queueRetryAfter)
}

func errGone() *Error {
	return &Error{Status: StatusClientClosed, Reason: ReasonClientGone, Message: "client disconnected while waiting"}
}

// Acquire resolves model (a profile name or alias, design 8.2 step 1) and
// blocks until the request is admitted to a ready instance or fails. The
// error is always an *Error carrying the HTTP status. When ctx ends while
// waiting, the request is withdrawn (8.2 step 5) and an *Error with
// ReasonClientGone is returned; a lease granted at the same moment is
// released.
func (s *Scheduler) Acquire(ctx context.Context, model string) (*Lease, error) {
	return s.AcquireFor(ctx, model, "")
}

// AcquireFor is Acquire for a request that carries a bench run id (the
// X-Bench-Run header). The id only tags the history records of the load the
// request causes (data.bench_run); scheduling is unchanged.
func (s *Scheduler) AcquireFor(ctx context.Context, model, benchRun string) (*Lease, error) {
	p, alias, ok := s.Config().Resolve(model)
	if !ok {
		return nil, errUnknown(model)
	}
	w := &waiter{profile: p.Name, alias: alias, reply: make(chan answer, 1), benchRun: benchRun}
	if !s.post(ctx, evAcquire{w: w}) {
		if ctx.Err() != nil {
			return nil, errGone()
		}
		return nil, errShutting()
	}
	select {
	case a := <-w.reply:
		return s.lease(a)
	case <-ctx.Done():
		if s.post(context.Background(), evCancel{w: w}) {
			select {
			case a := <-w.reply:
				if a.grant != nil {
					l, _ := s.lease(a)
					l.Release()
				}
			case <-s.loopDone:
			}
		}
		return nil, errGone()
	case <-s.loopDone:
		select {
		case a := <-w.reply:
			return s.lease(a)
		default:
			return nil, errShutting()
		}
	}
}

func (s *Scheduler) lease(a answer) (*Lease, error) {
	if a.err != nil {
		return nil, a.err
	}
	g := a.grant
	cfg := s.Config()
	stall := cfg.Defaults.StallTimeout // the profile was deleted since the grant
	if p := cfg.Profiles[g.Profile]; p != nil {
		stall = cfg.EffectiveStallTimeout(p)
	}
	return &Lease{ID: g.ID, Addr: g.Addr, Profile: g.Profile, Alias: g.Alias, StallTimeout: stall, s: s}, nil
}

// Load is POST /api/load: a waiter that never disconnects (8.2 step 6).
// It returns at once: nil (202) when the load is under way, queued or not
// needed, else the *Error a request would get now (unknown, backoff,
// does not fit, shutting down).
func (s *Scheduler) Load(ctx context.Context, model string) error {
	return s.LoadFor(ctx, model, "")
}

// LoadFor is Load for a request that carries a bench run id (the X-Bench-Run
// header): the id only tags the history records of the load (data.bench_run).
func (s *Scheduler) LoadFor(ctx context.Context, model, benchRun string) error {
	p, _, ok := s.Config().Resolve(model)
	if !ok {
		return errUnknown(model)
	}
	w := &waiter{profile: p.Name, synthetic: true, reply: make(chan answer, 1), benchRun: benchRun}
	if !s.post(ctx, evAcquire{w: w}) {
		return errShutting()
	}
	// Events are handled in order and effects delivered before the next
	// one, so after this query any immediate answer is in w.reply.
	if err := s.query(ctx, func(*schedCore) {}); err != nil {
		return err
	}
	select {
	case a := <-w.reply:
		if a.err != nil {
			return a.err
		}
	default:
	}
	return nil
}

// ask runs fn on the core goroutine and returns its result. The result
// travels over a channel the query owns, so a caller that gave up (its
// context ended while the loop was busy) gets the context error and a zero
// T, and the loop, when it runs fn later, writes nothing the caller holds.
// Every method that needs an answer from the core goes through ask.
func ask[T any](s *Scheduler, ctx context.Context, fn func(c *schedCore) T) (T, error) {
	out := make(chan T, 1)
	if err := s.query(ctx, func(c *schedCore) { out <- fn(c) }); err != nil {
		var zero T
		return zero, err
	}
	return <-out, nil // query returned nil: fn has run and sent
}

// query runs fn on the core goroutine and waits for it, or for ctx. fn may
// still run after query returned (ctx ended first), so it must not write
// memory of the caller's: use ask for a result.
func (s *Scheduler) query(ctx context.Context, fn func(c *schedCore)) error {
	done := make(chan struct{})
	if !s.post(ctx, evQuery{fn: func(c *schedCore) { fn(c); close(done) }}) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errShutting()
	}
	select {
	case <-done:
		return nil
	case <-s.loopDone:
		select {
		case <-done:
			return nil
		default:
			return errShutting()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unload is POST /api/unload and /api/models/unload/{model}: stop the
// instance if idle; force stops it even if busy or loading. ErrNotLoaded
// and ErrBusy report why nothing was done.
func (s *Scheduler) Unload(ctx context.Context, model string, force bool) error {
	p, _, ok := s.Config().Resolve(model)
	if !ok {
		return errUnknown(model)
	}
	err, qerr := ask(s, ctx, func(c *schedCore) error { return c.unload(p.Name, force) })
	if qerr != nil {
		return qerr
	}
	return err
}

// UnloadAllIdle stops every ready instance with nothing in flight
// (POST /api/models/unload, GET /unload) and returns their profiles.
func (s *Scheduler) UnloadAllIdle(ctx context.Context) ([]string, error) {
	return ask(s, ctx, func(c *schedCore) []string { return c.unloadIdle() })
}

// Status is the /api/status document.
func (s *Scheduler) Status(ctx context.Context) (Status, error) {
	return ask(s, ctx, func(c *schedCore) Status { return c.statusDoc() })
}

// Plan is POST /api/plan: what a request for model would meet now. It
// changes nothing.
func (s *Scheduler) Plan(ctx context.Context, model string) (PlanReport, error) {
	p, alias, ok := s.Config().Resolve(model)
	if !ok {
		return PlanReport{}, errUnknown(model)
	}
	r, err := ask(s, ctx, func(c *schedCore) PlanReport { return c.dryPlan(p.Name) })
	if err != nil {
		return PlanReport{}, err
	}
	r.Alias = alias
	return r, nil
}

// ForgetNeed drops the learned memory need of model (a profile name or
// alias), in memory and in the store, so fit decisions use the estimate or
// needs_mib again (for when the hardware changed). It reports whether there
// was one.
func (s *Scheduler) ForgetNeed(ctx context.Context, model string) (bool, error) {
	name := model
	if p, _, ok := s.Config().Resolve(model); ok {
		name = p.Name
	}
	type forgot struct {
		had bool
		err error
	}
	a, qerr := ask(s, ctx, func(c *schedCore) forgot { had, err := c.forgetNeed(name); return forgot{had, err} })
	if qerr != nil {
		return false, qerr
	}
	had, err := a.had, a.err
	if errors.Is(err, ErrNoProfile) {
		return false, errUnknown(model)
	}
	return had, err
}

// LearnedRows lists every learned memory need, by shape and device count
// (GET /api/needs).
func (s *Scheduler) LearnedRows(ctx context.Context) ([]LearnedRow, error) {
	return ask(s, ctx, func(c *schedCore) []LearnedRow { return c.learnedRows() })
}

// ForgetShape drops every learned memory need of a shape (a LearnedRow's
// Shape), in memory and in the database; it reports whether there was one.
func (s *Scheduler) ForgetShape(ctx context.Context, shape string) (bool, error) {
	return ask(s, ctx, func(c *schedCore) bool { return c.forgetShape(shape) })
}

// Upstream is the address for /upstream/{model}: set only while the
// profile's instance is ready or draining. It never waits.
func (s *Scheduler) Upstream(model string) (string, bool) {
	p, _, ok := s.Config().Resolve(model)
	if !ok {
		return "", false
	}
	a, ok := s.snap.Load().upstream[p.Name]
	return a, ok
}

// Running is the /running list (design A.1). It never waits.
func (s *Scheduler) Running() []RunningInstance {
	return append([]RunningInstance(nil), s.snap.Load().running...)
}

// RecentLoads is the outcome of the load attempts that finished within
// RecentLoadRetention, newest first, for /running. It never waits.
func (s *Scheduler) RecentLoads() []LoadOutcome {
	cut := s.opts.Clock.Now().Add(-RecentLoadRetention).UnixMilli()
	out := []LoadOutcome{}
	for _, o := range s.snap.Load().recent {
		if o.FinishedAt >= cut {
			out = append(out, o)
		}
	}
	return out
}

// Profiles is every profile with its state, for modelStatus (A.3).
func (s *Scheduler) Profiles() []ProfileState {
	return append([]ProfileState(nil), s.snap.Load().profiles...)
}

// Shutdown is design 8.5. It returns when every child has exited. If ctx
// ends first, everything is killed at once, Shutdown waits for that, and
// returns ctx.Err().
func (s *Scheduler) Shutdown(ctx context.Context) error {
	if !s.started.Load() {
		return errors.New("scheduler: not running")
	}
	if s.post(ctx, evShutdown{}) {
		select {
		case <-s.runDone:
			return nil
		case <-ctx.Done():
		}
	}
	select {
	case <-s.runDone:
		return nil
	default:
	}
	s.post(context.Background(), evShutdown{force: true})
	<-s.runDone
	return ctx.Err()
}

// ---- feed ----

type subscriber struct{ ch chan FeedEvent }

// Subscribe returns a feed of instance state changes and request
// admissions/endings (design A.3, A.4) and a cancel function. Each
// subscriber has a buffer of the given size (at least 1); when it is full
// the OLDEST event is dropped, so a slow subscriber never blocks the core.
// FeedEvent.Seq reveals gaps. The channel is closed by cancel or when Run
// returns.
func (s *Scheduler) Subscribe(buffer int) (<-chan FeedEvent, func()) {
	sub := &subscriber{ch: make(chan FeedEvent, max(buffer, 1))}
	s.subsMu.Lock()
	select {
	case <-s.runDone:
		close(sub.ch)
	default:
		s.subs[sub] = struct{}{}
	}
	s.subsMu.Unlock()
	return sub.ch, func() {
		s.subsMu.Lock()
		defer s.subsMu.Unlock()
		if _, ok := s.subs[sub]; ok {
			delete(s.subs, sub)
			close(sub.ch)
		}
	}
}

func (s *Scheduler) publish(evs []FeedEvent) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for sub := range s.subs {
		for _, ev := range evs {
			for {
				select {
				case sub.ch <- ev:
				default:
					select {
					case <-sub.ch: // drop the oldest
					default:
					}
					continue
				}
				break
			}
		}
	}
}

// Config is the configuration snapshot now in force. It never waits.
func (s *Scheduler) Config() *config.Config { return s.cfg.Load() }

// ApplyConfig hands the scheduler a new immutable configuration snapshot. It
// returns at once: request goroutines see the new snapshot immediately, and
// the core swaps to it as an event, between two steps. It reports false when
// the scheduler has stopped.
func (s *Scheduler) ApplyConfig(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	s.cfg.Store(cfg)
	select {
	case s.events <- evConfig{cfg: cfg}:
		return true
	case <-s.loopDone:
		return false
	}
}
