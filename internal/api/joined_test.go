package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
)

// joinedEnv is the whole system in one process: configuration and audit on a
// database, the benchmark manager (its runner calls the API on loopback like
// the real binary), the history store and the MCP server with the audit
// bridge, exactly as cmd/llm-scheduler wires them.
type joinedEnv struct {
	*env
	mcp http.Handler
}

func newJoined(t *testing.T) *joinedEnv {
	t.Helper()
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	var e *env
	svc := cfgsvc.New(context.Background(), cfgsvc.Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }},
		Apply:    func(c *config.Config) { e.sched.ApplyConfig(c) },
		OnChange: func(ev cfgsvc.ChangeEvent) { e.api.PublishConfig(ev) }})
	hist := openStore(t)
	e = newEnv(t, chatOK, func(o *Options) { o.Config, o.Audit, o.History, o.Version = svc, cs, hist, "joined" })
	m, err := bench.NewManager(bench.ManagerOptions{Store: st, API: &bench.HTTPAPI{Base: e.front.URL}, Version: "t",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdlePoll: 5 * time.Millisecond, LoadPoll: 5 * time.Millisecond,
		ProfileOK: e.api.profileOK})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop(context.Background()) })
	e.api.opts.Bench, e.api.opts.BenchStore = m, st
	e.api.opts.benchLocalOK = true // the test fleet is the scheduler's own machine
	h := mcpserver.New(e.api, mcpserver.Options{Version: "test",
		Bridge: func(ctx context.Context, actor string) context.Context { return audit.WithSource(ctx, "mcp", actor) }})
	return &joinedEnv{env: e, mcp: h}
}

// session performs the MCP handshake and returns the session id.
func (j *joinedEnv) session(client string) string {
	j.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"` + client + `","version":"1.0"}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	j.mcp.ServeHTTP(rec, req)
	sid := rec.Header().Get("Mcp-Session-Id")
	if sid == "" {
		j.t.Fatalf("no session: %s", rec.Body.String())
	}
	return sid
}

// call runs one tool in a session and returns its text and error flag.
func (j *joinedEnv) call(sid, name, args string) (string, bool) {
	j.t.Helper()
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", sid)
	rec := httptest.NewRecorder()
	j.mcp.ServeHTTP(rec, req)
	var o struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil || len(o.Result.Content) == 0 {
		j.t.Fatalf("%s: %v %s", name, err, rec.Body.String())
	}
	return o.Result.Content[0].Text, o.Result.IsError
}

func (j *joinedEnv) ok(sid, name, args string) map[string]any {
	j.t.Helper()
	txt, isErr := j.call(sid, name, args)
	if isErr {
		j.t.Fatalf("%s %s: %s", name, args, txt)
	}
	return jsonOf(j.t, txt)
}

func (j *joinedEnv) fails(sid, name, args, want string) {
	j.t.Helper()
	txt, isErr := j.call(sid, name, args)
	if !isErr || !strings.Contains(txt, want) {
		j.t.Fatalf("%s %s: want an error with %q, got error=%v %s", name, args, want, isErr, txt)
	}
}

// auditEntries reads GET /api/audit with the query, oldest first.
func (j *joinedEnv) auditEntries(q string) []map[string]any {
	j.t.Helper()
	_, m := j.cdo("GET", "/api/audit?order=asc&"+q, "", nil)
	var out []map[string]any
	for _, x := range m["entries"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func (j *joinedEnv) waitAudit(what, q string, n int) []map[string]any {
	j.t.Helper()
	var got []map[string]any
	eventually(j.t, what, func() bool { got = j.auditEntries(q); return len(got) >= n })
	return got
}

const joinedProfile = `{"id":"mcp-alpha","model":"a.gguf","ctx":2048,"needs_mib":100}`

// Every configuration and audit tool, run through the MCP server against the
// real REST handlers and the real database.
func TestMCPConfigAndAuditToolsAgainstTheRealHandlers(t *testing.T) {
	j := newJoined(t)
	sid := j.session("claude-code")

	cfg := j.ok(sid, "get_config", `{}`)
	if cfg["revision"] != 0.0 || cfg["configured"] != false {
		t.Fatalf("%v", cfg)
	}
	sch := j.ok(sid, "get_config_schema", `{}`)
	if f, _ := sch["fields"].([]any); len(f) < 10 {
		t.Fatalf("schema: %v", sch)
	}

	// dry run: validated and reported, nothing saved, nothing audited
	dry := j.ok(sid, "create_profile", `{"profile":`+joinedProfile+`,"dry_run":true}`)
	if dry["dry_run"] != true || dry["revision"] != 0.0 || len(dry["diff"].([]any)) == 0 {
		t.Fatalf("dry run: %v", dry)
	}
	if st, m := j.cdo("GET", "/api/config/profiles", "", nil); st != 200 || len(m["profiles"].(map[string]any)) != 0 {
		t.Fatalf("dry run saved something: %v", m)
	}
	if es := j.auditEntries("source=mcp"); len(es) != 0 {
		t.Fatalf("dry run was audited: %v", es)
	}
	// a rejected profile reaches the agent as a 400 with the messages
	j.fails(sid, "create_profile", `{"profile":{"id":"bad","model":"x.gguf","ctx":1,"kv_quant":"q8_0"}}`, "q8_0 requires flash_attn")

	// create through MCP: visible in REST and audited with source mcp and the client name
	res := j.ok(sid, "create_profile", `{"profile":`+joinedProfile+`,"comment":"first profile","actor":"builder-7"}`)
	if res["revision"] != 1.0 || res["id"] != "mcp-alpha" {
		t.Fatalf("%v", res)
	}
	_, m := j.cdo("GET", "/api/config/profiles", "", nil)
	if m["revision"] != 1.0 || m["profiles"].(map[string]any)["mcp-alpha"] == nil {
		t.Fatalf("GET /api/config/profiles: %v", m)
	}
	gp := j.ok(sid, "get_profile", `{"id":"mcp-alpha"}`)
	if gp["id"] != "mcp-alpha" || gp["profile"].(map[string]any)["model"] != "a.gguf" || gp["revision"] != 1.0 {
		t.Fatalf("get_profile: %v", gp)
	}
	es := j.waitAudit("create audited", "source=mcp&section=profiles", 1)
	if e0 := es[0]; e0["action"] != "create" || e0["target"] != "mcp-alpha" || e0["actor"] != "builder-7 (via claude-code 1.0)" || e0["comment"] != "first profile" || e0["revision"] != 1.0 {
		t.Fatalf("%v", e0)
	}
	eventually(t, "scheduler runs revision 1", func() bool { return j.sched.Config().Revision == 1 && j.sched.Config().Profiles["mcp-alpha"] != nil })

	// If-Match: a stale revision conflicts, the current one applies
	j.fails(sid, "update_profile", `{"id":"mcp-alpha","profile":{"model":"a.gguf","ctx":4096,"needs_mib":100},"if_revision":99}`, "stale_revision")
	up := j.ok(sid, "update_profile", `{"id":"mcp-alpha","profile":{"model":"a.gguf","ctx":4096,"needs_mib":100},"if_revision":1}`)
	if up["revision"] != 2.0 {
		t.Fatalf("%v", up)
	}

	// settings (the PATCH body is the settings object), tiers, exclude, overrides, recovery, fleet
	if r := j.ok(sid, "update_settings", `{"settings":{"ttl":"10m"},"comment":"ttl"}`); r["revision"] != 3.0 {
		t.Fatalf("%v", r)
	}
	j.fails(sid, "update_settings", `{"settings":{"nonsense":1}}`, "HTTP 400")
	if _, s := j.cdo("GET", "/api/config/settings", "", nil); s["settings"].(map[string]any)["ttl"] == nil {
		t.Fatalf("%v", s)
	}
	if r := j.ok(sid, "set_tiers", `{"value":[{"match":{"local":true},"priority":1,"headroom_mib":100}]}`); r["revision"] != 4.0 {
		t.Fatalf("%v", r)
	}
	if r := j.ok(sid, "set_exclude_nodes", `{"value":["ps5-x*"]}`); r["revision"] != 5.0 {
		t.Fatalf("%v", r)
	}
	if r := j.ok(sid, "set_node_overrides", `{"value":{"a":{"rpc":"192.0.2.3:50052"}}}`); r["revision"] != 6.0 {
		t.Fatalf("%v", r)
	}
	if r := j.ok(sid, "set_recovery_settings", `{"value":{"enabled":true,"actions":["restart_rpc_server"]}}`); r["revision"] != 7.0 {
		t.Fatalf("%v", r)
	}
	if r := j.ok(sid, "set_fleet_settings", `{"value":{"url":"http://f:8090","local_node":"host-1"},"dry_run":true}`); r["dry_run"] != true || r["revision"] != 7.0 {
		t.Fatalf("fleet dry run: %v", r)
	}
	if r := j.ok(sid, "set_fleet_settings", `{"value":{"url":"http://f:8090","local_node":"host-1"}}`); r["revision"] != 8.0 {
		t.Fatalf("%v", r)
	}
	full := j.ok(sid, "get_config", `{}`)
	if full["revision"] != 8.0 || len(full["tiers"].([]any)) != 1 || full["exclude_nodes"].([]any)[0] != "ps5-x*" || full["node_overrides"].(map[string]any)["a"] == nil ||
		full["recovery"].(map[string]any)["enabled"] != true || full["fleet"].(map[string]any)["local_node"] != "host-1" {
		t.Fatalf("%v", full)
	}

	// revisions and rollback (the rollback is itself a new revision)
	revs := j.ok(sid, "list_config_revisions", `{"limit":3}`)
	if len(revs["revisions"].([]any)) != 3 || revs["current"] != 8.0 {
		t.Fatalf("%v", revs)
	}
	r1 := j.ok(sid, "get_config_revision", `{"revision":1}`)
	if r1["config"].(map[string]any)["profiles"].(map[string]any)["mcp-alpha"].(map[string]any)["ctx"] != 2048.0 {
		t.Fatalf("%v", r1)
	}
	j.fails(sid, "rollback_config", `{"revision":1,"if_revision":3}`, "stale_revision")
	rb := j.ok(sid, "rollback_config", `{"revision":1,"comment":"undo","if_revision":8}`)
	if rb["revision"] != 9.0 || rb["rolled_back_to"] != 1.0 {
		t.Fatalf("%v", rb)
	}
	back := j.ok(sid, "get_config", `{}`)
	if len(back["tiers"].([]any)) != 0 || back["profiles"].(map[string]any)["mcp-alpha"].(map[string]any)["ctx"] != 2048.0 {
		t.Fatalf("rollback did not restore revision 1: %v", back)
	}
	if r := j.ok(sid, "delete_profile", `{"id":"mcp-alpha","dry_run":true}`); r["dry_run"] != true {
		t.Fatalf("%v", r)
	}

	// load and unload through MCP are audited in section runtime
	j.ok(sid, "load_model", `{"model":"mcp-alpha","actor":"loader"}`)
	eventually(t, "ready", func() bool {
		ins := j.ok(sid, "get_status", `{}`)["instances"].([]any)
		return len(ins) == 1 && ins[0].(map[string]any)["state"] == "ready"
	})
	j.ok(sid, "unload_model", `{"model":"mcp-alpha"}`)
	rt := j.waitAudit("runtime audited", "source=mcp&section=runtime", 2)
	if rt[0]["action"] != "load" || rt[0]["actor"] != "loader (via claude-code 1.0)" || rt[0]["target"] != "mcp-alpha" || rt[1]["action"] != "unload" || rt[1]["actor"] != "claude-code 1.0" {
		t.Fatalf("%v", rt)
	}
	// the same through REST is source rest
	j.post("/api/load", `{"model":"mcp-alpha"}`)
	j.waitAudit("rest load audited", "source=rest&section=runtime&action=load", 1)

	// query_audit: every filter, through the tool
	qa := func(args string) []any { return j.ok(sid, "query_audit", args)["entries"].([]any) }
	if n := len(qa(`{"source":"mcp","section":"runtime"}`)); n != 2 {
		t.Fatalf("runtime: %d", n)
	}
	if n := len(qa(`{"source":"rest"}`)); n != 1 {
		t.Fatalf("rest: %d", n)
	}
	if n := len(qa(`{"actor":"builder-7 (via claude-code 1.0)"}`)); n != 1 {
		t.Fatalf("actor: %d", n)
	}
	if n := len(qa(`{"target":"mcp-alpha","action":"create"}`)); n != 1 {
		t.Fatalf("target+action: %d", n)
	}
	if n := len(qa(`{"action":"rollback"}`)); n < 1 { // one entry per section the rollback changed
		t.Fatalf("rollback: %d", n)
	}
	if n := len(qa(`{"section":"settings"}`)); n < 1 {
		t.Fatalf("section: %d", n)
	}
	if n := len(qa(`{"q":"undo"}`)); n < 1 {
		t.Fatalf("q: %d", n)
	}
	if n := len(qa(`{"limit":2}`)); n != 2 {
		t.Fatalf("limit: %d", n)
	}
	if n := len(qa(`{"since":"2001-01-01T00:00:00Z","until":"2001-01-02T00:00:00Z"}`)); n != 0 {
		t.Fatalf("until: %d", n)
	}
	if n := len(qa(`{"since":"2001-01-01T00:00:00Z"}`)); n < 12 {
		t.Fatalf("since: %d", n)
	}
	asc, desc := qa(`{"order":"asc"}`), qa(`{"order":"desc"}`)
	if asc[0].(map[string]any)["id"].(float64) >= desc[0].(map[string]any)["id"].(float64) {
		t.Fatal("order not honoured")
	}
	if n := len(qa(`{"source":"mcp"}`)); n < 11 {
		t.Fatalf("mcp entries: %d", n)
	}
}

// The aggregates and the editor schema through MCP, plus the benchmark and
// suite tools: create, run, cancel, every change audited in section bench
// with the right source and actor.
func TestMCPBenchToolsAggregatesSchemaAndBenchAudit(t *testing.T) {
	j := newJoined(t)
	sid := j.session("claude-code")
	j.ok(sid, "create_profile", `{"profile":`+joinedProfile+`}`)
	eventually(t, "profile live", func() bool { return j.sched.Config().Profiles["mcp-alpha"] != nil })

	sch := j.ok(sid, "get_bench_schema", `{}`)
	if len(sch) < 2 {
		t.Fatalf("get_bench_schema: %v", sch)
	}
	// empty aggregates are complete
	if ag := j.ok(sid, "get_bench_aggregates", `{}`); ag["runs_considered"] != 0.0 {
		t.Fatalf("%v", ag)
	}
	if ag := j.ok(sid, "get_bench_aggregates", `{"since":"7d","limit_runs":5,"env":"same"}`); ag["runs_considered"] != 0.0 {
		t.Fatalf("%v", ag)
	}
	j.fails(sid, "get_bench_aggregates", `{"limit_runs":1001}`, "HTTP 400")

	loadOnly := `{"id":"load-only","name":"Load only","profile":"mcp-alpha","phases":[{"type":"load"},{"type":"unload"}]}`
	j.ok(sid, "create_benchmark", `{"benchmark":`+loadOnly+`,"actor":"bench-agent"}`)
	j.fails(sid, "create_benchmark", `{"benchmark":{"id":"g","profile":"ghost","phases":[{"type":"load"}]}}`, "HTTP 4")
	if st, _ := j.cdo("POST", "/api/bench/benchmarks", `{"id":"rest-b","profile":"mcp-alpha","phases":[{"type":"load"}]}`, map[string]string{"X-Source": "ui", "X-Actor": "alex"}); st != 201 {
		t.Fatal(st)
	}
	if st, _ := j.cdo("POST", "/api/bench/benchmarks", `{"id":"rest-c","profile":"mcp-alpha","phases":[{"type":"load"}]}`, nil); st != 201 {
		t.Fatal(st)
	}
	j.ok(sid, "update_benchmark", `{"id":"load-only","benchmark":`+strings.Replace(loadOnly, "Load only", "Load only v2", 1)+`}`)
	j.ok(sid, "create_suite", `{"suite":{"id":"quick","name":"Quick","benchmarks":["load-only"],"cooldown_s":0,"only_when_idle":false}}`)
	j.ok(sid, "update_suite", `{"id":"quick","suite":{"id":"quick","name":"Quick 2","benchmarks":["load-only"],"cooldown_s":0,"only_when_idle":false}}`)
	if b := j.ok(sid, "get_benchmark", `{"id":"load-only"}`); b["name"] != "Load only v2" {
		t.Fatalf("%v", b)
	}
	if su := j.ok(sid, "get_suite", `{"id":"quick"}`); su["name"] != "Quick 2" {
		t.Fatalf("%v", su)
	}
	j.ok(sid, "list_benchmarks", `{}`)
	j.ok(sid, "list_suites", `{}`)

	run := j.ok(sid, "run_suite", `{"id":"quick","note":"via mcp","comment":"test"}`)
	id := run["id"].(string)
	eventually(t, "run completed", func() bool { return j.ok(sid, "get_run", `{"id":"`+id+`"}`)["state"] == "completed" })
	got := j.ok(sid, "get_run", `{"id":"`+id+`"}`)
	if env, _ := got["env"].(map[string]any); env == nil || env["config_revision"] != "1" {
		t.Fatalf("the run's environment must carry the real config revision: %v", got["env"])
	}
	if rs := j.ok(sid, "list_runs", `{"suite":"quick","state":"completed","limit":5}`)["runs"].([]any); len(rs) != 1 {
		t.Fatalf("%v", rs)
	}
	j.ok(sid, "get_bench_state", `{}`)
	if txt, isErr := j.call(sid, "export_run", `{"id":"`+id+`","format":"markdown"}`); isErr || !strings.Contains(txt, id) {
		t.Fatalf("%v %s", isErr, txt)
	}
	if txt, isErr := j.call(sid, "export_run", `{"id":"`+id+`","format":"csv","detail":"phases"}`); isErr || !strings.Contains(txt, "phase_idx") {
		t.Fatalf("%v %s", isErr, txt)
	}
	j.ok(sid, "get_bench_trends", `{"benchmark":"load-only","metric":"unload_s"}`)
	ag := j.ok(sid, "get_bench_aggregates", `{"suite":"quick","benchmark":"load-only","profile":"mcp-alpha","env":"same"}`)
	if ag["runs_considered"] != 1.0 || len(ag["benchmarks"].([]any)) != 1 || ag["benchmarks"].([]any)[0].(map[string]any)["benchmark_id"] != "load-only" {
		t.Fatalf("aggregates: %v", ag)
	}

	// a second run, cancelled (or too late: both are fine, the start is audited)
	run2 := j.ok(sid, "run_suite", `{"id":"quick","comment":"test"}`)
	id2 := run2["id"].(string)
	j.call(sid, "cancel_run", `{"id":"`+id2+`","comment":"test"}`)
	eventually(t, "second run over", func() bool {
		s := j.ok(sid, "get_run", `{"id":"`+id2+`"}`)["state"]
		return s == "completed" || s == "cancelled"
	})
	j.ok(sid, "delete_suite", `{"id":"quick"}`)
	j.ok(sid, "delete_benchmark", `{"id":"load-only"}`)

	// the audit log: section bench, source and actor as asked
	es := j.waitAudit("bench audit", "section=bench", 9)
	type row struct{ source, actor, action, target string }
	var rows []row
	for _, e := range es {
		rows = append(rows, row{e["source"].(string), e["actor"].(string), e["action"].(string), e["target"].(string)})
	}
	find := func(w row) bool {
		for _, r := range rows {
			if r == w {
				return true
			}
		}
		return false
	}
	for _, w := range []row{
		{"mcp", "bench-agent (via claude-code 1.0)", "create", "benchmark:load-only"},
		{"ui", "alex", "create", "benchmark:rest-b"},
		{"rest", "anonymous", "create", "benchmark:rest-c"},
		{"mcp", "claude-code 1.0", "update", "benchmark:load-only"},
		{"mcp", "claude-code 1.0", "create", "suite:quick"},
		{"mcp", "claude-code 1.0", "update", "suite:quick"},
		{"mcp", "claude-code 1.0", "run_start", "suite:quick"},
		{"mcp", "claude-code 1.0", "delete", "suite:quick"},
		{"mcp", "claude-code 1.0", "delete", "benchmark:load-only"},
	} {
		if !find(w) {
			t.Errorf("missing audit row %+v in %+v", w, rows)
		}
	}
	for _, e := range es {
		if e["action"] == "create" && e["target"] == "benchmark:load-only" && (e["after"] == nil || len(e["diff"].([]any)) == 0) {
			t.Errorf("create entry has no after/diff: %v", e)
		}
	}
	if n := len(j.auditEntries("section=bench&action=run_start&source=mcp")); n != 2 {
		t.Errorf("both run starts must be audited, got %d: %v", n, rows)
	}
}

// A benchmark run's loads are sent to /api/load on loopback by the runner:
// they carry the run id (so the history tags them) and are audited as the
// system working (source system, actor "bench run <id>"), never as a human.
func TestBenchRunLoadsAreTaggedAndAuditedAsSystem(t *testing.T) {
	j := newJoined(t)
	sid := j.session("claude-code")
	j.ok(sid, "create_profile", `{"profile":`+joinedProfile+`}`)
	eventually(t, "profile live", func() bool { return j.sched.Config().Profiles["mcp-alpha"] != nil })
	j.ok(sid, "create_benchmark", `{"benchmark":{"id":"lo","profile":"mcp-alpha","phases":[{"type":"load"},{"type":"unload"}]}}`)
	j.ok(sid, "create_suite", `{"suite":{"id":"s","benchmarks":["lo"],"cooldown_s":0,"only_when_idle":false}}`)
	id := j.ok(sid, "run_suite", `{"id":"s","comment":"test"}`)["id"].(string)
	eventually(t, "run completed", func() bool { return j.ok(sid, "get_run", `{"id":"`+id+`"}`)["state"] == "completed" })

	sys := j.waitAudit("system load audited", "source=system&section=runtime", 2)
	if sys[0]["action"] != "load" || sys[0]["actor"] != "bench run "+id || sys[0]["target"] != "mcp-alpha" || sys[1]["action"] != "unload" {
		t.Fatalf("%v", sys)
	}
	if a := sys[0]["after"].(map[string]any); a["bench_run"] != id {
		t.Fatalf("after: %v", a)
	}
	// no human-looking runtime entry for the run's own calls
	for _, src := range []string{"rest", "mcp", "ui"} {
		if es := j.auditEntries("section=runtime&source=" + src); len(es) != 0 {
			t.Fatalf("a benchmark load was audited as %s: %v", src, es)
		}
	}
	// the history tags the load
	eventually(t, "load history tagged", func() bool {
		m := jsonOf(t, body(t, j.get("/api/history?limit=500")))
		for _, x := range m["events"].([]any) {
			ev := x.(map[string]any)
			if d, _ := ev["data"].(map[string]any); d["bench_run"] == id && strings.HasPrefix(ev["type"].(string), "load") {
				return true
			}
		}
		return false
	})
}

// The bench_* settings are ordinary settings of the configuration document:
// they are changed through the same tools and say they apply at restart.
func TestBenchSettingsAreConfigurationSettings(t *testing.T) {
	j := newJoined(t)
	sid := j.session("claude-code")
	j.ok(sid, "create_profile", `{"profile":`+joinedProfile+`}`)
	r := j.ok(sid, "update_settings", `{"settings":{"bench_regress_load_pct":33,"bench_max_run_minutes":7}}`)
	if rr, _ := r["restart_required"].([]any); len(rr) == 0 {
		t.Fatalf("bench_* settings apply at restart: %v", r)
	}
	_, s := j.cdo("GET", "/api/config/settings", "", nil)
	set := s["settings"].(map[string]any)
	if set["bench_regress_load_pct"] != 33.0 || set["bench_max_run_minutes"] != 7.0 {
		t.Fatalf("%v", set)
	}
}
