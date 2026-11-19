package main

// DEV-ONLY: a scripted bench.API. The REAL benchmark manager and runner
// (internal/bench) drive it exactly as they drive the scheduler's HTTP API,
// so every run, step, phase, request, comparison and progress document it
// produces is computed by the real code; only the scheduler's answers are
// scripted. It serves two purposes:
//
//   - the seed history: past runs written by a second real manager on the
//     real database, on a virtual clock days in the past (bench_seed.go);
//
//   - the frozen bench-* scenarios: a real manager on a snapshot of the
//     database whose run stops at a chosen point (a load at 40%, a turn
//     half streamed, ...) and stays there (bench_frozen.go).
//
// Status answers are built with the scripted status builder and reduced by
// the real bench.ParseStatus, so a load report has the real shape.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/scheduler"
)

// vclock is the clock of a scripted manager: real time plus a shift. A
// scripted operation moves it forward by what the operation pretends to take
// (a 74 s load is 74 s on the clock, a few ms of real time). It never runs
// ahead of the real time.
type vclock struct {
	mu    sync.Mutex
	shift time.Duration
}

func (c *vclock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.shift)
}

// set puts the clock at t.
func (c *vclock) set(t time.Time) {
	c.mu.Lock()
	c.shift = time.Until(t)
	c.mu.Unlock()
}

// advanceTo moves the clock forward to t (never back, never past real time).
func (c *vclock) advanceTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rt := time.Now()
	if t.After(rt) {
		t = rt
	}
	if d := t.Sub(rt.Add(c.shift)); d > 0 {
		c.shift += d
	}
}

type perfBase struct {
	load, decode, prefill float64
}

// perf is how fast each profile pretends to be (load seconds, decode and
// prefill tokens per second).
var perf = map[string]perfBase{
	"bench-gemma-12b-1w": {74, 14.2, 96},
	"bench-gemma-12b-2w": {41.5, 11.8, 88},
	"bench-gemma-12b-3w": {31.0, 10.1, 80},
	"bench-devstral-24b": {118, 7.9, 48},
	"bench-kimi-72b-3w":  {105, 3.1, 14},
	"qwen3.5-4b":         {29, 31.5, 210},
	"gemma-4-12b":        {74, 14.2, 96},
	"devstral-24b":       {118, 7.9, 48},
	"qwen3.8-27b":        {96, 9.8, 60},
}

func perfOf(profile string) perfBase {
	if p, ok := perf[profile]; ok {
		return p
	}
	return perfBase{60, 12, 90}
}

type scriptInst struct {
	profile   string
	hosts     []string
	realStart time.Time // real time the load began
	readyAt   time.Time // real time the load ends
	vstart    time.Time // clock time the load began
	vready    time.Time
	ready     bool
	freeze    float64 // >0: the load stays at this fraction
	inflight  int
}

// scriptedAPI is the scheduler as the scripted runs see it.
type scriptedAPI struct {
	mu    sync.Mutex
	clock *vclock
	quit  chan struct{} // closed: every blocked call returns

	// per run settings (set between runs)
	revision   string             // config revision the status reports
	rpcVersion string             // rpc_server_version of the PS5 workers
	factor     map[string]float64 // decode multiplier by profile (1 when absent)
	loadFactor float64            // load time multiplier
	loadBy     map[string]float64 // load time multiplier by profile (1 when absent)
	jitter     int64              // seeds the per-run noise

	// freezes (frozen scenarios)
	freezeLoad map[string]float64 // profile: the load stays at this fraction
	freezeChat map[string]int     // profile: from this chat on (1-based), a request streams part of its answer and stops

	insts  map[string]*scriptInst
	recent []scheduler.LoadOutcome
	hist   map[string]*bench.HistoryLoad
	chats  map[string]int
	lastP  map[string]int // prompt tokens of the last request (a pretend prompt cache)
	seq    int
}

func newScriptedAPI(clock *vclock) *scriptedAPI {
	return &scriptedAPI{clock: clock, quit: make(chan struct{}), revision: "1", rpcVersion: "b4567", factor: map[string]float64{}, loadFactor: 1,
		freezeLoad: map[string]float64{}, freezeChat: map[string]int{}, insts: map[string]*scriptInst{}, hist: map[string]*bench.HistoryLoad{},
		chats: map[string]int{}, lastP: map[string]int{}}
}

func (a *scriptedAPI) stop() {
	select {
	case <-a.quit:
	default:
		close(a.quit)
	}
}

// noise is a deterministic factor in [1-span, 1+span] for a key in this run.
func (a *scriptedAPI) noise(key string, span float64) float64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s", a.jitter, key)
	return 1 + span*(float64(h.Sum64()%2001)/1000-1)
}

// hostsOf is where a profile goes: its forced nodes, else the first PS5.
func hostsOf(profile string) []string {
	p, _ := profileByID(profile)
	if len(p.ForceNodes) > 0 {
		return p.ForceNodes
	}
	return []string{ps5A.Host}
}

// statusLocked builds the scheduler status of the scripted state. The caller holds a.mu.
func (a *scriptedAPI) statusLocked() *statusBuilder {
	now := a.clock.Now()
	b := newStatus(now)
	for _, in := range a.insts {
		if in.ready {
			b.ready(in.profile, in.hosts, instOpts{readyAgo: now.Sub(in.vready), inFlight: in.inflight})
			continue
		}
		prog := in.freeze
		if prog == 0 {
			prog = 0.99
			if total := in.readyAt.Sub(in.realStart); total > 0 {
				prog = math.Min(0.99, float64(time.Since(in.realStart))/float64(total))
			}
		}
		b.loading(in.profile, in.hosts, loadOpts{startedAgo: now.Sub(in.vstart), progress: prog, rateMBs: 101})
	}
	b.st.RecentLoads = append(b.st.RecentLoads, a.recent...)
	ver := a.rpcVersion
	for _, n := range []nodeDef{ps5A, ps5B, ps5C} {
		b.nodes[n.Host].RPCServerVersion = &ver
	}
	return b
}

// tick turns finished loads ready. The caller holds a.mu.
func (a *scriptedAPI) tickLocked() {
	for name, in := range a.insts {
		if in.ready || in.freeze > 0 || time.Now().Before(in.readyAt) {
			continue
		}
		p := perfOf(name)
		f := a.loadFactor
		if m, ok := a.loadBy[name]; ok {
			f *= m
		}
		loadS := round1(p.load * f * a.noise("load:"+name, 0.04))
		in.vready = in.vstart.Add(time.Duration(loadS * float64(time.Second)))
		a.clock.advanceTo(in.vready)
		in.ready = true
		a.seq++
		sid := fmt.Sprintf("ld-%d-%d", in.vready.UnixMilli(), a.seq)
		var workers []string
		var nodes []bench.NodeLoad
		pd, _ := profileByID(name)
		total := modelBytes[pd.Model]
		if total == 0 {
			total = 4_000_000_000
		}
		fr := fractions(pd, len(in.hosts))
		mem := measuredTotal(name, len(in.hosts))
		for i, h := range in.hosts {
			n, _ := nodeByHost(h)
			if !n.Local {
				workers = append(workers, n.Addr())
			}
			by := int64(float64(total) * fr[i])
			mi := round1(mem * fr[i])
			nodes = append(nodes, bench.NodeLoad{Node: h, Bytes: &by, MemoryMiB: &mi})
		}
		a.recent = append([]scheduler.LoadOutcome{{SessionID: sid, Model: name, Workers: workers, Outcome: "ready", FinishedAt: in.vready.UnixMilli(), LoadS: loadS}}, a.recent...)
		if len(a.recent) > 20 {
			a.recent = a.recent[:20]
		}
		a.hist[sid] = &bench.HistoryLoad{LoadS: loadS, Bytes: total, AvgMBs: round1(float64(total) / 1e6 / loadS), Nodes: nodes}
	}
}

func (a *scriptedAPI) Plan(ctx context.Context, model string) (*bench.PlanInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tickLocked()
	b := a.statusLocked()
	st := b.finishStatus()
	rep := planFor(model, st, b.now)
	if p, ok := profileByID(model); ok && len(p.ForceNodes) > 0 && rep.Action == scheduler.PlanStart {
		rep.Placement = nil
		for _, h := range p.ForceNodes {
			n, _ := nodeByHost(h)
			rep.Placement = append(rep.Placement, scheduler.PlacedNodeStatus{ID: core.NodeID(n.ID), Addr: n.Addr(), Local: n.Local, BudgetMiB: n.Budget()})
		}
	}
	raw, _ := json.Marshal(planJSON(rep))
	pi := &bench.PlanInfo{Action: string(rep.Action), Reason: rep.Reason, Raw: raw}
	for _, pn := range rep.Placement {
		pi.Nodes = append(pi.Nodes, string(pn.ID))
	}
	return pi, nil
}

func (a *scriptedAPI) refusal(model string) string {
	b := a.statusLocked()
	st := b.finishStatus()
	if rep := planFor(model, st, b.now); rep.Action == scheduler.PlanReject {
		return rep.Reason
	}
	return ""
}

func (a *scriptedAPI) beginLoadLocked(model string) {
	in := &scriptInst{profile: model, hosts: hostsOf(model), vstart: a.clock.Now(), realStart: time.Now(), readyAt: time.Now().Add(60 * time.Millisecond)}
	if f := a.freezeLoad[model]; f > 0 {
		in.freeze = f
	}
	a.insts[model] = in
}

func (a *scriptedAPI) Load(ctx context.Context, model string) (*bench.LoadAnswer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := profileByID(model); !ok {
		return &bench.LoadAnswer{Status: 404, Msg: fmt.Sprintf("unknown model: model %q not found", model), Reason: "unknown model"}, nil
	}
	if _, ok := a.insts[model]; ok {
		return &bench.LoadAnswer{Status: 202, Msg: "loading"}, nil
	}
	if why := a.refusal(model); why != "" {
		return &bench.LoadAnswer{Status: 503, Msg: why, Reason: "does not fit"}, nil
	}
	a.beginLoadLocked(model)
	return &bench.LoadAnswer{Status: 202, Msg: "loading"}, nil
}

func (a *scriptedAPI) Unload(ctx context.Context, model string, force bool) (*bench.UnloadInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, ok := a.insts[model]
	if !ok {
		return &bench.UnloadInfo{Status: 200, Msg: "not loaded"}, nil
	}
	if in.inflight > 0 && !force {
		return &bench.UnloadInfo{Status: 409, Msg: "the instance has requests in flight"}, nil
	}
	delete(a.insts, model)
	a.clock.advanceTo(a.clock.Now().Add(time.Duration(2.2 * a.noise("unload:"+model, 0.15) * float64(time.Second))))
	return &bench.UnloadInfo{Status: 200, Msg: "unloaded"}, nil
}

func (a *scriptedAPI) Status(ctx context.Context) (*bench.StatusInfo, error) {
	a.mu.Lock()
	a.tickLocked()
	b := a.statusLocked()
	rev := a.revision
	a.mu.Unlock()
	doc := b.finish()
	doc["config_revision"] = rev
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return bench.ParseStatus(raw)
}

func (a *scriptedAPI) LoadHistory(ctx context.Context, sid string) (*bench.HistoryLoad, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hist[sid], nil
}

func (a *scriptedAPI) wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-a.quit:
		return errors.New("the dev harness switched scenario")
	}
}

func (a *scriptedAPI) Chat(ctx context.Context, runID string, req bench.ChatRequest, onProgress func(bench.ChatProgress)) (*bench.ChatResponse, error) {
	var body struct {
		Model       string `json:"model"`
		Stream      bool   `json:"stream"`
		CachePrompt bool   `json:"cache_prompt"`
		MaxTokens   int    `json:"max_tokens"`
		Messages    []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(req.Body, &body)
	model := body.Model
	if model == "" {
		model = req.Model
	}
	a.mu.Lock()
	if _, ok := profileByID(model); !ok {
		a.mu.Unlock()
		return &bench.ChatResponse{Status: 404, ErrMessage: fmt.Sprintf("unknown model: model %q not found", model), ErrReason: "unknown model"}, nil
	}
	if _, ok := a.insts[model]; !ok { // a request for a model that is not loaded loads it
		if why := a.refusal(model); why != "" {
			a.mu.Unlock()
			return &bench.ChatResponse{Status: 503, ErrMessage: why, ErrReason: "does not fit"}, nil
		}
		a.beginLoadLocked(model)
	}
	a.mu.Unlock()
	for { // wait for ready
		a.mu.Lock()
		a.tickLocked()
		in := a.insts[model]
		ready := in != nil && in.ready
		a.mu.Unlock()
		if in == nil {
			return &bench.ChatResponse{Status: 502, ErrMessage: "the instance went away during the load"}, nil
		}
		if ready {
			break
		}
		if err := a.wait(ctx, 20*time.Millisecond); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	in := a.insts[model]
	if in == nil {
		a.mu.Unlock()
		return &bench.ChatResponse{Status: 502, ErrMessage: "the instance went away"}, nil
	}
	in.inflight++
	a.chats[model]++
	n := a.chats[model]
	freezeAt := a.freezeChat[model]
	vstart := a.clock.Now()
	chars := 0
	last := ""
	for _, m := range body.Messages {
		if s, ok := m.Content.(string); ok {
			chars += len(s)
			last = s
		}
	}
	pt := max(chars/4, 10) + 18
	cache := 0 // the slot still holds the previous prompt and its answer
	if body.CachePrompt && a.lastP[model] > 0 && pt > a.lastP[model] {
		cache = a.lastP[model]
	}
	a.lastP[model] = pt
	p := perfOf(model)
	dec := p.decode * a.noise(fmt.Sprintf("dec:%s:%d", model, n), 0.03)
	if f, ok := a.factor[model]; ok {
		dec *= f
	}
	pre := p.prefill * a.noise(fmt.Sprintf("pre:%s:%d", model, n), 0.05)
	a.mu.Unlock()
	defer func() { a.mu.Lock(); in.inflight--; a.mu.Unlock() }()

	maxTok := body.MaxTokens
	if maxTok <= 0 {
		maxTok = 128
	}
	if freezeAt > 0 && n >= freezeAt { // stream part of the answer, then stay there
		if body.Stream && onProgress != nil {
			onProgress(bench.ChatProgress{Tokens: maxTok * 45 / 100, ElapsedMS: 3400})
		}
		<-a.quit
		return nil, errors.New("the dev harness switched scenario")
	}
	// Simultaneous requests share the slots: a request that finds others in
	// flight runs in real time (about two seconds) so the phase's wall
	// clock means something; a lone one takes a few milliseconds.
	if err := a.wait(ctx, 12*time.Millisecond); err != nil {
		return nil, err
	}
	a.mu.Lock()
	concurrent := in.inflight > 1
	share := float64(in.inflight)
	a.mu.Unlock()
	// the answer depends only on the model and the prompt (deterministic requests repeat it exactly)
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%d|%s", model, len(body.Messages), last)
	seed := h.Sum64()
	outN := max(1, min(maxTok, int(float64(maxTok)*(0.7+0.3*float64(seed%1000)/1000))))
	if concurrent {
		dec = dec / math.Sqrt(share)
	}
	promptN := pt - cache
	promptMS := round1(float64(promptN) / pre * 1000)
	predMS := round1(float64(outN) / dec * 1000)
	wall := promptMS + predMS + 35
	ttft := promptMS + 28
	if concurrent {
		if err := a.wait(ctx, time.Duration(wall/4)*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if body.Stream && onProgress != nil {
		onProgress(bench.ChatProgress{Tokens: outN / 2, ElapsedMS: ttft + predMS/2})
		onProgress(bench.ChatProgress{Tokens: outN, ElapsedMS: wall})
	}
	a.clock.advanceTo(vstart.Add(time.Duration(wall * float64(time.Millisecond))))
	text := scriptedText(seed, outN)
	a.mu.Lock()
	a.lastP[model] = pt + outN
	a.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"role": "assistant", "content": text})
	tm, _ := json.Marshal(map[string]any{"prompt_n": promptN, "prompt_ms": promptMS, "prompt_per_second": round1(pre), "predicted_n": outN,
		"predicted_ms": predMS, "predicted_per_second": round1(dec), "cache_n": cache})
	resp := &bench.ChatResponse{Status: 200, WallMS: wall, Timings: tm, Message: msg, Text: text}
	if body.Stream {
		resp.TTFTMS = &ttft
	}
	return resp, nil
}

var scriptWords = strings.Fields(`the a model node load split worker cache prompt token answer request phase cluster memory link upload
layer tensor budget queue slot server switch budget fast slow first next because which this that with from into over under each
small large cheap quiet home lab rack power fan board drive network cable port host guest plan list table price used new`)

// scriptedText is a deterministic answer of about n tokens (four characters each).
func scriptedText(seed uint64, n int) string {
	var b strings.Builder
	for b.Len() < n*4 {
		seed = seed*6364136223846793005 + 1442695040888963407
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(scriptWords[(seed>>33)%uint64(len(scriptWords))])
	}
	return b.String()
}
