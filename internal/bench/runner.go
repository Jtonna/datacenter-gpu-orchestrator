package bench

import (
	"context"
	"encoding/json"
	"errors"
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
	ar := &activeRun{run: r, plan: q.plan, cancelCh: make(chan struct{}), loaded: map[string]bool{}}
	for _, it := range q.plan.Suite.Benchmarks {
		ar.total += len(leafBenchmarks(q.plan, it))
	}
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
	m.publishRun(ar.snapshot())
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
	m.publishRun(ar.snapshot())
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

func (x *exec) emitStepProgress(st *Step, extra map[string]any) {
	ev := map[string]any{"kind": "progress", "run_id": st.RunID, "idx": st.Idx, "benchmark_id": st.BenchmarkID}
	for k, v := range extra {
		ev[k] = v
	}
	x.m.emit(ev)
}

func stepBrief(st *Step) map[string]any {
	b := map[string]any{"idx": st.Idx, "group": st.Group, "benchmark_id": st.BenchmarkID, "name": st.Name, "profile": st.Profile,
		"state": st.State, "outcome": st.Outcome, "reason": st.Reason, "http_status": st.HTTPStatus, "started_at": st.StartedAt, "finished_at": st.FinishedAt}
	if st.Load != nil {
		b["load"] = st.Load
	}
	if st.Stats != nil {
		b["stats"] = st.Stats
	}
	if st.Expect != nil {
		b["expect"] = st.Expect
	}
	if st.Comparison != nil {
		b["comparison"] = st.Comparison
	}
	return b
}

type loadObs struct {
	mu      sync.Mutex
	logFile string
	last    *LoadInfo
}

func (o *loadObs) get() (string, *LoadInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.logFile, o.last
}

// watchLoad reads the scheduler's status while the first request is in flight:
// it forwards the load object of the profile's instance to the live channel and
// remembers the child's log file and the last progress. stop() ends it.
func (x *exec) watchLoad(st *Step) (obs *loadObs, stop func()) {
	obs = &loadObs{}
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(x.m.o.LoadPoll)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s, err := x.m.o.API.Status(ctx)
			cancel()
			if err != nil {
				continue
			}
			in := s.Instance(st.Profile)
			if in == nil {
				continue
			}
			obs.mu.Lock()
			if in.LogFile != "" {
				obs.logFile = in.LogFile
			}
			if in.Load != nil {
				obs.last = in.Load
			}
			obs.mu.Unlock()
			if in.Load != nil {
				x.m.emit(map[string]any{"kind": "load", "run_id": st.RunID, "idx": st.Idx, "benchmark_id": st.BenchmarkID, "load": json.RawMessage(in.Load.Raw)})
			}
		}
	}()
	return obs, func() { close(quit); <-done }
}

type wireTimings struct {
	PromptN            float64 `json:"prompt_n"`
	PromptMS           float64 `json:"prompt_ms"`
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedN         float64 `json:"predicted_n"`
	PredictedMS        float64 `json:"predicted_ms"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
	CacheN             float64 `json:"cache_n"`
}

// send performs one request of a step and records it.
func (x *exec) send(ctx context.Context, st *Step, b *benchspec.Benchmark, r benchspec.Request, phase string, seq int) (*RequestResult, *ChatResponse, error) {
	rr := &RequestResult{StepID: st.ID, RunID: st.RunID, Seq: seq, Name: r.Name, Phase: phase, Deterministic: r.Deterministic(), StartedAt: x.m.o.Now().UnixMilli()}
	x.emitStepProgress(st, map[string]any{"phase": phase, "request": r.Name, "seq": seq, "state": "started"})
	resp, err := x.m.o.API.Chat(ctx, st.RunID, BuildChatRequest(b.Profile, r), func(p ChatProgress) {
		x.emitStepProgress(st, map[string]any{"phase": phase, "request": r.Name, "seq": seq, "state": "streaming", "tokens": p.Tokens, "elapsed_ms": p.ElapsedMS})
	})
	if err != nil {
		rr.Error = err.Error()
		return rr, nil, err
	}
	rr.Status, rr.WallMS, rr.TTFTMS = resp.Status, resp.WallMS, resp.TTFTMS
	if resp.Status != 200 {
		rr.Error = resp.ErrMessage
		return rr, resp, nil
	}
	rr.Timings = resp.Timings
	var t wireTimings
	if len(resp.Timings) > 0 && json.Unmarshal(resp.Timings, &t) == nil {
		rr.PromptN, rr.PromptMS, rr.PromptTPS = int(t.PromptN), t.PromptMS, t.PromptPerSecond
		rr.PredictedN, rr.PredictedMS, rr.DecodeTPS, rr.CacheN = int(t.PredictedN), t.PredictedMS, t.PredictedPerSecond, int(t.CacheN)
	}
	if len(resp.Message) > 0 {
		rr.OutputSHA256 = HashMessage(resp.Message)
	}
	rr.OutputHead = Head(resp.Text, 400)
	return rr, resp, nil
}

func (x *exec) runBenchmark(b *benchspec.Benchmark, idx int, group string, g *gate, member int) (st *Step) {
	m := x.m
	st = &Step{RunID: x.a.run.ID, Idx: idx, Group: group, BenchmarkID: b.ID, Name: b.Title(), Profile: b.Profile, State: StepRunning,
		StartedAt: m.o.Now().UnixMilli(), Spec: b}
	if err := m.o.Store.AddStep(st); err != nil {
		st.State, st.Outcome, st.Error = StepFailed, OutcomeError, "cannot store the step: "+err.Error()
		x.a.cancel("the database failed: " + err.Error())
		if g != nil {
			g.arrive(member)
		}
		return st
	}
	x.a.mu.Lock()
	if x.a.step == nil {
		x.a.step = map[string]any{"idx": idx, "benchmark_id": b.ID, "name": st.Name, "profile": b.Profile}
	}
	x.a.mu.Unlock()
	m.emit(map[string]any{"kind": "step_started", "run_id": st.RunID, "step": stepBrief(st)})

	var reqs []RequestResult
	trigger := false // this step started the load
	ctx, cancelCtx := context.WithTimeout(context.Background(), b.Timeout())
	defer cancelCtx()
	defer func() { // finish: statistics, expectations, comparison, storage, events
		if g != nil {
			g.arrive(member)
		}
		x.finish(st, b, reqs, trigger)
	}()

	fail := func(outcome, why string) *Step {
		st.Outcome, st.Error = outcome, why
		return st
	}
	// 1. What the scheduler says it would do.
	plan, err := m.o.API.Plan(ctx, b.Profile)
	if err != nil {
		return fail(OutcomeError, "plan: "+err.Error())
	}
	st.Plan = plan.Raw
	// 2. Cold: make the profile leave, so the load is measured.
	status, err := m.o.API.Status(ctx)
	if err != nil {
		return fail(OutcomeError, "status: "+err.Error())
	}
	if in := status.Instance(b.Profile); in != nil && b.IsCold() {
		res, err := m.o.API.Unload(ctx, b.Profile, false)
		switch {
		case err != nil:
			return fail(OutcomeError, "unload: "+err.Error())
		case res.Status == 409:
			return fail(OutcomeError, "the profile is busy, so it cannot be made cold: "+res.Msg)
		case res.Status != 200:
			return fail(OutcomeError, fmt.Sprintf("unload answered %d: %s", res.Status, res.Msg))
		}
		if !x.waitGone(ctx, b.Profile) {
			if x.a.cancelled() {
				return fail(OutcomeCancelled, "cancelled")
			}
			return fail(OutcomeError, "the profile did not leave the scheduler in time")
		}
	}
	if g != nil { // a parallel step: every member fires its first request together
		g.arrive(member)
		g.wait()
	}
	if x.a.cancelled() {
		return fail(OutcomeCancelled, "cancelled before the first request")
	}
	// 3. The first request, which loads the model if it is not loaded.
	status, err = m.o.API.Status(ctx)
	if err != nil {
		return fail(OutcomeError, "status: "+err.Error())
	}
	seq := 0
	var obs *loadObs
	if in := status.Instance(b.Profile); in == nil || in.State != "ready" {
		trigger = true
		seq++
		var stopWatch func()
		obs, stopWatch = x.watchLoad(st)
		startMS := m.o.Now().UnixMilli()
		reqCtx, cancelReq := context.WithCancel(ctx)
		go func() { // a cancel does not wait out a long load: this run's own load is stopped below
			select {
			case <-x.a.cancelCh:
				cancelReq()
			case <-reqCtx.Done():
			}
		}()
		rr, resp, err := x.send(reqCtx, st, b, b.Requests[0], PhaseLoad, seq)
		cancelReq()
		stopWatch()
		reqs = append(reqs, *rr)
		m.o.Store.AddRequest(rr)
		if err != nil {
			x.a.markLoaded(b.Profile, true) // the load may be running: it is unloaded with force at the end
			switch {
			case x.a.cancelled():
				return fail(OutcomeCancelled, "cancelled while the model was loading")
			case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
				return fail(OutcomeFailed, fmt.Sprintf("timed out after %s waiting for the first request (timeout_s)", b.Timeout()))
			}
			return fail(OutcomeError, "the first request failed: "+err.Error())
		}
		st.HTTPStatus = resp.Status
		lf, _ := obs.get()
		st.LogFile = lf
		if resp.Status != 200 {
			st.Reason = resp.ErrMessage
			if resp.Status == 503 || (resp.Status >= 400 && resp.Status < 500) {
				st.Outcome = OutcomeRefused
			} else {
				st.Outcome = OutcomeFailed
			}
			x.captureLoad(ctx, st, b, startMS, obs, false)
			return st
		}
		x.a.markLoaded(b.Profile, true)
		x.captureLoad(ctx, st, b, startMS, obs, true)
		if rr.WallMS > 0 && st.Load != nil && st.Load.LoadS == 0 {
			st.Load.LoadS = round1(rr.WallMS / 1000)
			st.Load.Source = "request_wall"
		}
	} else if x.a.cancelled() {
		return fail(OutcomeCancelled, "cancelled")
	}
	st.Outcome = OutcomeReady
	// 4. Warmup (recorded, left out of the statistics), then the measured requests.
	one := func(r benchspec.Request, phase string) bool {
		if x.a.cancelled() {
			st.Outcome, st.Error = OutcomeCancelled, "cancelled"
			return false
		}
		seq++
		rr, resp, err := x.send(ctx, st, b, r, phase, seq)
		reqs = append(reqs, *rr)
		m.o.Store.AddRequest(rr)
		switch {
		case err != nil && ctx.Err() != nil:
			st.Outcome, st.Error = OutcomeFailed, fmt.Sprintf("timed out after %s (timeout_s)", b.Timeout())
			return false
		case err != nil:
			st.Outcome, st.Error = OutcomeError, "request failed: "+err.Error()
			return false
		case resp.Status != 200:
			st.Outcome, st.HTTPStatus, st.Reason = OutcomeFailed, resp.Status, resp.ErrMessage
			return false
		}
		return true
	}
	for i := 0; i < b.Warmups(); i++ {
		if !one(b.Requests[0], PhaseWarmup) {
			return st
		}
	}
	for _, r := range b.Requests {
		for i := 0; i < b.RepeatCount(); i++ {
			if !one(r, PhaseMeasured) {
				return st
			}
		}
	}
	return st
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// waitGone waits until the profile has no instance.
func (x *exec) waitGone(ctx context.Context, profile string) bool {
	deadline := time.Now().Add(x.m.o.GoneTimeout)
	for {
		s, err := x.m.o.API.Status(ctx)
		if err == nil && s.Instance(profile) == nil {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		if !x.sleep(x.m.o.IdlePoll) {
			return false
		}
	}
}

// captureLoad reads what the scheduler knows of the load that the first
// request caused: its session in recent_loads, the history record of that
// session (bytes, MB/s, per node bytes and memory) and the instance's measured
// memory. A load that failed has a session too, with its reason.
func (x *exec) captureLoad(ctx context.Context, st *Step, b *benchspec.Benchmark, startMS int64, obs *loadObs, ready bool) {
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := x.m.o.API.Status(cctx)
	if err != nil {
		return
	}
	lr := &LoadResult{}
	var recent *RecentLoad
	for i := range s.RecentLoads {
		rl := &s.RecentLoads[i]
		if rl.Model == b.Profile && rl.FinishedAt >= startMS-2000 {
			recent = rl
			break // newest first
		}
	}
	_, last := obs.get()
	switch {
	case recent != nil:
		lr.SessionID, lr.Outcome, lr.Reason, lr.LoadS, lr.Workers, lr.Source = recent.SessionID, recent.Outcome, recent.Reason, recent.LoadS, recent.Workers, "recent_loads"
	case last != nil:
		lr.SessionID, lr.Source = last.SessionID, "observed"
	default:
		if !ready {
			return
		}
	}
	if lr.SessionID != "" {
		if h, err := x.m.o.API.LoadHistory(cctx, lr.SessionID); err == nil && h != nil {
			lr.Source = "history"
			if h.LoadS > 0 {
				lr.LoadS = h.LoadS
			}
			lr.Bytes, lr.AvgMBs, lr.Nodes = h.Bytes, h.AvgMBs, h.Nodes
		}
	}
	if lr.Bytes == 0 && last != nil {
		switch {
		case last.BytesSent != nil && *last.BytesSent > 0:
			lr.Bytes = *last.BytesSent
		case last.BytesTotal != nil && ready:
			lr.Bytes = *last.BytesTotal
		}
	}
	if lr.AvgMBs == 0 && lr.Bytes > 0 && lr.LoadS > 0 {
		lr.AvgMBs = round1(float64(lr.Bytes) / 1e6 / lr.LoadS)
	}
	if in := s.Instance(b.Profile); in != nil && ready {
		lr.MemoryMiB = in.DeviceTotalMiB
		if len(lr.Nodes) == 0 {
			for _, n := range in.Nodes {
				name := s.Hostnames[n.ID]
				if name == "" {
					name = n.ID
				}
				mem := n.MemoryMiB
				nl := NodeLoad{Node: name}
				if mem > 0 {
					nl.MemoryMiB = &mem
				}
				lr.Nodes = append(lr.Nodes, nl)
			}
		}
		if lr.MemoryMiB == 0 {
			for _, n := range lr.Nodes {
				if n.MemoryMiB != nil {
					lr.MemoryMiB += *n.MemoryMiB
				}
			}
		}
		if st.LogFile == "" {
			st.LogFile = in.LogFile
		}
	}
	st.Load = lr
}

// finish completes a step: statistics, expectations, comparison, unload, storage and events.
func (x *exec) finish(st *Step, b *benchspec.Benchmark, reqs []RequestResult, triggered bool) {
	m := x.m
	if st.Outcome == "" {
		st.Outcome = OutcomeError
		if st.Error == "" {
			st.Error = "the step ended without an outcome"
		}
	}
	st.Stats = ComputeStats(reqs)
	if st.Outcome == OutcomeReady || st.Outcome == OutcomeRefused || st.Outcome == OutcomeFailed {
		st.State = StepDone
	} else if st.Outcome == OutcomeCancelled {
		st.State = StepCancelled
	} else {
		st.State = StepFailed
	}
	// unload_after: this run's own load goes away unless a later benchmark may use it (the run end cleans up).
	if b.UnloadsAfter() && x.a.loadedSet()[b.Profile] {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		res, err := m.o.API.Unload(ctx, b.Profile, st.Outcome == OutcomeCancelled || st.Outcome == OutcomeFailed)
		cancel()
		if err == nil && (res.Status == 200) {
			x.a.markLoaded(b.Profile, false)
		}
	}
	st.FinishedAt = m.o.Now().UnixMilli()
	if st.Outcome != OutcomeCancelled || len(reqs) > 0 {
		st.Expect = Evaluate(b, st)
	}
	if base := x.base[b.ID]; base != nil && st.Outcome != OutcomeCancelled {
		st.Comparison = Compare(st, base, x.a.snapshot().BaselineRunID, m.o.Thresholds)
	}
	if err := m.o.Store.UpdateStep(st); err != nil {
		m.log.Error("cannot store a benchmark step", "run", st.RunID, "benchmark", b.ID, "err", err)
		x.a.cancel("the database failed: " + err.Error())
	}
	x.a.mu.Lock()
	x.a.done++
	x.a.step = nil
	x.a.mu.Unlock()
	m.emit(map[string]any{"kind": "step_finished", "run_id": st.RunID, "step": stepBrief(st)})
	sev := history.Info
	pass := st.Expect != nil && st.Expect.Pass
	if !pass || (st.Comparison != nil && st.Comparison.Regression) {
		sev = history.Warn
	}
	data := map[string]any{"bench_run": st.RunID, "benchmark": b.ID, "idx": st.Idx, "outcome": st.Outcome, "pass": pass}
	if st.Load != nil {
		data["load_s"] = st.Load.LoadS
	}
	if st.Stats != nil && st.Stats.Decode != nil {
		data["decode_tps"] = st.Stats.Decode.Mean
	}
	if st.Comparison != nil {
		data["regression"] = st.Comparison.Regression
	}
	text := fmt.Sprintf("Benchmark %s on %s finished: %s", b.ID, b.Profile, st.Outcome)
	if st.Load != nil && st.Load.LoadS > 0 {
		text += fmt.Sprintf(", load %.1f s", st.Load.LoadS)
	}
	if st.Stats != nil && st.Stats.Decode != nil {
		text += fmt.Sprintf(", decode %.1f tok/s", st.Stats.Decode.Mean)
	}
	if !pass {
		text += ", expectations not met"
	}
	if st.Comparison != nil && st.Comparison.Regression {
		text += ", REGRESSION against the baseline"
	}
	m.record(history.TBenchStepFinished, sev, b.Profile, text+".", data)
	_ = triggered
}
