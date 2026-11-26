package scheduler

import (
	"fmt"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// ---- events (inputs to the step function) ----

type event interface{ isEvent() }

type (
	// evAcquire: a request arrived (8.2). The waiter carries the resolved
	// profile; its id is assigned by the core.
	evAcquire struct{ w *waiter }
	// evCancel: the client of a waiting request went away (8.2 step 5).
	evCancel struct{ w *waiter }
	// evRelease: an admitted request ended.
	evRelease struct{ id uint64 }
	// evPreflight: all probes of a starting instance finished (7.5).
	evPreflight struct {
		iid     uint64
		results []probeOutcome
	}
	// evChildStarted: Launcher.Start returned.
	evChildStarted struct {
		iid   uint64
		child instance.Child
		err   error
	}
	// evHealthy: WaitHealthy returned.
	evHealthy struct {
		iid uint64
		err error
	}
	// evServedCtx: the server's own context per slot, read from /props after
	// the load (realctx.go).
	evServedCtx struct {
		iid uint64
		ctx int
		err error
	}
	// evExited: the child's Exited channel delivered.
	evExited struct {
		iid    uint64
		status instance.ExitStatus
		// workers: the load's worker launches that had ended by themselves
		// when the child exited (agents membership; read by the runner
		// before the event is posted).
		workers []WorkerReport
	}
	// evFetched: the post-settle fetch of a node finished.
	evFetched struct {
		id   core.NodeID
		gen  uint64
		node core.Node
		err  error
	}
	// evLoadSample: a count of the bytes a loading child has sent to its
	// workers finished (see effSample).
	evLoadSample struct {
		iid     uint64
		bytes   int64
		sockets int
		peers   map[string]int64 // per worker, by address; nil when not counted per worker
		err     error
	}
	// evWorkerProbe: a worker connect probe finished (see effWorkerProbe).
	evWorkerProbe struct {
		id  core.NodeID
		gen uint64
		err error
	}
	// evRecoveryPosted: a recovery POST finished (see effRecoveryPost).
	evRecoveryPosted struct {
		id     core.NodeID
		epoch  uint64
		action string
		nonce  string
		status int
		tail   string
		err    error
	}
	// evConfig swaps in a new configuration snapshot between steps.
	evConfig      struct{ cfg *config.Config }
	evViewChanged struct{}
	evTick        struct{}
	evUnload      struct {
		profile string
		force   bool
		reply   chan error
	}
	evUnloadIdle struct{ reply chan []string }
	evShutdown   struct{ force bool }
	// evQuery runs fn inside the core goroutine (snapshots, dry runs).
	// read names a read-only query: a panic in fn is recovered, logged and
	// reported with panicked (readGuard). A query without it, like every
	// state-changing event, still stops the scheduler on a panic.
	evQuery struct {
		fn       func(c *schedCore)
		read     string
		panicked func(v any)
	}
	// evBuilds: both llama builds are known after a background read (FS2).
	evBuilds struct{ host, served string }
)

func (evAcquire) isEvent()        {}
func (evCancel) isEvent()         {}
func (evRelease) isEvent()        {}
func (evPreflight) isEvent()      {}
func (evChildStarted) isEvent()   {}
func (evHealthy) isEvent()        {}
func (evServedCtx) isEvent()      {}
func (evExited) isEvent()         {}
func (evFetched) isEvent()        {}
func (evLoadSample) isEvent()     {}
func (evWorkerProbe) isEvent()    {}
func (evRecoveryPosted) isEvent() {}
func (evConfig) isEvent()         {}
func (evViewChanged) isEvent()    {}
func (evTick) isEvent()           {}
func (evUnload) isEvent()         {}
func (evUnloadIdle) isEvent()     {}
func (evShutdown) isEvent()       {}
func (evQuery) isEvent()          {}
func (evBuilds) isEvent()         {}

type probeOutcome struct {
	id     core.NodeID
	addr   string
	result instance.ProbeResult
	err    error
	took   time.Duration // how long the probe ran
}

// ---- effects (slow work the runner performs and reports back) ----

type effect interface{ isEffect() }

type probeTarget struct {
	id   core.NodeID
	addr string
}

type (
	// effProbe: pre-flight every target in parallel, then send one evPreflight.
	effProbe struct {
		iid     uint64
		targets []probeTarget
	}
	// effStart: Launcher.Start; send evChildStarted. On success also watch
	// Exited (evExited) and run WaitHealthy (evHealthy) until effCancelHealth.
	effStart struct {
		iid  uint64
		spec instance.StartSpec
		host *HostLaunchSpec // set: a host launch on a node (HostLauncher), not spec
	}
	effCancelHealth struct{ iid uint64 }
	effStop         struct {
		iid   uint64
		child instance.Child
		mode  instance.StopMode
	}
	// effSample: read how many bytes counter's child has sent to peers (off
	// the event loop); send evLoadSample.
	effSample struct {
		iid     uint64
		counter instance.ByteCounter
		peers   []string
	}
	// effFetch: View.FetchNode; send evFetched.
	effFetch struct {
		id  core.NodeID
		gen uint64
	}
	// effWorkerProbe: TCP connect to a worker's rpc address (off the event
	// loop, nothing sent); send evWorkerProbe.
	effWorkerProbe struct {
		id   core.NodeID
		addr string
		gen  uint64
	}
	// effRecoveryPost: POST a recovery record to the fleet manager (off the
	// event loop); send evRecoveryPosted.
	effRecoveryPost struct {
		id      core.NodeID
		epoch   uint64
		action  string
		nonce   string
		url     string
		body    []byte
		timeout time.Duration
	}
	// effReply: deliver an answer to a waiter's channel (never blocks:
	// the channel has room for exactly one answer).
	effReply struct {
		w *waiter
		a answer
	}
)

func (effProbe) isEffect()        {}
func (effStart) isEffect()        {}
func (effCancelHealth) isEffect() {}
func (effStop) isEffect()         {}
func (effSample) isEffect()       {}
func (effFetch) isEffect()        {}
func (effReply) isEffect()        {}
func (effWorkerProbe) isEffect()  {}
func (effRecoveryPost) isEffect() {}

// checkInvariantsEachStep makes handle verify every invariant after each
// step and panic on a violation. Tests switch it on.
var checkInvariantsEachStep = false

// handle is the step function: it applies one event, then the time-based
// rules, then the queue pass of design 8.3. It never blocks.
func (c *schedCore) handle(ev event) {
	c.steps++
	c.now = c.clock.Now()
	if c.now.Before(c.last) {
		c.now = c.last // never let time run backwards inside the core
	}
	c.accumulate()
	c.refreshView()

	switch e := ev.(type) {
	case evAcquire:
		c.arrive(e.w)
	case evCancel:
		c.cancel(e.w)
	case evRelease:
		c.release(e.id)
	case evPreflight:
		c.preflightDone(e.iid, e.results)
	case evChildStarted:
		c.childStarted(e.iid, e.child, e.err)
	case evHealthy:
		c.healthy(e.iid, e.err)
	case evServedCtx:
		c.servedCtx(e.iid, e.ctx, e.err)
	case evExited:
		c.exited(e.iid, e.status, e.workers)
	case evFetched:
		c.fetched(e.id, e.gen, e.node, e.err)
	case evLoadSample:
		c.loadSampled(e.iid, e.bytes, e.sockets, e.peers, e.err)
	case evWorkerProbe:
		c.workerProbed(e.id, e.gen, e.err)
	case evRecoveryPosted:
		c.recoveryPosted(e)
	case evConfig:
		c.applyConfig(e.cfg)
	case evViewChanged, evTick:
	case evUnload:
		err := c.unload(e.profile, e.force)
		if e.reply != nil {
			e.reply <- err
		}
	case evUnloadIdle:
		got := c.unloadIdle()
		if e.reply != nil {
			e.reply <- got
		}
	case evShutdown:
		c.beginShutdown(e.force)
	case evQuery:
		if e.read != "" {
			c.readGuard(e)
		} else {
			e.fn(c)
		}
	case evLocalMem:
		c.localMemRead(e)
	case evBuilds:
		c.buildsKnown(e.host, e.served)
	case evHeadersRead:
		c.headersRead()
	default:
		if !c.handleAgents(ev) {
			panic(fmt.Sprintf("scheduler: unknown event %T", ev))
		}
	}

	c.timers()
	c.trackPeaks()             // reserve.go
	c.measureHostOverheads()   // hostoverhead.go
	c.checkHostMemorySizes()   // hostmem.go
	c.measureDeviceOverheads() // deviceoverhead.go
	c.pruneRecent()
	c.runPasses()
	c.prunePins()    // pin.go
	c.pruneScoped()  // scoped.go
	c.watchFigures() // neverfits.go
	c.shutdownProgress()
	c.last = c.now
	if checkInvariantsEachStep {
		if err := c.checkInvariants(); err != nil {
			panic(err)
		}
	}
}

// takeEffects returns and clears the outbox.
func (c *schedCore) takeEffects() []effect {
	out := c.effects
	c.effects = nil
	return out
}

// takeFeed returns and clears the feed outbox.
func (c *schedCore) takeFeed() []FeedEvent {
	out := c.feed
	c.feed = nil
	return out
}

// answer delivers a waiter's one answer and forgets it.
func (c *schedCore) answer(w *waiter, a answer) {
	if w.answered {
		panic(fmt.Sprintf("scheduler: waiter %d answered twice", w.id))
	}
	w.answered = true
	delete(c.waiters, w.id)
	if w.reply != nil {
		c.effects = append(c.effects, effReply{w: w, a: a})
	}
}

func (c *schedCore) fail(w *waiter, err *Error) {
	c.answer(w, answer{err: err})
	c.noteRefusal(w.profile, err, 1, w.benchRun)
}

func (c *schedCore) failAll(ws []*waiter, err *Error) {
	for _, w := range ws {
		e := *err
		c.answer(w, answer{err: &e})
	}
	if len(ws) > 0 {
		c.noteRefusal(ws[0].profile, err, len(ws), ws[0].benchRun)
	}
}

// readGuard runs a read-only query under supervise.GuardRead (task
// scheduler.read): a panic in it is logged at ERROR with the stack, recorded
// as an internalFailure and counted, reported to the caller (a 500), and the
// core loop goes on; it never escalates. Only queries that change no core
// state are posted this way (runner.go askRead: the plan, the status, the
// needs).
func (c *schedCore) readGuard(e evQuery) {
	supervise.GuardRead("scheduler.read", func() { e.fn(c) }, func(v any) {
		c.log.Warn("read-only query answered with an error; the scheduler goes on", "query", e.read, "panic", fmt.Sprint(v))
		e.panicked(v)
	})
}
