// DEV-ONLY MOCK of the benchmark runner of feat/bench-phases. It plays a
// suite run in real time, compressed (each step about 12 to 30 s; the
// members of a parallel step run side by side), emits the bench WebSocket
// frames of the real Manager (state, run, progress, step_started,
// step_finished, request, load, aggregate_updated), stores the finished run
// and records the bench history events. The progress document follows
// internal/bench/progress.go. The bench-* scenarios freeze a run part way
// through.

package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/api"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

type benchEngine struct {
	mu       sync.Mutex
	store    *benchStore
	emit     func(ev map[string]any) // a bench frame's data, to every live client
	record   func(history.Event)     // nil when history is off
	revision func() int              // the config revision a run records in env
	cur      *activeRun
	queue    []*mRun
	rnd      *rand.Rand
}

// track is the live progress of one step (progress.go benchTrack).
type track struct {
	step       *mStep
	phases     []phaseSpec
	reqs       [][]reqPlan
	durs       []float64 // expected seconds per phase
	cur        int       // -1 before the first phase
	frac       float64   // of the current phase
	detail     map[string]any
	load       map[string]any
	finished   bool
	maxPct     float64
	start      time.Time
	phaseAt    []time.Time // when each started phase began
	lastReq    int         // the request in flight (index into reqs[cur]); -1 none
	reqStarted time.Time
}

// runItem is one entry of the suite: a benchmark or a parallel step.
type runItem struct {
	id, name, desc, profile string
	steps                   []*mStep
	expected                float64
}

type activeRun struct {
	run       *mRun
	items     []runItem
	cur       int // item in flight; -1 before the first
	tracks    []*track
	done      int // steps finished
	maxPct    float64
	frozen    string // "", "load", "conversation", "last"
	frozenAt  time.Time
	stop      chan struct{}
	cancelled bool
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

// ---- tracks ----

// phaseSeconds is how long a phase is expected to take: compressed for the
// player, about real for the frozen scenarios.
func phaseSeconds(s *mStep, p phaseSpec, reqs []reqPlan, compressed bool, rnd *rand.Rand) float64 {
	n := float64(max(len(reqs), 1))
	refused := len(s.nodes) == 0
	if compressed {
		switch p.Type {
		case "load":
			if refused {
				return 2
			}
			return 6 + rnd.Float64()*4
		case "warmup":
			return 1.6 * n
		case "requests":
			return math.Min(1.6*n, 12)
		case "conversation":
			return math.Min(2.2*n, 13)
		case "hold":
			return 4.5
		case "concurrent_requests":
			return 4
		}
		return 1
	}
	switch p.Type {
	case "load":
		if refused {
			return 0.3
		}
		return perf[s.Profile].load
	case "warmup":
		return 3 * n
	case "requests":
		return 9 * n
	case "conversation":
		return 16 * n
	case "hold":
		return float64(intOpt(p.Opts, "seconds", 60)) + 5
	case "concurrent_requests":
		return 15
	}
	return 1
}

func newTrack(s *mStep, compressed bool, rnd *rand.Rand) *track {
	t := &track{step: s, phases: expandPhases(s.Spec), cur: -1, detail: map[string]any{}, lastReq: -1}
	for i, p := range t.phases {
		r := phaseRequests(t.phases, i)
		t.reqs = append(t.reqs, r)
		t.durs = append(t.durs, phaseSeconds(s, p, r, compressed, rnd))
	}
	return t
}

func (t *track) total() float64 {
	sum := 0.0
	for _, d := range t.durs {
		sum += d
	}
	return sum
}

// percent is how much of the step is done, never going back.
func (t *track) percent() float64 {
	var total, done float64
	for i, w := range t.durs {
		total += w
		switch {
		case t.finished || i < t.cur:
			done += w
		case i == t.cur:
			done += w * t.frac
		}
	}
	pct := 0.0
	if total > 0 {
		pct = 100 * done / total
	}
	if !t.finished && pct > 99.9 {
		pct = 99.9
	}
	if t.finished {
		pct = 100
	}
	pct = math.Max(pct, t.maxPct)
	t.maxPct = pct
	return pct
}

func (t *track) remaining() float64 {
	if t.finished {
		return 0
	}
	secs := 0.0
	for i, w := range t.durs {
		switch {
		case i == t.cur:
			secs += w * (1 - t.frac)
		case i > t.cur:
			secs += w
		}
	}
	return secs
}

func (t *track) phaseDoc() map[string]any {
	i := max(t.cur, 0)
	d := map[string]any{"index": i, "type": "", "label": "", "phases_total": len(t.phases)}
	if i < len(t.phases) {
		d["type"], d["label"] = t.phases[i].Type, t.phases[i].title()
	}
	return d
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

// stateLocked is Manager.State: {enabled, run, current, queue, next_scheduled}.
func (e *benchEngine) stateLocked() map[string]any {
	now := time.Now()
	doc := map[string]any{"enabled": true, "current": nil, "queue": []any{}, "next_scheduled": []any{}, "run": nil}
	if e.cur != nil {
		b := e.cur.run.brief()
		b["progress"] = map[string]any{"steps_total": len(e.cur.run.Steps), "steps_done": e.cur.done}
		doc["current"] = b
		doc["run"] = e.progressLocked(now)
	}
	q := []any{}
	for _, r := range e.queue {
		q = append(q, r.brief())
	}
	doc["queue"] = q
	e.store.mu.Lock()
	ns := []any{}
	for _, id := range sortedKeys(e.store.suites) {
		su := e.store.suites[id]
		sc := asMap(su["schedule"])
		name := strOpt(su, "name")
		if name == "" {
			name = id
		}
		if at, ok := sc["at"].(string); ok {
			ns = append(ns, map[string]any{"suite_id": id, "name": name, "at": at, "next": nextAt(at, now).UnixMilli()})
		} else if every, ok := sc["every"].(string); ok {
			d, _ := time.ParseDuration(every)
			ns = append(ns, map[string]any{"suite_id": id, "name": name, "every": every, "next": now.Add(d).UnixMilli()})
		}
	}
	e.store.mu.Unlock()
	doc["next_scheduled"] = ns
	return doc
}

// itemsOf groups the steps of a run into suite items (the members of a
// parallel step share its group).
func (e *benchEngine) itemsOf(run *mRun) []runItem {
	var out []runItem
	for _, s := range run.Steps {
		if s.Group != "" && len(out) > 0 && len(out[len(out)-1].steps) > 0 && out[len(out)-1].steps[0].Group == s.Group {
			it := &out[len(out)-1]
			it.steps = append(it.steps, s)
			it.profile += " + " + s.Profile
			continue
		}
		it := runItem{id: s.BenchmarkID, name: s.Name, desc: s.desc, profile: s.Profile, steps: []*mStep{s}}
		if s.Group != "" {
			bm := e.store.benchmarks[s.Group]
			it.id, it.name, it.desc = s.Group, benchTitle(bm), strOpt(bm, "description")
			if it.name == "" {
				it.name = s.Group
			}
		}
		out = append(out, it)
	}
	for i := range out {
		for _, s := range out[i].steps {
			out[i].expected = math.Max(out[i].expected, newTrack(s, false, e.rnd).total())
		}
	}
	return out
}

func r1(v float64) float64 { return math.Round(v*10) / 10 }

// progressLocked is Manager.progressDoc.
func (e *benchEngine) progressLocked(now time.Time) map[string]any {
	a := e.cur
	r := a.run
	var totalW, doneW float64
	for i, it := range a.items {
		totalW += it.expected
		if i < a.cur {
			doneW += it.expected
		}
	}
	var current any
	curRemaining := 0.0
	if a.cur >= 0 && a.cur < len(a.items) {
		it := a.items[a.cur]
		var lead *track
		leadPct, sumPct := 101.0, 0.0
		var model, nodes []string
		seen := map[string]bool{}
		for _, t := range a.tracks {
			p := t.percent()
			sumPct += p
			if p < leadPct && !t.finished || lead == nil {
				lead, leadPct = t, p
			}
			if mf := modelFileOf(t.step.Profile); mf != "" {
				model = append(model, mf)
			}
			for _, n := range t.step.nodes {
				if !seen[n] {
					seen[n] = true
					nodes = append(nodes, n)
				}
			}
			curRemaining = math.Max(curRemaining, t.remaining())
		}
		pct := 0.0
		if len(a.tracks) > 0 {
			pct = sumPct / float64(len(a.tracks))
		}
		if nodes == nil {
			nodes = []string{}
		}
		c := map[string]any{"index": a.cur, "benchmark_id": it.id, "name": it.name, "description": it.desc, "profile": it.profile,
			"model_file": strings.Join(model, ", "), "nodes": nodes, "percent": r1(pct)}
		if lead != nil {
			c["phase"] = lead.phaseDoc()
			d := map[string]any{}
			for k, v := range lead.detail {
				d[k] = v
			}
			if lead.load != nil {
				d["load"] = lead.load
			}
			if len(a.tracks) > 1 {
				var members []map[string]any
				for _, t := range a.tracks {
					md := map[string]any{"benchmark_id": t.step.BenchmarkID, "profile": t.step.Profile, "percent": r1(t.percent()), "phase": t.phaseDoc()}
					if t.load != nil {
						md["load"] = t.load
						if d["load"] == nil {
							d["load"] = t.load
						}
					}
					members = append(members, md)
				}
				d["members"] = members
			}
			c["detail"] = d
		} else {
			c["phase"] = map[string]any{"index": 0, "type": "", "label": "", "phases_total": 0}
			c["detail"] = map[string]any{}
		}
		current = c
		doneW += it.expected * pct / 100
	}
	pct := 0.0
	if totalW > 0 {
		pct = 100 * doneW / totalW
	}
	final := bench.IsFinal(r.State)
	if !final && pct > 99.9 {
		pct = 99.9
	}
	if r.State == bench.RunCompleted {
		pct = 100
	}
	pct = math.Max(pct, a.maxPct)
	a.maxPct = pct
	first := a.cur + 1
	if a.cur < 0 {
		first = 0
	}
	rem := curRemaining
	var next any
	queue := []string{}
	for i := first; i < len(a.items); i++ {
		rem += a.items[i].expected
		queue = append(queue, a.items[i].id)
		if next == nil {
			next = map[string]any{"benchmark_id": a.items[i].id, "name": a.items[i].name, "description": a.items[i].desc, "profile": a.items[i].profile}
		}
	}
	if final {
		rem = 0
	}
	elapsed := 0.0
	if r.StartedAt > 0 {
		end := now.UnixMilli()
		if r.FinishedAt > 0 {
			end = r.FinishedAt
		}
		elapsed = r1(float64(end-r.StartedAt) / 1000)
	}
	return map[string]any{"id": r.ID, "suite_id": r.SuiteID, "suite_name": r.SuiteName, "state": r.State, "percent": r1(pct),
		"started_at": r.StartedAt, "elapsed_s": elapsed, "eta_s": r1(rem), "steps_total": len(r.Steps), "steps_done": a.done,
		"current": current, "next": next, "queue": queue}
}

func (e *benchEngine) emitProgressLocked() {
	if e.cur == nil {
		return
	}
	e.send(map[string]any{"kind": "progress", "run": e.progressLocked(time.Now())})
}

func (e *benchEngine) emitRunLocked(r *mRun) {
	e.send(map[string]any{"kind": "run", "run": r.brief()})
}

func (e *benchEngine) emitRequest(s *mStep, extra map[string]any) {
	ev := map[string]any{"kind": "request", "run_id": e.cur.run.ID, "idx": s.Idx, "benchmark_id": s.BenchmarkID}
	for k, v := range extra {
		ev[k] = v
	}
	e.send(ev)
}

// ---- details ----

// loadObject is a synthetic scheduler load object (the snake_case LoadReport
// of /api/status) at progress p of a load phase that lasts dur.
func loadObject(s *mStep, started time.Time, p float64, dur time.Duration) map[string]any {
	total := modelBytes[modelFileOf(s.Profile)]
	if len(s.nodes) == 0 || total == 0 {
		return nil
	}
	fr := fractions(mustProfile(s.Profile), len(s.nodes))
	sent := int64(float64(total) * p)
	remaining := sent
	workers := []any{}
	for i, h := range s.nodes {
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
	return map[string]any{"session_id": sessionID(started, int(s.ID%97)), "started_at": started.UnixMilli(), "elapsed_s": round1(time.Since(started).Seconds()),
		"bytes_total": total, "bytes_sent": sent, "progress": math.Round(p*1000) / 1000, "eta_s": math.Round((1 - p) * dur.Seconds()),
		"source": "measured", "workers": workers}
}

func loadText(profile string, l map[string]any) string {
	text := "Loading " + profile
	if l != nil {
		sent, _ := l["bytes_sent"].(int64)
		total, _ := l["bytes_total"].(int64)
		if total > 0 {
			text += fmt.Sprintf(": %.1f of %.1f GB sent", float64(sent)/1e9, float64(total)/1e9)
		}
	}
	return text
}

// requestText is the progress line of a request phase with k units done of n.
func requestText(kind string, k, n int) string {
	switch kind {
	case "warmup":
		return fmt.Sprintf("Warm-up: %d of %d requests", k, n)
	case "measured":
		return fmt.Sprintf("Measured requests: %d of %d", k, n)
	case "conversation":
		return fmt.Sprintf("Conversation: turn %d of %d", min(k+1, n), n)
	case "concurrent":
		return fmt.Sprintf("Simultaneous requests: %d of %d done", k, n)
	}
	return "Sending a request after the idle time"
}

// seqBefore is the 1-based sequence number of request k of phase i.
func (t *track) seqBefore(i, k int) int {
	n := 0
	for j := 0; j < i; j++ {
		n += len(t.reqs[j])
	}
	return n + k + 1
}

// advance moves a played track to el seconds after its start and emits the
// request and load frames of what it is doing.
func (e *benchEngine) advance(t *track, el float64, emit bool) {
	s := t.step
	acc := 0.0
	i := 0
	for i < len(t.durs) && el >= acc+t.durs[i] {
		acc += t.durs[i]
		i++
	}
	if i >= len(t.durs) {
		t.finished = true
		return
	}
	for t.cur < i {
		t.cur++
		off := 0.0
		for j := 0; j < t.cur; j++ {
			off += t.durs[j]
		}
		at := t.start.Add(time.Duration(off * float64(time.Second)))
		t.phaseAt = append(t.phaseAt, at)
		p := t.phases[t.cur]
		s.Phases = append(s.Phases, mPhase{Idx: t.cur, Type: p.Type, Label: p.Label, State: "running", StartedAt: at.UnixMilli()})
		if t.cur > 0 {
			prev := &s.Phases[t.cur-1]
			prev.State, prev.Outcome, prev.FinishedAt = "done", "ok", at.UnixMilli()
			prev.DurationS = round3(float64(prev.FinishedAt-prev.StartedAt) / 1000)
		}
		t.detail, t.load, t.lastReq = map[string]any{}, nil, -1
		switch p.Type {
		case "load":
			t.detail["text"] = "Loading " + s.Profile
		case "unload":
			t.detail["text"] = "Unloading " + s.Profile
		default:
			t.detail["text"] = "Starting " + p.Type
		}
	}
	p := t.phases[i]
	dur := t.durs[i]
	in := el - acc
	t.frac = math.Min(in/dur, 1)
	reqs := t.reqs[i]
	switch {
	case p.Type == "load":
		t.load = loadObject(s, t.phaseAt[i], t.frac, time.Duration(dur*float64(time.Second)))
		t.detail = map[string]any{"text": loadText(s.Profile, t.load)}
		if emit && t.load != nil {
			e.send(map[string]any{"kind": "load", "run_id": e.cur.run.ID, "idx": s.Idx, "benchmark_id": s.BenchmarkID, "load": t.load})
		}
	case p.Type == "hold" && t.frac < 0.7:
		secs := intOpt(p.Opts, "seconds", 60)
		t.detail["text"] = fmt.Sprintf("Holding the model idle: %.0f of %d s", t.frac/0.7*float64(secs), secs)
	case len(reqs) > 0:
		n := len(reqs)
		var k int
		var within float64
		if p.Type == "hold" {
			k, within = 0, (in-0.7*dur)/(0.3*dur)
		} else if p.Type == "concurrent_requests" {
			k, within = 0, t.frac
		} else {
			per := dur / float64(n)
			k = min(int(in/per), n-1)
			within = (in - float64(k)*per) / per
		}
		rp := reqs[k]
		tokens := int(math.Min(within, 0.99) * float64(rp.max))
		if p.Type == "concurrent_requests" {
			done := min(int(t.frac*float64(n)), n)
			t.detail["text"] = requestText(rp.kind, done, n)
			t.frac = math.Min(t.frac, 0.99)
		} else {
			t.detail["text"] = requestText(rp.kind, k, n)
			t.detail["tokens"], t.detail["max_tokens"] = tokens, rp.max
			if rp.kind == "conversation" {
				t.detail["turn"], t.detail["turns"] = k+1, n
			}
			if p.Type == "hold" {
				t.frac = 0.7 + 0.3*math.Min(within, 0.99)
			} else {
				t.frac = (float64(k) + math.Min(within, 0.99)) / float64(n)
			}
		}
		if k != t.lastReq {
			t.lastReq, t.reqStarted = k, time.Now()
			if p.Type == "concurrent_requests" {
				for j, c := range reqs {
					e.emitRequest(s, map[string]any{"phase": c.kind, "request": c.name, "seq": t.seqBefore(i, j), "state": "started"})
				}
			} else {
				e.emitRequest(s, map[string]any{"phase": rp.kind, "request": rp.name, "seq": t.seqBefore(i, k), "state": "started"})
			}
		} else if emit && p.Type != "concurrent_requests" {
			e.emitRequest(s, map[string]any{"phase": rp.kind, "request": rp.name, "seq": t.seqBefore(i, k), "state": "streaming",
				"tokens": tokens, "elapsed_ms": time.Since(t.reqStarted).Milliseconds()})
		}
	}
}

// ---- enqueue, cancel, play ----

func (e *benchEngine) envNow() *mEnv {
	rev := 14
	if e.revision != nil {
		rev = e.revision()
	}
	return benchEnv(rev, rpcNew)
}

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
	name := strOpt(su, "name")
	if name == "" {
		name = suiteID
	}
	run := &mRun{ID: runID(now, suiteID, e.store.runSeq), SuiteID: suiteID, SuiteName: name, State: bench.RunQueued,
		Note: note, Trigger: trigger, QueuedAt: now.UnixMilli(), Version: devVersion, Suite: cloneMap(su)}
	if baseline == "" {
		if b := e.store.lastCompleted(suiteID, 0); b != nil {
			baseline = b.ID
		}
	}
	run.BaselineRunID = baseline
	run.Steps = e.store.newSteps(su)
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
	run.State, run.StartedAt, run.Env = bench.RunRunning, time.Now().UnixMilli(), e.envNow()
	a := &activeRun{run: run, cur: -1, stop: make(chan struct{})}
	e.store.mu.Lock()
	a.items = e.itemsOf(run)
	e.store.mu.Unlock()
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

// play runs one active run to its end. It holds e.mu only to change state.
func (e *benchEngine) play(a *activeRun) {
	run := a.run
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	e.store.mu.Lock()
	baseline := e.store.runByID(run.BaselineRunID)
	e.store.mu.Unlock()
	for ii, it := range a.items {
		if a.cancelled {
			break
		}
		e.mu.Lock()
		if e.cur != a {
			e.mu.Unlock()
			return
		}
		start := time.Now()
		a.cur, a.tracks = ii, nil
		for _, s := range it.steps {
			s.State, s.StartedAt, s.ModelFile, s.NodeCount = bench.StepRunning, start.UnixMilli(), modelFileOf(s.Profile), len(s.nodes)
			t := newTrack(s, true, e.rnd)
			t.start = start
			a.tracks = append(a.tracks, t)
			e.send(map[string]any{"kind": "step_started", "run_id": run.ID, "step": stepBrief(s)})
		}
		e.emitProgressLocked()
		e.mu.Unlock()
		lastEmit := time.Time{}
		for {
			e.mu.Lock()
			if e.cur != a {
				e.mu.Unlock()
				return
			}
			emit := time.Since(lastEmit) >= 500*time.Millisecond
			left := 0
			for _, t := range a.tracks {
				if t.finished {
					continue
				}
				e.advance(t, time.Since(start).Seconds(), emit)
				if t.finished {
					e.finishStepLocked(a, t, baseline)
				} else {
					left++
				}
			}
			if emit {
				lastEmit = time.Now()
				e.emitProgressLocked()
			}
			e.mu.Unlock()
			if left == 0 || a.cancelled {
				break
			}
			select {
			case <-tick.C:
			case <-a.stop:
			}
		}
		if a.cancelled {
			e.mu.Lock()
			for _, t := range a.tracks {
				if !t.finished {
					e.cancelStepLocked(a, t)
				}
			}
			e.mu.Unlock()
			break
		}
		if ii+1 < len(a.items) {
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

// finishStepLocked writes the results of a played step, keeping the times it really took.
func (e *benchEngine) finishStepLocked(a *activeRun, t *track, baseline *mRun) {
	s := t.step
	times := map[int][2]int64{}
	for _, p := range s.Phases {
		times[p.Idx] = [2]int64{p.StartedAt, p.FinishedAt}
	}
	e.store.mu.Lock()
	e.store.fillStep(s, t.start, e.rnd, 1, baseline)
	e.store.mu.Unlock()
	now := time.Now().UnixMilli()
	for k := range s.Phases {
		p := &s.Phases[k]
		if tm, ok := times[p.Idx]; ok {
			p.StartedAt, p.FinishedAt = tm[0], tm[1]
			if p.FinishedAt == 0 {
				p.FinishedAt = now
			}
			p.DurationS = round3(float64(p.FinishedAt-p.StartedAt) / 1000)
		}
	}
	s.StartedAt, s.FinishedAt = t.start.UnixMilli(), now
	t.finished, t.detail, t.load = true, map[string]any{}, nil
	a.done++
	e.send(map[string]any{"kind": "step_finished", "run_id": a.run.ID, "step": stepBrief(s)})
	e.recordStepLocked(a.run, s)
}

func (e *benchEngine) cancelStepLocked(a *activeRun, t *track) {
	s := t.step
	now := time.Now().UnixMilli()
	e.store.mu.Lock()
	s.State, s.Outcome, s.FinishedAt, s.Error = bench.StepCancelled, bench.OutcomeCancelled, now, "cancelled"
	if n := len(s.Phases); n > 0 {
		p := &s.Phases[n-1]
		p.State, p.Outcome, p.Error, p.FinishedAt = "cancelled", bench.OutcomeCancelled, "cancelled", now
		p.DurationS = round3(float64(p.FinishedAt-p.StartedAt) / 1000)
	}
	e.store.mu.Unlock()
	t.finished = true
	e.send(map[string]any{"kind": "step_finished", "run_id": a.run.ID, "step": stepBrief(s)})
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
			}
		}
	}
	run.State, run.FinishedAt = state, now.UnixMilli()
	run.Summary = summaryOf(run.Steps)
	var ids []string
	seen := map[string]bool{}
	for _, s := range run.Steps {
		if !seen[s.BenchmarkID] {
			seen[s.BenchmarkID] = true
			ids = append(ids, s.BenchmarkID)
		}
	}
	e.store.mu.Unlock()
	a.tracks = nil
	e.emitProgressLocked()
	e.cur = nil
	e.emitRunLocked(run)
	e.send(map[string]any{"kind": "aggregate_updated", "run_id": run.ID, "suite_id": run.SuiteID, "state": state, "benchmarks": ids})
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

// frozen describes the three frozen nightly runs: the step in flight, its
// phase, how many units of that phase are done and how long it has run.
var frozenAt = map[string]struct {
	step, phase, done int
	inPhase           time.Duration
}{
	"load":         {6, 0, 0, 42 * time.Second},
	"conversation": {3, 1, 2, 40 * time.Second},
	"last":         {7, 1, 4, 41 * time.Second},
}

// freeze builds a nightly run frozen in the middle: kind is load,
// conversation or last. since is when the scenario was selected.
func (e *benchEngine) freeze(kind string, since time.Time) {
	fz, ok := frozenAt[kind]
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store.mu.Lock()
	su := e.store.suites["nightly"]
	if su == nil {
		e.store.mu.Unlock()
		return
	}
	e.store.runSeq++
	start := since.Add(-2 * time.Hour)
	run := &mRun{ID: runID(start, "nightly", e.store.runSeq), SuiteID: "nightly", SuiteName: strOpt(su, "name"), State: bench.RunRunning,
		Trigger: "manual", Note: "run by hand after the link change", Version: devVersion, Suite: cloneMap(su), Env: e.envNow()}
	baseline := e.store.lastCompleted("nightly", 0)
	if baseline != nil {
		run.BaselineRunID = baseline.ID
	}
	rnd := rand.New(rand.NewSource(11))
	run.Steps = e.store.newSteps(su)
	t := start
	for i := 0; i < fz.step; i++ {
		t = e.store.fillStep(run.Steps[i], t, rnd, 1, baseline).Add(10 * time.Second)
	}
	// the step in flight: its full results, cut back to the phases and requests done so far
	s := run.Steps[fz.step]
	e.store.fillStep(s, t, rnd, 1, nil)
	phaseStart := since.Add(-fz.inPhase)
	shiftStep(s, phaseStart.UnixMilli()-s.Phases[fz.phase].StartedAt)
	kept := 0
	for _, r := range s.Requests {
		if r.PhaseIdx < fz.phase {
			kept++
		}
	}
	s.Requests = s.Requests[:kept+fz.done]
	s.Phases = s.Phases[:fz.phase+1]
	cp := &s.Phases[fz.phase]
	cp.State, cp.Outcome, cp.FinishedAt, cp.DurationS, cp.Stats, cp.Load, cp.Unload = "running", "", 0, 0, nil, nil, nil
	if len(cp.Turns) > fz.done {
		cp.Turns = cp.Turns[:fz.done]
	}
	if fz.phase == 0 {
		s.Load = nil
	}
	s.State, s.Outcome, s.FinishedAt, s.Stats, s.Expect, s.Comparison, s.LogFile = bench.StepRunning, "", 0, nil, nil, nil, ""
	// the earlier steps end 10 s before this one began
	delta := s.StartedAt - 10_000 - t.Add(-10*time.Second).UnixMilli()
	for i := 0; i < fz.step; i++ {
		shiftStep(run.Steps[i], delta)
	}
	run.StartedAt = start.UnixMilli() + delta
	run.QueuedAt = run.StartedAt - 2000
	e.store.runs = append(e.store.runs, run)
	a := &activeRun{run: run, cur: fz.step, frozen: kind, frozenAt: since, stop: make(chan struct{}), done: fz.step}
	a.items = e.itemsOf(run)
	e.store.mu.Unlock()
	tr := newTrack(s, false, rnd)
	tr.cur, tr.start = fz.phase, time.UnixMilli(s.StartedAt)
	a.tracks = []*track{tr}
	e.cur = a
	e.updateFrozenLocked(a, false)
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
			e.updateFrozenLocked(a, true)
			e.emitProgressLocked()
			e.mu.Unlock()
		}
	}()
}

// updateFrozenLocked moves the ticking figures of a frozen run: the load
// progress creeps and wraps, tokens count up and start over.
func (e *benchEngine) updateFrozenLocked(a *activeRun, emit bool) {
	el := time.Since(a.frozenAt)
	t := a.tracks[0]
	s := t.step
	fz := frozenAt[a.frozen]
	switch a.frozen {
	case "load":
		b := loadingOf(s.Profile, time.Now(), el, false)
		in := b.st.Instances[0]
		t.load, _ = api.StatusJSON(in.Load).(map[string]any)
		t.frac = *in.Load.Progress
		text := "Loading " + s.Profile
		if in.Load.BytesTotal != nil && in.Load.BytesSent != nil && *in.Load.BytesTotal > 0 {
			text += fmt.Sprintf(": %.1f of %.1f GB sent", float64(*in.Load.BytesSent)/1e9, float64(*in.Load.BytesTotal)/1e9)
		}
		t.detail = map[string]any{"text": text}
		if emit {
			e.send(map[string]any{"kind": "load", "run_id": a.run.ID, "idx": s.Idx, "benchmark_id": s.BenchmarkID, "load": t.load})
		}
	case "conversation", "last":
		reqs := t.reqs[fz.phase]
		n := len(reqs)
		rp := reqs[fz.done]
		cycle := map[string]time.Duration{"conversation": 16 * time.Second, "last": 18 * time.Second}[a.frozen]
		in := el % cycle
		tokens := int(float64(rp.max) * 0.95 * in.Seconds() / cycle.Seconds())
		t.detail = map[string]any{"text": requestText(rp.kind, fz.done, n), "tokens": tokens, "max_tokens": rp.max}
		if rp.kind == "conversation" {
			t.detail["turn"], t.detail["turns"] = fz.done+1, n
		}
		t.frac = (float64(fz.done) + float64(tokens)/float64(rp.max)) / float64(n)
		if emit {
			state := "streaming"
			if tokens == 0 {
				state = "started"
			}
			e.emitRequest(s, map[string]any{"phase": rp.kind, "request": rp.name, "seq": t.seqBefore(fz.phase, fz.done), "state": state,
				"tokens": tokens, "elapsed_ms": in.Milliseconds()})
		}
	}
}

func (e *benchEngine) finishFrozenLocked(a *activeRun) {
	close(a.stop)
	a.cancelled = true
	e.cancelStepLocked(a, a.tracks[0])
	e.finishLocked(a)
}

// reset stops everything and reseeds the store (empty when bare).
func (e *benchEngine) reset(bare bool, frozen string, since time.Time, setup ...func(*benchEngine, time.Time)) {
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
	for _, f := range setup {
		if f != nil {
			f(e, since)
		}
	}
	e.mu.Lock()
	e.send(map[string]any{"kind": "state", "state": e.stateLocked()})
	e.mu.Unlock()
}
