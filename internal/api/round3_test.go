package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
)

// ---- names ----

// One rule for display names, whatever wrote them, with the publisher apart.
func TestTidyModelName(t *testing.T) {
	for _, c := range []struct{ raw, pub, name, wantPub string }{
		{"mistralai_mixtral-8x7b-instruct-v0.1", "", "Mixtral 8x7B Instruct v0.1", "Mistral AI"},
		{"tinyllama_tinyllama-1.1b-chat-v1.0", "", "TinyLlama 1.1B Chat v1.0", "TinyLlama"},
		{"GLM 4 32B 0414", "THUDM", "GLM 4 32B 0414", "THUDM"},
		{"Qwen3-Coder-30B-A3B-Instruct", "", "Qwen3 Coder 30B A3B Instruct", ""},
		{"DeepSeek-V2-Lite-Chat", "", "DeepSeek V2 Lite Chat", ""},
		{"Devstral-Small-2-24B-Instruct-2512", "", "Devstral Small 2 24B Instruct 2512", ""},
		{"Gemma-4-12B-It", "", "Gemma 4 12B IT", ""},
		{"Kimi Dev 72B", "moonshotai", "Kimi Dev 72B", "Moonshot AI"},
		{"Qwen2.5 72B Instruct", "", "Qwen2.5 72B Instruct", ""},
		{"some_lab_model", "", "Lab Model", "some"},
	} {
		if name, pub := TidyModelName(c.raw, c.pub); name != c.name || pub != c.wantPub {
			t.Errorf("TidyModelName(%q, %q) = %q, %q; want %q, %q", c.raw, c.pub, name, pub, c.name, c.wantPub)
		}
	}
	for text, want := range map[string]string{"Gemma 4 12B IT": "gemma", "Kimi Dev 72B moonshotai_Kimi-Dev-72B-IQ2_S.gguf": "kimi", "TinyLlama 1.1B": "tinyllama",
		"Devstral Small 2": "mistral", "Mixtral 8x7B": "mistral", "Llama 3.3 70B": "llama", "Qwen3 Coder": "qwen", "GLM 4 32B": "glm", "DeepSeek V2": "deepseek", "whisper": "other"} {
		if got := BrandOf(text); got != want {
			t.Errorf("BrandOf(%q) = %q, want %q", text, got, want)
		}
	}
}

// ---- catalog: the last loads of a row, the chart series of a page ----

func TestModelCatalogRecentLoadsAndCharts(t *testing.T) {
	in := catalogFixture()
	now := time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC)
	in.Now = now
	ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	in.Events = []history.Event{ // newest first
		{Ts: ms(2 * time.Minute), Type: history.TRequestRefused, Profile: "gemma:8k", ModelFile: "gemma.gguf", Data: map[string]any{"reason": "cluster busy"}},
		{Ts: ms(3 * time.Minute), Type: history.TRequestRefused, Profile: "gemma:8k", ModelFile: "gemma.gguf", Data: map[string]any{"reason": "cluster busy"}},
		{Ts: ms(10 * time.Minute), Type: history.TInstanceUnloaded, Profile: "bench-gemma-2-2048", ModelFile: "gemma.gguf", SessionID: "ld-2", Data: map[string]any{"requests_served": 3.0, "loaded_for_s": 120.0, "reason": "ttl"}},
		{Ts: ms(12 * time.Minute), Type: history.TLoadFinished, Profile: "bench-gemma-2-2048", ModelFile: "gemma.gguf", Nodes: []string{"ps5-b", "ps5-c"}, SessionID: "ld-2",
			Data: map[string]any{"outcome": "ready", "load_s": 30.0, "avg_mb_s": 250.0, "nodes_detail": []any{map[string]any{"memory_mib": 4000.0}, map[string]any{"memory_mib": 3900.0}}}},
		{Ts: ms(3 * time.Hour), Type: history.TLoadFinished, Profile: "gemma:8k", ModelFile: "gemma.gguf", Nodes: []string{"ps5-a"}, Data: map[string]any{"outcome": "failed", "reason": "stalled", "load_s": 71.0}},
		{Ts: ms(30 * time.Hour), Type: history.TLoadFinished, Profile: "gemma:8k", ModelFile: "gemma.gguf", Nodes: []string{"ps5-a"}, SessionID: "ld-1", Data: map[string]any{"outcome": "ready", "load_s": 26.0, "avg_mb_s": 100.0}},
	}
	list := BuildModelCatalog(in, "")
	var g map[string]any
	for _, m := range list {
		if m["key"] == "gemma.gguf" {
			g = m
		}
		if _, has := m["charts"]; has {
			t.Errorf("%v: the list must not carry the chart series", m["key"])
		}
		if m["brand"] == nil || m["brand"] == "" {
			t.Errorf("%v: no brand", m["key"])
		}
	}
	recent := g["loads"].(map[string]any)["recent"].([]map[string]any)
	if len(recent) != 4 {
		t.Fatalf("recent: %v, want 4 marks (two refusals in a row are one mark)", recent)
	}
	if r := recent[0]; r["outcome"] != "refused" || r["count"] != 2 || r["nodes"] != 0 {
		t.Errorf("newest mark %v", r)
	}
	if r := recent[1]; r["outcome"] != "ready" || r["nodes"] != 2 || r["load_s"].(float64) != 30 || r["mb_s"].(float64) != 250 || r["profile"] != "bench-gemma-2-2048" || r["at"].(int64) != ms(12*time.Minute) {
		t.Errorf("a load mark %v", r)
	}
	if r := recent[2]; r["outcome"] != "failed" || r["nodes"] != 1 {
		t.Errorf("a failed mark %v", r)
	}
	if g["brand"] != "gemma" || g["icon"] != nil {
		t.Errorf("brand %v icon %v", g["brand"], g["icon"])
	}
	if b := g["fit"].(map[string]any)["budgets_mib"].([]float64); len(b) != 3 || b[0] != 13000 || b[2] != 39000 {
		t.Errorf("fit budgets %v", b)
	}

	d := BuildModelCatalog(in, "gemma.gguf")[0]
	charts := d["charts"].(map[string]any)
	loads := charts["loads"].([]map[string]any)
	if len(loads) != 3 || loads[0]["load_s"].(float64) != 26 || loads[2]["nodes"] != 2 || loads[2]["memory_mib"].(float64) != 7900 ||
		loads[2]["stayed_s"].(float64) != 120 || loads[2]["requests"].(float64) != 3 || loads[2]["ended"] != "ttl" || loads[2]["ctx"] != 2048 {
		t.Errorf("load series, oldest first: %v", loads)
	}
	act := charts["activity"].(map[string]any)
	hours, days := act["hours"].([]map[string]any), act["days"].([]map[string]any)
	if len(hours) != 24 || len(days) != 7 {
		t.Fatalf("buckets: %d hours, %d days", len(hours), len(days))
	}
	last := hours[23]
	if last["at"].(int64) != time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC).UnixMilli() || last["loads"] != 1 || last["refusals"] != 2 || last["requests"] != 3 || last["failed"] != 0 {
		t.Errorf("the current hour: %v", last)
	}
	if h := hours[20]; h["loads"] != 1 || h["failed"] != 1 {
		t.Errorf("three hours ago: %v", h)
	}
	total := 0
	for _, x := range days {
		total += x["loads"].(int)
	}
	if total != 3 || days[6]["loads"] != 2 || days[5]["loads"] != 1 {
		t.Errorf("days: %v", days)
	}
}

// ---- the catalog is kept in memory ----

// A warm GET /api/models reads no model file and does not query the history;
// an event of a type the catalog reads makes it build again, from the
// server's own copy of the events (still no history query).
func TestModelCatalogWarmCallReadsNothing(t *testing.T) {
	dir := t.TempDir()
	good := ggufBytes(map[string]string{"general.architecture": "gemma3", "general.name": "mistralai_mixtral-8x7b-instruct-v0.1"})
	for _, f := range []string{"a.gguf", "b.gguf", "c.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, f), good, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := openStore(t)
	seed(st)
	e := histEnv(t, st)
	e.cfg.ModelsDir = dir

	get := func() map[string]any {
		t.Helper()
		resp := e.get("/api/models")
		if resp.StatusCode != 200 {
			t.Fatalf("GET /api/models: %d", resp.StatusCode)
		}
		return jsonOf(t, body(t, resp))
	}
	first := get()
	var m0 map[string]any
	for _, m := range first["models"].([]any) {
		if mm := m.(map[string]any); mm["key"] == "a.gguf" {
			m0 = mm
		}
	}
	if m0 == nil {
		t.Fatalf("no a.gguf in %v", first["models"])
	}
	if m0["name"] != "Mixtral 8x7B Instruct v0.1" || m0["publisher"] != "Mistral AI" || m0["raw_name"] != "mistralai_mixtral-8x7b-instruct-v0.1" || m0["brand"] != "mistral" {
		t.Errorf("tidy name over REST: %v %v %v %v", m0["name"], m0["publisher"], m0["raw_name"], m0["brand"])
	}
	reads, queries, builds := metaReads.Load(), e.api.catalog.histQueries.Load(), e.api.catalog.builds.Load()
	if queries != 1 {
		t.Fatalf("the history was queried %d times for the first catalog, want once", queries)
	}
	for i := 0; i < 5; i++ {
		if v := get(); v["version"] != first["version"] {
			t.Fatalf("warm call %d: version %v, was %v", i, v["version"], first["version"])
		}
	}
	if resp := e.get("/api/models/a.gguf"); resp.StatusCode != 200 {
		t.Fatalf("detail: %d", resp.StatusCode)
	} else {
		body(t, resp)
	}
	if r, q, b := metaReads.Load(), e.api.catalog.histQueries.Load(), e.api.catalog.builds.Load(); r != reads || q != queries || b != builds {
		t.Fatalf("warm calls: %d file reads, %d history queries, %d builds more than before; want none", r-reads, q-queries, b-builds)
	}

	// a load finishes: the catalog follows, without asking the history store again
	st.Record(history.Event{Ts: time.Now().UnixMilli(), Type: history.TLoadFinished, Severity: "info", Profile: "alpha", ModelFile: "a.gguf", Nodes: []string{"host-1"},
		Data: map[string]any{"outcome": "ready", "load_s": 12.5, "avg_mb_s": 90.0}})
	eventually(t, "the new load in the catalog", func() bool {
		for _, m := range get()["models"].([]any) {
			mm := m.(map[string]any)
			if mm["key"] == "a.gguf" {
				return mm["loads"].(map[string]any)["count"] == float64(1)
			}
		}
		return false
	})
	if r, q := metaReads.Load(), e.api.catalog.histQueries.Load(); r != reads || q != queries {
		t.Fatalf("after an event: %d file reads and %d history queries more; want none", r-reads, q-queries)
	}
	if v := get()["version"].(float64); v <= first["version"].(float64) {
		t.Errorf("the version did not rise after a change: %v", v)
	}
}

// The file metadata is remembered in the database: a new process with the
// same database reads no GGUF header again.
func TestModelFileMetadataSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gemma-4-12b-it-Q4_K_M.gguf"), ggufBytes(map[string]string{"general.architecture": "gemma3", "general.name": "Gemma-4-12B-It"}), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, cs := cfgEnv(t)
	c1 := modelFileCache{store: cs}
	before := metaReads.Load()
	if got := c1.list(dir, nil); len(got) != 1 || got[0].Name != "Gemma 4 12B IT" {
		t.Fatalf("%+v", got)
	}
	if metaReads.Load() != before+1 {
		t.Fatal("the first listing must read the header once")
	}
	c2 := modelFileCache{store: cs} // "after a restart": an empty memory cache, the same database
	if got := c2.list(dir, nil); len(got) != 1 || got[0].Name != "Gemma 4 12B IT" || got[0].MetaSource != "gguf" {
		t.Fatalf("%+v", got)
	}
	if metaReads.Load() != before+1 {
		t.Fatal("the listing after a restart read the header again")
	}
	// a changed file is read again
	os.WriteFile(filepath.Join(dir, "gemma-4-12b-it-Q4_K_M.gguf"), ggufBytes(map[string]string{"general.architecture": "gemma3", "general.name": "Gemma-4-12B-It-v2"}), 0o644)
	if got := c2.list(dir, nil); got[0].Name != "Gemma 4 12B IT v2" || metaReads.Load() != before+2 {
		t.Fatalf("a changed file: %+v, reads %d", got, metaReads.Load()-before)
	}
}

// The live channel carries the catalog: the whole document when a client
// subscribes to "models", then only what changed.
func TestWSModelsTopic(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	w := dialWS(t, e, nil)
	hello := w.until(t, "hello", 2*time.Second)
	if !strings.Contains(string(hello.Data), `"models"`) {
		t.Errorf("hello does not list the models topic: %s", hello.Data)
	}
	for _, m := range w.collect(300 * time.Millisecond) {
		if m.Type == "models" {
			t.Fatal("a client that did not ask for the models topic got it")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribe","topics":["status","models"]}`)); err != nil {
		t.Fatal(err)
	}
	var full struct {
		Version int64
		Models  []map[string]any
		Dir     string `json:"models_dir"`
	}
	if err := json.Unmarshal(w.until(t, "models", 3*time.Second).Data, &full); err != nil || len(full.Models) != 3 || full.Version < 1 || full.Dir != "/models" {
		t.Fatalf("the snapshot: %v %+v", err, full)
	}
	st.Record(history.Event{Ts: time.Now().UnixMilli(), Type: history.TLoadFinished, Severity: "info", Profile: "beta", ModelFile: "b.gguf", Nodes: []string{"host-1"},
		Data: map[string]any{"outcome": "failed", "load_s": 3.0, "reason": "x"}})
	var d struct {
		Version, Base int64
		Changed       []map[string]any
		Removed       []string
		Order         []string
	}
	if err := json.Unmarshal(w.until(t, "models", 3*time.Second).Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Base != full.Version || d.Version != full.Version+1 || len(d.Changed) != 1 || d.Changed[0]["key"] != "b.gguf" || len(d.Removed) != 0 || len(d.Order) != 3 {
		t.Fatalf("the change message: %+v", d)
	}
}

// ---- agents ----

func (e *env) hdo(method, path string, body []byte, hdr map[string]string) (int, []byte, http.Header) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.front.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func auditOf(t *testing.T, e *env, query string) []map[string]any {
	t.Helper()
	st, m := e.cdo("GET", "/api/audit?"+query, "", nil)
	if st != 200 {
		t.Fatalf("GET /api/audit?%s: %d %v", query, st, m)
	}
	var out []map[string]any
	for _, x := range m["entries"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

// mcpCall sends one tools/call with the given headers and returns the HTTP status and the body.
func mcpCall(h http.Handler, name, args string, hdr map[string]string) (int, string) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// An agent key is identity for the audit log: created once, used on REST and
// on MCP, the actions are attributed to the agent; a wrong or revoked key is
// refused; no key works as before.
func TestAgentKeysAttributeActionsInTheAudit(t *testing.T) {
	e, _, cs := cfgEnv(t)
	mcp := mcpserver.New(e.api, mcpserver.Options{Version: "test", Identify: e.api.IdentifyAgent,
		Bridge: func(ctx context.Context, actor string) context.Context { return audit.WithSource(ctx, "mcp", actor) }})
	e.api.SetMCPTools(mcp.ToolInfo)
	ui := map[string]string{"X-Source": "ui", "X-Actor": "alex", "Content-Type": "application/json"}

	// create: the key is shown once, only its hash is stored
	st, m := e.cdo("POST", "/api/agents", `{"name":"claude-code laptop","description":"my coding agent","permissions":{"settings":"allow"}}`, ui)
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	key, _ := m["key"].(string)
	ag := m["agent"].(map[string]any)
	id := ag["id"].(string)
	if !strings.HasPrefix(key, "ptl_") || len(key) < 28 || ag["name"] != "claude-code laptop" || ag["key_prefix"] != key[:10] || ag["actor"] != "agent:claude-code laptop" ||
		ag["created_by"].(map[string]any)["actor"] != "alex" || ag["created_by"].(map[string]any)["source"] != "ui" {
		t.Fatalf("created agent: key %q, %v", key, ag)
	}
	stored, err := cs.Agent(context.Background(), id)
	if err != nil || stored.KeyHash != hashKey(key) || strings.Contains(stored.KeyHash, key) {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	_, lm := e.cdo("GET", "/api/agents", "", nil)
	if raw, _ := json.Marshal(lm); strings.Contains(string(raw), key) || strings.Contains(string(raw), stored.KeyHash) {
		t.Fatal("the list gives the key or its hash away")
	}
	if st, m := e.cdo("POST", "/api/agents", `{"name":"Claude-Code Laptop"}`, ui); st != 409 {
		t.Fatalf("a second agent with the same name: %d %v", st, m)
	}
	if st, _ := e.cdo("POST", "/api/agents", `{"name":"  "}`, ui); st != 400 {
		t.Fatalf("an agent without a name: %d", st)
	}

	// REST with the key, both headers: the change is the agent's
	profile := func(n string) string { return `{"id":"` + n + `","model":"` + n + `.gguf","ctx":2048,"needs_mib":100}` }
	if st, m := e.cdo("POST", "/api/config/profiles", profile("r1"), map[string]string{"Authorization": "Bearer " + key, "X-Actor": "someone-else"}); st != 201 {
		t.Fatalf("REST with Bearer: %d %v", st, m)
	}
	if st, m := e.cdo("POST", "/api/config/profiles", profile("r2"), map[string]string{"X-Agent-Key": key}); st != 201 {
		t.Fatalf("REST with X-Agent-Key: %d %v", st, m)
	}
	// MCP with the key
	if code, out := mcpCall(mcp, "create_profile", `{"profile":{"id":"m1","model":"m1.gguf","ctx":2048,"needs_mib":100}}`, map[string]string{"Authorization": "Bearer " + key}); code != 200 || strings.Contains(out, `"isError":true`) {
		t.Fatalf("MCP with the key: %d %s", code, out)
	}
	// no key: exactly as before
	if st, m := e.cdo("POST", "/api/config/profiles", profile("n1"), map[string]string{"X-Actor": "curl"}); st != 201 {
		t.Fatalf("REST without a key: %d %v", st, m)
	}
	if code, out := mcpCall(mcp, "create_profile", `{"profile":{"id":"n2","model":"n2.gguf","ctx":2048,"needs_mib":100}}`, nil); code != 200 || strings.Contains(out, `"isError":true`) {
		t.Fatalf("MCP without a key: %d %s", code, out)
	}
	// an OpenAI client's own bearer token is not an agent key and is ignored, as before
	if st, _, _ := e.hdo("GET", "/api/status", nil, map[string]string{"Authorization": "Bearer sk-anything"}); st != 200 {
		t.Fatalf("a foreign bearer token: %d", st)
	}

	// unknown key: refused, on both transports, and nothing is changed
	for _, h := range []map[string]string{{"Authorization": "Bearer lls_wrongwrongwrongwrongwrongwrong"}, {"X-Agent-Key": "typo"}} {
		st, m := e.cdo("POST", "/api/config/profiles", profile("bad"), h)
		if st != 401 || !strings.Contains(m["error"].(map[string]any)["message"].(string), "unknown agent key") || m["error"].(map[string]any)["type"] != "invalid_agent_key" {
			t.Fatalf("unknown key over REST: %d %v", st, m)
		}
		if code, out := mcpCall(mcp, "get_status", `{}`, h); code != 401 || !strings.Contains(out, "unknown agent key") {
			t.Fatalf("unknown key over MCP: %d %s", code, out)
		}
	}
	if st, _ := e.cdo("GET", "/api/config/profiles/bad", "", nil); st != 404 {
		t.Fatal("a request with an unknown key changed the configuration")
	}

	// the audit log: who did what
	byTarget := map[string]map[string]any{}
	for _, a := range auditOf(t, e, "section=profiles&limit=50") {
		byTarget[a["target"].(string)] = a
	}
	for target, want := range map[string][3]string{"r1": {"rest", "agent:claude-code laptop", id}, "r2": {"rest", "agent:claude-code laptop", id},
		"m1": {"mcp", "agent:claude-code laptop", id}, "n1": {"rest", "curl", ""}, "n2": {"mcp", "mcp-client", ""}} {
		a := byTarget[target]
		got := [3]string{a["source"].(string), a["actor"].(string), ""}
		if v, ok := a["agent_id"].(string); ok {
			got[2] = v
		}
		if got != want {
			t.Errorf("audit of %s: %v, want %v", target, got, want)
		}
	}
	if es := auditOf(t, e, "agent="+id); len(es) != 3 {
		t.Errorf("filter by agent: %d entries, want the 3 the agent made", len(es))
	}
	// the agent itself is in the log too (section agents), made by the web UI user
	if es := auditOf(t, e, "section=agents"); len(es) != 1 || es[0]["action"] != "create" || es[0]["actor"] != "alex" || es[0]["source"] != "ui" || es[0]["target"] != id ||
		strings.Contains(string(rawJSON(es[0])), key) {
		t.Errorf("audit of the agent: %v", es)
	}

	// use is counted
	_, lm = e.cdo("GET", "/api/agents", "", nil)
	a0 := lm["agents"].([]any)[0].(map[string]any)
	if a0["request_count"].(float64) != 3 || a0["last_used_at"] == nil || a0["audit_entries"].(float64) != 3 {
		t.Errorf("counters: %v", a0)
	}

	// rename: later entries carry the new name, the id stays
	if st, m := e.cdo("PATCH", "/api/agents/"+id, `{"name":"laptop agent"}`, ui); st != 200 || m["agent"].(map[string]any)["name"] != "laptop agent" {
		t.Fatalf("rename: %d %v", st, m)
	}
	e.cdo("POST", "/api/config/profiles", profile("r3"), map[string]string{"X-Agent-Key": key})
	if es := auditOf(t, e, "target=r3"); len(es) != 1 || es[0]["actor"] != "agent:laptop agent" || es[0]["agent_id"] != id {
		t.Errorf("after the rename: %v", es)
	}

	// rotate: the old key stops working, the new one works
	st, m = e.cdo("POST", "/api/agents/"+id+"/rotate", "", ui)
	key2, _ := m["key"].(string)
	if st != 200 || key2 == key || !strings.HasPrefix(key2, "ptl_") {
		t.Fatalf("rotate: %d %v", st, m)
	}
	if st, _ := e.cdo("GET", "/api/status", "", map[string]string{"X-Agent-Key": key}); st != 401 {
		t.Fatalf("the old key after a rotate: %d", st)
	}
	if st, _ := e.cdo("GET", "/api/status", "", map[string]string{"Authorization": "Bearer " + key2}); st != 200 {
		t.Fatalf("the new key: %d", st)
	}

	// revoke: refused with its own message, the row stays so old entries keep a name
	if st, m := e.cdo("DELETE", "/api/agents/"+id, "", ui); st != 200 || m["revoked"] != true {
		t.Fatalf("revoke: %d %v", st, m)
	}
	st, m = e.cdo("GET", "/api/status", "", map[string]string{"X-Agent-Key": key2})
	if st != 401 || !strings.Contains(m["error"].(map[string]any)["message"].(string), "revoked") {
		t.Fatalf("a revoked key over REST: %d %v", st, m)
	}
	if code, out := mcpCall(mcp, "get_status", `{}`, map[string]string{"X-Agent-Key": key2}); code != 401 || !strings.Contains(out, "revoked") {
		t.Fatalf("a revoked key over MCP: %d %s", code, out)
	}
	_, lm = e.cdo("GET", "/api/agents", "", nil)
	if as := lm["agents"].([]any); len(as) != 1 || as[0].(map[string]any)["revoked"] != true || as[0].(map[string]any)["name"] != "laptop agent" {
		t.Errorf("the revoked agent must stay listed: %v", lm)
	}
	if es := auditOf(t, e, "section=agents"); len(es) != 4 { // create, update, rotate, revoke
		t.Errorf("agent changes in the audit: %d, want 4", len(es))
	}
	if st, _ := e.cdo("POST", "/api/agents/"+id+"/rotate", "", ui); st != 404 {
		t.Errorf("rotating a revoked agent: %d", st)
	}
	// its name is free again
	if st, _ := e.cdo("POST", "/api/agents", `{"name":"laptop agent"}`, ui); st != 201 {
		t.Errorf("reusing the name of a revoked agent: %d", st)
	}

	// what an agent can do, read from the MCP server
	_, tm := e.cdo("GET", "/api/mcp/tools", "", nil)
	tools := tm["tools"].([]any)
	groups := map[string]int{}
	for _, x := range tools {
		tool := x.(map[string]any)
		groups[tool["group"].(string)]++
		if tool["name"] == "" || tool["summary"] == "" {
			t.Errorf("tool %v", tool)
		}
		if n := tool["name"]; (n == "load_model" || n == "unload_model") != (tool["group"] == "runtime") {
			t.Errorf("%v is in group %v", n, tool["group"])
		}
	}
	if len(tools) < 40 || groups["read"] == 0 || groups["configuration"] == 0 || groups["benchmarks"] == 0 || groups["runtime"] != 2 {
		t.Errorf("%d tools, groups %v", len(tools), groups)
	}
}

func rawJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// ---- icons ----

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

func TestModelIcons(t *testing.T) {
	e, _, _ := cfgEnv(t)
	ui := map[string]string{"X-Source": "ui", "X-Actor": "alex", "Content-Type": "image/png"}
	e.cdo("POST", "/api/config/profiles", `{"id":"g1","model":"gemma-4-12b-it-Q4_K_M.gguf","ctx":2048,"needs_mib":100}`, nil)
	e.cdo("POST", "/api/config/profiles", `{"id":"g2","model":"gemma-3-27b-it-Q4_K_M.gguf","ctx":2048,"needs_mib":100}`, nil)
	e.cdo("POST", "/api/config/profiles", `{"id":"q1","model":"Qwen_Qwen3.5-4B-Q4_K_M.gguf","ctx":2048,"needs_mib":100}`, nil)
	const g1, g2, q1 = "gemma-4-12b-it-Q4_K_M.gguf", "gemma-3-27b-it-Q4_K_M.gguf", "Qwen_Qwen3.5-4B-Q4_K_M.gguf"
	icons := func() map[string]any {
		_, m := e.cdo("GET", "/api/models", "", nil)
		out := map[string]any{}
		for _, x := range m["models"].([]any) {
			out[x.(map[string]any)["key"].(string)] = x.(map[string]any)["icon"]
		}
		return out
	}
	if ic := icons(); len(ic) != 3 || ic[g1] != nil {
		t.Fatalf("before: %v", ic)
	}
	if st, _, _ := e.hdo("GET", "/api/models/"+g1+"/icon", nil, nil); st != 404 {
		t.Fatalf("no icon yet: %d", st)
	}

	// refused uploads
	svgScript := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	svgHandler := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="x()"><rect width="1" height="1"/></svg>`)
	svgRemoteRef := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><image href="https://example.com/x.png"/></svg>`)
	for name, b := range map[string][]byte{"script": svgScript, "handler": svgHandler, "remote": svgRemoteRef, "text": []byte("hello"), "html": []byte("<html><body/></html>"),
		"empty": {}, "huge": append(append([]byte{}, pngBytes...), make([]byte, MaxIconBytes)...)} {
		if st, out, _ := e.hdo("PUT", "/api/models/"+g1+"/icon", b, ui); st != 400 && st != 413 {
			t.Errorf("%s icon: status %d %s, want a refusal", name, st, out)
		}
	}
	if st, _, _ := e.hdo("PUT", "/api/models/nope.gguf/icon", pngBytes, ui); st != 404 {
		t.Errorf("icon of an unknown model: %d", st)
	}
	if st, _, _ := e.hdo("PUT", "/api/models/"+g1, pngBytes, ui); st != 404 {
		t.Errorf("PUT on a model itself: %d", st)
	}

	// one model
	st, out, _ := e.hdo("PUT", "/api/models/"+g1+"/icon", pngBytes, ui)
	if st != 200 {
		t.Fatalf("PUT: %d %s", st, out)
	}
	ic := icons()
	i1, _ := ic[g1].(map[string]any)
	if i1 == nil || i1["scope"] != "model" || i1["etag"] == "" || ic[g2] != nil || ic[q1] != nil {
		t.Fatalf("after the model icon: %v", ic)
	}
	st, got, hdr := e.hdo("GET", "/api/models/"+g1+"/icon", nil, nil)
	if st != 200 || !bytes.Equal(got, pngBytes) || hdr.Get("Content-Type") != "image/png" || hdr.Get("ETag") != `"`+i1["etag"].(string)+`"` ||
		hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Cache-Control") != "no-cache" {
		t.Fatalf("GET icon: %d %v", st, hdr)
	}
	if st, _, hdr := e.hdo("GET", "/api/models/"+g1+"/icon?v="+i1["etag"].(string), nil, nil); st != 200 || !strings.Contains(hdr.Get("Cache-Control"), "immutable") {
		t.Errorf("versioned address: %d %q", st, hdr.Get("Cache-Control"))
	}
	if st, b, _ := e.hdo("GET", "/api/models/"+g1+"/icon", nil, map[string]string{"If-None-Match": hdr.Get("ETag")}); st != 304 || len(b) != 0 {
		t.Errorf("If-None-Match: %d", st)
	}

	// the family: one upload serves every Gemma file, a model's own icon wins
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8"><circle cx="4" cy="4" r="3" fill="#7ee0b0"/></svg>`)
	if st, out, _ := e.hdo("PUT", "/api/models/"+g2+"/icon?scope=family", svg, map[string]string{"X-Agent-Key": ""}); st != 200 {
		t.Fatalf("family icon: %d %s", st, out)
	}
	ic = icons()
	if i2, _ := ic[g2].(map[string]any); i2 == nil || i2["scope"] != "family" || ic[g1].(map[string]any)["scope"] != "model" || ic[q1] != nil {
		t.Fatalf("after the family icon: %v", ic)
	}
	if st, got, hdr := e.hdo("GET", "/api/models/"+g2+"/icon", nil, nil); st != 200 || !bytes.Equal(got, svg) || hdr.Get("Content-Type") != "image/svg+xml" || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("family icon served: %d %v", st, hdr)
	}

	// remove
	if st, _, _ := e.hdo("DELETE", "/api/models/"+g1+"/icon", nil, ui); st != 200 {
		t.Fatalf("DELETE: %d", st)
	}
	if ic = icons(); ic[g1].(map[string]any)["scope"] != "family" {
		t.Errorf("after removing its own icon the model falls back to the family's: %v", ic[g1])
	}
	if st, _, _ := e.hdo("DELETE", "/api/models/"+g1+"/icon", nil, ui); st != 404 {
		t.Errorf("DELETE again: %d", st)
	}

	// audited like other changes: section models, with the source and the actor
	es := auditOf(t, e, "section=models")
	if len(es) != 3 {
		t.Fatalf("audit entries: %d, want 3 (set, set for the family, remove)", len(es))
	}
	if a := es[2]; a["action"] != "create" || a["source"] != "ui" || a["actor"] != "alex" || a["target"] != "icon of "+g1 {
		t.Errorf("the first entry: %v", a)
	}
	if a := es[1]; a["source"] != "rest" || a["target"] != "icon of the gemma family" {
		t.Errorf("the family entry: %v", a)
	}
	if a := es[0]; a["action"] != "delete" {
		t.Errorf("the removal: %v", a)
	}
	var store *cfgstore.Store = e.api.store
	if tags, _ := store.IconTags(context.Background()); len(tags) != 1 || tags["family:gemma"] == "" {
		t.Errorf("stored icons: %v", tags)
	}
}
