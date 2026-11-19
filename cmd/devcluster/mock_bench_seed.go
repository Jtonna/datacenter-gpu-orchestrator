// DEV-ONLY MOCK: the seeded benchmarks, suites and past runs of the bench
// mock, and the synthetic step results (phases, turns, requests, stats,
// expectations, comparison) that the mock engine also uses for the runs it
// plays. Shapes follow internal/bench/types.go of feat/bench-phases.

package main

import (
	"crypto/sha256"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
)

type perfBase struct {
	load, decode, prefill, mem float64
}

var perf = map[string]perfBase{
	"bench-gemma-12b-1w": {74, 14.2, 96, 7514},
	"bench-gemma-12b-2w": {41.5, 11.8, 88, 7831},
	"bench-gemma-12b-3w": {31.0, 10.1, 80, 7852},
	"bench-devstral-24b": {118, 7.9, 48, 12020},
	"bench-kimi-72b-3w":  {105, 3.1, 14, 26510},
	"qwen3.5-4b":         {29, 31.5, 210, 3343},
	"gemma-4-12b":        {74, 14.2, 96, 7514},
}

// The regression thresholds of the harness config (bench.regress_*_pct).
const (
	thLoadPct   = 15.0
	thDecodePct = 7.0
)

func rq(name, prompt string, max int, stream bool) map[string]any {
	return map[string]any{"name": name, "prompt": prompt, "max_tokens": float64(max), "stream": stream}
}

func ph(t string, kv ...any) map[string]any {
	m := map[string]any{"type": t}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func turns(l ...string) []any {
	var out []any
	for _, s := range l {
		out = append(out, map[string]any{"content": s})
	}
	return out
}

func seedBenchmarks(now time.Time) map[string]map[string]any {
	day := int64(24 * time.Hour / time.Millisecond)
	t := now.UnixMilli()
	list := []map[string]any{
		{"id": "qwen4b-simple", "name": "Qwen 4B cold start", "description": "The simple form: cold load, one warm-up, three requests three times, unload.",
			"profile": "qwen3.5-4b", "cold": true, "warmup_requests": 1.0, "repeats": 3.0, "unload_after": true,
			"requests": []any{rq("short", "Say hello in five words.", 32, false), rq("streamed", "Count from one to ten.", 64, true), rq("summary", "Explain what a load balancer does in two sentences.", 96, false)},
			"expect":   map[string]any{"outcome": "ready", "max_load_s": 60.0}, "tags": []any{"smoke"}, "source": "config", "created_at": t - 9*day, "updated_at": t - 9*day},
		{"id": "gemma-load-1w", "name": "Gemma 12B load, 1 worker", "description": "Cold load of the Gemma 12B file on one PS5, then unload. Load time, upload rate, bytes and memory.", "profile": "bench-gemma-12b-1w",
			"phases": []any{ph("load"), ph("unload")}, "timeout_s": 600.0, "expect": map[string]any{"outcome": "ready", "max_load_s": 120.0},
			"tags": []any{"load", "gemma"}, "source": "api", "created_at": t - 7*day, "updated_at": t - 7*day},
		{"id": "gemma-load-2w", "name": "Gemma 12B load, 2 workers", "description": "Cold load of the Gemma 12B file split across two PS5s, then unload.", "profile": "bench-gemma-12b-2w",
			"phases": []any{ph("load"), ph("unload")}, "timeout_s": 600.0, "expect": map[string]any{"outcome": "ready", "max_load_s": 90.0},
			"tags": []any{"load", "gemma"}, "source": "api", "created_at": t - 7*day, "updated_at": t - 7*day},
		{"id": "gemma-load-3w", "name": "Gemma 12B load, 3 workers", "description": "Cold load of the Gemma 12B file split across three PS5s, then unload.", "profile": "bench-gemma-12b-3w",
			"phases": []any{ph("load"), ph("unload")}, "timeout_s": 600.0, "expect": map[string]any{"outcome": "ready", "max_load_s": 75.0, "max_unload_s": 5.0},
			"tags": []any{"load", "gemma"}, "source": "api", "created_at": t - 7*day, "updated_at": t - 7*day},
		{"id": "gemma-conversation", "name": "Gemma 12B conversation", "description": "Cold load on two PS5s, then a five-turn conversation; the prompt cache grows each turn.",
			"profile": "bench-gemma-12b-2w",
			"phases": []any{ph("load"), ph("conversation", "label", "home lab chat", "system", "You are a concise assistant.", "turns", turns("I am planning a small home lab. What should I buy first?", "My budget is 600 euros. Adjust the list.",
				"Which of these parts can I buy used?", "Write the final list as a table with prices.", "Summarise the plan in three sentences."), "max_tokens", 160.0, "stream", true, "cache_prompt", true),
				ph("unload")}, "timeout_s": 900.0, "expect": map[string]any{"outcome": "ready", "min_decode_tps": 9.0, "min_reused_tokens": 100.0},
			"tags": []any{"conversation"}, "source": "api", "created_at": t - 6*day, "updated_at": t - 20*60*60*1000},
		{"id": "qwen-simultaneous", "name": "Qwen 4B, four at once", "description": "Cold load, then four identical requests at the same moment on one Qwen 4B.", "profile": "qwen3.5-4b",
			"phases":    []any{ph("load"), ph("concurrent_requests", "n", 4.0, "request", rq("haiku", "Write a haiku about load balancing.", 96, true)), ph("unload")},
			"timeout_s": 300.0, "expect": map[string]any{"outcome": "ready", "max_concurrent_wall_s": 30.0, "max_errors": 0.0}, "tags": []any{"concurrency"}, "source": "api", "created_at": t - 6*day, "updated_at": t - 6*day},
		{"id": "llama70b-refused", "name": "Llama 3.3 70B is refused", "description": "The 70B Q2 profile needs more than the cluster holds: the scheduler must refuse it at once.",
			"profile": "llama33-70b-q2", "phases": []any{ph("load")}, "timeout_s": 60.0, "expect": map[string]any{"outcome": "refused"},
			"tags": []any{"safety"}, "source": "api", "created_at": t - 6*day, "updated_at": t - 6*day},
		{"id": "kimi-72b-load-3w", "name": "Kimi-Dev 72B load, 3 workers", "description": "Cold load of the 72B IQ2_S file on three PS5s, then two requests, then unload.",
			"profile": "bench-kimi-72b-3w",
			"phases": []any{ph("load"), ph("requests", "requests", []any{rq("short", "Say hello in five words.", 32, false), rq("code", "Write a Go function that reverses a slice.", 160, true)}, "repeats", 1.0),
				ph("unload")}, "timeout_s": 1200.0, "expect": map[string]any{"outcome": "ready", "max_load_s": 180.0},
			"tags": []any{"load", "large"}, "source": "api", "created_at": t - 5*day, "updated_at": t - 5*day},
		{"id": "devstral-load-requests", "name": "Devstral 24B load and requests", "description": "Cold load of Devstral 24B on one PS5, three requests twice, 30 s idle and one more request, then unload.",
			"profile": "bench-devstral-24b",
			"phases": []any{ph("load"), ph("requests", "requests", []any{rq("short", "Say hello in five words.", 32, false),
				rq("refactor", "Refactor this loop into a map call: for i in range(10): out.append(i*i)", 128, true), rq("explain", "Explain a mutex to a new programmer.", 160, false)}, "repeats", 2.0, "stream", true),
				ph("hold", "seconds", 30.0), ph("unload")}, "timeout_s": 900.0, "expect": map[string]any{"outcome": "ready", "min_decode_tps": 6.5, "max_ttft_ms": 4000.0},
			"tags": []any{"load", "code"}, "source": "api", "created_at": t - 6*day, "updated_at": t - 3*day},
		{"id": "qwen-and-gemma", "name": "Qwen 4B and Gemma 12B together", "description": "A concurrent step: the Qwen 4B cold start and the one-worker Gemma load start at the same moment.",
			"parallel": []any{"qwen4b-simple", "gemma-load-1w"}, "tags": []any{"concurrency"}, "source": "api", "created_at": t - 4*day, "updated_at": t - 4*day},
	}
	out := map[string]map[string]any{}
	for _, b := range list {
		out[b["id"].(string)] = cloneMap(b)
	}
	return out
}

var nightlyOrder = []string{"qwen4b-simple", "gemma-load-1w", "gemma-load-3w", "gemma-conversation", "qwen-simultaneous", "llama70b-refused", "kimi-72b-load-3w", "devstral-load-requests"}

func strList(l []string) []any {
	var out []any
	for _, s := range l {
		out = append(out, s)
	}
	return out
}

func seedSuites(now time.Time) map[string]map[string]any {
	t := now.UnixMilli()
	day := int64(24 * time.Hour / time.Millisecond)
	list := []map[string]any{
		{"id": "nightly", "name": "Nightly", "description": "Every benchmark once, every night at 03:00.", "benchmarks": strList(nightlyOrder),
			"cooldown_s": 10.0, "only_when_idle": true, "idle_wait_s": 1800.0, "schedule": map[string]any{"at": "03:00"},
			"source": "api", "created_at": t - 8*day, "updated_at": t - 5*day},
		{"id": "load-scaling", "name": "Load scaling", "description": "Gemma 12B loaded on 1, 2 and 3 workers.", "benchmarks": strList([]string{"gemma-load-1w", "gemma-load-2w", "gemma-load-3w"}),
			"cooldown_s": 5.0, "only_when_idle": true, "idle_wait_s": 600.0, "source": "api", "created_at": t - 7*day, "updated_at": t - 7*day},
		{"id": "smoke", "name": "Smoke test", "description": "A quick check that loads and requests work.", "benchmarks": strList([]string{"qwen4b-simple", "gemma-load-1w"}),
			"stop_on_failure": true, "cooldown_s": 5.0, "only_when_idle": true, "idle_wait_s": 600.0, "source": "config", "created_at": t - 9*day, "updated_at": t - 9*day},
		{"id": "side-by-side", "name": "Side by side", "description": "Two models loading at the same moment, then the Gemma conversation.", "benchmarks": strList([]string{"qwen-and-gemma", "gemma-conversation"}),
			"cooldown_s": 5.0, "source": "api", "created_at": t - 4*day, "updated_at": t - 4*day},
	}
	out := map[string]map[string]any{}
	for _, s := range list {
		out[s["id"].(string)] = cloneMap(s)
	}
	return out
}

// ---- environment ----

// benchEnv is a run's environment: config revision and the rpc-server build
// of the PS5 workers (the build changed eight days ago in the seed).
func benchEnv(rev int, rpc string) *mEnv {
	e := &mEnv{SchedulerVersion: devVersion, ConfigRevision: fmt.Sprint(rev), LlamaBinary: "/opt/llama.cpp/build/bin/llama-server"}
	for _, n := range []nodeDef{ps5A, ps5B, ps5C} {
		e.Workers = append(e.Workers, mWorkerEnv{Node: n.Host, RPCServerVersion: rpc})
	}
	return e
}

const (
	rpcOld = "b6512 (rpc-server, ps5-port 0.4.1)"
	rpcNew = "b6604 (rpc-server, ps5-port 0.4.2)"
)

// ---- synthetic results ----

func summarize(vs []float64) *bench.Summary {
	if len(vs) == 0 {
		return nil
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	sum := 0.0
	for _, v := range s {
		sum += v
	}
	mean := sum / float64(len(s))
	sd := 0.0
	if len(s) > 1 {
		for _, v := range s {
			sd += (v - mean) * (v - mean)
		}
		sd = math.Sqrt(sd / float64(len(s)-1))
	}
	r := func(v float64) float64 { return math.Round(v*100) / 100 }
	return &bench.Summary{N: len(s), Mean: r(mean), Median: r(median(s)), Min: r(s[0]), Max: r(s[len(s)-1]), Stdev: r(sd)}
}

func median(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func pct(a, b float64) *float64 {
	if b == 0 {
		return nil
	}
	v := math.Round((a-b)/b*1000) / 10
	return &v
}

// statsOfRows is bench.statsOf.
func statsOfRows(rows []mRequest) *bench.Stats {
	st := &bench.Stats{}
	var d, p, tt, w []float64
	hashes := map[string]string{}
	for _, r := range rows {
		st.Requests++
		if r.Status != 200 {
			st.Failed++
			continue
		}
		if r.DecodeTPS > 0 {
			d = append(d, r.DecodeTPS)
		}
		if r.PromptTPS > 0 {
			p = append(p, r.PromptTPS)
		}
		if r.TTFTMS != nil {
			tt = append(tt, *r.TTFTMS)
		}
		w = append(w, r.WallMS)
		if r.Deterministic && r.OutputSHA256 != "" {
			hashes[r.Name] = r.OutputSHA256
		}
	}
	st.Decode, st.Prefill, st.TTFT, st.WallMS = summarize(d), summarize(p), summarize(tt), summarize(w)
	if len(hashes) > 0 {
		st.OutputHashes = hashes
	}
	return st
}

// computeStats is bench.ComputeStats: the measured rows, else conversation, hold, concurrent.
func computeStats(rows []mRequest) *bench.Stats {
	for _, kind := range []string{"measured", "conversation", "hold", "concurrent"} {
		var l []mRequest
		for _, r := range rows {
			if r.Phase == kind {
				l = append(l, r)
			}
		}
		if len(l) > 0 {
			return statsOfRows(l)
		}
	}
	return &bench.Stats{}
}

func outputHash(bm, name string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(bm+"/"+name)))
}

// newStep is a pending step of a run. Its phases appear as they start.
func (st *benchStore) newStep(bm map[string]any, idx int, group string) *mStep {
	st.stepSeq++
	prof := strOpt(bm, "profile")
	return &mStep{ID: st.stepSeq, Idx: idx, Group: group, BenchmarkID: strOpt(bm, "id"), Name: benchTitle(bm), Profile: prof,
		State: bench.StepPending, Spec: cloneMap(bm), desc: strOpt(bm, "description"), nodes: hostsOfProfile(prof)}
}

// leaves lists the benchmarks a suite item runs: itself, or the members of a parallel step.
func (st *benchStore) leaves(id string) (members []map[string]any, group string) {
	bm := st.benchmarks[id]
	if bm == nil {
		return nil, ""
	}
	if !isParallel(bm) {
		return []map[string]any{bm}, ""
	}
	for _, m := range asList(bm["parallel"]) {
		if x := st.benchmarks[fmt.Sprint(m)]; x != nil {
			members = append(members, x)
		}
	}
	return members, id
}

// newSteps is the pending steps of a suite.
func (st *benchStore) newSteps(su map[string]any) []*mStep {
	var out []*mStep
	for _, id := range asList(su["benchmarks"]) {
		ms, group := st.leaves(fmt.Sprint(id))
		for _, m := range ms {
			out = append(out, st.newStep(m, len(out), group))
		}
	}
	return out
}

// fillStep writes plausible results into a step that started at start.
// loadFactor > 1 makes the load slower (a regression). It returns the end time.
func (st *benchStore) fillStep(s *mStep, start time.Time, rnd *rand.Rand, loadFactor float64, baseline *mRun) time.Time {
	bm := s.Spec
	noise := func(v, rel float64) float64 { return v * (1 + rnd.NormFloat64()*rel) }
	base, ok := perf[s.Profile]
	if !ok {
		base = perfBase{60, 10, 80, 6000}
	}
	phases := expandPhases(bm)
	s.StartedAt, s.State, s.Phases, s.Requests = start.UnixMilli(), bench.StepDone, nil, nil
	s.ModelFile = modelFileOf(s.Profile)
	s.NodeCount = len(s.nodes)
	s.Plan, _ = jsonRaw(planJSON(planFor(s.Profile, newStatus(start).finishStatus(), start)))
	t := start.Add(time.Duration(300+rnd.Intn(300)) * time.Millisecond)
	refused := s.Profile == "llama33-70b-q2"
	decode, prefill := noise(base.decode, 0.025), noise(base.prefill, 0.03)
	prevPrompt, prevOut := 0, 0
	end := func(p *mPhase) {
		p.FinishedAt = t.UnixMilli()
		p.DurationS = math.Round(float64(p.FinishedAt-p.StartedAt)) / 1000
	}
	for pi, sp := range phases {
		p := mPhase{Idx: pi, Type: sp.Type, Label: sp.Label, State: "done", Outcome: "ok", StartedAt: t.UnixMilli()}
		if refused {
			t = t.Add(time.Duration(150+rnd.Intn(100)) * time.Millisecond)
			p.State, p.Outcome = "failed", bench.OutcomeRefused
			p.Error = "does not fit: profile llama33-70b-q2 needs 47800 MiB (fit rule requires 54970 MiB) but the cluster can hold at most 45795 MiB; nodes excluded by config and not counted: alienware-m15"
			end(&p)
			s.Phases = append(s.Phases, p)
			break
		}
		switch sp.Type {
		case "load":
			loadS := round1(noise(base.load, 0.03) * loadFactor)
			bytes := modelBytes[s.ModelFile]
			var nodes []bench.NodeLoad
			fr := fractions(mustProfile(s.Profile), len(s.nodes))
			for i, h := range s.nodes {
				b := int64(float64(bytes) * fr[i])
				m := round1(noise(base.mem, 0.002) * fr[i])
				nodes = append(nodes, bench.NodeLoad{Node: h, Bytes: &b, MemoryMiB: &m})
			}
			lr := &bench.LoadResult{SessionID: sessionID(t, int(s.ID%97)), Outcome: "ready", LoadS: loadS, Bytes: bytes,
				AvgMBs: round1(float64(bytes) / 1e6 / loadS), MemoryMiB: round1(noise(base.mem, 0.002)), Workers: addrsOf(s.nodes), Nodes: nodes, Source: "history"}
			p.Load = lr
			if s.Load == nil {
				s.Load = lr
			}
			t = t.Add(time.Duration(loadS * float64(time.Second)))
		case "hold":
			secs := float64(intOpt(sp.Opts, "seconds", 60))
			p.HeldS = f64(secs)
			yes := true
			p.LoadedAfterHold = &yes
			t = t.Add(time.Duration(secs * float64(time.Second)))
		case "unload":
			u := round1(0.3 + rnd.Float64()*0.6)
			p.Unload = &mUnload{Loaded: true, Gone: true, UnloadS: u}
			t = t.Add(time.Duration(u * float64(time.Second)))
		}
		var rows []mRequest
		concStart := t
		for k, rp := range phaseRequests(phases, pi) {
			st.reqSeq++
			promptN := 40 + len(rp.prompt)/3 + rnd.Intn(30)
			cache := 0
			if rp.kind == "conversation" {
				cache = prevPrompt + prevOut
				promptN = 30 + len(rp.prompt)/3 // processed now; the rest is reused
			}
			outN := int(float64(rp.max) * (0.7 + rnd.Float64()*0.3))
			dec := noise(decode, 0.02)
			if rp.kind == "concurrent" {
				dec = noise(decode*0.42, 0.04)
			}
			pre := noise(prefill, 0.03)
			promptMS := float64(promptN) / pre * 1000
			predMS := float64(outN) / dec * 1000
			ttft := promptMS + 45 + rnd.Float64()*20
			wall := ttft + predMS
			at := t
			var wait *float64
			if rp.kind == "concurrent" {
				at = concStart
				wait = f64(math.Round(float64(k) * 35 * (1 + rnd.Float64()*0.2)))
				wall += *wait
			}
			r := mRequest{RequestResult: bench.RequestResult{ID: st.reqSeq, Seq: len(s.Requests) + 1, Name: rp.name, Phase: rp.kind, Status: 200,
				WallMS: math.Round(wall), PromptN: promptN, PromptMS: math.Round(promptMS*10) / 10,
				PromptTPS: math.Round(pre*10) / 10, PredictedN: outN, PredictedMS: math.Round(predMS*10) / 10, DecodeTPS: math.Round(dec*100) / 100,
				CacheN: cache, OutputSHA256: outputHash(s.BenchmarkID, rp.name), OutputHead: "Sure. " + rp.prompt, Deterministic: true, StartedAt: at.UnixMilli()},
				PhaseIdx: pi, Turn: rp.turn, WaitMS: wait}
			if rp.stream {
				r.TTFTMS = f64(math.Round(ttft))
			}
			if len(r.OutputHead) > 120 {
				r.OutputHead = r.OutputHead[:120]
			}
			s.Requests = append(s.Requests, r)
			rows = append(rows, r)
			if rp.kind != "concurrent" {
				t = t.Add(time.Duration(r.WallMS) * time.Millisecond)
			}
			if rp.kind == "conversation" {
				prevPrompt, prevOut = promptN+cache, outN
			}
		}
		if len(rows) > 0 {
			p.Stats = statsOfRows(rows)
		}
		switch sp.Type {
		case "conversation":
			for _, r := range rows {
				p.Turns = append(p.Turns, mTurn{Turn: r.Turn, Name: r.Name, Status: r.Status, PromptTokens: r.PromptN + r.CacheN, PromptN: r.PromptN, CacheN: r.CacheN,
					PrefillTPS: r.PromptTPS, DecodeTPS: r.DecodeTPS, TTFTMS: r.TTFTMS, WallMS: r.WallMS, PredictedN: r.PredictedN})
			}
		case "concurrent_requests":
			c := &mConcurrent{N: len(rows)}
			for _, r := range rows {
				c.Items = append(c.Items, mConcItem{Index: r.Turn, Status: r.Status, WaitMS: r.WaitMS, WallMS: r.WallMS, DecodeTPS: r.DecodeTPS, PrefillTPS: r.PromptTPS})
				c.WallMS = math.Max(c.WallMS, r.WallMS)
			}
			p.Concurrent = c
			t = t.Add(time.Duration(c.WallMS) * time.Millisecond)
		}
		end(&p)
		s.Phases = append(s.Phases, p)
	}
	s.FinishedAt = t.UnixMilli()
	want := "ready"
	expect := asMap(bm["expect"])
	if o := strOpt(expect, "outcome"); o != "" {
		want = o
	}
	if refused {
		s.Outcome, s.HTTPStatus = bench.OutcomeRefused, 503
		s.Reason = s.Phases[0].Error
	} else {
		s.Outcome = bench.OutcomeReady
		s.LogFile = fmt.Sprintf("/var/log/llm-children/%s-%s-%d.log", start.UTC().Format("20060102T150405Z"), s.Profile, s.ID%90+3)
	}
	s.Stats = computeStats(s.Requests)
	s.Expect = expectOf(s, expect, want)
	if baseline != nil {
		s.Comparison = compareStep(s, baseline)
	}
	return t
}

// expectOf checks the step against expect (bench expectations, design 9.3).
func expectOf(s *mStep, e map[string]any, want string) *bench.ExpectResult {
	checks := []bench.Check{}
	if s.Outcome == want {
		checks = append(checks, bench.Check{Name: "outcome", Pass: true, Detail: "the scheduler answered as expected: " + want})
	} else {
		checks = append(checks, bench.Check{Name: "outcome", Pass: false, Detail: fmt.Sprintf("expected %s, got %s", want, s.Outcome)})
	}
	num := func(k string) (float64, bool) { v, ok := e[k].(float64); return v, ok }
	if v, ok := num("max_load_s"); ok && s.Load != nil {
		checks = append(checks, bench.Check{Name: "max_load_s", Pass: s.Load.LoadS <= v, Detail: fmt.Sprintf("load took %.1f s, limit %.1f s", s.Load.LoadS, v)})
	}
	if v, ok := num("min_decode_tps"); ok && s.Stats.Decode != nil {
		checks = append(checks, bench.Check{Name: "min_decode_tps", Pass: s.Stats.Decode.Mean >= v, Detail: fmt.Sprintf("decode %.2f tok/s, minimum %.1f", s.Stats.Decode.Mean, v)})
	}
	if v, ok := num("min_prefill_tps"); ok && s.Stats.Prefill != nil {
		checks = append(checks, bench.Check{Name: "min_prefill_tps", Pass: s.Stats.Prefill.Mean >= v, Detail: fmt.Sprintf("prefill %.1f tok/s, minimum %.1f", s.Stats.Prefill.Mean, v)})
	}
	if v, ok := num("max_ttft_ms"); ok && s.Stats.TTFT != nil {
		checks = append(checks, bench.Check{Name: "max_ttft_ms", Pass: s.Stats.TTFT.Mean <= v, Detail: fmt.Sprintf("time to first token %.0f ms, limit %.0f ms", s.Stats.TTFT.Mean, v)})
	}
	for _, p := range s.Phases {
		if v, ok := num("min_reused_tokens"); ok && p.Type == "conversation" && len(p.Turns) > 1 {
			least := p.Turns[1].CacheN
			for _, t := range p.Turns[1:] {
				least = min(least, t.CacheN)
			}
			checks = append(checks, bench.Check{Name: "min_reused_tokens", Pass: float64(least) >= v,
				Detail: fmt.Sprintf("the least a later turn reused was %d prompt tokens, minimum %.0f", least, v)})
		}
		if v, ok := num("max_concurrent_wall_s"); ok && p.Concurrent != nil {
			w := p.Concurrent.WallMS / 1000
			checks = append(checks, bench.Check{Name: "max_concurrent_wall_s", Pass: w <= v, Detail: fmt.Sprintf("the simultaneous requests took %.1f s, limit %.1f s", w, v)})
		}
		if v, ok := num("max_errors"); ok && p.Concurrent != nil {
			checks = append(checks, bench.Check{Name: "max_errors", Pass: float64(p.Concurrent.Errors) <= v, Detail: fmt.Sprintf("%d errors, at most %.0f", p.Concurrent.Errors, v)})
		}
		if v, ok := num("max_unload_s"); ok && p.Unload != nil {
			checks = append(checks, bench.Check{Name: "max_unload_s", Pass: p.Unload.UnloadS <= v, Detail: fmt.Sprintf("unload took %.1f s, limit %.1f s", p.Unload.UnloadS, v)})
		}
	}
	pass := true
	for _, c := range checks {
		pass = pass && c.Pass
	}
	return &bench.ExpectResult{Pass: pass, Checks: checks}
}

func mustProfile(id string) profileDef { p, _ := profileByID(id); return p }

func compareStep(s *mStep, baseline *mRun) *bench.Comparison {
	c := &bench.Comparison{BaselineRunID: baseline.ID}
	var b *mStep
	for _, x := range baseline.Steps {
		if x.BenchmarkID == s.BenchmarkID && x.Outcome == s.Outcome {
			b = x
		}
	}
	if b == nil {
		return c
	}
	c.Found = true
	if s.Load != nil && b.Load != nil {
		c.LoadPct = pct(s.Load.LoadS, b.Load.LoadS)
		if *c.LoadPct > thLoadPct {
			c.Regression = true
			c.Reasons = append(c.Reasons, fmt.Sprintf("load %.1f%% slower than the baseline (%.1f s -> %.1f s, threshold %.0f%%)", *c.LoadPct, b.Load.LoadS, s.Load.LoadS, thLoadPct))
		}
	}
	if s.Stats != nil && b.Stats != nil {
		if s.Stats.Decode != nil && b.Stats.Decode != nil {
			c.DecodePct = pct(s.Stats.Decode.Mean, b.Stats.Decode.Mean)
			if *c.DecodePct < -thDecodePct {
				c.Regression = true
				c.Reasons = append(c.Reasons, fmt.Sprintf("decode %.1f%% slower than the baseline (threshold %.0f%%)", -*c.DecodePct, thDecodePct))
			}
		}
		if s.Stats.Prefill != nil && b.Stats.Prefill != nil {
			c.PrefillPct = pct(s.Stats.Prefill.Mean, b.Stats.Prefill.Mean)
		}
		if s.Stats.TTFT != nil && b.Stats.TTFT != nil {
			c.TTFTPct = pct(s.Stats.TTFT.Mean, b.Stats.TTFT.Mean)
		}
		if len(s.Stats.OutputHashes) > 0 && len(b.Stats.OutputHashes) > 0 {
			f := !jsonEqual(s.Stats.OutputHashes, b.Stats.OutputHashes)
			c.HashDiffers = &f
		}
	}
	return c
}

func summaryOf(steps []*mStep) *bench.RunSummary {
	s := &bench.RunSummary{Steps: len(steps)}
	for _, st := range steps {
		if st.State == bench.StepSkipped || st.State == bench.StepPending {
			continue
		}
		if st.Expect != nil && st.Expect.Pass {
			s.Passed++
		} else {
			s.Failed++
		}
		if st.Comparison != nil && st.Comparison.Regression {
			s.Regressions++
		}
	}
	return s
}

// runID is newRunID: run-<UTC time>-<6 hex>.
func runID(t time.Time, suite string, n int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", suite, n, t.UnixNano())))
	return fmt.Sprintf("run-%s-%x", t.UTC().Format("20060102T150405"), h[:3])
}

// seed fills the store with benchmarks, suites and about 65 finished runs
// going back 26 days.
func (st *benchStore) seed(now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.benchmarks = seedBenchmarks(now)
	st.suites = seedSuites(now)
	st.runs = nil
	rnd := rand.New(rand.NewSource(7))
	type plan struct {
		at      time.Time
		suite   string
		special string
	}
	var plans []plan
	y, m, d := now.Date()
	night := time.Date(y, m, d, 3, 0, 0, 0, now.Location())
	if night.After(now.Add(-2 * time.Hour)) {
		night = night.Add(-24 * time.Hour)
	}
	for i := 25; i >= 0; i-- {
		p := plan{at: night.Add(-time.Duration(i) * 24 * time.Hour).Add(time.Duration(rnd.Intn(40)) * time.Second), suite: "nightly"}
		switch i {
		case 0:
			p.special = "regression"
		case 4:
			p.special = "failed"
		}
		plans = append(plans, p)
	}
	for i := 24; i >= 0; i-- {
		at := now.Add(-time.Duration(i)*26*time.Hour - 5*time.Hour - time.Duration(rnd.Intn(3600))*time.Second)
		p := plan{at: at, suite: "load-scaling"}
		if i == 2 {
			p.special = "cancelled"
		}
		plans = append(plans, p)
	}
	for i, ago := range []time.Duration{20 * 24 * time.Hour, 14 * 24 * time.Hour, 9 * 24 * time.Hour, 5*24*time.Hour + 7*time.Hour, 3*24*time.Hour + 2*time.Hour, 26 * time.Hour} {
		p := plan{at: now.Add(-ago), suite: "smoke"}
		if i == 4 {
			p.special = "skipped"
		}
		plans = append(plans, p)
	}
	for _, ago := range []time.Duration{3*24*time.Hour + 9*time.Hour, 2*24*time.Hour + 4*time.Hour, 30 * time.Hour, 7 * time.Hour} {
		plans = append(plans, plan{at: now.Add(-ago), suite: "side-by-side"})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].at.Before(plans[j].at) })
	for n, p := range plans {
		su := st.suites[p.suite]
		trigger := "manual"
		if p.suite == "nightly" {
			trigger = "schedule"
		}
		age := now.Sub(p.at)
		rpc, rev := rpcNew, 14
		switch {
		case age > 8*24*time.Hour:
			rpc, rev = rpcOld, 9
		case age > 4*24*time.Hour:
			rev = 12
		}
		run := &mRun{ID: runID(p.at, p.suite, n+1), SuiteID: p.suite, SuiteName: su["name"].(string), Trigger: trigger,
			QueuedAt: p.at.Add(-2 * time.Second).UnixMilli(), Version: devVersion, Suite: cloneMap(su), Env: benchEnv(rev, rpc)}
		base := st.lastCompleted(p.suite, p.at.UnixMilli())
		if base != nil {
			run.BaselineRunID = base.ID
		}
		run.Steps = st.newSteps(su)
		if p.special == "skipped" {
			run.State, run.StartedAt, run.FinishedAt = bench.RunSkipped, p.at.UnixMilli(), p.at.Add(10*time.Minute).UnixMilli()
			run.Error = "the scheduler was not idle for idle_wait_s (600 s): qwen3.8-27b was serving requests"
			for _, s := range run.Steps {
				s.State, s.Outcome = bench.StepSkipped, bench.OutcomeSkipped
			}
			run.Summary = summaryOf(run.Steps)
			st.runs = append(st.runs, run)
			continue
		}
		run.StartedAt = p.at.UnixMilli()
		t := p.at
		groupEnd := map[string]time.Time{}
		groupStart := map[string]time.Time{}
		for i, s := range run.Steps {
			stop := (p.special == "failed" && i >= 3) || (p.special == "cancelled" && i >= 1)
			if stop {
				state := bench.StepSkipped
				if i == 3 && p.special == "failed" || i == 1 && p.special == "cancelled" {
					state = bench.StepCancelled
				}
				s.State, s.Outcome = state, map[string]string{bench.StepSkipped: bench.OutcomeSkipped, bench.StepCancelled: bench.OutcomeCancelled}[state]
				if state == bench.StepCancelled {
					s.StartedAt, s.FinishedAt = t.UnixMilli(), t.Add(23*time.Second).UnixMilli()
					s.Phases = []mPhase{{Idx: 0, Type: "load", State: "cancelled", Outcome: bench.OutcomeCancelled, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt, DurationS: 23,
						Error: "cancelled"}}
					t = t.Add(23 * time.Second)
				}
				continue
			}
			factor := 1.0
			if p.special == "regression" && s.BenchmarkID == "kimi-72b-load-3w" {
				factor = 1.22
			}
			// a slow drift on the 3-worker gemma load: the series is not flat
			if s.BenchmarkID == "gemma-load-3w" {
				factor *= 1 + 0.004*float64(n%9)
			}
			if s.Group != "" { // members of a parallel step start together
				if _, ok := groupStart[s.Group]; !ok {
					groupStart[s.Group] = t
				}
				e := st.fillStep(s, groupStart[s.Group], rnd, factor, base)
				if e.After(groupEnd[s.Group]) {
					groupEnd[s.Group] = e
				}
				if i+1 == len(run.Steps) || run.Steps[i+1].Group != s.Group {
					t = groupEnd[s.Group].Add(5 * time.Second)
				}
				continue
			}
			t = st.fillStep(s, t, rnd, factor, base).Add(10 * time.Second)
		}
		run.Summary = summaryOf(run.Steps)
		run.FinishedAt = t.UnixMilli()
		switch p.special {
		case "failed":
			run.State, run.Error = bench.RunFailed, "failed (interrupted): the scheduler restarted during the run"
		case "cancelled":
			run.State, run.Note = bench.RunCancelled, "cancelled by jacob: needed the PS5s"
		default:
			run.State = bench.RunCompleted
		}
		st.runs = append(st.runs, run)
	}
	st.runSeq = len(st.runs)
}
