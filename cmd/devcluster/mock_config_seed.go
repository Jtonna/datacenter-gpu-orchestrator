// DEV-ONLY MOCK of a server contract that is not built on this branch: the
// seeded configuration history and audit log of the config mock.
//
// The current document (mock_config_seed_doc.json) was produced by the real
// config.ParseYAMLImport on the bench branch from this harness's profiles,
// tiers and settings. Revision 1 is the import of that document with the 13
// edits below undone; the edits then lead back to it (revision 14).

package main

import (
	_ "embed"
	"encoding/json"
	"time"
)

//go:embed mock_config_seed_doc.json
var seedDocJSON []byte

func seedDoc() map[string]any {
	var d map[string]any
	if err := json.Unmarshal(seedDocJSON, &d); err != nil {
		panic(err)
	}
	normalizeDoc(d)
	return d
}

type seedEdit struct {
	ago                    time.Duration
	source, actor, comment string
	remote                 string
	fn                     func(d map[string]any)
}

func setKey(scope, key string, v any) func(d map[string]any) {
	return func(d map[string]any) {
		m := asMap(d[scope])
		if v == nil {
			delete(m, key)
		} else {
			m[key] = v
		}
		d[scope] = m
	}
}

func profileKey(id, key string, v any) func(d map[string]any) {
	return func(d map[string]any) { asMap(asMap(d["profiles"])[id])[key] = v }
}

// reset seeds the store. fresh (scenario bare) is a first start: revision 0,
// configured false, no profiles, no tiers, an empty audit log.
func (s *configStore) reset(fresh bool) {
	notify := s.notify
	s.notify = nil
	defer func() { s.notify = notify }()
	s.mu.Lock()
	s.rev, s.revs, s.updatedAt, s.doc = 0, nil, 0, emptyDoc()
	s.mu.Unlock()
	if s.audit != nil {
		s.audit.reset()
	}
	if fresh {
		return
	}
	now := time.Now()
	d := 24 * time.Hour
	cur := seedDoc()

	// the document as imported 9 days ago: the edits below undone
	init := cloneMap(cur)
	for _, k := range []string{"queue_timeout", "load_memory_floor_mib", "history_keep_files"} {
		delete(asMap(init["settings"]), k)
	}
	asMap(asList(init["tiers"])[0])["link_mb_s"] = 80.0
	init["exclude_nodes"], init["node_overrides"] = []any{}, map[string]any{}
	ps := asMap(init["profiles"])
	asMap(ps["kimi-dev-72b-iq2s"])["needs_mib"] = 28000.0
	g48 := asMap(ps["gemma-4-12b:48k"])
	g48["ctx"], g48["needs_mib"] = 32768.0, 8000.0
	delete(ps, "bench-kimi-72b-3w")
	ps["devstral-24b:24k"] = map[string]any{"model": "Devstral-Small-2-24B-Instruct-2512-Q3_K_M.gguf", "ctx": 24576.0,
		"kv_quant": "q4_0", "flash_attn": true, "aliases": []any{"devstral-24b-q4"}}

	commit := func(o origin, action string, fn func(d map[string]any)) {
		if _, err := s.mutate(writeOpts{o: o}, mutation{action: action, requireEmpty: action == "import", fn: func(doc map[string]any) error {
			fn(doc)
			return nil
		}}); err != nil {
			panic("devcluster config seed: " + err.Error())
		}
	}
	commit(origin{Source: "import", Actor: "import-config", Comment: "llm-scheduler import-config /etc/llm-scheduler/config.yaml", Remote: "127.0.0.1",
		UserAgent: "llm-scheduler/0.1.0", Ts: now.Add(-9*d - 3*time.Hour).UnixMilli()}, "import",
		func(doc map[string]any) {
			for k, v := range init {
				doc[k] = clone(v)
			}
		})

	edits := []seedEdit{
		{ago: 8*d + 20*time.Hour, source: "ui", actor: "jacob", comment: "big models wait longer behind a 72B load", fn: setKey("settings", "queue_timeout", "15m0s")},
		{ago: 8*d + 2*time.Hour, source: "mcp", actor: "claude-agent", comment: "needs_mib measured on three PS5s: load log sum 26510 MiB",
			fn: profileKey("kimi-dev-72b-iq2s", "needs_mib", 26800.0)},
		{ago: 7*d + 5*time.Hour, source: "ui", actor: "jacob", comment: "OMEN moved to the 2.5 GbE port",
			fn: func(doc map[string]any) { asMap(asList(doc["tiers"])[0])["link_mb_s"] = 100.0 }},
		{ago: 6*d + 9*time.Hour, source: "rest", actor: "anonymous", remote: "192.168.1.41",
			fn: func(doc map[string]any) { doc["exclude_nodes"] = []any{"alienware-m15"} }},
		{ago: 6*d + 8*time.Hour, source: "ui", actor: "jacob", comment: "pin the laptop until the hub patch is live",
			fn: func(doc map[string]any) {
				doc["node_overrides"] = map[string]any{"alienware-m15": map[string]any{"rpc": "10.0.0.3:50052"}}
			}},
		{ago: 5*d + 3*time.Hour, source: "mcp", actor: "claude-agent", comment: "forced three-worker profile for the nightly load test",
			fn: func(doc map[string]any) {
				asMap(doc["profiles"])["bench-kimi-72b-3w"] = clone(asMap(cur["profiles"])["bench-kimi-72b-3w"])
			}},
		{ago: 4*d + 22*time.Hour, source: "ui", actor: "jacob", comment: "never tested; q4_0 KV was too lossy",
			fn: func(doc map[string]any) { delete(asMap(doc["profiles"]), "devstral-24b:24k") }},
		{ago: 4*d + 2*time.Hour, source: "ui", actor: "jacob", comment: "a PS5 went down at 900 MiB free during the kimi load",
			fn: setKey("settings", "load_memory_floor_mib", 1024.0)},
		{ago: 3*d + 7*time.Hour, source: "rest", actor: "curl", remote: "192.168.1.41", fn: setKey("settings", "worker_silence", "45s")},
		{ago: 2*d + 11*time.Hour, source: "rest", actor: "curl", remote: "192.168.1.41", fn: profileKey("qwen3.5-4b", "ttl", "15m0s")},
		{ago: 2*d + 1*time.Hour, source: "mcp", actor: "claude-agent", comment: "48k context measured on two PS5s: 8206 MiB",
			fn: func(doc map[string]any) {
				p := asMap(asMap(doc["profiles"])["gemma-4-12b:48k"])
				p["ctx"], p["needs_mib"] = 49152.0, 8250.0
			}},
		{ago: 1*d + 6*time.Hour, source: "ui", actor: "jacob", comment: "keep a month of events", fn: setKey("settings", "history_keep_files", 30.0)},
		{ago: 9 * time.Hour, source: "mcp", actor: "claude-agent", comment: "revert: 45 s hid a real hang for too long",
			fn: setKey("settings", "worker_silence", nil)},
	}
	for _, e := range edits {
		remote, ua := e.remote, "curl/8.9.1"
		switch e.source {
		case "ui":
			remote, ua = "192.168.1.20", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36"
		case "mcp":
			remote, ua = "127.0.0.1", "llm-scheduler-mcp"
		}
		commit(origin{Source: e.source, Actor: e.actor, Comment: e.comment, Remote: remote, UserAgent: ua, Ts: now.Add(-e.ago).UnixMilli()}, "", e.fn)
	}
	// the store's document is now the seed document again (revision 14)

	if s.audit == nil {
		return
	}
	// benchmark, suite and runtime entries: audited, no config revision
	for _, b := range []struct {
		ago                                       time.Duration
		source, actor, action, section, target, c string
	}{
		{8*d + 23*time.Hour, "import", "import-config", "import", "benchmarks", "qwen4b-simple", "seeded from config.yaml"},
		{8*d + 23*time.Hour, "import", "import-config", "import", "suites", "smoke", "seeded from config.yaml"},
		{7*d + 4*time.Hour, "ui", "jacob", "create", "benchmarks", "gemma-load-1w", ""},
		{7*d + 4*time.Hour - 3*time.Minute, "ui", "jacob", "create", "benchmarks", "gemma-load-2w", ""},
		{7*d + 4*time.Hour - 5*time.Minute, "ui", "jacob", "create", "benchmarks", "gemma-load-3w", ""},
		{7*d + 3*time.Hour, "ui", "jacob", "create", "suites", "load-scaling", "gemma on 1, 2 and 3 workers"},
		{5*d + 2*time.Hour, "mcp", "claude-agent", "create", "benchmarks", "kimi-72b-load-3w", "nightly load test of the 72B model"},
		{5*d + 1*time.Hour, "mcp", "claude-agent", "update", "suites", "nightly", "add kimi-72b-load-3w as step 7"},
		{3*d + 4*time.Hour, "rest", "curl", "update", "benchmarks", "devstral-load-requests", ""},
		{1*d + 2*time.Hour, "ui", "jacob", "delete", "benchmarks", "tinyllama-cold", "the profile is gone"},
		{20 * time.Hour, "ui", "jacob", "update", "benchmarks", "gemma-conversation", "five turns instead of four"},
		{3 * time.Hour, "mcp", "claude-agent", "load", "runtime", "qwen3.8-27b", "warm up for the code review session"},
		{70 * time.Minute, "rest", "curl", "unload", "runtime", "kimi-dev-72b-iq2s", ""},
	} {
		var before, after any
		switch b.action {
		case "create", "import":
			after = map[string]any{"id": b.target}
		case "update":
			before, after = map[string]any{"id": b.target}, map[string]any{"id": b.target}
		case "delete":
			before = map[string]any{"id": b.target}
		case "load":
			after = map[string]any{"profile": b.target, "nodes": []any{ps5A.Host, ps5B.Host}}
		case "unload":
			before = map[string]any{"profile": b.target}
		}
		remote := map[string]string{"ui": "192.168.1.20", "mcp": "127.0.0.1", "rest": "192.168.1.41", "import": "127.0.0.1"}[b.source]
		s.audit.add(auditEntry{Ts: now.Add(-b.ago).UnixMilli(), Source: b.source, Actor: b.actor, RemoteAddr: remote, UserAgent: "devcluster-seed",
			Action: b.action, Section: b.section, Target: b.target, Before: before, After: after, Comment: b.c})
	}
	s.audit.renumber()
}

// auditBench records a benchmark or suite write of the bench mock.
func (s *configStore) auditBench(section, action, target string, before, after any, o origin) {
	if s.audit == nil {
		return
	}
	s.audit.add(auditEntry{Ts: o.Ts, Source: o.Source, Actor: o.Actor, RemoteAddr: o.Remote, UserAgent: o.UserAgent, Action: action,
		Section: section, Target: target, Before: before, After: after, Diff: diffValues(section+"."+target, before, after), Comment: o.Comment})
}
