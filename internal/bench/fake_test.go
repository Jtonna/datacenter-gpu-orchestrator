package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// fakeProf scripts how the fake scheduler treats one profile.
type fakeProf struct {
	refuse    bool          // 503 "does not fit"
	fail      bool          // the load fails
	loadDelay time.Duration // how long the load takes
	loadS     float64       // load time the scheduler reports
	decode    float64       // decode tokens per second in the timings
	prefill   float64
	output    string
	chatDelay time.Duration
	nodes     []string
	modelFile string
	chatCode  int // a status other than 200 for every chat request (after the load)

	loadCode    int    // /api/load answers this status (with the scheduler's words) instead of 202
	loadReason  string // the reason code of that answer
	loadMsg     string // its message (default "scripted load answer")
	failOutcome string // outcome of a failed load in recent_loads (default "failed")
	failReason  string // reason of a failed load (default "child exited during load")
	// refuseAfter makes the scheduler accept the load (202), show the instance
	// for loadDelay, then refuse it before any child starts: "status" records
	// it in last_refusals, "history" and "backoff" only in the history.
	refuseAfter  string
	refuseReason string // reason code of that refusal (default "forced nodes do not fit")
	refuseText   string
	refuseTag    string // bench run tag the refusal carries (default: the run that sent the load)
	// loadErr makes /api/load start the load and then fail on the way back
	// (a connection reset after the scheduler accepted it).
	loadErr bool
}

type fakeInst struct {
	state    string
	inflight int
	started  time.Time
	delay    time.Duration
}

// fakeAPI is the scheduler's HTTP API, scripted. It records every call.
type fakeAPI struct {
	mu       sync.Mutex
	profs    map[string]*fakeProf
	insts    map[string]*fakeInst
	external map[string]bool // instances of other users, shown in status
	extReq   int             // requests in flight of other users
	recent   []RecentLoad
	hist     map[string]*HistoryLoad
	calls    []string
	chats    map[string][]time.Time // chat start times by profile
	loads    map[string][]time.Time // /api/load times by profile
	nChat    map[string]int
	hdrRuns  []string
	seq      int
	busyUnl  map[string]bool // unload answers 409
	slowOnce map[string]bool // the first request after a load is slow
	bodies   []string        // every chat body
	lastP    map[string]int  // prompt tokens of the last request by profile (a pretend prompt cache)
	unloadMS time.Duration   // how long an unloaded instance stays in the status
	gone     map[string]time.Time
	revision string
	version  string // rpc_server_version of the nodes
	loadHook func()
	lastRef  []Refusal // status last_refusals
	histRef  []Refusal // history refusal events
	runOf    map[string]string
	// mem gives a node's available memory for the status (nil: unknown);
	// loading says whether an instance is starting, chats counts the chat
	// requests sent so far.
	mem      func(id string, loading bool, chats int) *int64
	loadMode string       // instances' load_mode
	reload   []NodeReload // the plan's reload_from_cache
	// The file cache eviction (POST /api/cache/evict): planNodes are the
	// nodes the plan names (default n1); evictNode scripts a node's answers
	// in turn (the last one repeats; default: dropped to 0); evictLocal is
	// the scheduler machine's answer; evictErr fails the call.
	planNodes  []string
	evictNode  map[string][]NodeEvict
	evictLocal *LocalEvict
	evictErr   error
	// strictUnload makes an unload without force refuse (409) while the
	// instance counts requests in flight, as the scheduler does; linger keeps
	// a finished request counted that long after its answer was returned
	// (the scheduler counts the end a moment after the client has the
	// body); stuck adds requests in flight that never end.
	strictUnload bool
	linger       time.Duration
	stuck        map[string]int
}

func newFake() *fakeAPI {
	return &fakeAPI{profs: map[string]*fakeProf{}, insts: map[string]*fakeInst{}, external: map[string]bool{}, hist: map[string]*HistoryLoad{},
		chats: map[string][]time.Time{}, loads: map[string][]time.Time{}, nChat: map[string]int{}, busyUnl: map[string]bool{}, slowOnce: map[string]bool{},
		lastP: map[string]int{}, gone: map[string]time.Time{}, version: "b4567"}
}

func (f *fakeAPI) prof(name string, p fakeProf) *fakeAPI {
	if p.decode == 0 {
		p.decode = 30
	}
	if p.prefill == 0 {
		p.prefill = 200
	}
	if p.output == "" {
		p.output = "hello from " + name
	}
	if p.loadS == 0 {
		p.loadS = 66.1
	}
	if p.nodes == nil {
		p.nodes = []string{"ps5-a"}
	}
	if p.modelFile == "" {
		p.modelFile = name + ".gguf"
	}
	f.profs[name] = &p
	return f
}

func (f *fakeAPI) log(s string) { f.calls = append(f.calls, s) }

func (f *fakeAPI) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAPI) has(inst string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.insts[inst]
	return ok
}

func (f *fakeAPI) Plan(ctx context.Context, model string) (*PlanInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("plan:" + model)
	nodes := f.planNodes
	if nodes == nil {
		nodes = []string{"n1"}
	}
	return &PlanInfo{Action: "start", Nodes: append([]string(nil), nodes...), LoadMode: f.loadMode, Reload: append([]NodeReload(nil), f.reload...),
		Raw: json.RawMessage(`{"model":"` + model + `","action":"start"}`)}, nil
}

// beginLoad starts the load of a profile that has no instance (the caller holds f.mu).
func (f *fakeAPI) beginLoad(model string, p *fakeProf) {
	in := &fakeInst{state: "starting", started: time.Now(), delay: p.loadDelay}
	f.insts[model] = in
	f.loads[model] = append(f.loads[model], time.Now())
	go func() {
		time.Sleep(p.loadDelay)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.insts[model] != in { // unloaded meanwhile
			return
		}
		f.seq++
		sid := fmt.Sprintf("ld-%d", f.seq)
		if p.refuseAfter != "" {
			delete(f.insts, model)
			r := Refusal{Profile: model, AtMS: time.Now().UnixMilli(), Status: 503, Reason: orStr(p.refuseReason, "forced nodes do not fit"), Message: p.refuseText, BenchRun: orStr(p.refuseTag, f.runOf[model])}
			switch p.refuseAfter {
			case "none":
				// nothing is recorded
			case "status":
				f.lastRef = append(f.lastRef, r)
			case "backoff":
				r.Type = "profileBackedOff"
				f.histRef = append(f.histRef, r)
			default:
				r.Type = "requestRefused"
				f.histRef = append(f.histRef, r)
			}
			return
		}
		if p.fail {
			delete(f.insts, model)
			f.recent = append([]RecentLoad{{SessionID: sid, Model: model, Outcome: orStr(p.failOutcome, "failed"), Reason: orStr(p.failReason, "child exited during load"), FinishedAt: time.Now().UnixMilli(), LoadS: 3}}, f.recent...)
			return
		}
		in.state = "ready"
		f.recent = append([]RecentLoad{{SessionID: sid, Model: model, Outcome: "ready", FinishedAt: time.Now().UnixMilli(), LoadS: p.loadS, Workers: []string{"192.0.2.2:50052"}}}, f.recent...)
		var nodes []NodeLoad
		for _, nn := range p.nodes {
			b, mem := int64(6700000000), 7900.0
			nodes = append(nodes, NodeLoad{Node: nn, Bytes: &b, MemoryMiB: &mem})
		}
		f.hist[sid] = &HistoryLoad{LoadS: p.loadS, Bytes: 6700000000, AvgMBs: 101.4, Nodes: nodes}
		f.slowOnce[model] = true
	}()
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (f *fakeAPI) Refusals(ctx context.Context, profile string, since int64, types ...string) ([]Refusal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("refusals:" + profile)
	var out []Refusal
	for _, r := range f.histRef {
		if r.Profile != profile || r.AtMS < since {
			continue
		}
		for _, t := range types {
			if t == r.Type {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeAPI) Load(ctx context.Context, model string) (*LoadAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("load:" + model)
	if f.loadHook != nil {
		f.loadHook()
	}
	p := f.profs[model]
	if p == nil {
		return &LoadAnswer{Status: 404, Msg: "unknown model"}, nil
	}
	if _, ok := f.insts[model]; ok {
		return &LoadAnswer{Status: 202, Msg: "loading"}, nil
	}
	if p.refuse {
		return &LoadAnswer{Status: 503, Msg: "does not fit: needs 99999 MiB", Reason: "does not fit"}, nil
	}
	if p.loadCode != 0 {
		msg := p.loadMsg
		if msg == "" {
			msg = "scripted load answer"
		}
		return &LoadAnswer{Status: p.loadCode, Msg: msg, Reason: p.loadReason}, nil
	}
	if f.runOf == nil {
		f.runOf = map[string]string{}
	}
	if id, _ := ctx.Value(runCtxKey{}).(string); id != "" {
		f.runOf[model] = id
	}
	f.beginLoad(model, p)
	if p.loadErr {
		return nil, fmt.Errorf("read tcp: connection reset by peer")
	}
	return &LoadAnswer{Status: 202, Msg: "loading"}, nil
}

func (f *fakeAPI) EvictCache(ctx context.Context, req CacheEvictRequest) (*CacheEvictAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(fmt.Sprintf("evict:%s nodes=%s local=%v", req.Model, strings.Join(req.Nodes, ","), req.Local))
	if f.evictErr != nil {
		return nil, f.evictErr
	}
	ans := &CacheEvictAnswer{Model: req.Model, File: req.Model + ".gguf", Nodes: []NodeEvict{}}
	if req.Local {
		ans.Local = &LocalEvict{EvictFigures: &EvictFigures{FileBytes: 1000, CachedBefore: 1000}}
		if f.evictLocal != nil {
			l := *f.evictLocal
			ans.Local = &l
		}
	}
	for _, id := range req.Nodes {
		n := NodeEvict{Capable: true, Status: 200, EvictFigures: &EvictFigures{FileBytes: 1000, CachedBefore: 900}}
		if seq := f.evictNode[id]; len(seq) > 0 {
			n = seq[0]
			if len(seq) > 1 {
				f.evictNode[id] = seq[1:]
			}
		}
		n.ID = id
		ans.Nodes = append(ans.Nodes, n)
	}
	return ans, nil
}

func (f *fakeAPI) Unload(ctx context.Context, model string, force bool) (*UnloadInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(fmt.Sprintf("unload:%s force=%v", model, force))
	if f.busyUnl[model] {
		return &UnloadInfo{Status: 409, Msg: "busy"}, nil
	}
	in, ok := f.insts[model]
	if !ok {
		return &UnloadInfo{Status: 200, Msg: "not loaded"}, nil
	}
	if n := in.inflight + f.stuck[model]; f.strictUnload && !force && n > 0 {
		return &UnloadInfo{Status: 409, Msg: fmt.Sprintf("model busy: %s has %d requests in flight (use force to stop it anyway)", model, n)}, nil
	}
	delete(f.insts, model)
	if f.unloadMS > 0 {
		f.gone[model] = time.Now().Add(f.unloadMS)
	}
	return &UnloadInfo{Status: 200, Msg: "ok"}, nil
}

func (f *fakeAPI) Status(ctx context.Context) (*StatusInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := &StatusInfo{Hostnames: map[string]string{"n1": "ps5-a", "n2": "ps5-b", "n3": "ps5-c"}, RecentLoads: append([]RecentLoad(nil), f.recent...), LastRefusals: append([]Refusal(nil), f.lastRef...), ConfigRevision: f.revision}
	st.Workers = []WorkerInfo{{ID: "n0", Hostname: "host-1", Local: true}, {ID: "n1", Hostname: "ps5-a", RPCServerVersion: f.version},
		{ID: "n2", Hostname: "ps5-b", RPCServerVersion: f.version}, {ID: "n3", Hostname: "ps5-c", RPCServerVersion: f.version}}
	for name, in := range f.insts {
		p := f.profs[name]
		ii := InstanceInfo{Profile: name, State: in.state, InFlight: in.inflight + f.stuck[name], LogFile: "/var/log/llm-children/" + name + ".log",
			Cmd: "/opt/llama/llama-server-rpc --model x --rpc 192.0.2.2:50052", DeviceTotalMiB: 7900, LoadMode: f.loadMode}
		if p != nil {
			ii.ModelFile = p.modelFile
			for k := range p.nodes {
				ii.Nodes = append(ii.Nodes, InstanceNodeInfo{ID: fmt.Sprintf("n%d", k+1), MemoryMiB: 7900})
			}
		}
		if in.state == "starting" {
			prog := 0.5
			if in.delay > 0 {
				prog = float64(time.Since(in.started)) / float64(in.delay)
			}
			if prog > 0.99 {
				prog = 0.99
			}
			tot := int64(1000)
			sent := int64(prog * 1000)
			ii.Load = &LoadInfo{SessionID: "ld-live", BytesSent: &sent, BytesTotal: &tot, Progress: &prog,
				Raw: json.RawMessage(fmt.Sprintf(`{"session_id":"ld-live","progress":%.4f,"bytes_total":1000,"bytes_sent":%d,"workers":[{"addr":"192.0.2.2:50052","bytes_total":1000,"bytes_sent":%d,"state":"receiving"}]}`, prog, sent, sent))}
		}
		st.Instances = append(st.Instances, ii)
	}
	for name := range f.external {
		st.Instances = append(st.Instances, InstanceInfo{Profile: name, State: "ready", InFlight: f.extReq})
	}
	if f.mem != nil {
		loading, chats := false, 0
		for _, in := range f.insts {
			loading = loading || in.state == "starting"
		}
		for _, n := range f.nChat {
			chats += n
		}
		for i := range st.Workers {
			st.Workers[i].AvailMiB = f.mem(st.Workers[i].ID, loading, chats)
		}
	}
	for name, until := range f.gone { // an unloaded instance lingers for a moment
		if time.Now().Before(until) {
			st.Instances = append(st.Instances, InstanceInfo{Profile: name, State: "stopping"})
		}
	}
	return st, nil
}

func (f *fakeAPI) LoadHistory(ctx context.Context, sid string) (*HistoryLoad, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hist[sid], nil
}

// promptTokens is the fake tokenizer: a quarter of the characters, at least 10.
func promptTokens(body struct {
	Messages []struct {
		Content string `json:"content"`
	} `json:"messages"`
}) int {
	n := 0
	for _, m := range body.Messages {
		n += len(m.Content)
	}
	if n/4 < 10 {
		return 10
	}
	return n / 4
}

func (f *fakeAPI) Chat(ctx context.Context, runID string, req ChatRequest, onProgress func(ChatProgress)) (*ChatResponse, error) {
	var body struct {
		Model       string `json:"model"`
		Stream      bool   `json:"stream"`
		CachePrompt bool   `json:"cache_prompt"`
		MaxTokens   int    `json:"max_tokens"`
		Messages    []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(req.Body, &body)
	f.mu.Lock()
	f.hdrRuns = append(f.hdrRuns, runID)
	f.bodies = append(f.bodies, string(req.Body))
	p := f.profs[body.Model]
	f.nChat[body.Model]++
	n := f.nChat[body.Model]
	f.log(fmt.Sprintf("chat:%s#%d", body.Model, n))
	f.chats[body.Model] = append(f.chats[body.Model], time.Now())
	if p == nil {
		f.mu.Unlock()
		return &ChatResponse{Status: 404, ErrMessage: "model not found"}, nil
	}
	in, loaded := f.insts[body.Model]
	if !loaded { // a request for a model that is not loaded loads it, like the real scheduler
		if p.refuse {
			f.mu.Unlock()
			return &ChatResponse{Status: 503, ErrMessage: "does not fit: needs 99999 MiB", ErrReason: "does not fit"}, nil
		}
		f.beginLoad(body.Model, p)
		in = f.insts[body.Model]
	}
	f.mu.Unlock()
	for { // wait for ready or for the load to fail
		f.mu.Lock()
		cur, ok := f.insts[body.Model]
		ready := ok && cur == in && in.state == "ready"
		failed := !ok || cur != in
		f.mu.Unlock()
		if ready {
			break
		}
		if failed {
			return &ChatResponse{Status: 502, ErrMessage: "load failed: child exited during load"}, nil
		}
		select {
		case <-time.After(2 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	if p.chatCode != 0 {
		f.mu.Unlock()
		return &ChatResponse{Status: p.chatCode, ErrMessage: "scripted failure"}, nil
	}
	in.inflight++
	decode := p.decode
	if f.slowOnce[body.Model] { // the first request after a load is slow on this hardware
		decode = p.decode / 5
		delete(f.slowOnce, body.Model)
	}
	delay := p.chatDelay
	pt := promptTokens(struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}{body.Messages})
	cache := 0
	if body.CachePrompt && f.lastP[body.Model] > 0 && pt > f.lastP[body.Model] {
		cache = f.lastP[body.Model]
	}
	f.lastP[body.Model] = pt
	f.mu.Unlock()
	defer func() {
		if f.linger > 0 {
			go func() { time.Sleep(f.linger); f.mu.Lock(); in.inflight--; f.mu.Unlock() }()
			return
		}
		f.mu.Lock()
		in.inflight--
		f.mu.Unlock()
	}()
	if delay > 0 {
		if body.Stream && onProgress != nil && body.MaxTokens > 0 {
			steps := 4
			for k := 1; k <= steps; k++ {
				select {
				case <-time.After(delay / time.Duration(steps)):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				onProgress(ChatProgress{Tokens: body.MaxTokens * k / (steps + 1), ElapsedMS: float64(k)})
			}
		} else {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	reply := p.output
	if strings.Contains(string(req.Body), "second") {
		reply = p.output + " (turn two)"
	}
	msg, _ := json.Marshal(map[string]any{"role": "assistant", "content": reply})
	tm, _ := json.Marshal(map[string]any{"prompt_n": pt - cache, "prompt_ms": 5, "prompt_per_second": p.prefill, "predicted_n": 8, "predicted_ms": 80, "predicted_per_second": decode, "cache_n": cache})
	resp := &ChatResponse{Status: 200, WallMS: 12, Timings: tm, Message: msg, Text: reply}
	if body.Stream {
		v := 7.0
		resp.TTFTMS = &v
		if onProgress != nil {
			onProgress(ChatProgress{Tokens: 8, ElapsedMS: 12})
		}
	}
	return resp, nil
}
