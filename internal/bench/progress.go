package bench

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// The progress model (design 9.3): how far a run is, which benchmark and phase
// it is in, what comes next and how long it should still take. Everything is
// derived from expected durations, which come from earlier completed runs of
// the same benchmark and fall back to defaults.

const progressEvery = 500 * time.Millisecond // at most 2 progress messages a second between changes

// Default durations (seconds) of a phase when no earlier run is known.
const (
	defLoadS         = 60
	defWarmupEachS   = 5
	defRequestEachS  = 5
	defTurnEachS     = 8
	defConcurrentS   = 15
	defUnloadS       = 3
	holdRequestExtra = 5
)

// itemPlan is one entry of the suite as the run sees it.
type itemPlan struct {
	id       string
	name     string
	desc     string
	profile  string
	members  []*benchspec.Benchmark
	expected float64 // seconds; 0 when unknown
	known    bool
}

// trackPhase is one phase of a benchmark in flight.
type trackPhase struct {
	typ    string
	label  string
	weight float64 // expected seconds
	known  bool    // the expectation comes from earlier runs
}

// benchTrack is the live progress of one benchmark. Guarded by activeRun.mu.
type benchTrack struct {
	b         *benchspec.Benchmark
	item      int
	phases    []trackPhase
	cur       int // -1 before the first phase
	planned   int // work units of the current phase (requests, turns, ...)
	done      int
	inflight  float64 // 0..1 of the unit in flight (tokens out of max_tokens)
	direct    float64 // fraction set directly (load progress, unload)
	useDirect bool
	detail    map[string]any
	load      json.RawMessage
	modelFile string
	nodes     []string
	finished  bool
	maxPct    float64
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// frac is how much of the current phase is done.
func (t *benchTrack) frac() float64 {
	if t.useDirect {
		return clamp01(t.direct)
	}
	if t.planned <= 0 {
		return 0
	}
	return clamp01((float64(t.done) + clamp01(t.inflight)) / float64(t.planned))
}

// percent is how much of the benchmark is done (0 to 100), never going back and
// reaching 100 only when the benchmark has ended.
func (t *benchTrack) percent() float64 {
	var total, done float64
	for i, p := range t.phases {
		total += p.weight
		switch {
		case t.finished || i < t.cur:
			done += p.weight
		case i == t.cur:
			done += p.weight * t.frac()
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
	if pct < t.maxPct {
		pct = t.maxPct
	}
	t.maxPct = pct
	return pct
}

// remaining is the expected seconds left and whether any of it is known from earlier runs.
func (t *benchTrack) remaining() (secs float64, known bool) {
	if t.finished {
		return 0, true
	}
	for i, p := range t.phases {
		switch {
		case i < t.cur:
		case i == t.cur:
			secs += p.weight * (1 - t.frac())
			known = known || p.known
		default:
			secs += p.weight
			known = known || p.known
		}
	}
	return secs, known
}

func (t *benchTrack) phaseDoc() map[string]any {
	i := t.cur
	if i < 0 {
		i = 0
	}
	d := map[string]any{"index": i, "type": "", "label": "", "phases_total": len(t.phases)}
	if i < len(t.phases) {
		d["type"], d["label"] = t.phases[i].typ, t.phases[i].label
	}
	return d
}

// defaultWeight is the expected seconds of a phase when no earlier run says.
func defaultWeight(p *benchspec.Phase) float64 {
	switch p.Type {
	case benchspec.PhaseLoad:
		return defLoadS
	case benchspec.PhaseWarmup:
		return float64(p.WarmupCount()*defWarmupEachS) + 0.5
	case benchspec.PhaseRequests:
		return float64(len(p.Requests)*p.RepeatCount()*defRequestEachS) + 0.5
	case benchspec.PhaseConversation:
		return float64(len(p.Turns)*defTurnEachS) + 0.5
	case benchspec.PhaseHold:
		return float64(p.HoldS() + holdRequestExtra)
	case benchspec.PhaseConcurrent:
		return defConcurrentS
	case benchspec.PhaseUnload:
		return defUnloadS
	}
	return 1
}

func phaseKey(idx int, typ string) string { return fmt.Sprintf("%d:%s", idx, typ) }

// newTrack plans the progress of a benchmark. hist has the median seconds of its
// phases in earlier completed runs, keyed by phaseKey.
func newTrack(b *benchspec.Benchmark, item int, hist map[string]float64) *benchTrack {
	t := &benchTrack{b: b, item: item, cur: -1, detail: map[string]any{}}
	ps := b.Expand()
	for i := range ps {
		p := &ps[i]
		tp := trackPhase{typ: p.Type, label: p.Title(), weight: defaultWeight(p)}
		if v, ok := hist[phaseKey(i, p.Type)]; ok {
			tp.weight, tp.known = v, true
		}
		t.phases = append(t.phases, tp)
	}
	return t
}

// ---- run level ----

// planItems lists the suite's entries with their expected durations (the median
// duration of the benchmark in earlier completed runs; unknown ones get the
// median of the known ones, or equal weights when none is known).
func (m *Manager) planItems(p *runPlan) []*itemPlan {
	var out []*itemPlan
	for _, it := range p.Suite.Benchmarks {
		b := it.Inline
		if b == nil {
			b = p.Benchmarks[it.Ref]
		}
		if b == nil {
			continue
		}
		ip := &itemPlan{id: b.ID, name: b.Title(), desc: b.Description, profile: b.Profile, members: leafBenchmarks(p, it)}
		if b.IsParallel() {
			var profs []string
			for _, mb := range ip.members {
				profs = append(profs, mb.Profile)
			}
			ip.profile = strings.Join(profs, " + ")
		}
		for _, mb := range ip.members {
			if v, ok := m.o.Store.MedianStepSeconds(mb.ID); ok && v > ip.expected {
				ip.expected, ip.known = v, true
			}
		}
		out = append(out, ip)
	}
	return out
}

// itemWeights returns the weight of every item: its expected seconds, the median
// of the known ones for an unknown item, or 1 each when nothing is known.
func itemWeights(items []*itemPlan) (w []float64, anyKnown bool) {
	var known []float64
	for _, it := range items {
		if it.known {
			known = append(known, it.expected)
		}
	}
	w = make([]float64, len(items))
	if len(known) == 0 {
		for i := range w {
			w[i] = 1
		}
		return w, false
	}
	sort.Float64s(known)
	med := known[len(known)/2]
	for i, it := range items {
		if it.known {
			w[i] = it.expected
		} else {
			w[i] = med
		}
	}
	return w, true
}

// startTrack registers the progress of a benchmark that is about to run.
func (x *suiteExec) startTrack(b *benchspec.Benchmark, item int) *benchTrack {
	hist := x.m.o.Store.MedianPhaseSeconds(b.ID)
	t := newTrack(b, item, hist)
	t.modelFile = x.m.modelFileOf(b.Profile)
	func() {
		x.a.mu.Lock()
		defer supervise.Unlocked("bench", x.a.mu.Unlock)
		x.a.tracks[b.ID] = t
	}()
	return t
}

// with runs f on the track under the run's lock and publishes the change when force is set.
func (x *suiteExec) track(t *benchTrack, force bool, f func(t *benchTrack)) {
	func() {
		x.a.mu.Lock()
		defer supervise.Unlocked("bench", x.a.mu.Unlock)
		f(t)
	}()
	x.m.emitProgress(x.a, force)
}

func (x *suiteExec) setPhase(t *benchTrack, i int, text string) {
	x.track(t, true, func(t *benchTrack) {
		t.cur, t.planned, t.done, t.inflight, t.direct, t.useDirect = i, 0, 0, 0, 0, false
		t.load = nil
		t.detail = map[string]any{"text": text}
	})
}

// setUnits says how many units of work (requests, turns) the current phase has.
func (x *suiteExec) setUnits(t *benchTrack, planned int, text string) {
	x.track(t, false, func(t *benchTrack) {
		t.planned = planned
		t.detail["text"] = text
	})
}

func (x *suiteExec) unitDone(t *benchTrack, text string) {
	x.track(t, false, func(t *benchTrack) {
		t.done++
		t.inflight = 0
		if text != "" {
			t.detail["text"] = text
		}
	})
}

func (x *suiteExec) setInflight(t *benchTrack, tokens, maxTokens int) {
	x.track(t, false, func(t *benchTrack) {
		if maxTokens > 0 {
			t.inflight = float64(tokens) / float64(maxTokens)
			if t.inflight > 0.99 {
				t.inflight = 0.99
			}
		}
		t.detail["tokens"], t.detail["max_tokens"] = tokens, maxTokens
	})
}

func (x *suiteExec) setDetail(t *benchTrack, kv map[string]any) {
	x.track(t, false, func(t *benchTrack) {
		for k, v := range kv {
			t.detail[k] = v
		}
	})
}

func (x *suiteExec) finishTrack(t *benchTrack) {
	x.track(t, true, func(t *benchTrack) { t.finished = true })
}

// ---- the document ----

// progressDoc builds the progress object (GET /api/bench/state "run", and the
// "progress" message on the live channel). The caller holds ar.mu.
func (m *Manager) progressDoc(ar *activeRun) map[string]any {
	r := ar.run
	now := m.o.Now()
	items := ar.items
	weights, anyKnown := itemWeights(items)
	var totalW, doneW float64
	for i, w := range weights {
		totalW += w
		if i < ar.cur {
			doneW += w
		}
	}
	var current map[string]any
	var curRemaining float64
	curKnown := false
	if ar.cur >= 0 && ar.cur < len(items) {
		it := items[ar.cur]
		var tracks []*benchTrack
		for _, mb := range it.members {
			if t := ar.tracks[mb.ID]; t != nil {
				tracks = append(tracks, t)
			}
		}
		var sumPct float64
		lead := (*benchTrack)(nil)
		leadPct := 101.0
		var model, nodes []string
		seenNode := map[string]bool{}
		for _, t := range tracks {
			p := t.percent()
			sumPct += p
			if p < leadPct && !t.finished || lead == nil {
				lead, leadPct = t, p
			}
			if t.modelFile != "" {
				model = append(model, t.modelFile)
			}
			for _, n := range t.nodes {
				if !seenNode[n] {
					seenNode[n] = true
					nodes = append(nodes, n)
				}
			}
			rem, kn := t.remaining()
			if rem > curRemaining {
				curRemaining = rem
			}
			curKnown = curKnown || kn
		}
		pct := 0.0
		if len(tracks) > 0 {
			pct = sumPct / float64(len(tracks))
		}
		current = map[string]any{"index": ar.cur, "benchmark_id": it.id, "name": it.name, "description": it.desc, "profile": it.profile,
			"model_file": strings.Join(model, ", "), "nodes": nonNilStrings(nodes), "percent": round1(pct)}
		if lead != nil {
			current["phase"] = lead.phaseDoc()
			d := map[string]any{}
			for k, v := range lead.detail {
				d[k] = v
			}
			if len(lead.load) > 0 {
				d["load"] = lead.load
			}
			if len(tracks) > 1 {
				var members []map[string]any
				for _, t := range tracks {
					md := map[string]any{"benchmark_id": t.b.ID, "profile": t.b.Profile, "percent": round1(t.percent()), "phase": t.phaseDoc()}
					if len(t.load) > 0 {
						md["load"] = t.load
					}
					members = append(members, md)
				}
				d["members"] = members
				if len(lead.load) == 0 {
					for _, t := range tracks {
						if len(t.load) > 0 {
							d["load"] = t.load
							break
						}
					}
				}
			}
			current["detail"] = d
		} else {
			current["phase"] = map[string]any{"index": 0, "type": "", "label": "", "phases_total": 0}
			current["detail"] = map[string]any{}
		}
		doneW += weights[ar.cur] * pct / 100
	}
	pct := 0.0
	if totalW > 0 {
		pct = 100 * doneW / totalW
	}
	final := IsFinal(r.State)
	if !final && pct > 99.9 {
		pct = 99.9
	}
	if r.State == RunCompleted {
		pct = 100
	}
	if pct < ar.maxPct {
		pct = ar.maxPct
	}
	ar.maxPct = pct
	// eta: expected seconds of what remains
	var eta any
	if anyKnown || curKnown {
		rem := curRemaining
		first := ar.cur + 1
		if ar.cur < 0 {
			first = 0
		}
		for i := first; i < len(items); i++ {
			rem += weights[i]
		}
		if !anyKnown { // only the current benchmark has a history: nothing meaningful for the rest
			rem = curRemaining
		}
		if final {
			rem = 0
		}
		eta = round1(rem)
	}
	var next any
	queue := []string{}
	first := ar.cur + 1
	if ar.cur < 0 {
		first = 0
	}
	for i := first; i < len(items); i++ {
		queue = append(queue, items[i].id)
		if next == nil {
			next = map[string]any{"benchmark_id": items[i].id, "name": items[i].name, "description": items[i].desc, "profile": items[i].profile}
		}
	}
	elapsed := 0.0
	started := r.StartedAt
	if started > 0 {
		end := now.UnixMilli()
		if r.FinishedAt > 0 {
			end = r.FinishedAt
		}
		elapsed = round1(float64(end-started) / 1000)
	}
	doc := map[string]any{"id": r.ID, "suite_id": r.SuiteID, "suite_name": r.SuiteName, "state": r.State, "percent": round1(pct),
		"started_at": started, "elapsed_s": elapsed, "eta_s": eta, "steps_total": ar.total, "steps_done": ar.done,
		"current": nil, "next": next, "queue": queue}
	if current != nil {
		doc["current"] = current
	}
	if ar.paused != "" {
		doc["paused_because"] = ar.paused
	}
	return doc
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// emitProgress publishes the progress message: at most twice a second unless
// force is set (a phase or step changed).
func (m *Manager) emitProgress(ar *activeRun, force bool) {
	if doc := m.progressDue(ar, force); doc != nil {
		m.emit(map[string]any{"kind": "progress", "run": doc})
	}
}

// progressDue is the progress document to publish now, or nil.
func (m *Manager) progressDue(ar *activeRun, force bool) map[string]any {
	ar.mu.Lock()
	defer supervise.Unlocked("bench", ar.mu.Unlock)
	now := time.Now()
	if !force && now.Sub(ar.lastEmit) < progressEvery {
		return nil
	}
	ar.lastEmit = now
	return m.progressDoc(ar)
}

// Progress is the progress object of the running run, or nil when none runs.
func (m *Manager) Progress() map[string]any {
	var ar *activeRun
	func() {
		m.mu.Lock()
		defer supervise.Unlocked("bench", m.mu.Unlock)
		ar = m.cur
	}()
	if ar == nil {
		return nil
	}
	ar.mu.Lock()
	defer supervise.Unlocked("bench", ar.mu.Unlock)
	return m.progressDoc(ar)
}
