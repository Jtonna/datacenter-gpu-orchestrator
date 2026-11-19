package bench

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

// activeRun is the run being executed.
type activeRun struct {
	mu         sync.Mutex
	run        *Run
	plan       *runPlan
	cancelCh   chan struct{}
	cancelOnce sync.Once
	reason     string
	timedOut   bool
	loaded     map[string]bool // profiles this run loaded and has not unloaded yet
	total      int
	done       int
	step       map[string]any // the step in flight (the first one, for a parallel step)
	paused     string         // why the run is paused, when it is

	// progress model (progress.go)
	items    []*itemPlan
	cur      int                    // the suite entry in flight; -1 before the first
	tracks   map[string]*benchTrack // by benchmark id
	lastEmit time.Time
	maxPct   float64
	env      Environment
	hosts    map[string]string // node id to host name, from the status
}

func (a *activeRun) cancel(reason string) {
	a.cancelOnce.Do(func() {
		a.mu.Lock()
		a.reason = reason
		a.mu.Unlock()
		close(a.cancelCh)
	})
}

func (a *activeRun) cancelled() bool {
	select {
	case <-a.cancelCh:
		return true
	default:
		return false
	}
}

func (a *activeRun) cancelReason() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reason
}

func (a *activeRun) snapshot() *Run {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := *a.run
	return &cp
}

func (a *activeRun) progress() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := map[string]any{"steps_total": a.total, "steps_done": a.done}
	if a.step != nil {
		p["step"] = a.step
	}
	if a.paused != "" {
		p["paused_because"] = a.paused
	}
	return p
}

func (a *activeRun) setRun(f func(r *Run)) {
	a.mu.Lock()
	f(a.run)
	a.mu.Unlock()
}

func (a *activeRun) markLoaded(profile string, v bool) {
	a.mu.Lock()
	if v {
		a.loaded[profile] = true
	} else {
		delete(a.loaded, profile)
	}
	a.mu.Unlock()
}

func (a *activeRun) loadedSet() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]bool, len(a.loaded))
	for k := range a.loaded {
		out[k] = true
	}
	return out
}

// exec carries one run through its suite.
type exec struct {
	m    *Manager
	a    *activeRun
	base map[string]*Step // the baseline run's steps by benchmark id
}

// sleep waits for d or until the run is cancelled; false means cancelled.
func (x *exec) sleep(d time.Duration) bool {
	if d <= 0 {
		return !x.a.cancelled()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-x.a.cancelCh:
		return false
	}
}

func (m *Manager) execute(ctx context.Context, q *queued) {
	r := q.run
	ar := &activeRun{run: r, plan: q.plan, cancelCh: make(chan struct{}), loaded: map[string]bool{}, cur: -1, tracks: map[string]*benchTrack{}}
	for _, it := range q.plan.Suite.Benchmarks {
		ar.total += len(leafBenchmarks(q.plan, it))
	}
	ar.items = m.planItems(q.plan)
	ar.env = Environment{SchedulerVersion: m.o.Version}
	m.mu.Lock()
	m.cur = ar
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.cur = nil
		m.mu.Unlock()
	}()
	fin := make(chan struct{})
	defer close(fin)
	go func() {
		select {
		case <-ctx.Done():
			ar.cancel("scheduler shutting down")
		case <-fin:
		}
	}()
	maxTimer := time.AfterFunc(m.o.MaxRun, func() {
		ar.mu.Lock()
		ar.timedOut = true
		ar.mu.Unlock()
		ar.cancel(fmt.Sprintf("the run took longer than max_run_minutes (%s)", m.o.MaxRun))
	})
	defer maxTimer.Stop()

	x := &exec{m: m, a: ar, base: map[string]*Step{}}
	now := m.o.Now().UnixMilli()
	ar.setRun(func(r *Run) { r.State, r.StartedAt = RunRunning, now })
	if err := m.o.Store.SetRunState(r.ID, RunRunning, now, 0, ""); err != nil {
		m.log.Error("cannot start a benchmark run", "run", r.ID, "err", err)
	}
	// The baseline: the run asked for, else the previous completed run of the suite.
	var baseRun *Run
	if r.BaselineRunID != "" {
		baseRun, _ = m.o.Store.GetRun(r.BaselineRunID)
	} else if prev, err := m.o.Store.PreviousCompleted(r.SuiteID, r.ID); err == nil && prev != nil {
		baseRun = prev
		ar.setRun(func(r *Run) { r.BaselineRunID = prev.ID })
		m.o.Store.SetRunSummary(r.ID, nil, prev.ID)
	}
	if baseRun != nil {
		for i := range baseRun.Steps {
			st := &baseRun.Steps[i]
			if _, dup := x.base[st.BenchmarkID]; !dup {
				x.base[st.BenchmarkID] = st
			}
		}
	}
	x.captureEnv()
	m.publishRun(ar.snapshot())
	m.emitProgress(ar, true)
	tickDone := make(chan struct{})
	defer close(tickDone)
	go func() { // keeps the progress message coming (twice a second) while nothing else changes
		t := time.NewTicker(progressEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.emitProgress(ar, false)
			case <-tickDone:
				return
			}
		}
	}()
	m.record(history.TBenchRunStarted, history.Info, "", fmt.Sprintf("Benchmark run %s of suite %s started (%d steps).", r.ID, r.SuiteName, ar.total),
		map[string]any{"bench_run": r.ID, "suite": r.SuiteID, "steps": ar.total, "trigger": r.Trigger, "baseline": ar.snapshot().BaselineRunID})

	suite := q.plan.Suite
	var steps []*Step
	state, errText := RunCompleted, ""
	items := suite.Benchmarks
run:
	for i, it := range items {
		if ar.cancelled() {
			break
		}
		if i > 0 && !x.sleep(suite.Cooldown()) {
			break
		}
		if suite.WaitsForIdle() {
			switch x.waitIdle(suite.IdleWait()) {
			case idleCancelled:
				break run
			case idleTimeout:
				state, errText = RunSkipped, fmt.Sprintf("the scheduler was not idle within idle_wait_s (%s): something else was using it", suite.IdleWait())
				break run
			}
		}
		ar.mu.Lock()
		ar.cur = i
		ar.mu.Unlock()
		got := x.runItem(i, it)
		steps = append(steps, got...)
		if suite.StopOnFailure {
			for _, st := range got {
				if st.Outcome == OutcomeError || (st.Expect != nil && !st.Expect.Pass) {
					state, errText = RunFailed, fmt.Sprintf("stopped after %q did not pass (stop_on_failure)", st.BenchmarkID)
					break run
				}
			}
		}
	}
	// Steps that never ran are recorded as skipped, so the run shows the whole suite.
	ran := map[int]bool{}
	for _, st := range steps {
		ran[st.Idx] = true
	}
	for i, it := range items {
		if ran[i] {
			continue
		}
		for _, b := range leafBenchmarks(q.plan, it) {
			st := &Step{RunID: r.ID, Idx: i, BenchmarkID: b.ID, Name: b.Title(), Profile: b.Profile, State: StepSkipped, Outcome: OutcomeSkipped, Spec: b}
			if err := m.o.Store.AddStep(st); err == nil {
				steps = append(steps, st)
			}
		}
	}
	// Whatever this run loaded is unloaded at the end, however it ended.
	x.cleanup()

	ar.mu.Lock()
	timedOut := ar.timedOut
	ar.mu.Unlock()
	switch {
	case timedOut:
		state, errText = RunFailed, ar.cancelReason()
	case ar.cancelled():
		state, errText = RunCancelled, ar.cancelReason()
	}
	sum := &RunSummary{Steps: len(steps)}
	for _, st := range steps {
		if st.Outcome == OutcomeSkipped {
			continue
		}
		if st.Expect != nil && st.Expect.Pass {
			sum.Passed++
		} else {
			sum.Failed++
		}
		if st.Comparison != nil && st.Comparison.Regression {
			sum.Regressions++
		}
	}
	end := m.o.Now().UnixMilli()
	if err := m.o.Store.SetRunSummary(r.ID, sum, ""); err != nil {
		m.log.Warn("cannot store a run summary", "run", r.ID, "err", err)
	}
	if err := m.o.Store.SetRunState(r.ID, state, 0, end, errText); err != nil {
		m.log.Error("cannot finish a benchmark run", "run", r.ID, "err", err)
	}
	ar.setRun(func(r *Run) { r.State, r.FinishedAt, r.Error, r.Summary = state, end, errText, sum })
	env := x.envSnapshot()
	if err := m.o.Store.SetRunEnv(r.ID, env); err != nil {
		m.log.Warn("cannot store a run environment", "run", r.ID, "err", err)
	}
	ar.setRun(func(r *Run) { r.Env = env })
	m.publishRun(ar.snapshot())
	m.emitProgress(ar, true)
	var ids []string
	seenID := map[string]bool{}
	for _, st := range steps {
		if st.Outcome != OutcomeSkipped && !seenID[st.BenchmarkID] {
			seenID[st.BenchmarkID] = true
			ids = append(ids, st.BenchmarkID)
		}
	}
	m.emit(map[string]any{"kind": "aggregate_updated", "run_id": r.ID, "suite_id": r.SuiteID, "state": state, "benchmarks": ids})
	sev := history.Info
	if state == RunFailed || sum.Failed > 0 || sum.Regressions > 0 {
		sev = history.Warn
	}
	summary := fmt.Sprintf("Benchmark run %s %s: %d steps, %d passed, %d failed, %d regressions.", r.ID, state, sum.Steps, sum.Passed, sum.Failed, sum.Regressions)
	if errText != "" {
		summary += " " + strings.TrimRight(errText, ".") + "."
	}
	m.record(history.TBenchRunFinished, sev, "", summary, map[string]any{"bench_run": r.ID, "suite": r.SuiteID, "state": state, "error": errText,
		"steps": sum.Steps, "passed": sum.Passed, "failed": sum.Failed, "regressions": sum.Regressions, "duration_s": float64(end-r.QueuedAt) / 1000})
}

// leafBenchmarks are the plain benchmarks an item runs (a parallel step expands to its members).
func leafBenchmarks(p *runPlan, it benchspec.SuiteItem) []*benchspec.Benchmark {
	b := it.Inline
	if b == nil {
		b = p.Benchmarks[it.Ref]
	}
	if b == nil {
		return nil
	}
	if !b.IsParallel() {
		return []*benchspec.Benchmark{b}
	}
	var out []*benchspec.Benchmark
	for _, id := range b.Parallel {
		if mb := p.Benchmarks[id]; mb != nil {
			out = append(out, mb)
		}
	}
	return out
}

// ---- idle ----

type idleResult int

const (
	idleOK idleResult = iota
	idleTimeout
	idleCancelled
)

// busy says why the scheduler is in use by something other than this run, or "".
func busyReason(st *StatusInfo, ours map[string]bool) string {
	var why []string
	for _, in := range st.Instances {
		switch {
		case !ours[in.Profile]:
			why = append(why, fmt.Sprintf("%s is %s (not loaded by this run)", in.Profile, in.State))
		case in.InFlight > 0 || in.Waiters > 0:
			why = append(why, fmt.Sprintf("%s has %d requests in flight or waiting that are not this run's", in.Profile, in.InFlight+in.Waiters))
		}
	}
	if st.QueueLen > 0 {
		why = append(why, fmt.Sprintf("%d queue entries", st.QueueLen))
	}
	sort.Strings(why)
	return strings.Join(why, "; ")
}

// waitIdle waits until the scheduler has no instance this run did not load,
// no request in flight that is not this run's and nothing queued, for at most
// max. While it waits the run is paused.
func (x *exec) waitIdle(max time.Duration) idleResult {
	deadline := time.Now().Add(max)
	paused := false
	defer func() {
		if paused {
			x.setPaused("")
			x.a.setRun(func(r *Run) { r.State = RunRunning })
			x.m.o.Store.SetRunState(x.a.run.ID, RunRunning, 0, 0, "")
			x.m.publishRun(x.a.snapshot())
		}
	}()
	for {
		if x.a.cancelled() {
			return idleCancelled
		}
		st, err := x.m.o.API.Status(context.Background())
		why := ""
		if err != nil {
			why = "the scheduler's status could not be read: " + err.Error()
		} else {
			why = busyReason(st, x.a.loadedSet())
		}
		if why == "" {
			return idleOK
		}
		if !time.Now().Before(deadline) {
			return idleTimeout
		}
		if !paused {
			paused = true
			x.a.setRun(func(r *Run) { r.State = RunPaused })
			x.m.o.Store.SetRunState(x.a.run.ID, RunPaused, 0, 0, "")
			x.setPaused(why)
			x.m.publishRun(x.a.snapshot())
		} else {
			x.setPaused(why)
		}
		if !x.sleep(x.m.o.IdlePoll) {
			return idleCancelled
		}
	}
}

func (x *exec) setPaused(why string) {
	x.a.mu.Lock()
	x.a.paused = why
	x.a.mu.Unlock()
}

// cleanup unloads every profile this run loaded and still has loaded.
func (x *exec) cleanup() {
	loaded := x.a.loadedSet()
	var names []string
	for p := range loaded {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		ctx, cancel := context.WithTimeout(WithRun(context.Background(), x.a.run.ID), 30*time.Second)
		res, err := x.m.o.API.Unload(ctx, p, true)
		cancel()
		switch {
		case err != nil:
			x.m.log.Warn("benchmark run could not unload what it loaded", "profile", p, "err", err)
		case res.Status != 200:
			x.m.log.Warn("benchmark run could not unload what it loaded", "profile", p, "status", res.Status, "msg", res.Msg)
		}
		x.a.markLoaded(p, false)
	}
}

// ---- steps ----

type gate struct {
	n       int
	arrived sync.WaitGroup
	once    []sync.Once
	open    chan struct{}
}

func newGate(n int) *gate {
	g := &gate{n: n, once: make([]sync.Once, n), open: make(chan struct{})}
	g.arrived.Add(n)
	go func() { g.arrived.Wait(); close(g.open) }()
	return g
}

// arrive says member i is ready (idempotent); wait blocks until all are.
func (g *gate) arrive(i int) { g.once[i].Do(g.arrived.Done) }
func (g *gate) wait()        { <-g.open }

func (x *exec) runItem(idx int, it benchspec.SuiteItem) []*Step {
	b := it.Inline
	if b == nil {
		b = x.a.plan.Benchmarks[it.Ref]
	}
	if !b.IsParallel() {
		return []*Step{x.runBenchmark(b, idx, "", nil, 0)}
	}
	members := leafBenchmarks(x.a.plan, it)
	g := newGate(len(members))
	out := make([]*Step, len(members))
	var wg sync.WaitGroup
	for i, mb := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = x.runBenchmark(mb, idx, b.ID, g, i)
		}()
	}
	wg.Wait()
	return out
}
