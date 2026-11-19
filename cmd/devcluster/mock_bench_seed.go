// DEV-ONLY MOCK of a server contract that is not built yet: the seeded
// benchmarks, suites and past runs of the benchmark mock, and the synthetic
// step results the mock engine also uses for the runs it plays.

package main

import (
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

func seedBenchmarks(now time.Time) map[string]map[string]any {
	day := int64(24 * time.Hour / time.Millisecond)
	t := now.UnixMilli()
	list := []map[string]any{
		{"id": "qwen4b-simple", "name": "Qwen 4B cold start", "description": "The design 9.3 simple form: cold load, one warmup, three requests three times.",
			"profile": "qwen3.5-4b", "cold": true, "warmup_requests": 1.0, "repeats": 3.0, "unload_after": true,
			"requests": []any{rq("short", "Say hello in five words.", 32, false), rq("streamed", "Count from one to ten.", 64, true), rq("summary", "Explain what a load balancer does in two sentences.", 96, false)},
			"expect":   map[string]any{"outcome": "ready", "max_load_s": 60.0}, "tags": []any{"smoke"}, "source": "config", "created_at": t - 9*day, "updated_at": t - 9*day},
		{"id": "gemma-load-1w", "name": "Gemma 12B load, 1 worker", "description": "Cold load of the Gemma 12B file on one PS5.", "profile": "bench-gemma-12b-1w",
			"phases": []any{ph("load", "cold", true), ph("unload")}, "timeout_s": 600.0, "expect": map[string]any{"outcome": "ready", "max_load_s": 120.0},
			"tags": []any{"load", "gemma"}, "source": "api", "created_at": t - 7*day, "updated_at": t - 7*day},
		{"id": "gemma-load-2w", "name": "Gemma 12B load, 2 workers", "description": "Cold load of the Gemma 12B file split across two PS5s.", "profile": "bench-gemma-12b-2w",
			"phases": []any{ph("load", "cold", true), ph("unload")}, "timeout_s": 600.0, "expect": map[string]any{"outcome": "ready", "max_load_s": 90.0},
			"tags": []any{"load", "gemma"}, "source": "api", "created_at": t - 7*day, "updated_at": t - 7*day},
		{"id": "gemma-load-3w", "name": "Gemma 12B load, 3 workers", "description": "Cold load of the Gemma 12B file split across three PS5s.", "profile": "bench-gemma-12b-3w",
			"phases": []any{ph("load", "cold", true), ph("unload")}, "timeout_s": 600.0, "expect": map[string]any{"outcome": "ready", "max_load_s": 75.0},
			"tags": []any{"load", "gemma"}, "source": "api", "created_at": t - 7*day, "updated_at": t - 7*day},
		{"id": "gemma-conversation", "name": "Gemma 12B conversation", "description": "Five-turn conversation on two PS5s; the prompt cache grows each turn.",
			"profile": "bench-gemma-12b-2w",
			"phases": []any{ph("load", "cold", true), ph("conversation", "turns", []any{"I am planning a small home lab. What should I buy first?", "My budget is 600 euros. Adjust the list.",
				"Which of these parts can I buy used?", "Write the final list as a table with prices.", "Summarise the plan in three sentences."}, "max_tokens", 160.0, "stream", true, "cache_prompt", true),
				ph("unload")}, "timeout_s": 900.0, "expect": map[string]any{"outcome": "ready", "min_decode_tps": 9.0},
			"tags": []any{"conversation"}, "source": "api", "created_at": t - 6*day, "updated_at": t - 20*60*60*1000},
		{"id": "qwen-simultaneous", "name": "Qwen 4B, four at once", "description": "Four simultaneous requests on one loaded Qwen 4B.", "profile": "qwen3.5-4b",
			"phases":    []any{ph("load", "cold", true), ph("concurrent_requests", "concurrency", 4.0, "prompt", "Write a haiku about load balancing.", "max_tokens", 96.0), ph("unload")},
			"timeout_s": 300.0, "expect": map[string]any{"outcome": "ready"}, "tags": []any{"concurrency"}, "source": "api", "created_at": t - 6*day, "updated_at": t - 6*day},
		{"id": "llama70b-refused", "name": "Llama 3.3 70B is refused", "description": "The 70B Q2 profile needs more than the cluster holds: the scheduler must refuse it at once.",
			"profile": "llama33-70b-q2", "phases": []any{ph("load", "cold", true)}, "timeout_s": 60.0, "expect": map[string]any{"outcome": "refused"},
			"tags": []any{"safety"}, "source": "api", "created_at": t - 6*day, "updated_at": t - 6*day},
		{"id": "kimi-72b-load-3w", "name": "Kimi-Dev 72B load, 3 workers", "description": "Cold load of the 72B IQ2_S file on three PS5s, then two requests.",
			"profile": "bench-kimi-72b-3w",
			"phases": []any{ph("load", "cold", true), ph("requests", "requests", []any{rq("short", "Say hello in five words.", 32, false), rq("code", "Write a Go function that reverses a slice.", 160, true)}, "repeats", 1.0),
				ph("unload")}, "timeout_s": 1200.0, "expect": map[string]any{"outcome": "ready", "max_load_s": 180.0},
			"tags": []any{"load", "large"}, "source": "api", "created_at": t - 5*day, "updated_at": t - 5*day},
		{"id": "devstral-load-requests", "name": "Devstral 24B load and requests", "description": "Cold load of Devstral 24B on one PS5, then three requests twice.",
			"profile": "bench-devstral-24b",
			"phases": []any{ph("load", "cold", true), ph("requests", "requests", []any{rq("short", "Say hello in five words.", 32, false),
				rq("refactor", "Refactor this loop into a map call: for i in range(10): out.append(i*i)", 128, true), rq("explain", "Explain a mutex to a new programmer.", 160, false)}, "repeats", 2.0),
				ph("unload")}, "timeout_s": 900.0, "expect": map[string]any{"outcome": "ready", "min_decode_tps": 6.5},
			"tags": []any{"load", "code"}, "source": "api", "created_at": t - 6*day, "updated_at": t - 3*day},
	}
	out := map[string]map[string]any{}
	for _, b := range list {
		out[b["id"].(string)] = cloneMap(b)
	}
	return out
}

var nightlyOrder = []string{"qwen4b-simple", "gemma-load-1w", "gemma-load-3w", "gemma-conversation", "qwen-simultaneous", "llama70b-refused", "kimi-72b-load-3w", "devstral-load-requests"}

func seedSuites(now time.Time) map[string]map[string]any {
	t := now.UnixMilli()
	day := int64(24 * time.Hour / time.Millisecond)
	ids := func(l []string) []any {
		var out []any
		for _, s := range l {
			out = append(out, s)
		}
		return out
	}
	list := []map[string]any{
		{"id": "nightly", "name": "Nightly", "description": "Every benchmark once, every night at 03:00.", "benchmarks": ids(nightlyOrder),
			"stop_on_failure": false, "cooldown_s": 10.0, "only_when_idle": true, "idle_wait_s": 1800.0, "schedule": map[string]any{"at": "03:00"},
			"source": "api", "created_at": t - 8*day, "updated_at": t - 5*day},
		{"id": "load-scaling", "name": "Load scaling", "description": "Gemma 12B loaded on 1, 2 and 3 workers.", "benchmarks": ids([]string{"gemma-load-1w", "gemma-load-2w", "gemma-load-3w"}),
			"stop_on_failure": false, "cooldown_s": 5.0, "only_when_idle": true, "idle_wait_s": 600.0, "source": "api", "created_at": t - 7*day, "updated_at": t - 7*day},
		{"id": "smoke", "name": "Smoke test", "description": "A quick check that loads and requests work.", "benchmarks": ids([]string{"qwen4b-simple", "gemma-load-1w"}),
			"stop_on_failure": true, "cooldown_s": 5.0, "only_when_idle": true, "idle_wait_s": 600.0, "source": "config", "created_at": t - 9*day, "updated_at": t - 9*day},
	}
	out := map[string]map[string]any{}
	for _, s := range list {
		out[s["id"].(string)] = cloneMap(s)
	}
	return out
}

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
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	r := func(v float64) float64 { return math.Round(v*100) / 100 }
	return &bench.Summary{N: len(s), Mean: r(mean), Median: r(med), Min: r(s[0]), Max: r(s[len(s)-1]), Stdev: r(sd)}
}

func pct(a, b float64) *float64 {
	if b == 0 {
		return nil
	}
	v := math.Round((a-b)/b*1000) / 10
	return &v
}

// stepSpec is what a step needs to know about its benchmark.
type stepSpec struct {
	bm      map[string]any
	id      string
	profile string
}

func specOf(bm map[string]any) stepSpec {
	id, _ := bm["id"].(string)
	p, _ := bm["profile"].(string)
	return stepSpec{bm: bm, id: id, profile: p}
}

func (s stepSpec) title() string {
	if n, _ := s.bm["name"].(string); n != "" {
		return n
	}
	return s.id
}

// newStep is a pending step of a run.
func (st *benchStore) newStep(sp stepSpec, idx int) *mStep {
	st.stepSeq++
	ps, _ := phasesOf(sp.bm)
	var phases []mPhase
	for i, p := range ps {
		phases = append(phases, mPhase{Index: i, Type: p.Type, Label: phaseLabel(p), State: "pending"})
	}
	desc, _ := sp.bm["description"].(string)
	return &mStep{ID: st.stepSeq, Idx: idx, BenchmarkID: sp.id, Name: sp.title(), Description: desc, Profile: sp.profile,
		ModelFile: modelFileOf(sp.profile), Nodes: hostsOfProfile(sp.profile), State: bench.StepPending, Spec: cloneMap(sp.bm), Phases: phases}
}

// fillStep writes plausible results into a step that started at start.
// loadFactor > 1 makes the load slower (a regression). It returns the end time.
func (st *benchStore) fillStep(s *mStep, sp stepSpec, start time.Time, rnd *rand.Rand, loadFactor float64, baseline *mRun) time.Time {
	noise := func(v, rel float64) float64 { return v * (1 + rnd.NormFloat64()*rel) }
	base, ok := perf[sp.profile]
	if !ok {
		base = perfBase{60, 10, 80, 6000}
	}
	ps, simple := phasesOf(sp.bm)
	s.StartedAt = start.UnixMilli()
	s.State = bench.StepDone
	t := start.Add(time.Duration(800+rnd.Intn(400)) * time.Millisecond) // plan + unload of a cold step
	refused := sp.profile == "llama33-70b-q2"
	planDoc := planJSON(planFor(sp.profile, newStatus(start).finishStatus(), start))
	s.Plan, _ = jsonRaw(planDoc)
	decode, prefill := noise(base.decode, 0.025), noise(base.prefill, 0.03)
	var measured []mRequest
	prevPrompt, prevOut := 0, 0
	for pi, p := range ps {
		ph := &s.Phases[pi]
		ph.StartedAt, ph.State = t.UnixMilli(), "done"
		if refused {
			if p.Type == "load" {
				t = t.Add(time.Duration(150+rnd.Intn(100)) * time.Millisecond)
			} else {
				ph.State, ph.StartedAt = "skipped", 0
				continue
			}
		}
		if p.Type == "load" && !refused {
			loadS := round1(noise(base.load, 0.03) * loadFactor)
			bytes := modelBytes[s.ModelFile]
			var nodes []bench.NodeLoad
			fr := fractions(mustProfile(sp.profile), len(s.Nodes))
			for i, h := range s.Nodes {
				b := int64(float64(bytes) * fr[i])
				m := round1(noise(base.mem, 0.002) * fr[i])
				nodes = append(nodes, bench.NodeLoad{Node: h, Bytes: &b, MemoryMiB: &m})
			}
			s.Load = &bench.LoadResult{SessionID: sessionID(t, int(s.ID%97)), Outcome: "ready", LoadS: loadS, Bytes: bytes,
				AvgMBs: round1(float64(bytes) / 1e6 / loadS), MemoryMiB: round1(noise(base.mem, 0.002)), Workers: addrsOf(s.Nodes), Nodes: nodes, Source: "history"}
			t = t.Add(time.Duration(loadS * float64(time.Second)))
		}
		for _, rp := range requestsOf(sp.bm, p, simple) {
			if refused {
				break
			}
			st.reqSeq++
			promptN := 40 + len(rp.prompt)/3 + rnd.Intn(30)
			cache := 0
			if p.Type == "conversation" {
				promptN = prevPrompt + prevOut + 30 + len(rp.prompt)/3
				cache = prevPrompt + prevOut
			}
			outN := int(float64(rp.max) * (0.7 + rnd.Float64()*0.3))
			dec := noise(decode, 0.02)
			if p.Type == "concurrent_requests" {
				dec = noise(decode*0.42, 0.04)
			}
			pre := noise(prefill, 0.03)
			promptMS := float64(promptN-cache) / pre * 1000
			predMS := float64(outN) / dec * 1000
			ttft := promptMS + 45 + rnd.Float64()*20
			wall := ttft + predMS
			phase := p.Type
			if simple {
				phase = map[string]string{"load": "load", "warmup": "warmup", "measured": "measured"}[p.Type]
			}
			r := mRequest{RequestResult: bench.RequestResult{ID: st.reqSeq, Seq: len(s.Requests) + 1, Name: rp.name, Phase: phase, Status: 200,
				WallMS: math.Round(wall), TTFTMS: f64(math.Round(ttft)), PromptN: promptN, PromptMS: math.Round(promptMS*10) / 10,
				PromptTPS: math.Round(pre*10) / 10, PredictedN: outN, PredictedMS: math.Round(predMS*10) / 10, DecodeTPS: math.Round(dec*100) / 100,
				CacheN: cache, OutputSHA256: fmt.Sprintf("%064x", rnd.Uint64()), OutputHead: "Sure. " + rp.prompt, Deterministic: true, StartedAt: t.UnixMilli()},
				PhaseIndex: pi, Turn: rp.turn}
			if p.Type == "load" && simple {
				r.WallMS += s.Load.LoadS * 1000 // the first request of a simple benchmark carries the load
			}
			s.Requests = append(s.Requests, r)
			if p.Type == "concurrent_requests" {
				continue // all at once: time advances once below
			}
			t = t.Add(time.Duration(r.WallMS) * time.Millisecond)
			if p.Type != "warmup" && p.Type != "load" {
				measured = append(measured, r)
			}
			prevPrompt, prevOut = promptN, outN
		}
		if p.Type == "concurrent_requests" {
			longest := 0.0
			for _, r := range s.Requests {
				if r.PhaseIndex == pi {
					measured = append(measured, r)
					longest = math.Max(longest, r.WallMS)
				}
			}
			t = t.Add(time.Duration(longest) * time.Millisecond)
		}
		if p.Type == "hold" {
			t = t.Add(time.Duration(intOpt(p.Opts, "idle_s", 60)) * time.Second)
		}
		if p.Type == "unload" {
			t = t.Add(time.Duration(300+rnd.Intn(300)) * time.Millisecond)
		}
		ph.FinishedAt = t.UnixMilli()
		ph.WallMS = float64(ph.FinishedAt - ph.StartedAt)
	}
	s.FinishedAt = t.UnixMilli()
	want := "ready"
	if e, ok := sp.bm["expect"].(map[string]any); ok {
		if o, ok := e["outcome"].(string); ok {
			want = o
		}
	}
	if refused {
		s.Outcome, s.HTTPStatus = bench.OutcomeRefused, 503
		s.Reason = "does not fit: profile llama33-70b-q2 needs 47800 MiB (fit rule requires 54970 MiB) but the cluster can hold at most 45795 MiB; nodes excluded by config and not counted: alienware-m15"
	} else {
		s.Outcome = bench.OutcomeReady
		s.LogFile = fmt.Sprintf("/var/log/llm-children/%s-%s-%d.log", start.UTC().Format("20060102T150405Z"), sp.profile, s.ID%90+3)
	}
	if len(measured) > 0 {
		var d, p, tt, w []float64
		for _, r := range measured {
			d, p, w = append(d, r.DecodeTPS), append(p, r.PromptTPS), append(w, r.WallMS)
			if r.TTFTMS != nil {
				tt = append(tt, *r.TTFTMS)
			}
		}
		s.Stats = &bench.Stats{Requests: len(measured), Decode: summarize(d), Prefill: summarize(p), TTFT: summarize(tt), WallMS: summarize(w)}
	}
	checks := []bench.Check{{Name: "outcome", Pass: s.Outcome == want, Detail: fmt.Sprintf("%s (expected %s)", s.Outcome, want)}}
	if e, ok := sp.bm["expect"].(map[string]any); ok {
		if v, ok := e["max_load_s"].(float64); ok && s.Load != nil {
			checks = append(checks, bench.Check{Name: "max_load_s", Pass: s.Load.LoadS <= v, Detail: fmt.Sprintf("load %.1f s, limit %.0f s", s.Load.LoadS, v)})
		}
		if v, ok := e["min_decode_tps"].(float64); ok && s.Stats != nil && s.Stats.Decode != nil {
			checks = append(checks, bench.Check{Name: "min_decode_tps", Pass: s.Stats.Decode.Mean >= v, Detail: fmt.Sprintf("decode %.2f tok/s, floor %.1f", s.Stats.Decode.Mean, v)})
		}
	}
	pass := true
	for _, c := range checks {
		pass = pass && c.Pass
	}
	s.Expect = &bench.ExpectResult{Pass: pass, Checks: checks}
	if baseline != nil {
		s.Comparison = compareStep(s, baseline)
	}
	return t
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
		if *c.LoadPct > 15 {
			c.Regression = true
			c.Reasons = append(c.Reasons, fmt.Sprintf("load %.1f%% slower than the baseline (%.1f s -> %.1f s, threshold 15%%)", *c.LoadPct, b.Load.LoadS, s.Load.LoadS))
		}
	}
	if s.Stats != nil && b.Stats != nil {
		if s.Stats.Decode != nil && b.Stats.Decode != nil {
			c.DecodePct = pct(s.Stats.Decode.Mean, b.Stats.Decode.Mean)
			if *c.DecodePct < -7 {
				c.Regression = true
				c.Reasons = append(c.Reasons, fmt.Sprintf("decode %.1f%% slower than the baseline (threshold 7%%)", -*c.DecodePct))
			}
		}
		if s.Stats.Prefill != nil && b.Stats.Prefill != nil {
			c.PrefillPct = pct(s.Stats.Prefill.Mean, b.Stats.Prefill.Mean)
		}
		if s.Stats.TTFT != nil && b.Stats.TTFT != nil {
			c.TTFTPct = pct(s.Stats.TTFT.Mean, b.Stats.TTFT.Mean)
		}
		f := false
		c.HashDiffers = &f
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

func runID(t time.Time, suite string, n int) string {
	return fmt.Sprintf("run-%s-%s-%d", t.UTC().Format("20060102T1504"), suite, n)
}

// seed fills the store with benchmarks, suites and about 60 finished runs
// going back 26 days (about 12 of them in the last 6 days).
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
	sort.Slice(plans, func(i, j int) bool { return plans[i].at.Before(plans[j].at) })
	for n, p := range plans {
		su := st.suites[p.suite]
		trigger := "manual"
		if p.suite == "nightly" {
			trigger = "schedule"
		}
		run := &mRun{ID: runID(p.at, p.suite, n+1), SuiteID: p.suite, SuiteName: su["name"].(string), Trigger: trigger,
			QueuedAt: p.at.Add(-2 * time.Second).UnixMilli(), Version: "0.1.0", Suite: cloneMap(su)}
		base := st.lastCompleted(p.suite, p.at.UnixMilli())
		if base != nil {
			run.BaselineRunID = base.ID
		}
		if p.special == "skipped" {
			run.State, run.StartedAt, run.FinishedAt = bench.RunSkipped, p.at.UnixMilli(), p.at.Add(10*time.Minute).UnixMilli()
			run.Error = "the scheduler was not idle for idle_wait_s (600 s): qwen3.8-27b was serving requests"
			for i, id := range asList(su["benchmarks"]) {
				s := st.newStep(specOf(st.benchmarks[id.(string)]), i)
				s.State, s.Outcome = bench.StepSkipped, bench.OutcomeSkipped
				for k := range s.Phases {
					s.Phases[k].State = "skipped"
				}
				run.Steps = append(run.Steps, s)
			}
			run.Summary = summaryOf(run.Steps)
			st.runs = append(st.runs, run)
			continue
		}
		run.StartedAt = p.at.UnixMilli()
		t := p.at
		for i, id := range asList(su["benchmarks"]) {
			sp := specOf(st.benchmarks[id.(string)])
			s := st.newStep(sp, i)
			run.Steps = append(run.Steps, s)
			stop := (p.special == "failed" && i >= 3) || (p.special == "cancelled" && i >= 1)
			if stop {
				state := bench.StepSkipped
				if i == 3 && p.special == "failed" || i == 1 && p.special == "cancelled" {
					state = bench.StepCancelled
				}
				s.State, s.Outcome = state, map[string]string{bench.StepSkipped: bench.OutcomeSkipped, bench.StepCancelled: bench.OutcomeCancelled}[state]
				for k := range s.Phases {
					s.Phases[k].State = "skipped"
				}
				if state == bench.StepCancelled {
					s.StartedAt, s.FinishedAt = t.UnixMilli(), t.Add(23*time.Second).UnixMilli()
					s.Phases[0].State, s.Phases[0].StartedAt, s.Phases[0].FinishedAt = "cancelled", s.StartedAt, s.FinishedAt
					s.Phases[0].WallMS = 23000
					t = t.Add(23 * time.Second)
				}
				continue
			}
			factor := 1.0
			if p.special == "regression" && sp.id == "kimi-72b-load-3w" {
				factor = 1.22
			}
			// a slow drift on the 3-worker gemma load: the series is not flat
			if sp.id == "gemma-load-3w" {
				factor *= 1 + 0.004*float64(n%9)
			}
			t = st.fillStep(s, sp, t, rnd, factor, base).Add(10 * time.Second)
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
