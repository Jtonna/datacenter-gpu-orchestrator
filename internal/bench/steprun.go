package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// PhaseOK is the outcome of a phase that did what it should.
const PhaseOK = "ok"

func (x *suiteExec) emitRequest(st *Step, extra map[string]any) {
	ev := map[string]any{"kind": "request", "run_id": st.RunID, "idx": st.Idx, "benchmark_id": st.BenchmarkID}
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

type wireTimings struct {
	PromptN            float64 `json:"prompt_n"`
	PromptMS           float64 `json:"prompt_ms"`
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedN         float64 `json:"predicted_n"`
	PredictedMS        float64 `json:"predicted_ms"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
	CacheN             float64 `json:"cache_n"`
}

// benchRun is one benchmark executing its phases.
type benchRun struct {
	x      *suiteExec
	st     *Step
	b      *benchspec.Benchmark
	ctx    context.Context
	g      *gate
	member int
	track  *benchTrack

	mu     sync.Mutex
	seq    int
	reqs   []RequestResult
	gated  bool
	loaded bool // this benchmark knows the profile is loaded

	lastLoad *LoadResult // the load measured last

	// What the step ran on (stepfacts.go): the lowest available memory per
	// node, the plan, the instance's nodes as last seen, the local host, and
	// the load mode the scheduler reported for the instance.
	watch    *memWatch
	plan     *PlanInfo
	nodes    []InstanceNodeInfo
	localID  string
	loadMode string

	// wantCold asks loadModel to drop the model file from the file caches
	// before the load (the load phase's cold option); cold is the result.
	wantCold bool
	cold     *ColdResult
}

func (br *benchRun) fail(outcome, why string) {
	if br.st.Outcome == "" {
		br.st.Outcome, br.st.Error = outcome, why
	}
}

func (br *benchRun) stopped() bool { return br.st.Outcome != "" }

// syncGate lets the members of a parallel step start their loads together.
func (br *benchRun) syncGate() {
	if br.g == nil || br.gated {
		return
	}
	br.gated = true
	br.g.arrive(br.member)
	br.g.wait()
}

func (x *suiteExec) runBenchmark(b *benchspec.Benchmark, idx int, group string, g *gate, member int) (st *Step) {
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

	ctx, cancelCtx := context.WithTimeout(WithRun(context.Background(), st.RunID), b.Timeout())
	defer cancelCtx()
	br := &benchRun{x: x, st: st, b: b, ctx: ctx, g: g, member: member, watch: newMemWatch()}
	br.watch.start(m.o.MemSample, m.o.API.Status)
	br.track = x.startTrack(b, idx)
	st.ModelFile = br.track.modelFile
	defer func() {
		if g != nil {
			g.arrive(member)
		}
		br.cleanUp()
		x.finish(br)
	}()

	// What the scheduler says it would do.
	plan, err := m.o.API.Plan(ctx, b.Profile)
	if err != nil {
		br.fail(OutcomeError, "plan: "+err.Error())
		return st
	}
	st.Plan = plan.Raw
	br.mu.Lock()
	br.plan = plan
	br.mu.Unlock()
	planned := x.hostnames(plan.Nodes)
	x.track(br.track, false, func(t *benchTrack) { t.nodes = planned })

	phases := b.Expand()
	for i := range phases {
		br.runPhase(i, &phases[i])
	}
	if !br.stopped() {
		st.Outcome = OutcomeReady
	}
	return st
}

// hostnames maps node ids to host names when the last status knew them.
func (x *suiteExec) hostnames(ids []string) []string {
	out := []string{}
	x.a.mu.Lock()
	known := x.a.hosts
	x.a.mu.Unlock()
	for _, id := range ids {
		if h := known[id]; h != "" {
			id = h
		}
		out = append(out, id)
	}
	return out
}

// runPhase runs one phase and records its result. After a failure the later
// phases are recorded as skipped, except unload, which still cleans up.
func (br *benchRun) runPhase(i int, p *benchspec.Phase) {
	x, st := br.x, br.st
	pr := &PhaseResult{StepID: st.ID, RunID: st.RunID, Idx: i, Type: p.Type, Label: p.Label, State: PhaseRunning, StartedAt: x.m.o.Now().UnixMilli()}
	if !br.stopped() && x.a.cancelled() {
		br.fail(OutcomeCancelled, "cancelled")
	}
	if br.stopped() && p.Type != benchspec.PhaseUnload {
		pr.State, pr.FinishedAt = PhaseSkipped, pr.StartedAt
		if err := x.m.o.Store.AddPhase(pr); err != nil {
			x.m.log.Warn("cannot store a phase", "err", err)
		}
		st.Phases = append(st.Phases, *pr)
		return
	}
	if err := x.m.o.Store.AddPhase(pr); err != nil {
		x.m.log.Warn("cannot store a phase", "err", err)
	}
	before := st.Outcome
	x.setPhase(br.track, i, firstText(p.Type, br.b.Profile))
	switch p.Type {
	case benchspec.PhaseLoad:
		br.phaseLoad(p, pr)
	case benchspec.PhaseWarmup:
		br.phaseWarmup(i, p, pr)
	case benchspec.PhaseRequests:
		br.phaseRequests(i, p, pr)
	case benchspec.PhaseConversation:
		br.phaseConversation(i, p, pr)
	case benchspec.PhaseHold:
		br.phaseHold(i, p, pr)
	case benchspec.PhaseConcurrent:
		br.phaseConcurrent(i, p, pr)
	case benchspec.PhaseTasks:
		br.phaseTasks(i, p, pr)
	case benchspec.PhaseUnload:
		br.phaseUnload(p, pr)
	}
	pr.FinishedAt = x.m.o.Now().UnixMilli()
	pr.DurationS = round3(float64(pr.FinishedAt-pr.StartedAt) / 1000)
	switch {
	case pr.State == PhaseSkipped:
	case st.Outcome != "" && before == "":
		pr.Outcome, pr.Error = st.Outcome, st.Error
		pr.State = PhaseFailed
		if st.Outcome == OutcomeCancelled {
			pr.State = PhaseCancelled
		}
	default:
		pr.Outcome, pr.State = PhaseOK, PhaseDone
	}
	if rows := br.rowsOf(i); len(rows) > 0 && pr.Stats == nil {
		pr.Stats = statsOf(rows)
	}
	br.sampleNow() // the phase's last memory figures
	if err := x.m.o.Store.UpdatePhase(pr); err != nil {
		x.m.log.Warn("cannot store a phase", "err", err)
	}
	st.Phases = append(st.Phases, *pr)
}

func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }

func (br *benchRun) rowsOf(phaseIdx int) []RequestResult {
	br.mu.Lock()
	defer br.mu.Unlock()
	var out []RequestResult
	for _, r := range br.reqs {
		if r.PhaseIdx == phaseIdx {
			out = append(out, r)
		}
	}
	return out
}

// ---- load ----

func recentFailure(s *StatusInfo, profile string, since int64) *RecentLoad {
	for i := range s.RecentLoads {
		rl := &s.RecentLoads[i]
		if rl.Model == profile && rl.FinishedAt >= since-2000 && rl.Outcome != "" && rl.Outcome != "ready" {
			return rl
		}
	}
	return nil
}

// noteInstance records what the scheduler knows of the profile's instance.
func (br *benchRun) noteInstance(s *StatusInfo, in *InstanceInfo) {
	x, st := br.x, br.st
	x.a.mu.Lock()
	x.a.hosts = mergeHosts(x.a.hosts, s.Hostnames)
	x.a.mu.Unlock()
	br.observe(s)
	if in == nil {
		return
	}
	br.mu.Lock()
	if len(in.Nodes) > 0 {
		br.nodes = append([]InstanceNodeInfo(nil), in.Nodes...)
	}
	if in.LoadMode != "" {
		br.loadMode = in.LoadMode
	}
	br.mu.Unlock()
	if in.ModelFile != "" {
		st.ModelFile = in.ModelFile
	}
	remote := 0
	var names []string
	for _, n := range in.Nodes {
		if !n.Local {
			remote++
		}
		name := s.Hostnames[n.ID]
		if name == "" {
			name = n.ID
		}
		names = append(names, name)
	}
	if len(in.Nodes) > 0 {
		st.NodeCount = remote
	}
	x.track(br.track, false, func(t *benchTrack) {
		if in.ModelFile != "" {
			t.modelFile = in.ModelFile
		}
		if len(names) > 0 {
			t.nodes = names
		}
	})
	x.noteEnv(s, in)
}

func mergeHosts(a, b map[string]string) map[string]string {
	if a == nil {
		a = map[string]string{}
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}

func (x *suiteExec) unloadFirst(ctx context.Context, profile string) (string, bool) {
	res, err := x.unloadGraceful(ctx, profile)
	switch {
	case err != nil:
		return "unload: " + err.Error(), false
	case res.Status == 409:
		return "the profile is busy, so it cannot be made cold: " + res.Msg, false
	case res.Status != 200:
		return fmt.Sprintf("unload answered %d: %s", res.Status, res.Msg), false
	}
	if !x.waitGone(ctx, profile) {
		if x.a.cancelled() {
			return "cancelled", false
		}
		return "the profile did not leave the scheduler in time", false
	}
	return "", true
}

// loadModel loads the profile with POST /api/load, without any chat request,
// and waits until it is ready. cold unloads it first when it is loaded. It
// reports whether the profile is ready; otherwise the step has an outcome.
func (br *benchRun) loadModel(cold bool) bool {
	x, st, b, ctx := br.x, br.st, br.b, br.ctx
	m := x.m
	status, err := m.o.API.Status(ctx)
	if err != nil {
		br.fail(OutcomeError, "status: "+err.Error())
		return false
	}
	br.noteInstance(status, status.Instance(b.Profile))
	in := status.Instance(b.Profile)
	if in != nil && cold {
		if why, ok := x.unloadFirst(ctx, b.Profile); !ok {
			if why == "cancelled" {
				br.fail(OutcomeCancelled, "cancelled")
			} else {
				br.fail(OutcomeError, why)
			}
			return false
		}
		in = nil
	}
	if br.wantCold {
		br.cold = br.makeCold()
	}
	if in != nil && in.State == "ready" { // nothing to load: someone has it loaded already
		br.syncGate()
		br.loaded = true
		return true
	}
	br.syncGate()
	if x.a.cancelled() {
		br.fail(OutcomeCancelled, "cancelled before the load")
		return false
	}
	obs := &loadObs{}
	start := time.Now()
	startMS := m.o.Now().UnixMilli()
	// Marked before the call: an answer lost on the way back (a timeout, a
	// reset connection) may follow a load the scheduler accepted, and the
	// step's clean-up must unload it. A refusal clears the mark.
	x.a.markLoaded(b.Profile, true)
	ans, err := m.o.API.Load(ctx, b.Profile)
	switch {
	case err != nil && ctx.Err() != nil:
		br.fail(OutcomeFailed, fmt.Sprintf("timed out after %s (timeout_s)", b.Timeout()))
		return false
	case err != nil:
		br.fail(OutcomeError, "load: "+err.Error())
		return false
	case ans.Status >= 300:
		x.a.markLoaded(b.Profile, false)
		st.HTTPStatus, st.Reason = ans.Status, ans.Msg
		if ans.Status == 503 && ans.Reason == ReasonNodeExcluded {
			br.fail(OutcomeRefusedByExclusion, ans.Msg)
		} else if ans.Status == 503 || (ans.Status >= 400 && ans.Status < 500) {
			br.fail(OutcomeRefused, ans.Msg)
		} else {
			br.fail(OutcomeFailed, ans.Msg)
		}
		return false
	}
	br.watch.loading.Store(true)
	defer br.watch.loading.Store(false)
	ready, failure := false, ""
	var refused *Refusal
	seen := false
	for !ready && failure == "" && refused == nil {
		if x.a.cancelled() {
			br.fail(OutcomeCancelled, "cancelled while the model was loading")
			return false
		}
		if ctx.Err() != nil {
			br.fail(OutcomeFailed, fmt.Sprintf("timed out after %s waiting for the load (timeout_s)", b.Timeout()))
			return false
		}
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s, err := m.o.API.Status(sctx)
		cancel()
		if err == nil {
			cur := s.Instance(b.Profile)
			switch {
			case cur != nil:
				seen = true
				br.noteInstance(s, cur)
				obs.mu.Lock()
				if cur.LogFile != "" {
					obs.logFile = cur.LogFile
				}
				if cur.Load != nil {
					obs.last = cur.Load
				}
				obs.mu.Unlock()
				if cur.Load != nil {
					br.loadProgress(cur.Load, time.Since(start))
					m.emit(map[string]any{"kind": "load", "run_id": st.RunID, "idx": st.Idx, "benchmark_id": st.BenchmarkID, "load": json.RawMessage(cur.Load.Raw)})
				} else {
					br.loadProgress(nil, time.Since(start))
				}
				if cur.State == "ready" {
					ready = true
				}
			}
			if !ready && refused == nil && cur == nil {
				refused = br.refusalFor(ctx, s, startMS)
			}
			if !ready && refused == nil && cur == nil {
				if rl := recentFailure(s, b.Profile, startMS); rl != nil {
					failure = rl.Reason
					if failure == "" {
						failure = "the load " + rl.Outcome
					}
				} else if rf := br.historyRefusal(ctx, startMS, "profileBackedOff"); rf != nil {
					refused = rf
				} else if seen {
					failure = "the instance disappeared during the load"
				}
			}
		}
		if !ready && failure == "" && refused == nil && !x.sleep(m.o.LoadPoll) {
			br.fail(OutcomeCancelled, "cancelled while the model was loading")
			return false
		}
	}
	lf, _ := obs.get()
	st.LogFile = lf
	if refused != nil { // the scheduler turned the load down before any child started
		x.a.markLoaded(b.Profile, false)
		st.HTTPStatus, st.Reason = refused.Status, refused.Text()
		if st.HTTPStatus == 0 {
			st.HTTPStatus = 503
		}
		if refused.Reason == ReasonNodeExcluded {
			br.fail(OutcomeRefusedByExclusion, st.Reason)
		} else {
			br.fail(OutcomeRefused, st.Reason)
		}
		return false
	}
	if failure != "" {
		st.Reason = failure
		br.fail(OutcomeFailed, failure)
		br.captureLoad(startMS, start, obs, false)
		return false
	}
	br.loaded = true
	br.captureLoad(startMS, start, obs, true)
	br.checkColdPlacement(br.cold)
	return true
}

// ownRefusal reports whether a refusal record answers this step's load call:
// recorded after the call, and tagged with this run when it carries a tag.
func (br *benchRun) ownRefusal(r Refusal, startMS int64) bool {
	return r.AtMS >= startMS && (r.BenchRun == "" || r.BenchRun == br.st.RunID)
}

// refusalFor finds the scheduler's refusal of this step's load: first in the
// status (last_refusals), then in the history (requestRefused, preflightFailed).
// It is asked only once the instance is gone, so a refusal of someone else's
// request cannot stop a load that is going well. It returns nil when there is none.
func (br *benchRun) refusalFor(ctx context.Context, s *StatusInfo, startMS int64) *Refusal {
	var best *Refusal
	for i := range s.LastRefusals {
		r := &s.LastRefusals[i]
		if r.Profile == br.b.Profile && br.ownRefusal(*r, startMS) && (best == nil || r.AtMS > best.AtMS) {
			best = r
		}
	}
	if best != nil {
		return best
	}
	return br.historyRefusal(ctx, startMS, "requestRefused", "preflightFailed")
}

// historyRefusal is the newest history event of the given types that answers this step's load call.
func (br *benchRun) historyRefusal(ctx context.Context, startMS int64, types ...string) *Refusal {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := br.x.m.o.API.Refusals(hctx, br.b.Profile, startMS, types...)
	if err != nil {
		return nil
	}
	var best *Refusal
	for i := range list {
		if r := &list[i]; br.ownRefusal(*r, startMS) && (best == nil || r.AtMS > best.AtMS) {
			best = r
		}
	}
	return best
}

// loadProgress feeds the scheduler's load progress into the progress model.
func (br *benchRun) loadProgress(l *LoadInfo, elapsed time.Duration) {
	x := br.x
	x.track(br.track, false, func(t *benchTrack) {
		t.useDirect = true
		if l != nil && l.Progress != nil {
			t.direct = *l.Progress
		} else {
			// no measured progress: assume the usual load time, never reaching the end
			exp := t.phases[maxInt(t.cur, 0)].weight
			if exp <= 0 {
				exp = defLoadS
			}
			t.direct = 0.95 * clamp01(elapsed.Seconds()/exp)
		}
		if l != nil {
			t.load = l.Raw
		}
		text := fmt.Sprintf("Loading %s", br.b.Profile)
		if l != nil && l.BytesTotal != nil && l.BytesSent != nil && *l.BytesTotal > 0 {
			text += fmt.Sprintf(": %.1f of %.1f GB sent", float64(*l.BytesSent)/1e9, float64(*l.BytesTotal)/1e9)
		}
		t.detail["text"] = text
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// captureLoad reads what the scheduler knows of the load: its session in
// recent_loads, the history record of that session (bytes, MB/s, per node bytes
// and memory) and the instance's measured memory. A load that failed has a
// session too, with its reason.
func (br *benchRun) captureLoad(startMS int64, start time.Time, obs *loadObs, ready bool) {
	x, st, b := br.x, br.st, br.b
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
	if in := s.Instance(b.Profile); in != nil && in.LoadMode != "" {
		lr.LoadMode = in.LoadMode
	}
	br.mu.Lock()
	if lr.LoadMode == "" {
		lr.LoadMode = br.loadMode
	}
	if lr.LoadMode == "" && br.plan != nil {
		lr.LoadMode = br.plan.LoadMode
	}
	br.mu.Unlock()
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
	if lr.LoadS == 0 && ready {
		lr.LoadS = round1(time.Since(start).Seconds())
		lr.Source = "wall"
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
		br.noteInstance(s, in)
	}
	if st.Load == nil {
		st.Load = lr
	}
	br.mu.Lock()
	br.lastLoad = lr
	br.mu.Unlock()
}

// phaseLoad is the load phase: cold load, no chat request. With the cold
// option the model file is dropped from the file caches first (cold.go).
func (br *benchRun) phaseLoad(p *benchspec.Phase, pr *PhaseResult) {
	br.wantCold, br.cold = p.EvictsCache(), nil
	ok := br.loadModel(true)
	br.wantCold = false
	pr.Cold, br.cold = br.cold, nil
	if !ok {
		br.mu.Lock()
		pr.Load = br.lastLoad
		br.mu.Unlock()
		return
	}
	br.mu.Lock()
	pr.Load = br.lastLoad
	br.mu.Unlock()
	if pr.Load != nil {
		br.x.setDetail(br.track, map[string]any{"text": fmt.Sprintf("%s is loaded in %.1f s", br.b.Profile, pr.Load.LoadS)})
	}
	br.x.track(br.track, true, func(t *benchTrack) { t.direct, t.useDirect = 1, true })
}

// ensureLoaded makes sure the profile is ready before a phase sends requests:
// a model that is not loaded is loaded (the same way as the load phase, without
// making it cold).
func (br *benchRun) ensureLoaded() bool {
	if br.loaded {
		return true
	}
	if br.stopped() {
		return false
	}
	if !br.loadModel(false) {
		return false
	}
	return true
}

// ---- requests ----

// send performs one request and records it.
func (br *benchRun) send(ctx context.Context, r benchspec.Request, phase string, pidx, turn int) (*RequestResult, *ChatResponse, error) {
	x, st := br.x, br.st
	br.mu.Lock()
	br.seq++
	seq := br.seq
	br.mu.Unlock()
	rr := &RequestResult{StepID: st.ID, RunID: st.RunID, Seq: seq, Name: r.Name, Phase: phase, PhaseIdx: pidx, Turn: turn,
		Deterministic: r.Deterministic(), StartedAt: x.m.o.Now().UnixMilli()}
	x.emitRequest(st, map[string]any{"phase": phase, "request": r.Name, "seq": seq, "state": "started"})
	resp, err := x.m.o.API.Chat(ctx, st.RunID, BuildChatRequest(br.b.Profile, r), func(p ChatProgress) {
		x.emitRequest(st, map[string]any{"phase": phase, "request": r.Name, "seq": seq, "state": "streaming", "tokens": p.Tokens, "elapsed_ms": p.ElapsedMS})
		if phase != PhaseConcurrent {
			x.setInflight(br.track, p.Tokens, r.MaxTok())
		}
	})
	if err != nil {
		rr.Error = err.Error()
		br.record(rr)
		return rr, nil, err
	}
	rr.Status, rr.WallMS, rr.TTFTMS = resp.Status, resp.WallMS, resp.TTFTMS
	if resp.Status != 200 {
		rr.Error = resp.ErrMessage
		br.record(rr)
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
	if phase == PhaseConcurrent {
		wait := rr.WallMS - rr.PromptMS - rr.PredictedMS
		if rr.TTFTMS != nil {
			wait = *rr.TTFTMS - rr.PromptMS
		}
		if wait < 0 {
			wait = 0
		}
		rr.WaitMS = &wait
	}
	br.record(rr)
	return rr, resp, nil
}

func (br *benchRun) record(rr *RequestResult) {
	br.mu.Lock()
	br.reqs = append(br.reqs, *rr)
	br.mu.Unlock()
	if err := br.x.m.o.Store.AddRequest(rr); err != nil {
		br.x.m.log.Warn("cannot store a request", "err", err)
	}
}

// one sends a request of a phase and turns a failure into the step's outcome.
func (br *benchRun) one(r benchspec.Request, phase string, pidx, turn int) (*RequestResult, *ChatResponse, bool) {
	st := br.st
	if br.x.a.cancelled() {
		br.fail(OutcomeCancelled, "cancelled")
		return nil, nil, false
	}
	rr, resp, err := br.send(br.ctx, r, phase, pidx, turn)
	switch {
	case err != nil && br.ctx.Err() != nil:
		br.fail(OutcomeFailed, fmt.Sprintf("timed out after %s (timeout_s)", br.b.Timeout()))
		return rr, nil, false
	case err != nil && br.x.a.cancelled():
		br.fail(OutcomeCancelled, "cancelled")
		return rr, nil, false
	case err != nil:
		br.fail(OutcomeError, "request failed: "+err.Error())
		return rr, nil, false
	case resp.Status != 200:
		st.HTTPStatus, st.Reason = resp.Status, resp.ErrMessage
		br.fail(OutcomeFailed, resp.ErrMessage)
		return rr, resp, false
	}
	return rr, resp, true
}

func (br *benchRun) phaseWarmup(i int, p *benchspec.Phase, pr *PhaseResult) {
	if !br.ensureLoaded() {
		return
	}
	req, _ := br.b.RequestOf(p)
	n := p.WarmupCount()
	br.x.setUnits(br.track, n, fmt.Sprintf("Warm-up: %d requests", n))
	for k := 0; k < n; k++ {
		if _, _, ok := br.one(req, PhaseWarmup, i, 0); !ok {
			return
		}
		br.x.unitDone(br.track, fmt.Sprintf("Warm-up: %d of %d requests", k+1, n))
	}
}

func (br *benchRun) phaseRequests(i int, p *benchspec.Phase, pr *PhaseResult) {
	if !br.ensureLoaded() {
		return
	}
	reqs := p.PhaseRequests()
	total := len(reqs) * p.RepeatCount()
	br.x.setUnits(br.track, total, fmt.Sprintf("Measured requests: 0 of %d", total))
	done := 0
	for _, r := range reqs {
		for k := 0; k < p.RepeatCount(); k++ {
			if _, _, ok := br.one(r, PhaseMeasured, i, 0); !ok {
				return
			}
			done++
			br.x.unitDone(br.track, fmt.Sprintf("Measured requests: %d of %d", done, total))
		}
	}
}

func (br *benchRun) phaseConversation(i int, p *benchspec.Phase, pr *PhaseResult) {
	if !br.ensureLoaded() {
		return
	}
	var hist []benchspec.Message
	if p.System != "" {
		hist = append(hist, benchspec.Message{Role: "system", Content: p.System})
	}
	n := len(p.Turns)
	br.x.setUnits(br.track, n, fmt.Sprintf("Conversation: turn 0 of %d", n))
	for j, t := range p.Turns {
		msgs := append(append([]benchspec.Message(nil), hist...), benchspec.Message{Role: "user", Content: t.Content})
		req := p.ConversationRequest(t)
		req.Messages = msgs
		br.x.setDetail(br.track, map[string]any{"turn": j + 1, "turns": n, "text": fmt.Sprintf("Conversation: turn %d of %d", j+1, n)})
		rr, resp, ok := br.one(req, PhaseConversation, i, j+1)
		if !ok {
			return
		}
		pr.Turns = append(pr.Turns, TurnResult{Turn: j + 1, Name: t.Name, Status: rr.Status, PromptTokens: rr.PromptN + rr.CacheN, PromptN: rr.PromptN, CacheN: rr.CacheN,
			PrefillTPS: rr.PromptTPS, DecodeTPS: rr.DecodeTPS, TTFTMS: rr.TTFTMS, WallMS: rr.WallMS, PredictedN: rr.PredictedN})
		reply := resp.Text
		if reply == "" {
			reply = "(no answer)"
		}
		hist = append(msgs, benchspec.Message{Role: "assistant", Content: reply})
		br.x.unitDone(br.track, fmt.Sprintf("Conversation: turn %d of %d done", j+1, n))
	}
}

func (br *benchRun) phaseHold(i int, p *benchspec.Phase, pr *PhaseResult) {
	if !br.ensureLoaded() {
		return
	}
	secs := p.HoldS()
	total := float64(secs + holdRequestExtra)
	br.x.track(br.track, false, func(t *benchTrack) {
		t.useDirect = true
		t.detail["text"] = fmt.Sprintf("Holding the model idle: 0 of %d s", secs)
	})
	start := time.Now()
	for {
		held := time.Since(start).Seconds()
		if held >= float64(secs) {
			held = float64(secs)
		}
		hv := round1(held)
		br.x.track(br.track, false, func(t *benchTrack) {
			t.direct = held / total
			t.detail["held_s"], t.detail["hold_s"] = hv, secs
			t.detail["text"] = fmt.Sprintf("Holding the model idle: %.0f of %d s", held, secs)
		})
		if held >= float64(secs) {
			break
		}
		step := 250 * time.Millisecond
		if rest := time.Duration((float64(secs) - held) * float64(time.Second)); rest < step {
			step = rest
		}
		if !br.x.sleep(step) {
			br.fail(OutcomeCancelled, "cancelled during the hold")
			return
		}
		if br.ctx.Err() != nil {
			br.fail(OutcomeFailed, fmt.Sprintf("timed out after %s (timeout_s)", br.b.Timeout()))
			return
		}
	}
	h := round1(time.Since(start).Seconds())
	pr.HeldS = &h
	if s, err := br.x.m.o.API.Status(br.ctx); err == nil {
		in := s.Instance(br.b.Profile)
		still := in != nil && in.State == "ready"
		pr.LoadedAfterHold = &still
	}
	// after the idle time the model may have been unloaded: the request then loads it again
	br.loaded = false
	if !br.ensureLoaded() {
		return
	}
	br.x.track(br.track, false, func(t *benchTrack) { t.detail["text"] = "Sending a request after the idle time" })
	req, _ := br.b.RequestOf(p)
	br.one(req, PhaseHold, i, 0)
}

func (br *benchRun) phaseConcurrent(i int, p *benchspec.Phase, pr *PhaseResult) {
	if !br.ensureLoaded() {
		return
	}
	n := p.Conc()
	req, _ := br.b.RequestOf(p)
	br.x.setUnits(br.track, n, fmt.Sprintf("Simultaneous requests: 0 of %d done", n))
	items := make([]ConcurrentItem, n)
	var wg sync.WaitGroup
	var dmu sync.Mutex
	finished := 0
	fire := make(chan struct{})
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-fire
			rr, resp, err := br.send(br.ctx, req, PhaseConcurrent, i, k+1)
			it := ConcurrentItem{Index: k + 1, WallMS: rr.WallMS, DecodeTPS: rr.DecodeTPS, PrefillTPS: rr.PromptTPS}
			switch {
			case err != nil:
				it.Error = err.Error()
			case resp.Status != 200:
				it.Status, it.Error = resp.Status, resp.ErrMessage
			default:
				it.Status, it.WaitMS = 200, rr.WaitMS
			}
			dmu.Lock()
			items[k] = it
			finished++
			f := finished
			dmu.Unlock()
			br.x.unitDone(br.track, fmt.Sprintf("Simultaneous requests: %d of %d done", f, n))
		}()
	}
	start := time.Now()
	close(fire)
	wg.Wait()
	res := &ConcurrentResult{N: n, WallMS: round1(float64(time.Since(start)) / float64(time.Millisecond)), Items: items}
	for _, it := range items {
		if it.Status != 200 {
			res.Errors++
		}
	}
	pr.Concurrent = res
	switch {
	case br.ctx.Err() != nil:
		br.fail(OutcomeFailed, fmt.Sprintf("timed out after %s (timeout_s)", br.b.Timeout()))
	case br.x.a.cancelled():
		br.fail(OutcomeCancelled, "cancelled")
	}
}

// ---- unload ----

func (br *benchRun) phaseUnload(p *benchspec.Phase, pr *PhaseResult) {
	x, st, b := br.x, br.st, br.b
	if p.OnlyIfLoadedByRun() && !x.a.loadedSet()[b.Profile] {
		pr.State = PhaseSkipped
		return
	}
	// a fresh context: the benchmark's own may have run out, and a failed or cancelled step must still clean up
	ctx, cancel := context.WithTimeout(WithRun(context.Background(), st.RunID), x.m.o.GoneTimeout+30*time.Second)
	defer cancel()
	force := st.Outcome == OutcomeFailed || st.Outcome == OutcomeCancelled
	status, err := x.m.o.API.Status(ctx)
	if err != nil {
		br.fail(OutcomeError, "status: "+err.Error())
		return
	}
	res := &UnloadResult{Loaded: status.Instance(b.Profile) != nil}
	pr.Unload = res
	br.loaded = false
	if !res.Loaded {
		res.Gone = true
		x.a.markLoaded(b.Profile, false)
		return
	}
	start := time.Now()
	var ans *UnloadInfo
	if force {
		ans, err = x.m.o.API.Unload(ctx, b.Profile, true)
	} else {
		ans, err = x.unloadGraceful(ctx, b.Profile)
	}
	switch {
	case err != nil:
		br.fail(OutcomeError, "unload: "+err.Error())
		return
	case ans.Status == 409:
		br.fail(OutcomeError, "the profile is busy, so it cannot be unloaded: "+ans.Msg)
		return
	case ans.Status != 200:
		br.fail(OutcomeError, fmt.Sprintf("unload answered %d: %s", ans.Status, ans.Msg))
		return
	}
	res.Gone = x.waitGoneNoCancel(ctx, b.Profile)
	res.UnloadS = round3(time.Since(start).Seconds())
	if res.Gone { // still there: the step's clean-up unloads it again with force
		x.a.markLoaded(b.Profile, false)
	}
	if !res.Gone {
		br.fail(OutcomeError, "the profile did not leave the scheduler in time")
	}
}

// cleanUp ends a step that did not end ready with a forced unload of its
// profile when the run still counts it as loaded (a failed phase, a timeout,
// a cancel, a lost load answer, an unload that did not finish), so the next
// step starts from what it expects. A ready step keeps what it left loaded
// on purpose; the run's own clean-up unloads that at the end.
func (br *benchRun) cleanUp() {
	x, st, b := br.x, br.st, br.b
	if st.Outcome == OutcomeReady || !x.a.loadedSet()[b.Profile] {
		return
	}
	ctx, cancel := context.WithTimeout(WithRun(context.Background(), st.RunID), x.m.o.GoneTimeout+30*time.Second)
	defer cancel()
	// Give the step's own last request the grace to be counted as ended,
	// then unload with force, which the scheduler does not refuse for
	// requests in flight. A failed call is tried twice more.
	x.waitNoneInFlight(ctx, b.Profile)
	var ans *UnloadInfo
	var err error
	for try := 0; try < 3 && (try == 0 || err != nil || ans.Status != 200); try++ {
		if try > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		ans, err = x.m.o.API.Unload(ctx, b.Profile, true)
	}
	switch {
	case err != nil:
		x.m.log.Warn("benchmark step could not unload its profile", "run", st.RunID, "step", st.Idx, "profile", b.Profile, "err", err)
		return
	case ans.Status != 200:
		x.m.log.Warn("benchmark step could not unload its profile", "run", st.RunID, "step", st.Idx, "profile", b.Profile, "status", ans.Status, "msg", ans.Msg)
		return
	}
	gone := x.waitGoneNoCancel(ctx, b.Profile)
	if gone {
		x.a.markLoaded(b.Profile, false)
	}
	br.loaded = false
	x.m.log.Info("benchmark step unloaded its profile after it ended "+st.Outcome, "run", st.RunID, "step", st.Idx, "profile", b.Profile, "gone", gone)
}

// unloadGraceful unloads without force and, while the scheduler answers 409
// (requests in flight), tries again for ManagerOptions.UnloadGrace: the
// scheduler counts a request's end a moment after the client has the whole
// answer, so an unload right after the run's last request can meet it. A
// request that really stays in flight still gets the 409 after the grace.
func (x *suiteExec) unloadGraceful(ctx context.Context, profile string) (*UnloadInfo, error) {
	deadline := time.Now().Add(x.m.o.UnloadGrace)
	for {
		ans, err := x.m.o.API.Unload(ctx, profile, false)
		if err != nil || ans.Status != 409 || !time.Now().Before(deadline) {
			return ans, err
		}
		select {
		case <-ctx.Done():
			return ans, nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// waitNoneInFlight waits up to ManagerOptions.UnloadGrace until the
// profile's instance counts no request in flight (or is gone).
func (x *suiteExec) waitNoneInFlight(ctx context.Context, profile string) {
	deadline := time.Now().Add(x.m.o.UnloadGrace)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		s, err := x.m.o.API.Status(ctx)
		if err == nil {
			if in := s.Instance(profile); in == nil || in.InFlight == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitGoneNoCancel is waitGone for cleanup: a cancelled run still waits.
func (x *suiteExec) waitGoneNoCancel(ctx context.Context, profile string) bool {
	deadline := time.Now().Add(x.m.o.GoneTimeout)
	for {
		s, err := x.m.o.API.Status(ctx)
		if err == nil && s.Instance(profile) == nil {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(x.m.o.IdlePoll)
	}
}

// waitGone waits until the profile has no instance (or the run is cancelled).
func (x *suiteExec) waitGone(ctx context.Context, profile string) bool {
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

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// ---- finish ----

// finish completes a step: statistics, expectations, comparison, storage and events.
func (x *suiteExec) finish(br *benchRun) {
	m, st, b := x.m, br.st, br.b
	br.watch.stop()
	br.stepFacts()
	if st.Outcome == "" {
		st.Outcome = OutcomeError
		if st.Error == "" {
			st.Error = "the step ended without an outcome"
		}
	}
	br.mu.Lock()
	reqs := append([]RequestResult(nil), br.reqs...)
	br.mu.Unlock()
	st.Stats = ComputeStats(reqs)
	switch st.Outcome {
	case OutcomeReady, OutcomeRefused, OutcomeRefusedByExclusion, OutcomeFailed:
		st.State = StepDone
	case OutcomeCancelled:
		st.State = StepCancelled
	default:
		st.State = StepFailed
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
	x.finishTrack(br.track)
	x.a.mu.Lock()
	x.a.done++
	x.a.step = nil
	x.a.mu.Unlock()
	m.emit(map[string]any{"kind": "step_finished", "run_id": st.RunID, "step": stepBrief(st)})
	sev := history.Info
	pass := st.Expect != nil && st.Expect.Pass
	notJudged := st.Outcome == OutcomeRefusedByExclusion
	if !notJudged && (!pass || (st.Comparison != nil && st.Comparison.Regression)) {
		sev = history.Warn
	}
	data := map[string]any{"bench_run": st.RunID, "benchmark": b.ID, "idx": st.Idx, "outcome": st.Outcome, "result": StepResult(st)}
	if !notJudged {
		data["pass"] = pass // a step refused by an exclusion has no pass value: it was not judged
	}
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
	switch {
	case notJudged:
		text += ", not judged: the profile forces a node that exclude_nodes names"
	case !pass:
		text += ", expectations not met"
	}
	if st.Comparison != nil && st.Comparison.Regression {
		text += ", REGRESSION against the baseline"
	}
	m.record(history.TBenchStepFinished, sev, b.Profile, text+".", data)
}

func firstText(typ, profile string) string {
	switch typ {
	case benchspec.PhaseLoad:
		return "Loading " + profile
	case benchspec.PhaseUnload:
		return "Unloading " + profile
	}
	return "Starting " + typ
}
