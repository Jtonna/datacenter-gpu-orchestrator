// DEV-ONLY MOCK of a server contract that is not built yet: the benchmark
// engine. It plays a suite run in real time, compressed (each step about 12
// to 25 s), emits the bench WebSocket frames (run, step_started,
// step_finished, progress), stores the finished run and records the bench
// history events. The bench-* scenarios freeze a run part way through.

package main

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/api"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

type benchEngine struct {
	mu     sync.Mutex
	store  *benchStore
	emit   func(ev map[string]any) // a bench frame's data, to every live client
	record func(history.Event)     // nil when history is off
	cur    *activeRun
	queue  []*mRun
	rnd    *rand.Rand
}

type activeRun struct {
	run       *mRun
	frozen    string // "", "load", "conversation", "last"
	frozenAt  time.Time
	stop      chan struct{}
	cancelled bool
	stepIdx   int
	phaseIdx  int
	phaseFrac float64
	detail    map[string]any
}

func newBenchEngine(store *benchStore) *benchEngine {
	return &benchEngine{store: store, rnd: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (e *benchEngine) send(ev map[string]any) {
	if e.emit != nil {
		e.emit(ev)
	}
}

func (e *benchEngine) rec(typ, sev, profile, summary string, data map[string]any) {
	if e.record != nil {
		e.record(history.Event{Ts: time.Now().UnixMilli(), Type: typ, Severity: sev, Profile: profile, ModelFile: modelFileOf(profile), Summary: summary, Data: data})
	}
}

// ---- state and progress ----

func nextAt(hhmm string, now time.Time) time.Time {
	var h, m int
	fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	y, mo, d := now.Date()
	t := time.Date(y, mo, d, h, m, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

func (e *benchEngine) stateDoc() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stateLocked()
}

func (e *benchEngine) stateLocked() map[string]any {
	now := time.Now()
	doc := map[string]any{"enabled": true, "current": nil, "queue": []any{}, "next_scheduled": []any{}, "run": nil}
	if e.cur != nil {
		doc["current"] = e.cur.run.brief()
		doc["run"] = e.progressLocked(now)
	}
	q := []any{}
	for _, r := range e.queue {
		q = append(q, r.brief())
	}
	doc["queue"] = q
	e.store.mu.Lock()
	ns := []any{}
	for _, id := range sortedKeysOf(e.store.suites) {
		su := e.store.suites[id]
		sc := asMap(su["schedule"])
		if at, ok := sc["at"].(string); ok {
			name, _ := su["name"].(string)
			ns = append(ns, map[string]any{"suite_id": id, "name": name, "at": at, "next": nextAt(at, now).UnixMilli()})
		} else if every, ok := sc["every"].(string); ok {
			d, _ := time.ParseDuration(every)
			name, _ := su["name"].(string)
			ns = append(ns, map[string]any{"suite_id": id, "name": name, "every": every, "next": now.Add(d).UnixMilli()})
		}
	}
	e.store.mu.Unlock()
	doc["next_scheduled"] = ns
	return doc
}

func stepRef(s *mStep) map[string]any {
	return map[string]any{"benchmark_id": s.BenchmarkID, "name": s.Name, "description": s.Description, "profile": s.Profile}
}

func (e *benchEngine) progressLocked(now time.Time) map[string]any {
	a := e.cur
	run := a.run
	done := 0
	for _, s := range run.Steps {
		if s.State == bench.StepDone || s.State == bench.StepFailed {
			done++
		}
	}
	total := len(run.Steps)
	var cur, next any
	stepPct := 0.0
	queue := []string{}
	for _, s := range run.Steps {
		queue = append(queue, s.BenchmarkID)
	}
	if a.stepIdx < total && run.State == bench.RunRunning {
		s := run.Steps[a.stepIdx]
		n := max(len(s.Phases), 1)
		stepPct = (float64(a.phaseIdx) + a.phaseFrac) / float64(n)
		var phase any
		if a.phaseIdx < len(s.Phases) {
			p := s.Phases[a.phaseIdx]
			phase = map[string]any{"index": p.Index, "type": p.Type, "label": p.Label, "of": len(s.Phases)}
		}
		detail := a.detail
		if detail == nil {
			detail = map[string]any{}
		}
		cur = map[string]any{"index": a.stepIdx, "benchmark_id": s.BenchmarkID, "name": s.Name, "description": s.Description, "profile": s.Profile,
			"model_file": s.ModelFile, "nodes": s.Nodes, "phase": phase, "percent": math.Round(stepPct*1000) / 10, "detail": detail}
		if a.stepIdx+1 < total {
			next = stepRef(run.Steps[a.stepIdx+1])
		}
	}
	percent := 0.0
	if total > 0 {
		percent = math.Round((float64(done)+stepPct)/float64(total)*1000) / 10
	}
	if run.State != bench.RunRunning && bench.IsFinal(run.State) {
		percent = 100
	}
	elapsed := 0.0
	if run.StartedAt > 0 {
		elapsed = math.Round(float64(now.UnixMilli()-run.StartedAt)/100) / 10
	}
	var eta any
	if percent >= 2 && percent < 100 {
		eta = math.Round(elapsed/(percent/100) - elapsed)
	}
	return map[string]any{"id": run.ID, "suite_id": run.SuiteID, "suite_name": run.SuiteName, "state": run.State, "percent": percent,
		"started_at": run.StartedAt, "elapsed_s": elapsed, "eta_s": eta, "steps_total": total, "steps_done": done,
		"current": cur, "next": next, "queue": queue}
}

func (e *benchEngine) emitProgressLocked() {
	if e.cur == nil {
		return
	}
	e.send(map[string]any{"kind": "progress", "run_id": e.cur.run.ID, "run": e.progressLocked(time.Now())})
}

func (e *benchEngine) emitRunLocked(r *mRun) {
	e.send(map[string]any{"kind": "run", "run": r.brief()})
}

// ---- details ----

// loadDetail is a synthetic load object (the snake_case LoadReport) at
// progress p of a phase that lasts dur.
func loadDetail(s *mStep, started time.Time, p float64, dur time.Duration) map[string]any {
	total := modelBytes[s.ModelFile]
	if len(s.Nodes) == 0 || total == 0 {
		return map[string]any{}
	}
	fr := fractions(mustProfile(s.Profile), len(s.Nodes))
	sent := int64(float64(total) * p)
	remaining := sent
	workers := []any{}
	for i, h := range s.Nodes {
		n, _ := nodeByHost(h)
		wt := int64(float64(total) * fr[i])
		ws := min(remaining, wt)
		remaining -= ws
		state := "receiving"
		if ws >= wt {
			state = "done"
		} else if ws <= 0 {
			state = "waiting"
		}
		workers = append(workers, map[string]any{"addr": n.Addr(), "bytes_total": wt, "bytes_sent": ws, "state": state})
	}
	el := time.Since(started).Seconds()
	return map[string]any{"session_id": sessionID(started, int(s.ID%97)), "started_at": started.UnixMilli(), "elapsed_s": round1(el),
		"bytes_total": total, "bytes_sent": sent, "progress": math.Round(p*1000) / 1000, "eta_s": math.Round((1 - p) * dur.Seconds()),
		"source": "measured", "workers": workers}
}

func requestDetail(phaseType string, done, total int, name string, inReq time.Duration, tps float64, turn, turns int) map[string]any {
	tokens := int(inReq.Seconds() * tps)
	state := "streaming"
	if tokens == 0 {
		state = "started"
	}
	d := map[string]any{"requests_done": done, "requests_total": total, "request": name, "state": state, "tokens": tokens,
		"elapsed_ms": inReq.Milliseconds(), "tokens_per_second": tps}
	if phaseType == "conversation" {
		d["turn"], d["turns_total"] = turn, turns
	}
	if phaseType == "concurrent_requests" {
		d["in_flight"] = total - done
	}
	return d
}

// ---- enqueue, cancel, play ----

func (e *benchEngine) enqueue(suiteID, baseline, note, trigger string) (mRun, int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store.mu.Lock()
	su, ok := e.store.suites[suiteID]
	if !ok {
		e.store.mu.Unlock()
		return mRun{}, 404, "not found"
	}
	if len(e.queue) >= 20 {
		e.store.mu.Unlock()
		return mRun{}, 429, "queue full: 20 runs are queued: wait for a run to finish or cancel one"
	}
	if baseline != "" && e.store.runByID(baseline) == nil {
		e.store.mu.Unlock()
		return mRun{}, 422, fmt.Sprintf("baseline run %q does not exist", baseline)
	}
	now := time.Now()
	e.store.runSeq++
	run := &mRun{ID: runID(now, suiteID, e.store.runSeq), SuiteID: suiteID, SuiteName: fmt.Sprint(su["name"]), State: bench.RunQueued,
		Note: note, Trigger: trigger, QueuedAt: now.UnixMilli(), Version: devVersion, Suite: cloneMap(su)}
	if baseline == "" {
		if b := e.store.lastCompleted(suiteID, 0); b != nil {
			baseline = b.ID
		}
	}
	run.BaselineRunID = baseline
	for i, id := range asList(su["benchmarks"]) {
		if bm := e.store.benchmarks[fmt.Sprint(id)]; bm != nil {
			run.Steps = append(run.Steps, e.store.newStep(specOf(bm), i))
		}
	}
	e.store.runs = append(e.store.runs, run)
	out := run.short()
	e.store.mu.Unlock()
	e.emitRunLocked(run)
	if e.cur == nil {
		e.startLocked(run)
	} else {
		e.queue = append(e.queue, run)
	}
	return out, 0, ""
}

func (e *benchEngine) startLocked(run *mRun) {
	run.State, run.StartedAt = bench.RunRunning, time.Now().UnixMilli()
	a := &activeRun{run: run, stop: make(chan struct{})}
	e.cur = a
	e.emitRunLocked(run)
	e.rec(history.TBenchRunStarted, history.Info, "", fmt.Sprintf("Benchmark run %s of suite %s started (%d steps).", run.ID, run.SuiteName, len(run.Steps)),
		map[string]any{"bench_run": run.ID, "suite": run.SuiteID, "steps": len(run.Steps), "trigger": run.Trigger, "baseline": run.BaselineRunID})
	go e.play(a)
}

func (e *benchEngine) cancel(id string) (int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur != nil && e.cur.run.ID == id {
		if e.cur.frozen != "" {
			e.finishFrozenLocked(e.cur)
			return 0, ""
		}
		if !e.cur.cancelled {
			e.cur.cancelled = true
			close(e.cur.stop)
		}
		return 0, ""
	}
	for i, r := range e.queue {
		if r.ID == id {
			e.queue = append(e.queue[:i], e.queue[i+1:]...)
			e.store.mu.Lock()
			r.State, r.FinishedAt, r.Error = bench.RunCancelled, time.Now().UnixMilli(), "cancelled before it started"
			for _, s := range r.Steps {
				s.State, s.Outcome = bench.StepSkipped, bench.OutcomeSkipped
			}
			r.Summary = summaryOf(r.Steps)
			e.store.mu.Unlock()
			e.emitRunLocked(r)
			return 0, ""
		}
	}
	e.store.mu.Lock()
	r := e.store.runByID(id)
	e.store.mu.Unlock()
	if r == nil {
		return 404, "not found"
	}
	return 409, fmt.Sprintf("run %s is %s: only a queued or running run can be cancelled", id, r.State)
}

// phaseDuration is how long the compressed player spends in a phase.
func (e *benchEngine) phaseDuration(s *mStep, p phaseSpec, n int) time.Duration {
	sec := func(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }
	switch p.Type {
	case "load":
		if len(s.Nodes) == 0 {
			return 2 * time.Second
		}
		return sec(6 + e.rnd.Float64()*4)
	case "warmup":
		return sec(1.6 * float64(max(n, 1)))
	case "requests", "measured":
		return sec(math.Min(1.6*float64(max(n, 1)), 12))
	case "conversation":
		return sec(math.Min(2.2*float64(max(n, 1)), 13))
	case "hold":
		return sec(4.5)
	case "concurrent_requests":
		return sec(4)
	}
	return time.Second
}

// play runs one active run to its end. It holds e.mu only to change state.
func (e *benchEngine) play(a *activeRun) {
	run := a.run
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	e.store.mu.Lock()
	baseline := e.store.runByID(run.BaselineRunID)
	e.store.mu.Unlock()
	for i, s := range run.Steps {
		if a.cancelled {
			break
		}
		e.mu.Lock()
		stepStart := time.Now()
		s.State, s.StartedAt = bench.StepRunning, stepStart.UnixMilli()
		a.stepIdx, a.phaseIdx, a.phaseFrac, a.detail = i, 0, 0, map[string]any{}
		e.send(map[string]any{"kind": "step_started", "run_id": run.ID, "step": stepBrief(s)})
		e.mu.Unlock()
		sp := specOf(s.Spec)
		ps, simple := phasesOf(s.Spec)
		tps := perf[s.Profile].decode
		if tps == 0 {
			tps = 14
		}
		var phaseTimes [][2]int64
		for pi, p := range ps {
			reqs := requestsOf(sp.bm, p, simple)
			dur := e.phaseDuration(s, p, len(reqs))
			start := time.Now()
			e.mu.Lock()
			a.phaseIdx, a.phaseFrac = pi, 0
			s.Phases[pi].State, s.Phases[pi].StartedAt = "running", start.UnixMilli()
			e.mu.Unlock()
			lastEmit := time.Time{}
			for {
				el := time.Since(start)
				frac := math.Min(el.Seconds()/dur.Seconds(), 1)
				e.mu.Lock()
				a.phaseFrac = frac
				switch {
				case p.Type == "load":
					a.detail = loadDetail(s, start, frac, dur)
				case p.Type == "unload" || len(reqs) == 0:
					a.detail = map[string]any{}
				case p.Type == "concurrent_requests":
					a.detail = requestDetail(p.Type, 0, len(reqs), reqs[0].name, el, tps*0.42, 0, 0)
				default:
					per := dur / time.Duration(len(reqs))
					k := min(int(el/per), len(reqs)-1)
					a.detail = requestDetail(p.Type, k, len(reqs), reqs[k].name, el-time.Duration(k)*per, tps, reqs[k].turn, len(reqs))
				}
				if time.Since(lastEmit) >= 500*time.Millisecond {
					lastEmit = time.Now()
					e.emitProgressLocked()
				}
				e.mu.Unlock()
				if frac >= 1 || a.cancelled {
					break
				}
				select {
				case <-tick.C:
				case <-a.stop:
				}
			}
			phaseTimes = append(phaseTimes, [2]int64{start.UnixMilli(), time.Now().UnixMilli()})
			if a.cancelled {
				break
			}
		}
		e.mu.Lock()
		if a.cancelled {
			s.State, s.Outcome, s.FinishedAt = bench.StepCancelled, bench.OutcomeCancelled, time.Now().UnixMilli()
			for k := range s.Phases {
				switch {
				case k < len(phaseTimes)-1:
					s.Phases[k].State = "done"
				case k == len(phaseTimes)-1:
					s.Phases[k].State = "cancelled"
				default:
					s.Phases[k].State = "skipped"
				}
			}
		} else {
			e.store.mu.Lock()
			e.store.fillStep(s, sp, stepStart, e.rnd, 1, baseline)
			e.store.mu.Unlock()
			for k, pt := range phaseTimes {
				if s.Phases[k].State != "skipped" {
					s.Phases[k].StartedAt, s.Phases[k].FinishedAt, s.Phases[k].WallMS = pt[0], pt[1], float64(pt[1]-pt[0])
				}
			}
			s.FinishedAt = time.Now().UnixMilli()
			e.recordStepLocked(run, s)
		}
		e.send(map[string]any{"kind": "step_finished", "run_id": run.ID, "step": stepBrief(s)})
		e.emitProgressLocked()
		e.mu.Unlock()
		if !a.cancelled && i+1 < len(run.Steps) {
			select {
			case <-time.After(time.Second):
			case <-a.stop:
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur != a {
		return // reset by a scenario switch
	}
	e.finishLocked(a)
}

func (e *benchEngine) recordStepLocked(run *mRun, s *mStep) {
	pass := s.Expect != nil && s.Expect.Pass
	sev := history.Info
	if !pass || (s.Comparison != nil && s.Comparison.Regression) {
		sev = history.Warn
	}
	data := map[string]any{"bench_run": run.ID, "benchmark": s.BenchmarkID, "idx": s.Idx, "outcome": s.Outcome, "pass": pass}
	text := fmt.Sprintf("Benchmark %s on %s finished: %s", s.BenchmarkID, s.Profile, s.Outcome)
	if s.Load != nil {
		data["load_s"] = s.Load.LoadS
		text += fmt.Sprintf(", load %.1f s", s.Load.LoadS)
	}
	if s.Stats != nil && s.Stats.Decode != nil {
		data["decode_tps"] = s.Stats.Decode.Mean
		text += fmt.Sprintf(", decode %.1f tok/s", s.Stats.Decode.Mean)
	}
	if s.Comparison != nil {
		data["regression"] = s.Comparison.Regression
		if s.Comparison.Regression {
			text += ", REGRESSION against the baseline"
		}
	}
	if !pass {
		text += ", expectations not met"
	}
	e.rec(history.TBenchStepFinished, sev, s.Profile, text+".", data)
}

// finishLocked ends the active run and starts the next queued one.
func (e *benchEngine) finishLocked(a *activeRun) {
	run := a.run
	now := time.Now()
	e.store.mu.Lock()
	state := bench.RunCompleted
	if a.cancelled {
		state = bench.RunCancelled
		for _, s := range run.Steps {
			if s.State == bench.StepPending {
				s.State, s.Outcome = bench.StepSkipped, bench.OutcomeSkipped
				for k := range s.Phases {
					s.Phases[k].State = "skipped"
				}
			}
		}
	}
	run.State, run.FinishedAt = state, now.UnixMilli()
	run.Summary = summaryOf(run.Steps)
	e.store.mu.Unlock()
	e.cur = nil
	e.emitRunLocked(run)
	sev := history.Info
	if run.Summary.Failed > 0 || run.Summary.Regressions > 0 || state != bench.RunCompleted {
		sev = history.Warn
	}
	e.rec(history.TBenchRunFinished, sev, "", fmt.Sprintf("Benchmark run %s finished: %s, %d of %d steps passed, %d regressions.", run.ID, state,
		run.Summary.Passed, run.Summary.Steps, run.Summary.Regressions),
		map[string]any{"bench_run": run.ID, "suite": run.SuiteID, "state": state, "error": run.Error, "steps": run.Summary.Steps, "passed": run.Summary.Passed,
			"failed": run.Summary.Failed, "regressions": run.Summary.Regressions, "duration_s": float64(run.FinishedAt-run.QueuedAt) / 1000})
	if len(e.queue) > 0 {
		next := e.queue[0]
		e.queue = e.queue[1:]
		e.startLocked(next)
	}
}

// ---- frozen scenarios ----

func shiftStep(s *mStep, d int64) {
	sh := func(v *int64) {
		if *v != 0 {
			*v += d
		}
	}
	sh(&s.StartedAt)
	sh(&s.FinishedAt)
	for i := range s.Phases {
		sh(&s.Phases[i].StartedAt)
		sh(&s.Phases[i].FinishedAt)
	}
	for i := range s.Requests {
		sh(&s.Requests[i].StartedAt)
	}
}

// freeze builds a nightly run frozen in the middle: kind is load,
// conversation or last. since is when the scenario was selected.
func (e *benchEngine) freeze(kind string, since time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store.mu.Lock()
	su := e.store.suites["nightly"]
	if su == nil {
		e.store.mu.Unlock()
		return
	}
	cur := map[string]int{"load": 6, "conversation": 3, "last": 7}[kind]
	phase := map[string]int{"load": 0, "conversation": 1, "last": 1}[kind]
	e.store.runSeq++
	start := since.Add(-2 * time.Hour)
	run := &mRun{ID: runID(start, "nightly", e.store.runSeq), SuiteID: "nightly", SuiteName: fmt.Sprint(su["name"]), State: bench.RunRunning,
		Trigger: "manual", Note: "run by hand after the link change", Version: devVersion, Suite: cloneMap(su)}
	baseline := e.store.lastCompleted("nightly", 0)
	if baseline != nil {
		run.BaselineRunID = baseline.ID
	}
	rnd := rand.New(rand.NewSource(11))
	t := start
	for i, id := range asList(su["benchmarks"]) {
		sp := specOf(e.store.benchmarks[fmt.Sprint(id)])
		s := e.store.newStep(sp, i)
		run.Steps = append(run.Steps, s)
		if i < cur {
			t = e.store.fillStep(s, sp, t, rnd, 1, baseline).Add(10 * time.Second)
		}
	}
	// the current step: its earlier phases are done
	s := run.Steps[cur]
	curStart := map[string]time.Duration{"load": 130 * time.Second, "conversation": 50*time.Second + 45*time.Second, "last": 95*time.Second + 50*time.Second}[kind]
	stepStart := since.Add(-curStart)
	delta := stepStart.Add(-10*time.Second).UnixMilli() - t.UnixMilli()
	for i := 0; i < cur; i++ {
		shiftStep(run.Steps[i], delta)
	}
	run.StartedAt = start.UnixMilli() + delta
	run.QueuedAt = run.StartedAt - 2000
	s.State, s.StartedAt = bench.StepRunning, stepStart.UnixMilli()
	pt := stepStart
	for k := range s.Phases {
		switch {
		case k < phase:
			d := map[string]time.Duration{"conversation": 50 * time.Second, "last": 95 * time.Second}[kind]
			s.Phases[k].State, s.Phases[k].StartedAt, s.Phases[k].FinishedAt, s.Phases[k].WallMS = "done", pt.UnixMilli(), pt.Add(d).UnixMilli(), float64(d.Milliseconds())
			pt = pt.Add(d)
		case k == phase:
			s.Phases[k].State, s.Phases[k].StartedAt = "running", pt.UnixMilli()
		}
	}
	e.store.runs = append(e.store.runs, run)
	e.store.mu.Unlock()
	a := &activeRun{run: run, frozen: kind, frozenAt: since, stop: make(chan struct{}), stepIdx: cur, phaseIdx: phase}
	e.cur = a
	e.updateFrozenLocked(a)
	go func() {
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-tk.C:
			}
			e.mu.Lock()
			if e.cur != a {
				e.mu.Unlock()
				return
			}
			e.updateFrozenLocked(a)
			e.emitProgressLocked()
			e.mu.Unlock()
		}
	}()
}

// updateFrozenLocked moves the ticking figures of a frozen run: the load
// progress creeps and wraps, tokens count up and start over.
func (e *benchEngine) updateFrozenLocked(a *activeRun) {
	t := time.Since(a.frozenAt)
	s := a.run.Steps[a.stepIdx]
	switch a.frozen {
	case "load":
		b := loadingOf(s.Profile, time.Now(), t, false)
		in := b.st.Instances[0]
		a.detail = api.StatusJSON(in.Load).(map[string]any)
		a.phaseFrac = *in.Load.Progress
	case "conversation":
		cycle := 16 * time.Second
		in := t % cycle
		a.detail = requestDetail("conversation", 2, 5, "turn-3", in, 11.8, 3, 5)
		a.phaseFrac = (2 + 0.5) / 5
	case "last":
		cycle := 18 * time.Second
		in := t % cycle
		a.detail = requestDetail("requests", 4, 6, "refactor", in, 8.0, 0, 0)
		a.phaseFrac = (4 + 0.5) / 6
	}
}

func (e *benchEngine) finishFrozenLocked(a *activeRun) {
	close(a.stop)
	a.cancelled = true
	e.store.mu.Lock()
	s := a.run.Steps[a.stepIdx]
	s.State, s.Outcome, s.FinishedAt = bench.StepCancelled, bench.OutcomeCancelled, time.Now().UnixMilli()
	for k := range s.Phases {
		if s.Phases[k].State == "running" {
			s.Phases[k].State = "cancelled"
		} else if s.Phases[k].State == "pending" {
			s.Phases[k].State = "skipped"
		}
	}
	e.store.mu.Unlock()
	e.finishLocked(a)
}

// reset stops everything and reseeds the store (empty when bare).
func (e *benchEngine) reset(bare bool, frozen string, since time.Time) {
	e.mu.Lock()
	if e.cur != nil {
		if !e.cur.cancelled {
			e.cur.cancelled = true
			close(e.cur.stop)
		}
		e.cur = nil
	}
	e.queue = nil
	e.mu.Unlock()
	if bare {
		e.store.mu.Lock()
		e.store.benchmarks, e.store.suites, e.store.runs = map[string]map[string]any{}, map[string]map[string]any{}, nil
		e.store.mu.Unlock()
	} else {
		e.store.seed(time.Now())
	}
	if frozen != "" {
		e.freeze(frozen, since)
	}
	e.mu.Lock()
	e.send(map[string]any{"kind": "state", "state": e.stateLocked()})
	e.mu.Unlock()
}
