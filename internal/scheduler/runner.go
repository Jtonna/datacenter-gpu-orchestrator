package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
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
	// HostReadings returns the history's learnedNeed events; the host
	// overheads copied from the release before device counts read their
	// readings from it once (hostoverhead.go). nil: none.
	HostReadings HostReadingEvents
	// DeviceHistory answers event-history queries: the stored per-device
	// rows whose load was a cached reload are found with it and dropped
	// once at start (deviceoverhead.go). nil: none.
	DeviceHistory DeviceHistory
	// LlamaBuild is the llama.cpp commit the loads run on (the host
	// program's); stored with each measurement, and a restored measurement
	// of another build is not trusted (needcount.go). "" when not known.
	LlamaBuild string
	// ReleaseBuild gives the llama build a program release's release.json
	// names ("" when not known): the served release's is what the workers
	// run, and a stored need without a build is stamped through its own
	// release's (needcount.go). nil: none is known.
	ReleaseBuild func(release string) string
	// HostBuild reads the host program's llama build again. When the host
	// build or the served release's build is not known at start, Run reads
	// both again in the background (backoff from 5 s to 5 min) until they
	// are, then decides trust (builds.go; fourth review, FS2). nil: the host
	// build is only LlamaBuild.
	HostBuild func(ctx context.Context) (string, error)
	// ReadSlotCtx reads a ready llama server's context per slot (/props)
	// after every load (realctx.go); default instance.ReadSlotCtx.
	ReadSlotCtx func(ctx context.Context, addr, key string) (int, error)
	// LocalMemory reads the scheduler machine's available memory in MiB
	// (MemAvailable), sampled every localMemEvery by Run; a change of
	// localMemStep MiB or more, or an error, is posted to the core
	// (hostbudget.go). nil: never read, and the host memory of loads hosted
	// on the scheduler's machine is not checked.
	LocalMemory func() (LocalMem, error)
	// ProcMemory reads a host process's anonymous resident memory in MiB
	// (RssAnon), for the models hosted on the scheduler's machine, read with
	// LocalMemory. nil: their growth is not read, and each counts at its
	// full maximum ("conservative").
	ProcMemory func(pid int) (int, error)
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

	// dryPlans counts the dry-run plans asked of the loop (Plan), from any
	// caller: one step of the loop each
	dryPlans atomic.Int64

	events   chan event
	tick     chan struct{}
	started  atomic.Bool
	loopDone chan struct{} // closed when the loop stops taking events
	// readPanics counts read-only queries that panicked (askRead).
	readPanics atomic.Int64
	// stepMax is the longest time one event took in the core loop, in
	// nanoseconds, with stepMaxEv its event type (tests: the loop must
	// never stall).
	stepMax   atomic.Int64
	stepMaxEv atomic.Value
	runDone   chan struct{} // closed when Run has returned
	snap      atomic.Pointer[snapshot]

	// loop is the production loop health (loophealth.go).
	loop loopHealth

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
	upKeys   map[string]string // the key of an upstream that wants one (a host on a node)
	upPlace  map[string]UpstreamPlacement
	scoped   map[string]*core.Profile // scoped.go: the run-scoped profiles held now (never modified)
	// hostPIDs: iid -> pid of the ready host processes on this machine
	// (hostbudget.go startLocalMemory reads their memory)
	hostPIDs map[uint64]int
}

// UpstreamPlacement is where a ready instance runs, for a passthrough
// request that holds no lease: what the request log keeps of its machines.
type UpstreamPlacement struct {
	Instance uint64
	Session  string
	Nodes    []LeaseNode // shared read-only: do not modify
}

// New builds a scheduler for a validated config. Call Run to start it.
func New(cfg *config.Config, o Options) (*Scheduler, error) {
	if cfg == nil || o.View == nil || o.Prober == nil || o.Launcher == nil {
		return nil, errors.New("scheduler: config, View, Prober and Launcher are required")
	}
	if o.ReadSlotCtx == nil {
		o.ReadSlotCtx = instance.ReadSlotCtx
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
	s.core.hostEventsFn = o.HostReadings
	s.core.devEventsFn = o.DeviceHistory
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
		s.needs.start()
		defer s.needs.close() // what is queued is written before Run returns
	}
	s.startBuildReads()  // builds.go
	s.startLocalMemory() // hostbudget.go
	s.warmHeaders()      // derive.go
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
	func() {
		s.subsMu.Lock()
		defer supervise.Unlocked("scheduler", s.subsMu.Unlock)
		for sub := range s.subs {
			delete(s.subs, sub)
			close(sub.ch)
		}
	}()
	return nil
}

// step runs the step function, refreshes the snapshot, then performs the
// effects and publishes the feed. The snapshot comes first so that a caller
// woken by a reply (say, a new lease) already sees the state it implies.
func (s *Scheduler) step(ev event) {
	supervise.Point("scheduler.loop", s) // test hook: a panic here must end the process (cmd/llm-scheduler)
	t0 := time.Now()
	s.loop.begin(t0, ev)
	defer func() {
		t1 := time.Now()
		d := t1.Sub(t0).Nanoseconds()
		if d > s.stepMax.Load() {
			s.stepMax.Store(d)
			s.stepMaxEv.Store(eventName(ev))
		}
		warn := s.loopWarn()
		if s.loop.end(t1, time.Duration(d), func() string { return eventName(ev) }, warn) && s.opts.History != nil {
			s.opts.History.Record(slowStepEvent(t1, time.Duration(d), warn, eventName(ev)))
		}
	}()
	s.core.handle(ev)
	up, keys, place := map[string]string{}, map[string]string{}, map[string]UpstreamPlacement{}
	for name, in := range s.core.insts {
		if a, ok := s.core.upstream(name); ok {
			up[name] = a
			if k := instance.APIKeyOf(in.child); k != "" {
				keys[name] = k
			}
			place[name] = UpstreamPlacement{Instance: in.iid, Session: in.sessionID, Nodes: s.core.leaseNodesOf(in)} // built once per instance
		}
	}
	pids := map[uint64]int{}
	for _, in := range s.core.insts {
		if (in.state == stReady || in.state == stDraining) && in.child != nil && serverHosted(in.prof, in.placement) {
			if pid := in.child.PID(); pid > 0 {
				pids[in.iid] = pid
			}
		}
	}
	s.snap.Store(&snapshot{running: s.core.runningList(), recent: s.core.recentLoads(), profiles: s.core.profileStates(), upstream: up, upKeys: keys, upPlace: place, scoped: maps.Clone(s.core.scoped), hostPIDs: pids})
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

// goBg runs a one-shot effect worker off the loop. The core waits for the
// result event each one posts, so onPanic must post that event as a failure
// (background-task isolation): a recovered worker that posts nothing would
// leave its instance stuck.
//
// The worker is counted in s.wg until onPanic has returned too, so that Run
// does not return while a failed start still kills its child.
func (s *Scheduler) goBg(name string, f func(ctx context.Context), onPanic func(p any)) {
	supervise.OnceIn(&s.wg, name, func() { f(s.bgCtx) }, onPanic)
}

// internalErr is the failure result of an effect worker that panicked.
func internalErr(p any) error { return fmt.Errorf("internal error: %v", p) }

// exec performs one effect. Only effReply, effStop and effCancelHealth run
// inline; they never block.
func (s *Scheduler) exec(ef effect) {
	switch ef := ef.(type) {
	case effReply:
		ef.w.reply <- ef.a // room for exactly one answer
	case effStop:
		ef.child.Stop(ef.mode)
	case effCancelHealth:
		func() {
			s.healthMu.Lock()
			defer supervise.Unlocked("scheduler", s.healthMu.Unlock)
			if cancel := s.healthCancel[ef.iid]; cancel != nil {
				cancel()
				delete(s.healthCancel, ef.iid)
			}
		}()
	case effProbe:
		failed := func(err error) []probeOutcome {
			out := make([]probeOutcome, len(ef.targets))
			for i, tg := range ef.targets {
				out[i] = probeOutcome{id: tg.id, addr: tg.addr, err: err}
			}
			return out
		}
		s.goBg("scheduler.probe", func(ctx context.Context) {
			out := make([]probeOutcome, len(ef.targets))
			var wg sync.WaitGroup
			fan := supervise.NewFanout("scheduler.probe") // one pre-flight's targets count once toward escalation
			for i, tg := range ef.targets {
				fan.Go(&wg, func() {
					began := s.opts.Clock.Now()
					r, err := s.opts.Prober.Probe(ctx, tg.addr)
					out[i] = probeOutcome{id: tg.id, addr: tg.addr, result: r, err: err, took: s.opts.Clock.Now().Sub(began)}
				}, func(any) { // a panic makes this target's result an error
					out[i] = probeOutcome{id: tg.id, addr: tg.addr, err: errors.New("internal error in the probe")}
				})
			}
			wg.Wait()
			s.post(context.Background(), evPreflight{iid: ef.iid, results: out})
		}, func(p any) { s.post(context.Background(), evPreflight{iid: ef.iid, results: failed(internalErr(p))}) })
	case effStart:
		st := &startState{}
		s.goBg("scheduler.start", func(ctx context.Context) { s.startChild(ctx, ef, st) }, func(p any) {
			if st.posted.Load() {
				return // the core has the child; its exit watcher reports the rest
			}
			if ch := st.child.Load(); ch != nil {
				(*ch).Stop(instance.StopKill)
			}
			s.post(context.Background(), evChildStarted{iid: ef.iid, err: internalErr(p)})
		})
	case effSample:
		// Reading the kernel's socket table is off the event loop; the
		// result comes back as an event.
		s.goBg("scheduler.sample", func(context.Context) {
			sc, err := ef.counter.SentBytes(ef.peers)
			s.post(context.Background(), evLoadSample{iid: ef.iid, bytes: sc.Bytes, sockets: sc.Sockets, peers: sc.Peers, err: err})
		}, func(p any) { s.post(context.Background(), evLoadSample{iid: ef.iid, err: internalErr(p)}) })
	case effWorkerProbe:
		// A plain connect with a timeout, off the event loop.
		s.goBg("scheduler.worker_probe", func(ctx context.Context) {
			err := s.opts.WorkerProber.Connect(ctx, ef.addr, workerProbeTimeout)
			s.post(context.Background(), evWorkerProbe{id: ef.id, gen: ef.gen, err: err})
		}, func(p any) { s.post(context.Background(), evWorkerProbe{id: ef.id, gen: ef.gen, err: internalErr(p)}) })
	case effRecoveryPost:
		// The POST to the fleet manager, off the event loop.
		s.goBg("scheduler.recovery_post", func(ctx context.Context) {
			r, err := s.opts.RecoveryPoster.Post(ctx, ef.url, ef.body, ef.timeout)
			s.post(context.Background(), evRecoveryPosted{id: ef.id, epoch: ef.epoch, action: ef.action, nonce: ef.nonce,
				status: r.Status, tail: r.BodyTail, err: err})
		}, func(p any) {
			s.post(context.Background(), evRecoveryPosted{id: ef.id, epoch: ef.epoch, action: ef.action, nonce: ef.nonce, err: internalErr(p)})
		})
	case effFetch:
		s.goBg("scheduler.fetch", func(ctx context.Context) {
			n, err := s.opts.View.FetchNode(ctx, ef.id)
			s.post(context.Background(), evFetched{id: ef.id, gen: ef.gen, node: n, err: err})
		}, func(p any) { s.post(context.Background(), evFetched{id: ef.id, gen: ef.gen, err: internalErr(p)}) })
	default:
		s.execAgents(ef)
	}
}

// startState is what a start worker did before it failed: whether it
// started the child and whether it told the core.
type startState struct {
	child  atomic.Pointer[instance.Child]
	posted atomic.Bool
}

func (s *Scheduler) startChild(ctx context.Context, ef effStart, st *startState) {
	var ch instance.Child
	var err error
	if ef.host != nil {
		ch, err = s.startHost(ctx, *ef.host) // hostnode.go
	} else {
		ch, err = s.opts.Launcher.Start(ef.spec)
	}
	if err != nil {
		st.posted.Store(true)
		s.post(context.Background(), evChildStarted{iid: ef.iid, err: err})
		return
	}
	st.child.Store(&ch)
	supervise.Point("scheduler.start", s) // test hook: a failure after the child started
	hctx, cancel := context.WithCancel(ctx)
	func() {
		s.healthMu.Lock()
		defer supervise.Unlocked("scheduler", s.healthMu.Unlock)
		s.healthCancel[ef.iid] = cancel
	}()
	st.posted.Store(true)
	if !s.post(context.Background(), evChildStarted{iid: ef.iid, child: ch}) {
		// The loop is gone (shutdown raced the start): end the child here.
		cancel()
		ch.Stop(instance.StopKill)
		<-ch.Exited()
		return
	}
	s.goBg("scheduler.exit_watch", func(context.Context) {
		st, ok := <-ch.Exited()
		if ok {
			s.post(context.Background(), evExited{iid: ef.iid, status: st, workers: s.workerExits(ef.iid, st)})
		}
	}, func(p any) { // the child is ended so that the exit the core waits for is true
		ch.Stop(instance.StopKill)
		s.post(context.Background(), evExited{iid: ef.iid, status: instance.ExitStatus{Code: -1, Requested: true, Err: internalErr(p)}})
	})
	s.goBg("scheduler.health_wait", func(context.Context) {
		err := ch.WaitHealthy(hctx)
		func() {
			s.healthMu.Lock()
			defer supervise.Unlocked("scheduler", s.healthMu.Unlock)
			delete(s.healthCancel, ef.iid)
		}()
		cancel()
		s.post(context.Background(), evHealthy{iid: ef.iid, err: err})
		if err == nil && ef.spec.HealthPath != "" && ef.spec.HealthPath != "/health" {
			return // a command profile: no model server context
		}
		if err == nil {
			n, perr := s.opts.ReadSlotCtx(ctx, instance.AddrOf(ch), instance.APIKeyOf(ch))
			s.post(context.Background(), evServedCtx{iid: ef.iid, ctx: n, err: perr})
		}
	}, func(p any) {
		cancel()
		s.post(context.Background(), evHealthy{iid: ef.iid, err: internalErr(p)})
	})
}

// ---- public API ----

// Lease is an admitted request. Call Release exactly once when the
// response has ended (extra calls are ignored).
type Lease struct {
	ID   uint64 // request id, also used in the feed
	Addr string // upstream address: 127.0.0.1:port, or a host node's address (phase 6)
	// APIKey is sent as "Authorization: Bearer <key>" to Addr when set (a
	// host on a node has a per-launch key).
	APIKey string
	// Gone is closed when the instance is removed (it exited, was stopped
	// or its node was lost): a request still forwarded then is ended. Nil
	// never closes.
	Gone <-chan struct{}
	// CutBy, when the instance was stopped on purpose while the request
	// ran (a forced unload), is the answer the caller gets instead of the
	// broken connection: 503 unloaded, saying why. It returns nil when the
	// instance was not stopped on purpose; read it once the upstream failed.
	CutBy   func() *Error
	Profile string // profile name, never an alias
	Alias   bool   // the request named an alias (log it with the caller's address)
	// StallTimeout is the profile's stall_timeout for the proxy (8.4): the
	// longest gap between streamed chunks after the first; 0 disables it.
	StallTimeout time.Duration
	// Instance is the instance that serves the request, Session its load
	// session (the history's session_id) and Nodes its machines with their
	// roles, as placed when the request was admitted. Nodes is shared: never
	// modify it.
	Instance uint64
	Session  string
	Nodes    []LeaseNode
	// Ctx is the context per slot the instance runs with (the server's own
	// figure once read, else the planned one): the request log's ctx.
	Ctx int

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
	p, alias, ok := s.resolve(model)
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
	if p, _, ok := s.resolve(g.Profile); ok {
		stall = cfg.EffectiveStallTimeout(p)
	}
	return &Lease{ID: g.ID, Addr: g.Addr, APIKey: g.APIKey, Gone: g.Gone, CutBy: g.CutBy, Profile: g.Profile, Alias: g.Alias, StallTimeout: stall,
		Instance: g.Instance, Session: g.Session, Nodes: g.Nodes, Ctx: g.Ctx, s: s}, nil
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
	p, _, ok := s.resolve(model)
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

// LoadOn is POST /api/load with nodes: load model on those nodes only,
// as force_nodes would, for this load (pin.go). The profile is unchanged.
// A model already loaded or queued is refused (409).
func (s *Scheduler) LoadOn(ctx context.Context, model string, nodes []string) error {
	p, _, ok := s.Config().Resolve(model)
	if !ok {
		return errUnknown(model)
	}
	w := &waiter{profile: p.Name, synthetic: true, reply: make(chan answer, 1), pin: append([]string{}, nodes...)}
	if !s.post(ctx, evAcquire{w: w}) {
		return errShutting()
	}
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

// PlanOn is the plan of a load on chosen nodes (LoadOn); nothing changes.
func (s *Scheduler) PlanOn(ctx context.Context, model string, nodes []string) (PlanReport, error) {
	p, alias, ok := s.Config().Resolve(model)
	if !ok {
		return PlanReport{}, errUnknown(model)
	}
	r, err := askRead(s, ctx, "plan of "+p.Name+" on chosen nodes", func(c *schedCore) PlanReport {
		if e := c.pinConflict(p); e != nil {
			return PlanReport{Profile: p.Name, Action: PlanReject, Reason: e.Message}
		}
		if e := c.pinUnusable(p, nodes); e != nil {
			return PlanReport{Profile: p.Name, Action: PlanReject, Reason: e.Message}
		}
		return c.dryPlanOf(p.Name, pinCopy(p, nodes))
	})
	if err != nil {
		return PlanReport{}, err
	}
	r.Alias = alias
	return r, nil
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

// askRead is ask for a query that changes no core state: a panic in fn is
// recovered on the core goroutine (readGuard), counted (ReadPanics), and
// returned as a 500 error naming the query.
func askRead[T any](s *Scheduler, ctx context.Context, name string, fn func(c *schedCore) T) (T, error) {
	var zero T
	out := make(chan T, 1)
	failed := make(chan struct{})
	done := make(chan struct{})
	ev := evQuery{read: name,
		fn: func(c *schedCore) { out <- fn(c); close(done) },
		panicked: func(any) {
			s.readPanics.Add(1)
			close(failed)
		}}
	if !s.post(ctx, ev) {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		return zero, errShutting()
	}
	select {
	case <-done:
		return <-out, nil
	case <-failed:
		return zero, &Error{Status: http.StatusInternalServerError, Reason: ReasonInternal,
			Message: "the scheduler failed answering " + name + "; it goes on (see its log)"}
	case <-s.loopDone:
		select {
		case <-done:
			return <-out, nil
		default:
			return zero, errShutting()
		}
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// ReadPanics is how many read-only queries panicked and were recovered since
// the start (GET /api/system read_panics; 0 when all is well).
func (s *Scheduler) ReadPanics() int64 { return s.readPanics.Load() }

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
	return s.UnloadFor(ctx, model, force, "")
}

// UnloadFor is Unload with why: a forced unload's requests in flight are
// answered 503 unloaded, "<profile> was unloaded: <why>" (without why,
// "<profile> was unloaded").
func (s *Scheduler) UnloadFor(ctx context.Context, model string, force bool, why string) error {
	p, _, ok := s.resolve(model)
	if !ok {
		return errUnknown(model)
	}
	err, qerr := ask(s, ctx, func(c *schedCore) error { return c.unloadFor(p.Name, force, why) })
	if qerr != nil {
		return qerr
	}
	return err
}

// CancelQueued answers the queued requests of the named profiles 503
// unloaded and drops their queue entries (cancelQueued); the number of
// requests answered.
func (s *Scheduler) CancelQueued(ctx context.Context, profiles []string) (int, error) {
	return ask(s, ctx, func(c *schedCore) int { return c.cancelQueued(profiles) })
}

// DropBenchLoads withdraws the loads benchmark run run sent for model that
// still wait in the queue (dropBenchLoads); the number withdrawn.
func (s *Scheduler) DropBenchLoads(ctx context.Context, model, run string) (int, error) {
	p, _, ok := s.resolve(model)
	if !ok {
		return 0, errUnknown(model)
	}
	return ask(s, ctx, func(c *schedCore) int { return c.dropBenchLoads(p.Name, run) })
}

// Drain is POST /api/models/drain/{model}: the instance admits nothing
// new and stops once its runs finish (drain.go); the answer is the ETA in
// seconds (nil: idle and stopped now, or no run length known yet).
func (s *Scheduler) Drain(ctx context.Context, model string) (*float64, error) {
	p, _, ok := s.resolve(model)
	if !ok {
		return nil, errUnknown(model)
	}
	type res struct {
		eta *float64
		err error
	}
	r, qerr := ask(s, ctx, func(c *schedCore) res { eta, err := c.drain(p.Name); return res{eta, err} })
	if qerr != nil {
		return nil, qerr
	}
	return r.eta, r.err
}

// Undrain takes a drain back: the instance admits again.
func (s *Scheduler) Undrain(ctx context.Context, model string) error {
	p, _, ok := s.resolve(model)
	if !ok {
		return errUnknown(model)
	}
	err, qerr := ask(s, ctx, func(c *schedCore) error { return c.undrain(p.Name) })
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
	return askRead(s, ctx, "status", func(c *schedCore) Status { return c.statusDoc() })
}

// Plan is POST /api/plan: what a request for model would meet now. It
// changes nothing.
// DryPlans is how many dry-run plans were asked of the loop so far.
func (s *Scheduler) DryPlans() int64 { return s.dryPlans.Load() }

func (s *Scheduler) Plan(ctx context.Context, model string) (PlanReport, error) {
	p, alias, ok := s.resolve(model)
	if !ok {
		return PlanReport{}, errUnknown(model)
	}
	s.dryPlans.Add(1)
	r, err := askRead(s, ctx, "plan of "+p.Name, func(c *schedCore) PlanReport { return c.dryPlan(p.Name) })
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
	return askRead(s, ctx, "learned needs", func(c *schedCore) []LearnedRow { return c.learnedRows() })
}

// Needs is GET /api/needs: the stored measurements and the two llama builds
// their trust depends on.
func (s *Scheduler) Needs(ctx context.Context) (NeedsDoc, error) {
	return askRead(s, ctx, "needs", func(c *schedCore) NeedsDoc {
		return NeedsDoc{HostBuild: c.llamaBuild, Release: c.buildRelease, ServedBuild: c.servedBuild, Rows: c.learnedRows(),
			DerivePairs: c.derivePairs(nil), DeriveToleranceMiB: deriveToleranceMiB,
			HostOverheads: c.hostOverheadRows(), HostOverheadSettingMiB: max(c.cfg.Defaults.HostOverheadMiB, 0),
			HostOverheadSettingOnly: c.cfg.Defaults.HostOverheadSettingOnly,
			DeviceOverheads:         c.deviceOverheadRows(), DeviceOverheadSettingMiB: max(c.cfg.Defaults.DeviceOverheadMiB, 0),
			DeviceOverheadSettingOnly: c.cfg.Defaults.DeviceOverheadSettingOnly}
	})
}

// ForgetShape drops every learned memory need of a shape (a LearnedRow's
// Shape), in memory and in the database; it reports whether there was one.
func (s *Scheduler) ForgetShape(ctx context.Context, shape string) (bool, error) {
	return ask(s, ctx, func(c *schedCore) bool { return c.forgetShape(shape) })
}

// Upstream is the address for /upstream/{model}: set only while the
// profile's instance is ready or draining. It never waits.
func (s *Scheduler) Upstream(model string) (string, bool) {
	p, _, ok := s.resolve(model)
	if !ok {
		return "", false
	}
	a, ok := s.snap.Load().upstream[p.Name]
	return a, ok
}

// UpstreamPlacement is where Upstream(model)'s instance runs; false when
// it is not ready. It never waits and takes no lease.
func (s *Scheduler) UpstreamPlacement(model string) (UpstreamPlacement, bool) {
	p, _, ok := s.resolve(model)
	if !ok {
		return UpstreamPlacement{}, false
	}
	pl, ok := s.snap.Load().upPlace[p.Name]
	return pl, ok
}

// UpstreamKey is the key requests to Upstream(model) must carry as
// "Authorization: Bearer <key>"; "" when none (phase 6: a host on a node).
func (s *Scheduler) UpstreamKey(model string) string {
	p, _, ok := s.resolve(model)
	if !ok {
		return ""
	}
	return s.snap.Load().upKeys[p.Name]
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
	func() {
		s.subsMu.Lock()
		defer supervise.Unlocked("scheduler", s.subsMu.Unlock)
		select {
		case <-s.runDone:
			close(sub.ch)
		default:
			s.subs[sub] = struct{}{}
		}
	}()
	return sub.ch, func() {
		s.subsMu.Lock()
		defer supervise.Unlocked("scheduler", s.subsMu.Unlock)
		if _, ok := s.subs[sub]; ok {
			delete(s.subs, sub)
			close(sub.ch)
		}
	}
}

func (s *Scheduler) publish(evs []FeedEvent) {
	s.subsMu.Lock()
	defer supervise.Unlocked("scheduler", s.subsMu.Unlock)
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

// eventName names an event for stepMax: a query by what it reads.
func eventName(ev event) string {
	if q, ok := ev.(evQuery); ok {
		return "read " + q.read
	}
	return fmt.Sprintf("%T", ev)
}
