package main

// The startup seed, all through the real server:
//
//   - seedConfig: about fifteen configuration writes after the import, through
//     the REST handlers (as the UI with X-Source: ui and X-Actor: alex, as curl
//     with X-Actor: curl) and through MCP, so the revisions and the audit log
//     hold every source (import, ui, rest, mcp);
//
//   - seedBenchDefs: the benchmarks and suites, created through the REST API
//     and MCP;
//
//   - seedBenchRuns: twelve past runs over six days with one regression,
//     executed by a second REAL benchmark manager on the real database against
//     the scripted scheduler (bench_scripted.go) on a clock set in the past.
//     Nothing is written to the database by hand; aggregates and trends are
//     the real ones computed from these runs.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
)

// seedConfig makes the configuration history.
func seedConfig(c *cluster, log *slog.Logger) error {
	s := newSeedClient(c.url)
	agent, err := newMCP(c.url, "claude-code", "2.1.4")
	if err != nil {
		return err
	}
	var errs []error
	step := func(what string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}
	put := func(path string, w who, body any) error { _, err := s.do("PUT", path, w, body); return err }

	_, err = s.do("PATCH", "/api/config/settings", asAlex.say("Keep idle models ten minutes: reloading the 27B costs more than it saves"), map[string]any{"ttl": "600s"})
	step("settings ttl", err)

	if p, err := s.do("GET", "/api/config/profiles/gemma-4-12b", asAlex, nil); err == nil {
		prof, _ := p["profile"].(map[string]any)
		prof["needs_mib"] = 8100
		step("gemma needs_mib", put("/api/config/profiles/gemma-4-12b", asAlex.say("Measured 8.1 GB after the llama.cpp update"), map[string]any{"profile": prof}))
	} else {
		step("get gemma", err)
	}

	_, err = s.do("POST", "/api/config/profiles", asCurl.say("32k context variant for long documents"),
		map[string]any{"id": "qwen3.5-4b:32k", "model": fileQwen4, "ctx": 32768, "needs_mib": 4600, "kv_quant": "q8_0", "flash_attn": true})
	step("create qwen 32k", err)

	if t, err := s.do("GET", "/api/config/tiers", asAlex, nil); err == nil {
		tiers, _ := t["tiers"].([]any)
		if len(tiers) > 0 {
			tiers[0].(map[string]any)["link_mb_s"] = 112
		}
		step("tiers", put("/api/config/tiers", asAlex.say("PS5 link speed measured after the move to the 2.5 GbE switch"), map[string]any{"tiers": tiers}))
	} else {
		step("get tiers", err)
	}

	_, err = agent.call("create_profile", map[string]any{"profile": map[string]any{"id": "scratch-qwen-8k", "model": fileQwen4, "ctx": 8192, "needs_mib": 3300},
		"comment": "temporary profile for a context-length experiment", "actor": "ctx sweep"})
	step("mcp create scratch", err)

	step("exclude test consoles", put("/api/config/exclude-nodes", asAlex.say("a test console is on the bench this week; keep every ps5-test-* out of the pool"),
		map[string]any{"exclude_nodes": []string{"alienware-m15", "ps5-test-*"}}))

	_, err = s.do("PATCH", "/api/config/settings", asAlex.say("Queue longer: the nightly benchmarks hold the PS5s for a while"),
		map[string]any{"queue_timeout": "900s", "drain_after": "45s"})
	step("settings queue", err)

	_, err = agent.call("update_settings", map[string]any{"settings": map[string]any{"load_stall_timeout": "240s"},
		"comment": "the 72B load stalls for up to 200 s on a busy link before it continues"})
	step("mcp settings", err)

	_, err = agent.call("delete_profile", map[string]any{"id": "scratch-qwen-8k", "comment": "experiment finished", "actor": "ctx sweep"})
	step("mcp delete scratch", err)

	_, err = s.do("PATCH", "/api/config/recovery", asCurl.say("fewer restarts: one recovery request per node every 15 minutes"), map[string]any{"min_interval": "900s"})
	step("recovery", err)

	step("node overrides", put("/api/config/node-overrides", asAlex.say("pin the Alienware's rpc port while it is on the bench"),
		map[string]any{"node_overrides": map[string]any{"alienware-m15": map[string]any{"rpc": "10.0.0.3:50053"}}}))

	step("exclude back", put("/api/config/exclude-nodes", asCurl.say("test console returned"),
		map[string]any{"exclude_nodes": []string{"alienware-m15"}}))

	_, err = s.do("POST", "/api/config/profiles", asAlex.say("Devstral with a 16k context for longer diffs"),
		map[string]any{"id": "devstral-24b:16k", "model": fileDevstral, "ctx": 16384, "needs_mib": 13300, "kv_quant": "q8_0", "flash_attn": true})
	step("create devstral 16k", err)

	_, err = s.do("DELETE", "/api/config/profiles/qwen3.5-4b:32k", asCurl.say("never used; the 4B is fine at 16k"), nil)
	step("delete qwen 32k", err)

	_, err = agent.call("update_settings", map[string]any{"settings": map[string]any{"bench_regress_decode_pct": 7.5}, "actor": "nightly report"})
	step("mcp bench setting", err)

	if len(errs) > 0 {
		return fmt.Errorf("%d of the config seed writes failed, first: %w", len(errs), errs[0])
	}
	return nil
}

// ---- benchmarks and suites ----

func rq(name, prompt string, max int, stream bool) map[string]any {
	return map[string]any{"name": name, "prompt": prompt, "max_tokens": max, "stream": stream}
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

// seedBenchmarks are the benchmark definitions, in creation order, with who creates each.
func seedBenchmarks() []struct {
	by   string // ui, rest or mcp
	spec map[string]any
} {
	type e = struct {
		by   string
		spec map[string]any
	}
	return []e{
		{"ui", map[string]any{"id": "qwen4b-simple", "name": "Qwen 4B cold start", "description": "The simple form: cold load, one warm-up, three requests three times, unload.",
			"profile": "qwen3.5-4b", "cold": true, "warmup_requests": 1, "repeats": 3, "unload_after": true,
			"requests": []any{rq("short", "Say hello in five words.", 32, false), rq("streamed", "Count from one to ten.", 64, true), rq("summary", "Explain what a load balancer does in two sentences.", 96, false)},
			"expect":   map[string]any{"outcome": "ready", "max_load_s": 60}, "tags": []any{"smoke"}}},
		{"ui", map[string]any{"id": "gemma-load-1w", "name": "Gemma 12B load, 1 worker", "description": "Cold load of the Gemma 12B file on one PS5, then unload. Load time, upload rate, bytes and memory.",
			"profile": "bench-gemma-12b-1w", "phases": []any{ph("load"), ph("unload")}, "timeout_s": 600, "expect": map[string]any{"outcome": "ready", "max_load_s": 120},
			"tags": []any{"load", "gemma"}}},
		{"rest", map[string]any{"id": "gemma-load-2w", "name": "Gemma 12B load, 2 workers", "description": "Cold load of the Gemma 12B file split across two PS5s, then unload.",
			"profile": "bench-gemma-12b-2w", "phases": []any{ph("load"), ph("unload")}, "timeout_s": 600, "expect": map[string]any{"outcome": "ready", "max_load_s": 90},
			"tags": []any{"load", "gemma"}}},
		{"ui", map[string]any{"id": "gemma-load-3w", "name": "Gemma 12B load, 3 workers", "description": "Cold load of the Gemma 12B file split across three PS5s, then unload.",
			"profile": "bench-gemma-12b-3w", "phases": []any{ph("load"), ph("unload")}, "timeout_s": 600, "expect": map[string]any{"outcome": "ready", "max_load_s": 75, "max_unload_s": 5},
			"tags": []any{"load", "gemma"}}},
		{"ui", map[string]any{"id": "gemma-conversation", "name": "Gemma 12B conversation", "description": "Cold load on two PS5s, then a five-turn conversation; the prompt cache grows each turn.",
			"profile": "bench-gemma-12b-2w",
			"phases": []any{ph("load"), ph("conversation", "label", "home lab chat", "system", "You are a concise assistant.", "turns", turns("I am planning a small home lab. What should I buy first?", "My budget is 600 euros. Adjust the list.",
				"Which of these parts can I buy used?", "Write the final list as a table with prices.", "Summarise the plan in three sentences."), "max_tokens", 160, "stream", true, "cache_prompt", true),
				ph("unload")}, "timeout_s": 900, "expect": map[string]any{"outcome": "ready", "min_decode_tps": 9, "min_reused_tokens": 100},
			"tags": []any{"conversation"}}},
		{"mcp", map[string]any{"id": "qwen-simultaneous", "name": "Qwen 4B, four at once", "description": "Cold load, then four identical requests at the same moment on one Qwen 4B.", "profile": "qwen3.5-4b",
			"phases":    []any{ph("load"), ph("concurrent_requests", "n", 4, "request", rq("haiku", "Write a haiku about load balancing.", 64, true)), ph("unload")},
			"timeout_s": 300, "expect": map[string]any{"outcome": "ready", "max_concurrent_wall_s": 30, "max_errors": 0}, "tags": []any{"concurrency"}}},
		{"mcp", map[string]any{"id": "llama70b-refused", "name": "Llama 3.3 70B is refused", "description": "The 70B Q2 profile needs more than the cluster holds: the scheduler must refuse it at once.",
			"profile": "llama33-70b-q2", "phases": []any{ph("load")}, "timeout_s": 60, "expect": map[string]any{"outcome": "refused"}, "tags": []any{"safety"}}},
		{"ui", map[string]any{"id": "kimi-72b-load-3w", "name": "Kimi-Dev 72B load, 3 workers", "description": "Cold load of the 72B IQ2_S file on three PS5s, then two requests, then unload.",
			"profile": "bench-kimi-72b-3w",
			"phases": []any{ph("load"), ph("requests", "requests", []any{rq("short", "Say hello in five words.", 32, false), rq("code", "Write a Go function that reverses a slice.", 160, true)}, "repeats", 1),
				ph("unload")}, "timeout_s": 1200, "expect": map[string]any{"outcome": "ready", "max_load_s": 180}, "tags": []any{"load", "large"}}},
		{"ui", map[string]any{"id": "devstral-load-requests", "name": "Devstral 24B load and requests", "description": "Cold load of Devstral 24B on one PS5, three requests twice, then unload.",
			"profile": "bench-devstral-24b",
			"phases": []any{ph("load"), ph("requests", "requests", []any{rq("short", "Say hello in five words.", 32, false),
				rq("refactor", "Refactor this loop into a map call: for i in range(10): out.append(i*i)", 128, true), rq("explain", "Explain a mutex to a new programmer.", 160, false)}, "repeats", 2, "stream", true),
				ph("unload")}, "timeout_s": 900, "expect": map[string]any{"outcome": "ready", "min_decode_tps": 6.5, "max_ttft_ms": 4000}, "tags": []any{"load", "code"}}},
		{"ui", map[string]any{"id": "gemma-idle-hold", "name": "Gemma 12B after two idle minutes", "description": "Load on one PS5, one request, two minutes idle, one more request: is the model still loaded and as fast?",
			"profile":   "bench-gemma-12b-1w",
			"phases":    []any{ph("load"), ph("requests", "requests", []any{rq("short", "Say hello in five words.", 32, false)}), ph("hold", "seconds", 120, "request", rq("after idle", "Say hello in five words.", 32, true)), ph("unload")},
			"timeout_s": 600, "expect": map[string]any{"outcome": "ready", "min_decode_tps": 12}, "tags": []any{"idle"}}},
		{"rest", map[string]any{"id": "tour-every-phase", "name": "Every phase type, quickly", "description": "Qwen 4B through each phase type once: load, warm-up, requests, a two-turn conversation, a 10 s hold, three at once, unload. About a minute.",
			"profile": "qwen3.5-4b",
			"phases": []any{ph("load"), ph("warmup", "count", 1), ph("requests", "requests", []any{rq("short", "Say hello in five words.", 32, true), rq("list", "Name three sorting algorithms.", 48, true)}),
				ph("conversation", "turns", turns("Pick a name for a home server.", "Now a shorter one."), "max_tokens", 32, "stream", true, "cache_prompt", true),
				ph("hold", "seconds", 10), ph("concurrent_requests", "n", 3, "request", rq("haiku", "Write a haiku about load balancing.", 32, true)), ph("unload")},
			"timeout_s": 300, "expect": map[string]any{"outcome": "ready"}, "tags": []any{"tour"}}},
		{"ui", map[string]any{"id": "qwen-and-gemma", "name": "Qwen 4B and Gemma 12B together", "description": "A concurrent step: the Qwen 4B cold start and the one-worker Gemma load start at the same moment.",
			"parallel": []any{"qwen4b-simple", "gemma-load-1w"}, "tags": []any{"concurrency"}}},
	}
}

var nightlyOrder = []any{"qwen4b-simple", "gemma-load-1w", "gemma-load-3w", "gemma-conversation", "qwen-simultaneous", "llama70b-refused", "kimi-72b-load-3w", "devstral-load-requests"}

// seedSuites are the suites as created: cooldown 0 and no schedule, so the
// seed runs go at once; seedBenchFinish then gives them their real
// cooldowns and the nightly schedule (more audited writes).
func seedSuites() []struct {
	by   string
	spec map[string]any
} {
	type e = struct {
		by   string
		spec map[string]any
	}
	return []e{
		{"ui", map[string]any{"id": "nightly", "name": "Nightly", "description": "Every benchmark once, every night at 03:00.", "benchmarks": nightlyOrder,
			"cooldown_s": 0, "only_when_idle": true, "idle_wait_s": 1800}},
		{"ui", map[string]any{"id": "load-scaling", "name": "Load scaling", "description": "Gemma 12B loaded on 1, 2 and 3 workers.", "benchmarks": []any{"gemma-load-1w", "gemma-load-2w", "gemma-load-3w"},
			"cooldown_s": 0, "only_when_idle": true, "idle_wait_s": 600}},
		{"rest", map[string]any{"id": "smoke", "name": "Smoke test", "description": "A quick check that loads and requests work.", "benchmarks": []any{"qwen4b-simple", "gemma-load-1w"},
			"stop_on_failure": true, "cooldown_s": 0, "only_when_idle": true, "idle_wait_s": 600}},
		{"mcp", map[string]any{"id": "side-by-side", "name": "Side by side", "description": "Two models loading at the same moment, then the Gemma conversation.", "benchmarks": []any{"qwen-and-gemma", "gemma-conversation"},
			"cooldown_s": 0}},
		{"ui", map[string]any{"id": "phase-tour", "name": "Phase tour", "description": "Every phase type once on Qwen 4B, the two-worker Gemma load, then the five-turn Gemma conversation: a run of about a minute.",
			"benchmarks": []any{"tour-every-phase", "gemma-load-2w", "gemma-conversation"}, "cooldown_s": 3, "only_when_idle": true, "idle_wait_s": 120}},
		{"ui", map[string]any{"id": "soak", "name": "Idle soak", "description": "Does a model stay loaded and fast through two idle minutes?", "benchmarks": []any{"gemma-idle-hold"}, "cooldown_s": 5}},
	}
}

// seedBenchDefs creates the benchmarks and suites through the API.
func seedBenchDefs(c *cluster) error {
	s := newSeedClient(c.url)
	agent, err := newMCP(c.url, "claude-code", "2.1.4")
	if err != nil {
		return err
	}
	create := func(kind, by string, spec map[string]any) error {
		path, tool, arg := "/api/bench/benchmarks", "create_benchmark", "benchmark"
		if kind == "suite" {
			path, tool, arg = "/api/bench/suites", "create_suite", "suite"
		}
		switch by {
		case "mcp":
			_, err := agent.call(tool, map[string]any{arg: spec, "actor": "bench setup"})
			return err
		case "rest":
			_, err := s.do("POST", path, asCurl, spec)
			return err
		default:
			_, err := s.do("POST", path, asAlex, spec)
			return err
		}
	}
	for _, b := range seedBenchmarks() {
		if err := create("benchmark", b.by, b.spec); err != nil {
			return fmt.Errorf("benchmark %s: %w", b.spec["id"], err)
		}
	}
	for _, su := range seedSuites() {
		if err := create("suite", su.by, su.spec); err != nil {
			return fmt.Errorf("suite %s: %w", su.spec["id"], err)
		}
	}
	return nil
}

// seedBenchFinish gives the suites their real run options after the seed runs.
func seedBenchFinish(c *cluster) error {
	s := newSeedClient(c.url)
	edits := []struct {
		id      string
		w       who
		changes map[string]any
	}{
		{"nightly", asAlex.say("run it every night at 03:00, ten seconds between benchmarks"), map[string]any{"cooldown_s": 10, "schedule": map[string]any{"at": "03:00"}}},
		{"load-scaling", asCurl.say("let the PS5s settle between loads"), map[string]any{"cooldown_s": 5}},
		{"smoke", asAlex, map[string]any{"cooldown_s": 5}},
		{"side-by-side", asAlex, map[string]any{"cooldown_s": 5}},
	}
	for _, e := range edits {
		got, err := s.do("GET", "/api/bench/suites/"+e.id, asAlex, nil)
		if err != nil {
			return err
		}
		spec, _ := got["suite"].(map[string]any)
		if spec == nil {
			spec = got
		}
		body := map[string]any{}
		for _, k := range []string{"id", "name", "description", "benchmarks", "stop_on_failure", "cooldown_s", "only_when_idle", "idle_wait_s", "schedule", "baseline"} {
			if v, ok := spec[k]; ok && v != nil {
				body[k] = v
			}
		}
		for k, v := range e.changes {
			body[k] = v
		}
		if _, err := s.do("PUT", "/api/bench/suites/"+e.id, e.w, body); err != nil {
			return err
		}
	}
	return nil
}

// ---- past runs ----

type pastRun struct {
	daysAgo  int
	at       string // local clock time
	suite    string
	trigger  string
	note     string
	rpc      string
	revision string
	slow     map[string]float64 // decode factor by profile
	slowLoad map[string]float64 // load time factor by profile
}

func pastRuns() []pastRun {
	return []pastRun{
		{6, "03:00", "nightly", "schedule", "scheduled", "b4501", "4", nil, nil},
		{5, "03:00", "nightly", "schedule", "scheduled", "b4501", "4", nil, nil},
		{5, "14:20", "load-scaling", "manual", "after moving the PS5s to the new switch", "b4501", "5", nil, nil},
		{4, "03:00", "nightly", "schedule", "scheduled", "b4567", "6", nil, nil},
		{4, "11:05", "smoke", "manual", "after the rpc-server update to b4567", "b4567", "6", nil, nil},
		{3, "03:00", "nightly", "schedule", "scheduled", "b4567", "8", nil, nil},
		{3, "16:40", "side-by-side", "manual", "", "b4567", "8", nil, nil},
		// two days ago: Devstral decodes below its minimum, a real failed step
		{2, "03:00", "nightly", "schedule", "scheduled", "b4567", "11", map[string]float64{"bench-devstral-24b": 0.78}, nil},
		{2, "10:15", "load-scaling", "manual", "checking the loads after last night's slow decode", "b4567", "11", nil, nil},
		// the newest nightly: the 3-worker Gemma load is 22% slower than its
		// baseline (a regression, within its expectation); 1.155 is 22% with
		// this run's load noise against the baseline's
		{1, "03:00", "nightly", "schedule", "scheduled", "b4567", "13", nil, map[string]float64{"bench-gemma-12b-3w": 1.155}},
		{1, "09:30", "smoke", "manual", "", "b4567", "13", nil, nil},
		{1, "15:00", "phase-tour", "manual", "trying the new phase types", "b4567", "14", nil, nil},
	}
}

// seedBenchRuns executes the past runs with a second real manager on the real
// database, against the scripted scheduler, on a clock set to each run's day.
func seedBenchRuns(c *cluster, log *slog.Logger) error {
	clock := &vclock{}
	sa := newScriptedAPI(clock)
	defer sa.stop()
	cfg := c.svc.Config()
	m, err := bench.NewManager(bench.ManagerOptions{Store: c.db, API: sa, Version: devVersion, Logger: log,
		Thresholds: bench.Thresholds{LoadPct: cfg.Bench.RegressLoadPct, DecodePct: cfg.Bench.RegressDecodePct},
		ProfileOK:  func(id string) bool { _, _, ok := c.svc.Config().Resolve(id); return ok },
		Now:        clock.Now, IdlePoll: 20 * time.Millisecond, LoadPoll: 20 * time.Millisecond, GoneTimeout: 10 * time.Second})
	if err != nil {
		return err
	}
	m.SetModelFile(modelFileOf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		m.Stop(sctx)
		scancel()
	}()
	now := time.Now()
	for i, pr := range pastRuns() {
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -pr.daysAgo)
		at, _ := time.ParseInLocation("15:04", pr.at, time.Local)
		start := day.Add(time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute + time.Duration(i*7)*time.Second)
		clock.set(start)
		sa.mu.Lock()
		sa.revision, sa.rpcVersion, sa.factor, sa.jitter = pr.revision, pr.rpc, pr.slow, int64(i+1)
		if sa.factor == nil {
			sa.factor = map[string]float64{}
		}
		sa.loadFactor, sa.loadBy = 1, pr.slowLoad
		if pr.rpc == "b4501" { // the older rpc-server build loaded a little slower
			sa.loadFactor = 1.08
		}
		sa.mu.Unlock()
		run, err := m.Enqueue(pr.suite, bench.EnqueueOptions{Trigger: pr.trigger, Note: pr.note})
		if err != nil {
			return fmt.Errorf("seed run %d (%s): %w", i+1, pr.suite, err)
		}
		deadline := time.Now().Add(3 * time.Minute)
		for {
			r, err := c.db.GetRun(run.ID)
			if err == nil && bench.IsFinal(r.State) {
				if r.State != bench.RunCompleted {
					log.Warn("a seed run did not complete", "run", r.ID, "state", r.State, "error", r.Error)
				}
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("seed run %s (%s) did not finish in 3 minutes", run.ID, pr.suite)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	return nil
}
