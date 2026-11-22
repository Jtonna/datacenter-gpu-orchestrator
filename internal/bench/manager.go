package bench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// MaxQueuedRuns bounds the run queue.
const MaxQueuedRuns = 20

// ErrInvalid wraps a problem with what the caller asked for (a 4xx).
var ErrInvalid = errors.New("invalid")

// ErrQueueFull means too many runs are waiting.
var ErrQueueFull = errors.New("too many runs are queued")

// ErrNotRunnable means the suite is planned or blocked (a 409 with its reason).
var ErrNotRunnable = errors.New("not runnable")

// ManagerOptions configure a Manager. Store and API are required.
type ManagerOptions struct {
	Store      *Store
	API        API
	Version    string
	Thresholds Thresholds
	// MaxRun stops a run that takes longer (bench.max_run_minutes).
	MaxRun time.Duration
	// ProfileOK says whether a profile id exists (checked again when a run is queued).
	ProfileOK func(string) bool
	// Record receives the benchmark history events; nil drops them.
	Record func(history.Event)
	Logger *slog.Logger
	Now    func() time.Time
	// IdlePoll is how often the scheduler is looked at while waiting for it to
	// be idle or for a model to leave (default 2 s); LoadPoll is how often the
	// load progress of a step is read (default 1 s). Tests shorten both.
	IdlePoll time.Duration
	LoadPoll time.Duration
	// GoneTimeout is how long to wait for an unloaded model to leave (default 2 min).
	GoneTimeout time.Duration
	// MemSample is how often a step samples the nodes' available memory in
	// the background (default 500 ms); a negative value turns the background
	// sampling off (the step still samples at its load polls and phase ends).
	MemSample time.Duration
	// ColdRetry is how long a cold load keeps asking a node that is still
	// stopping its launch or evicted a moment ago (default 20 s, negative:
	// once); ColdSettle is the wait before the plan is read again after the
	// eviction, so the nodes' file cache figures are fresh (default 2.5 s,
	// negative: none).
	ColdRetry  time.Duration
	ColdSettle time.Duration
	// UnloadGrace is how long an unload without force is tried again while
	// the profile still counts requests in flight: the scheduler counts a
	// request's end a moment after the client has the answer, so an unload
	// right after the last request can meet it (default 3 s, negative: no
	// retry). The end-of-step clean-up unloads with force after it.
	UnloadGrace time.Duration
}

// runPlan is the suite and every benchmark it names, as they were when the run
// was queued. It is stored with the run, so a result can be explained later.
type runPlan struct {
	Suite      *benchspec.Suite                `json:"suite"`
	Benchmarks map[string]*benchspec.Benchmark `json:"benchmarks"`
}

type queued struct {
	run  *Run
	plan *runPlan
}

type schedState struct {
	sig string
	due time.Time
}

// Manager owns the run queue, the schedule and the one running run.
type Manager struct {
	o   ManagerOptions
	log *slog.Logger

	mu       sync.Mutex
	queue    []*queued
	cur      *activeRun
	stopping bool
	subs     map[int]func(map[string]any)
	subSeq   int
	modelOf  func(profile string) string // the model file of a profile, from the config
	sched    map[string]*schedState
	wake     chan struct{}
	done     chan struct{}
	running  *queued // the run the loop executes (the loop's goroutine only; recoverRun reads it)
	started  bool
}

// NewManager creates a manager and marks runs that an earlier process left
// running as failed (interrupted).
func NewManager(o ManagerOptions) (*Manager, error) {
	if o.Store == nil || o.API == nil {
		return nil, errors.New("bench: Store and API are required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.IdlePoll <= 0 {
		o.IdlePoll = 2 * time.Second
	}
	if o.LoadPoll <= 0 {
		o.LoadPoll = time.Second
	}
	if o.MemSample == 0 {
		o.MemSample = DefaultMemSample
	}
	if o.ColdRetry == 0 {
		o.ColdRetry = DefaultColdRetry
	}
	if o.ColdSettle == 0 {
		o.ColdSettle = DefaultColdSettle
	}
	if o.UnloadGrace == 0 {
		o.UnloadGrace = DefaultUnloadGrace
	}
	if o.GoneTimeout <= 0 {
		o.GoneTimeout = 2 * time.Minute
	}
	if o.MaxRun <= 0 {
		o.MaxRun = 240 * time.Minute
	}
	if o.Thresholds.LoadPct <= 0 {
		o.Thresholds.LoadPct = 15
	}
	if o.Thresholds.DecodePct <= 0 {
		o.Thresholds.DecodePct = 7
	}
	m := &Manager{o: o, log: o.Logger, subs: map[int]func(map[string]any){}, sched: map[string]*schedState{},
		wake: make(chan struct{}, 1), done: make(chan struct{})}
	n, err := o.Store.RecoverInterrupted(o.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	if n > 0 {
		m.log.Warn("benchmark runs interrupted by the last stop were marked failed", "runs", n)
	}
	return m, nil
}

// Seed upserts the config's benchmarks and suites (source "config"). Items
// created through the API are never overwritten. A problem with one item is
// logged and does not stop the others.
func (m *Manager) Seed(bms []*benchspec.Benchmark, suites []*benchspec.Suite) {
	now := m.o.Now().UnixMilli()
	for _, b := range bms {
		if err := m.o.Store.SeedBenchmark(b, now); err != nil {
			m.log.Warn("cannot seed benchmark from the config", "id", b.ID, "err", err)
		}
	}
	for _, s := range suites {
		if err := m.o.Store.SeedSuite(s, now); err != nil {
			m.log.Warn("cannot seed suite from the config", "id", s.ID, "err", err)
		}
	}
}

// Start runs the queue and the schedule until ctx ends or Stop is called.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	// A failure fails the run it was executing (its models are unloaded on
	// the way out), keeps the queue, and the loop starts again (isolation.go).
	supervise.Go(ctx, supervise.Spec{Name: "bench.manager", Policy: supervise.Restart, Rebuild: m.recoverRun,
		Done: func() { close(m.done) }}, m.loop)
}

func (m *Manager) loop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	m.checkSchedules(true)
	for {
		m.mu.Lock()
		var next *queued
		if !m.stopping && len(m.queue) > 0 {
			next = m.queue[0]
			m.queue = m.queue[1:]
		}
		m.mu.Unlock()
		if next != nil {
			m.running = next
			m.execute(ctx, next)
			m.running = nil
			continue
		}
		select {
		case <-ctx.Done():
			m.Stop(ctx) // ctx is done: Stop cancels the runs and does not wait for this loop
			return
		case <-m.wake:
		case <-tick.C:
			m.checkSchedules(true)
		}
		m.mu.Lock()
		stop := m.stopping
		m.mu.Unlock()
		if stop {
			return
		}
	}
}

// Stop cancels the running run (marked cancelled, "scheduler shutting down"),
// cancels the queued ones and waits for the loop to end or ctx to expire.
func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	m.stopping = true
	cur := m.cur
	q := m.queue
	m.queue = nil
	started := m.started
	m.mu.Unlock()
	if cur != nil {
		cur.cancel("scheduler shutting down")
	}
	for _, it := range q {
		m.finishUnstarted(it.run, RunCancelled, "scheduler shutting down")
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
	if started {
		select {
		case <-m.done:
		case <-ctx.Done():
		}
	}
}

// ---- events ----

// Subscribe registers fn for bench events: maps with a "kind" key (run,
// step_started, step_finished, progress, load). fn must not block.
func (m *Manager) Subscribe(fn func(map[string]any)) (cancel func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subSeq++
	id := m.subSeq
	m.subs[id] = fn
	return func() { m.mu.Lock(); delete(m.subs, id); m.mu.Unlock() }
}

func (m *Manager) emit(ev map[string]any) {
	m.mu.Lock()
	fns := make([]func(map[string]any), 0, len(m.subs))
	for _, f := range m.subs {
		fns = append(fns, f)
	}
	m.mu.Unlock()
	for _, f := range fns {
		// Subscribers are the catalog and the live channel: a panic in one
		// must not end the run that emitted (background-task isolation).
		supervise.Guard("bench.subscriber", func() { f(ev) }, nil)
	}
}

func (m *Manager) record(typ, sev, profile, summary string, data map[string]any) {
	if m.o.Record == nil {
		return
	}
	m.o.Record(history.Event{Ts: m.o.Now().UnixMilli(), Type: typ, Severity: sev, Profile: profile, Summary: summary, Data: data})
}

// Brief is a run without its steps, with live progress, for lists and the WebSocket.
func (m *Manager) brief(r *Run) map[string]any {
	b := map[string]any{"id": r.ID, "suite_id": r.SuiteID, "suite_name": r.SuiteName, "state": r.State, "trigger": r.Trigger,
		"queued_at": r.QueuedAt, "started_at": r.StartedAt, "finished_at": r.FinishedAt, "note": r.Note,
		"baseline_run_id": r.BaselineRunID, "error": r.Error}
	if r.Summary != nil {
		b["summary"] = r.Summary
	}
	m.mu.Lock()
	if m.cur != nil && m.cur.run.ID == r.ID {
		b["progress"] = m.cur.progress()
	}
	m.mu.Unlock()
	return b
}

func (m *Manager) publishRun(r *Run) {
	m.emit(map[string]any{"kind": "run", "run": m.brief(r)})
}

// ---- enqueue, cancel, state ----

// EnqueueOptions are the optional fields of a run request.
type EnqueueOptions struct {
	Baseline string
	Note     string
	Trigger  string // manual (default) or schedule
	// EvictOthers sets evict_others for this run only (nil: the suite's
	// setting). The run's stored suite snapshot carries the value used.
	EvictOthers *bool
}

func newRunID(now time.Time) string {
	var b [3]byte
	rand.Read(b[:])
	return "run-" + now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

func (m *Manager) resolve(s *benchspec.Suite) (*runPlan, error) {
	plan := &runPlan{Suite: s, Benchmarks: map[string]*benchspec.Benchmark{}}
	var errs []error
	need := func(id string) *benchspec.Benchmark {
		if b, ok := plan.Benchmarks[id]; ok {
			return b
		}
		sb, err := m.o.Store.GetBenchmark(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("benchmark %q: %w", id, err))
			return nil
		}
		plan.Benchmarks[id] = sb.Spec
		return sb.Spec
	}
	var walk func(b *benchspec.Benchmark)
	walk = func(b *benchspec.Benchmark) {
		plan.Benchmarks[b.ID] = b
		for _, id := range b.Parallel {
			if mb := need(id); mb != nil && mb.IsParallel() {
				errs = append(errs, fmt.Errorf("benchmark %q: parallel member %q is itself a parallel step", b.ID, id))
			}
		}
	}
	for _, it := range s.Benchmarks {
		if it.Inline != nil {
			walk(it.Inline)
		} else if b := need(it.Ref); b != nil {
			walk(b)
		}
	}
	for _, b := range plan.Benchmarks {
		if b.IsParallel() {
			seen := map[string]string{}
			for _, id := range b.Parallel {
				if mb := plan.Benchmarks[id]; mb != nil {
					if other, dup := seen[mb.Profile]; dup {
						errs = append(errs, fmt.Errorf("parallel step %q: %q and %q use the same profile %q; two loads of one profile cannot start together", b.ID, other, id, mb.Profile))
					}
					seen[mb.Profile] = id
				}
			}
		}
		if !b.IsParallel() && m.o.ProfileOK != nil && !m.o.ProfileOK(b.Profile) {
			errs = append(errs, fmt.Errorf("benchmark %q: profile %q does not exist (any more)", b.ID, b.Profile))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return plan, nil
}

// Enqueue queues a run of a suite and returns it.
func (m *Manager) Enqueue(suiteID string, o EnqueueOptions) (*Run, error) {
	ss, err := m.o.Store.GetSuite(suiteID)
	if err != nil {
		return nil, err
	}
	if !ss.Spec.Runnable() {
		return nil, fmt.Errorf("%w: suite %q is %s and cannot be run: %s", ErrNotRunnable, suiteID, ss.Spec.Status, ss.Spec.Reason)
	}
	if len(o.Note) > 500 {
		return nil, fmt.Errorf("%w: note is longer than 500 characters", ErrInvalid)
	}
	spec := ss.Spec
	if o.EvictOthers != nil {
		c := *spec
		v := *o.EvictOthers
		c.EvictOthers = &v
		spec = &c
	}
	plan, err := m.resolve(spec)
	if err != nil {
		return nil, err
	}
	baseline := o.Baseline
	if baseline == "" {
		baseline = ss.Spec.Baseline
	}
	if baseline != "" {
		if _, err := m.o.Store.GetRun(baseline); err != nil {
			return nil, fmt.Errorf("%w: baseline run %q: %v", ErrInvalid, baseline, err)
		}
	}
	snap, _ := json.Marshal(plan)
	trig := o.Trigger
	if trig == "" {
		trig = "manual"
	}
	now := m.o.Now()
	r := &Run{ID: newRunID(now), SuiteID: suiteID, SuiteName: ss.Spec.Title(), State: RunQueued, Note: o.Note, Trigger: trig,
		BaselineRunID: baseline, QueuedAt: now.UnixMilli(), Version: m.o.Version, Suite: snap}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, errors.New("the scheduler is shutting down")
	}
	if len(m.queue) >= MaxQueuedRuns {
		m.mu.Unlock()
		return nil, ErrQueueFull
	}
	if err := m.o.Store.CreateRun(r); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	copyOut := *r // the queue owns r from here on; callers get a snapshot
	m.queue = append(m.queue, &queued{run: r, plan: plan})
	m.mu.Unlock()
	m.publishRun(&copyOut)
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return &copyOut, nil
}

// Cancel cancels a queued run at once, or asks the running run to stop after
// its current request (it then unloads what it loaded).
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	for i, it := range m.queue {
		if it.run.ID == id {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			m.mu.Unlock()
			m.finishUnstarted(it.run, RunCancelled, "cancelled before it started")
			return nil
		}
	}
	cur := m.cur
	m.mu.Unlock()
	if cur != nil && cur.run.ID == id {
		cur.cancel("cancelled by request")
		return nil
	}
	r, err := m.o.Store.GetRun(id)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: run %s is already %s", ErrInvalid, id, r.State)
}

func (m *Manager) finishUnstarted(r *Run, state, why string) {
	now := m.o.Now().UnixMilli()
	if err := m.o.Store.SetRunState(r.ID, state, 0, now, why); err != nil {
		m.log.Warn("cannot update a run", "run", r.ID, "err", err)
	}
	r.State, r.FinishedAt, r.Error = state, now, why
	m.publishRun(r)
	m.record(history.TBenchRunFinished, history.Info, "", fmt.Sprintf("Benchmark run %s was %s before it started: %s.", r.ID, state, why),
		map[string]any{"bench_run": r.ID, "suite": r.SuiteID, "state": state, "error": why})
}

// SetModelFile tells the manager how to name the model file of a profile before
// its instance exists (the progress and the step show it from the start). The
// API layer sets it from the configuration.
func (m *Manager) SetModelFile(f func(profile string) string) {
	m.mu.Lock()
	m.modelOf = f
	m.mu.Unlock()
}

func (m *Manager) modelFileOf(profile string) string {
	m.mu.Lock()
	f := m.modelOf
	m.mu.Unlock()
	if f == nil {
		return ""
	}
	return f(profile)
}

// Aggregates is GET /api/bench/aggregates: the stored runs, aggregated.
func (m *Manager) Aggregates(f AggFilter) (*AggregatesDoc, error) {
	return m.o.Store.Aggregate(f, m.o.Thresholds, m.o.Now())
}

// StateDoc is GET /api/bench/state.
type StateDoc struct {
	Enabled bool `json:"enabled"`
	// Run is the progress of the running run (design 9.3): percent, the benchmark
	// and phase in flight, the next one, the queue and the eta; null when idle.
	Run           map[string]any   `json:"run"`
	Current       map[string]any   `json:"current"`
	Queue         []map[string]any `json:"queue"`
	NextScheduled []ScheduledRun   `json:"next_scheduled"`
}

// ScheduledRun is a suite with a schedule and when it is next due.
type ScheduledRun struct {
	SuiteID string `json:"suite_id"`
	Name    string `json:"name"`
	Every   string `json:"every,omitempty"`
	At      string `json:"at,omitempty"`
	Next    int64  `json:"next"`
}

// State is the current run, the queue and the next scheduled runs.
func (m *Manager) State() StateDoc {
	m.checkSchedules(false)
	d := StateDoc{Enabled: true, Queue: []map[string]any{}, NextScheduled: []ScheduledRun{}, Run: m.Progress()}
	m.mu.Lock()
	var cur *Run
	if m.cur != nil {
		cur = m.cur.snapshot()
	}
	var q []*Run
	for _, it := range m.queue {
		q = append(q, it.run)
	}
	m.mu.Unlock()
	if cur != nil {
		d.Current = m.brief(cur)
	}
	for _, r := range q {
		d.Queue = append(d.Queue, m.brief(r))
	}
	suites, _ := m.o.Store.ListSuites()
	m.mu.Lock()
	for _, su := range suites {
		if su.Spec.Schedule == nil {
			continue
		}
		if st := m.sched[su.Spec.ID]; st != nil {
			d.NextScheduled = append(d.NextScheduled, ScheduledRun{SuiteID: su.Spec.ID, Name: su.Spec.Title(), Every: su.Spec.Schedule.Every, At: su.Spec.Schedule.At, Next: st.due.UnixMilli()})
		}
	}
	m.mu.Unlock()
	sort.Slice(d.NextScheduled, func(i, j int) bool { return d.NextScheduled[i].Next < d.NextScheduled[j].Next })
	return d
}

// ---- schedule ----

func schedSig(s *benchspec.Schedule) string { return s.Every + "|" + s.At }

// checkSchedules queues a run of every suite whose schedule is due, unless one
// of that suite is already queued or running. Called every 30 s (fire true); State
// calls it with fire false only to learn the next due times.
func (m *Manager) checkSchedules(fire bool) {
	suites, err := m.o.Store.ListSuites()
	if err != nil {
		return
	}
	now := m.o.Now()
	var due []string
	m.mu.Lock()
	live := map[string]bool{}
	for _, su := range suites {
		sc := su.Spec.Schedule
		if sc == nil || !su.Spec.Runnable() { // a planned or blocked suite never runs
			continue
		}
		live[su.Spec.ID] = true
		st := m.sched[su.Spec.ID]
		if st == nil || st.sig != schedSig(sc) {
			var last time.Time
			if ms, err := m.o.Store.LastScheduledStart(su.Spec.ID); err == nil && ms > 0 {
				last = time.UnixMilli(ms)
			}
			next, ok := sc.Next(now, last)
			if !ok {
				continue
			}
			st = &schedState{sig: schedSig(sc), due: next}
			m.sched[su.Spec.ID] = st
		}
		if fire && !now.Before(st.due) {
			due = append(due, su.Spec.ID)
			if next, ok := sc.Next(now, now); ok {
				st.due = next
			}
		}
	}
	for id := range m.sched {
		if !live[id] {
			delete(m.sched, id)
		}
	}
	busy := map[string]bool{}
	for _, it := range m.queue {
		busy[it.run.SuiteID] = true
	}
	if m.cur != nil {
		busy[m.cur.run.SuiteID] = true
	}
	stopping := m.stopping
	m.mu.Unlock()
	if stopping {
		return
	}
	for _, id := range due {
		if busy[id] {
			m.log.Info("scheduled benchmark run skipped: the suite already has a run queued or running", "suite", id)
			continue
		}
		if _, err := m.Enqueue(id, EnqueueOptions{Trigger: "schedule", Note: "scheduled"}); err != nil {
			m.log.Warn("scheduled benchmark run could not be queued", "suite", id, "err", err)
		}
	}
}
