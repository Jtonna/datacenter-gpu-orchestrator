package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeInner records every in-process request and answers with a canned reply.
type fakeInner struct {
	mu     sync.Mutex
	reqs   []seen
	status int
	body   string
}

type seen struct {
	Method, Path, RawQuery, Body string
	Header                       http.Header
	IsMCP                        bool
	Actor                        string
}

func (f *fakeInner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	isMCP, actor := SourceFromContext(r.Context())
	f.mu.Lock()
	f.reqs = append(f.reqs, seen{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone(), isMCP, actor})
	st, body := f.status, f.body
	f.mu.Unlock()
	if st == 0 {
		st = 200
	}
	if body == "" && st != 204 {
		body = `{"ok":true}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	io.WriteString(w, body)
}

func (f *fakeInner) last() seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func rpc(t *testing.T, h http.Handler, headers map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

type rpcOut struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func call(t *testing.T, h http.Handler, headers map[string]string, method, params string) rpcOut {
	t.Helper()
	if params == "" {
		params = "{}"
	}
	_, b := rpc(t, h, headers, `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`)
	var o rpcOut
	if err := json.Unmarshal([]byte(b), &o); err != nil {
		t.Fatalf("bad reply %q: %v", b, err)
	}
	return o
}

type toolOut struct {
	Content []struct{ Type, Text string }
	IsError bool
}

func callTool(t *testing.T, h http.Handler, headers map[string]string, name, args string) toolOut {
	t.Helper()
	o := call(t, h, headers, "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)
	if o.Error != nil {
		t.Fatalf("%s: rpc error %+v", name, o.Error)
	}
	var out toolOut
	if err := json.Unmarshal(o.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func newTest(inner http.Handler) *Server {
	return New(inner, Options{Version: "9.9.9", Guide: "# guide"})
}

func TestInitialize(t *testing.T) {
	h := newTest(&fakeInner{})
	for _, tc := range []struct{ ask, want string }{
		{"2025-06-18", "2025-06-18"}, {"2024-11-05", "2024-11-05"}, {"1999-01-01", protocolVersions[0]}, {"", protocolVersions[0]},
	} {
		resp, b := rpc(t, h, nil, `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"`+tc.ask+`","capabilities":{},"clientInfo":{"name":"tester","version":"1.2"}}}`)
		var o struct {
			ID     int
			Result struct {
				ProtocolVersion string
				Capabilities    map[string]any
				ServerInfo      struct{ Name, Version string }
				Instructions    string
			}
		}
		if err := json.Unmarshal([]byte(b), &o); err != nil {
			t.Fatal(err)
		}
		if o.ID != 7 || o.Result.ProtocolVersion != tc.want || o.Result.ServerInfo.Name != "llm-scheduler" || o.Result.ServerInfo.Version != "9.9.9" {
			t.Errorf("%q: %s", tc.ask, b)
		}
		if !strings.Contains(o.Result.Instructions, "plan_load") || o.Result.Capabilities["tools"] == nil {
			t.Errorf("instructions or capabilities missing: %s", b)
		}
		if resp.Header.Get("Mcp-Session-Id") == "" {
			t.Error("no session id")
		}
	}
}

func TestNotificationsAndPing(t *testing.T) {
	h := newTest(&fakeInner{})
	resp, b := rpc(t, h, nil, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != 202 || b != "" {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
	if o := call(t, h, nil, "ping", ""); o.Error != nil || string(o.Result) != "{}" {
		t.Fatalf("%+v", o)
	}
}

func TestMalformedAndInvalid(t *testing.T) {
	h := newTest(&fakeInner{})
	for name, tc := range map[string]struct {
		body string
		code int
		http int
	}{
		"not json":       {`{`, codeParse, 400},
		"empty":          {``, codeParse, 400},
		"wrong version":  {`{"jsonrpc":"1.0","id":1,"method":"ping"}`, codeInvalid, 400},
		"no method":      {`{"jsonrpc":"2.0","id":1}`, codeInvalid, 400},
		"unknown method": {`{"jsonrpc":"2.0","id":1,"method":"nope/nope"}`, codeMethod, 200},
		"call no name":   {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, codeParams, 200},
		"unknown tool":   {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"drop_database","arguments":{}}}`, codeParams, 200},
		"args not obj":   {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_status","arguments":[1]}}`, codeParams, 200},
		"empty batch":    {`[]`, codeInvalid, 400},
	} {
		resp, b := rpc(t, h, nil, tc.body)
		var o rpcOut
		json.Unmarshal([]byte(b), &o)
		if o.Error == nil || o.Error.Code != tc.code || resp.StatusCode != tc.http {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
}

func TestBatch(t *testing.T) {
	h := newTest(&fakeInner{})
	_, b := rpc(t, h, nil, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`)
	var out []rpcOut
	if err := json.Unmarshal([]byte(b), &out); err != nil || len(out) != 2 {
		t.Fatalf("%v %s", err, b)
	}
}

func TestTransportRules(t *testing.T) {
	h := newTest(&fakeInner{})
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "/mcp", nil))
	if get.Code != 405 || get.Header().Get("Allow") == "" {
		t.Errorf("GET: %d", get.Code)
	}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Errorf("text/plain: %d", rec.Code)
	}
	for origin, want := range map[string]int{"https://evil.example.com": 403, "http://192.168.1.5:3000": 200, "http://localhost:5173": 200, "": 200} {
		resp, _ := rpc(t, h, map[string]string{"Origin": origin}, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		if resp.StatusCode != want {
			t.Errorf("origin %q: %d, want %d", origin, resp.StatusCode, want)
		}
	}
	if resp, _ := rpc(t, h, map[string]string{"MCP-Protocol-Version": "1999-01-01"}, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != 400 {
		t.Errorf("bad protocol header: %d", resp.StatusCode)
	}
}

func TestToolsListSchemas(t *testing.T) {
	h := newTest(&fakeInner{})
	o := call(t, h, nil, "tools/list", "")
	var res struct {
		Tools []struct {
			Name, Description string
			InputSchema       map[string]any
			Annotations       map[string]any
		}
	}
	if err := json.Unmarshal(o.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != len(expected) {
		t.Fatalf("%d tools, table has %d", len(res.Tools), len(expected))
	}
	for _, tl := range res.Tools {
		if _, ok := expected[tl.Name]; !ok {
			t.Errorf("tool %s not in the table", tl.Name)
		}
		if tl.Name != strings.ToLower(tl.Name) || strings.ContainsAny(tl.Name, " -") {
			t.Errorf("name %q is not snake_case", tl.Name)
		}
		if len(tl.Description) < 20 || strings.Count(tl.Description, ". ") > 4 {
			t.Errorf("%s: description %q", tl.Name, tl.Description)
		}
		sc := tl.InputSchema
		props, _ := sc["properties"].(map[string]any)
		if sc["type"] != "object" || props == nil || sc["additionalProperties"] != false {
			t.Errorf("%s: schema %v", tl.Name, sc)
		}
		req, ok := sc["required"].([]any)
		if !ok {
			t.Errorf("%s: required must be an array", tl.Name)
		}
		for _, r := range req {
			if _, ok := props[r.(string)]; !ok {
				t.Errorf("%s: required %v not in properties", tl.Name, r)
			}
		}
		for p, v := range props {
			pm := v.(map[string]any)
			if pm["description"] == "" || pm["description"] == nil {
				t.Errorf("%s.%s: no description", tl.Name, p)
			}
		}
		ro, _ := tl.Annotations["readOnlyHint"].(bool)
		_, hasActor := props["actor"]
		if ro == hasActor && tl.Name != "query_audit" { // query_audit's actor is a filter
			t.Errorf("%s: readOnly=%v but actor arg=%v", tl.Name, ro, hasActor)
		}
		if _, hasDestructive := tl.Annotations["destructiveHint"]; ro == hasDestructive {
			t.Errorf("%s: destructive annotation on a read-only tool or missing on a write tool", tl.Name)
		}
	}
	// Spot-check the semantics agents rely on.
	ann := map[string]map[string]any{}
	for _, tl := range res.Tools {
		ann[tl.Name] = tl.Annotations
	}
	if ann["unload_model"]["destructiveHint"] != true || ann["rollback_config"]["destructiveHint"] != true || ann["delete_profile"]["destructiveHint"] != true {
		t.Error("destructive tools must say so")
	}
	if ann["get_status"]["readOnlyHint"] != true || ann["load_model"]["destructiveHint"] != false {
		t.Error("read-only / non-destructive annotations wrong")
	}
}

// expected is the contract: every tool, a sample call, and the one REST
// request it must make.
var expected = map[string]struct {
	args                      string
	method, path, query, body string
	header                    map[string]string
}{
	"get_status":            {`{}`, "GET", "/api/status", "", "", nil},
	"list_models":           {`{}`, "GET", "/v1/models", "", "", nil},
	"list_nodes":            {`{}`, "GET", "/api/status", "", "", nil},
	"get_queue":             {`{}`, "GET", "/api/status", "", "", nil},
	"list_profiles":         {`{}`, "GET", "/api/profiles", "", "", nil},
	"get_profile":           {`{"id":"a b"}`, "GET", "/api/config/profiles/a b", "", "", nil},
	"list_model_files":      {`{}`, "GET", "/api/models", "", "", nil},
	"get_model_file":        {`{"file":"gemma 4.gguf"}`, "GET", "/api/models/gemma 4.gguf", "", "", nil},
	"plan_load":             {`{"model":"gemma"}`, "POST", "/api/plan", "", `{"model":"gemma"}`, nil},
	"query_history":         {`{"since":5,"type":"nodeLost,loadFinished","severity":"warn","limit":10,"order":"asc","q":"x y"}`, "GET", "/api/history", "limit=10&order=asc&q=x+y&severity=warn&since=5&type=nodeLost%2CloadFinished", "", nil},
	"get_history_summary":   {`{"since":1,"until":2}`, "GET", "/api/history/summary", "since=1&until=2", "", nil},
	"get_availability":      {`{"node":"zone-a","from":"7d","step":"day"}`, "GET", "/api/nodes/availability", "from=7d&node=zone-a&step=day", "", nil},
	"query_logs":            {`{"query":"node_id:=\"02:00:00:00:00:01\" _time:5m","start":"1h"}`, "GET", "/api/logs/query", "limit=100&query=node_id%3A%3D%2202%3A00%3A00%3A00%3A00%3A01%22+_time%3A5m&start=1h", "", nil},
	"get_system_info":       {`{}`, "GET", "/api/system", "", "", nil},
	"get_system_health":     {`{}`, "GET", "/api/health", "", "", nil},
	"get_config":            {`{}`, "GET", "/api/config", "", "", nil},
	"get_config_schema":     {`{}`, "GET", "/api/config/schema", "", "", nil},
	"list_config_revisions": {`{"limit":5}`, "GET", "/api/config/revisions", "limit=5", "", nil},
	"get_config_revision":   {`{"revision":12}`, "GET", "/api/config/revisions/12", "", "", nil},
	"query_audit":           {`{"source":"mcp","actor":"bot","section":"tiers","target":"t","action":"update","q":"z","limit":3,"since":"2026-01-01","until":"2026-02-01"}`, "GET", "/api/audit", "action=update&actor=bot&limit=3&q=z&section=tiers&since=2026-01-01&source=mcp&target=t&until=2026-02-01", "", nil},
	"list_requests":         {`{"status":"done,failed","profile":"p","node":"n1","from":1,"to":2,"bench":"no","caller":"bot","limit":5,"before":9}`, "GET", "/api/requests", "before=9&bench=no&caller=bot&from=1&limit=5&node=n1&profile=p&status=done%2Cfailed&to=2", "", nil},
	"get_request":           {`{"id":1795000000000000}`, "GET", "/api/requests/1795000000000000", "", "", nil},
	"get_request_summary":   {`{"from":1,"to":2,"step":"day","profile":"p"}`, "GET", "/api/requests/summary", "from=1&profile=p&step=day&to=2", "", nil},
	"list_benchmarks":       {`{}`, "GET", "/api/bench/benchmarks", "", "", nil},
	"get_benchmark":         {`{"id":"b1"}`, "GET", "/api/bench/benchmarks/b1", "", "", nil},
	"list_suites":           {`{}`, "GET", "/api/bench/suites", "", "", nil},
	"get_suite":             {`{"id":"s1"}`, "GET", "/api/bench/suites/s1", "", "", nil},
	"get_bench_state":       {`{}`, "GET", "/api/bench/state", "", "", nil},
	"list_runs":             {`{"suite":"s1","state":"completed","limit":5,"kind":"suite","benchmark":"b"}`, "GET", "/api/bench/runs", "benchmark=b&kind=suite&limit=5&state=completed&suite=s1", "", nil},
	"get_run_measurements":  {`{"id":"br-1","step":0,"metric":"load_s"}`, "GET", "/api/bench/runs/br-1/measurements", "metric=load_s&step=0", "", nil},
	"query_measurements":    {`{"benchmark":"b","metric":"ttft_ms","since":"7d","limit":50,"ctx":8192,"placement":"any:2"}`, "GET", "/api/bench/measurements", "benchmark=b&ctx=8192&limit=50&metric=ttft_ms&placement=any%3A2&since=7d", "", nil},
	"list_bench_metrics":    {`{}`, "GET", "/api/bench/metrics", "", "", nil},
	"expand_group":          {`{"id":"g"}`, "GET", "/api/bench/groups/g/expand", "", "", nil},
	"get_run_report":        {`{"id":"r1","step":0,"format":"md"}`, "GET", "/api/bench/runs/r1/report", "format=md&step=0", "", nil},
	"list_run_removals":     {`{"limit":5}`, "GET", "/api/bench/removals", "limit=5", "", nil},
	"preview_run_removals":  {`{"days":183}`, "GET", "/api/bench/removals/preview", "days=183", "", nil},
	"pin_run":               {`{"id":"r1"}`, "PUT", "/api/bench/runs/r1/pin", "", "", nil},
	"unpin_run":             {`{"id":"r1"}`, "DELETE", "/api/bench/runs/r1/pin", "", "", nil},
	"list_run_transcripts":  {`{"id":"r1"}`, "GET", "/api/bench/runs/r1/transcripts", "", "", nil},
	"get_step_transcript":   {`{"id":"r1","step":2,"format":"md"}`, "GET", "/api/bench/runs/r1/steps/2/transcript", "format=md", "", nil},
	"run_benchmark":         {`{"id":"lo","note":"n","only_when_idle":false,"inputs":{"model":"m.gguf"}}`, "POST", "/api/bench/benchmarks/lo/run", "", `{"inputs":{"model":"m.gguf"},"note":"n","only_when_idle":false}`, nil},
	"get_run":               {`{"id":"br-1"}`, "GET", "/api/bench/runs/br-1", "", "", nil},
	"export_run":            {`{"id":"br-1","format":"csv","detail":"phases"}`, "GET", "/api/bench/runs/br-1/export", "detail=phases&format=csv", "", nil},
	"get_bench_aggregates":  {`{"suite":"s","benchmark":"b","profile":"p","since":"7d","until":"2","limit_runs":4,"env":"same"}`, "GET", "/api/bench/aggregates", "benchmark=b&env=same&limit_runs=4&profile=p&since=7d&suite=s&until=2", "", nil},
	"get_bench_trends":      {`{"benchmark":"b","metric":"task_points","scorer":"kit","limit":9}`, "GET", "/api/bench/trends", "benchmark=b&limit=9&metric=task_points&scorer=kit", "", nil},
	"get_bench_schema":      {`{}`, "GET", "/api/bench/schema", "", "", nil},
	"list_external_results": {`{}`, "GET", "/api/bench/external", "", "", nil},
	"get_external_results":  {`{"id":"ctx-1"}`, "GET", "/api/bench/external/ctx-1", "", "", nil},
	"list_task_sets":        {`{}`, "GET", "/api/bench/task-sets", "", "", nil},
	"get_task_set":          {`{"id":"coding","version":2}`, "GET", "/api/bench/task-sets/coding", "version=2", "", nil},
	"get_run_tasks":         {`{"id":"br-1","step":0,"answers":true}`, "GET", "/api/bench/runs/br-1/tasks", "answers=true&step=0", "", nil},
	"get_task_answer":       {`{"id":"br-1","result":7}`, "GET", "/api/bench/runs/br-1/tasks/7", "", "", nil},
	"create_task_set":       {`{"task_set":{"id":"coding","version":1}}`, "POST", "/api/bench/task-sets", "", `{"id":"coding","version":1}`, nil},
	"put_task_scores":       {`{"id":"br-1","scorer":"kit-tests","scorer_version":"1","scores":[{"step":0,"task":"t1","points":17,"max_points":17}]}`, "PUT", "/api/bench/runs/br-1/scores/kit-tests", "", `{"scorer_version":"1","scores":[{"max_points":17,"points":17,"step":0,"task":"t1"}]}`, nil},
	"load_model":            {`{"model":"gemma","actor":"task-7"}`, "POST", "/api/load", "", `{"model":"gemma"}`, nil},
	"unload_model":          {`{"model":"gemma","force":true}`, "POST", "/api/unload", "", `{"force":true,"model":"gemma"}`, nil},
	"update_settings":       {`{"settings":{"queue_timeout":"2m"},"comment":"why","dry_run":true,"if_revision":4}`, "PATCH", "/api/config/settings", "dry_run=1", `{"queue_timeout":"2m"}`, map[string]string{"X-Comment": "why", "If-Match": "4"}},
	"create_profile":        {`{"profile":{"id":"p","model":"p.gguf"}}`, "POST", "/api/config/profiles", "", `{"id":"p","model":"p.gguf"}`, nil},
	"update_profile":        {`{"id":"p","profile":{"id":"p","ctx":4096}}`, "PUT", "/api/config/profiles/p", "", `{"ctx":4096,"id":"p"}`, nil},
	"delete_profile":        {`{"id":"p","comment":"gone"}`, "DELETE", "/api/config/profiles/p", "", "", map[string]string{"X-Comment": "gone"}},
	"set_tiers":             {`{"value":[{"priority":1}]}`, "PUT", "/api/config/tiers", "", `[{"priority":1}]`, nil},
	"set_exclude_nodes":     {`{"value":["ps5-1"]}`, "PUT", "/api/config/exclude-nodes", "", `["ps5-1"]`, nil},
	"set_no_auto_nodes":     {`{"value":["laptop-1"]}`, "PUT", "/api/config/no-auto-nodes", "", `["laptop-1"]`, nil},
	"set_node_overrides":    {`{"value":{"x":{"ip":"1.2.3.4"}}}`, "PUT", "/api/config/node-overrides", "", `{"x":{"ip":"1.2.3.4"}}`, nil},
	"set_recovery_settings": {`{"value":{"enabled":true}}`, "PUT", "/api/config/recovery", "", `{"enabled":true}`, nil},
	"set_fleet_settings":    {`{"value":{"url":"u"},"dry_run":true}`, "PUT", "/api/config/fleet", "dry_run=1", `{"url":"u"}`, nil},
	"rollback_config":       {`{"revision":3,"if_revision":9}`, "POST", "/api/config/revisions/3/rollback", "", "", map[string]string{"If-Match": "9"}},
	"create_benchmark":      {`{"benchmark":{"id":"b"}}`, "POST", "/api/bench/benchmarks", "", `{"id":"b"}`, nil},
	"update_benchmark":      {`{"id":"b","benchmark":{"id":"b","repeats":2}}`, "PUT", "/api/bench/benchmarks/b", "", `{"id":"b","repeats":2}`, nil},
	"delete_benchmark":      {`{"id":"b"}`, "DELETE", "/api/bench/benchmarks/b", "", "", nil},
	"create_suite":          {`{"suite":{"id":"s"}}`, "POST", "/api/bench/suites", "", `{"id":"s"}`, nil},
	"update_suite":          {`{"id":"s","suite":{"id":"s"}}`, "PUT", "/api/bench/suites/s", "", `{"id":"s"}`, nil},
	"delete_suite":          {`{"id":"s"}`, "DELETE", "/api/bench/suites/s", "", "", nil},
	"run_suite":             {`{"id":"s","baseline":"br-0","note":"n"}`, "POST", "/api/bench/suites/s/run", "", `{"baseline":"br-0","note":"n"}`, nil},
	"cancel_run":            {`{"id":"br-2"}`, "POST", "/api/bench/runs/br-2/cancel", "", "", nil},
	"pause_node":            {`{"id":"02:00:00:00:00:01","comment":"fan noise"}`, "POST", "/api/nodes/02:00:00:00:00:01/pause", "", `{"comment":"fan noise"}`, nil},
	"unpause_node":          {`{"id":"n1","comment":"fixed"}`, "POST", "/api/nodes/n1/unpause", "", `{"comment":"fixed"}`, nil},
	"force_unload_node":     {`{"id":"n1","comment":"hung"}`, "POST", "/api/nodes/n1/force-unload", "", `{"comment":"hung"}`, nil},
}

func TestEveryToolMapsToItsRESTRequest(t *testing.T) {
	f := &fakeInner{body: `{"profiles":[],"instances":[],"view":{"nodes":[]},"queue":[]}`}
	h := newTest(f)
	for name, e := range expected {
		out := callTool(t, h, nil, name, e.args)
		if out.IsError {
			t.Errorf("%s: %+v", name, out)
			continue
		}
		got := f.last()
		if got.Method != e.method || got.Path != e.path || got.RawQuery != e.query || got.Body != e.body {
			t.Errorf("%s: got %s %s?%s body %q\nwant %s %s?%s body %q", name, got.Method, got.Path, got.RawQuery, got.Body, e.method, e.path, e.query, e.body)
		}
		for k, v := range e.header {
			if got.Header.Get(k) != v {
				t.Errorf("%s: header %s = %q, want %q", name, k, got.Header.Get(k), v)
			}
		}
		if !got.IsMCP {
			t.Errorf("%s: request not marked as MCP", name)
		}
		if _, hasGuardHeaders := got.Header["X-Comment"]; hasGuardHeaders && e.header["X-Comment"] == "" {
			t.Errorf("%s: unexpected X-Comment", name)
		}
	}
	if len(expected) != len(allTools()) {
		t.Errorf("table has %d tools, server %d", len(expected), len(allTools()))
	}
}

func TestArgumentValidation(t *testing.T) {
	f := &fakeInner{}
	h := newTest(f)
	for name, tc := range map[string]struct{ tool, args string }{
		"missing required": {"plan_load", `{}`},
		"unknown arg":      {"get_status", `{"nope":1}`},
		"wrong type":       {"unload_model", `{"model":"m","force":"yes"}`},
		"not integer":      {"get_config_revision", `{"revision":1.5}`},
		"bad enum":         {"get_bench_trends", `{"benchmark":"b","metric":"speed"}`},
		"empty id":         {"get_benchmark", `{"id":""}`},
		"read-only actor":  {"get_status", `{"actor":"x"}`},
		"bare config arg":  {"load_model", `{"model":"m","dry_run":true}`},
	} {
		n := len(f.reqs)
		out := callTool(t, h, nil, tc.tool, tc.args)
		if !out.IsError || len(f.reqs) != n {
			t.Errorf("%s: %+v (requests %d -> %d)", name, out, n, len(f.reqs))
		}
	}
}

func TestErrorsAndNotMerged(t *testing.T) {
	f := &fakeInner{status: 404, body: `{"error":{"message":"no such path: /api/config","type":"invalid_request_error","code":404}}`}
	h := newTest(f)
	out := callTool(t, h, nil, "get_config", `{}`)
	if !out.IsError || !strings.Contains(out.Content[0].Text, "does not have that endpoint") {
		t.Errorf("not merged: %+v", out)
	}
	// A handler's own 404 is an ordinary error with the body.
	f.body = `{"error":{"message":"no such benchmark x"}}`
	out = callTool(t, h, nil, "get_benchmark", `{"id":"x"}`)
	if !out.IsError || !strings.Contains(out.Content[0].Text, "HTTP 404") || !strings.Contains(out.Content[0].Text, "no such benchmark") {
		t.Errorf("own 404: %+v", out)
	}
	f.status, f.body = 422, `{"error":{"message":"bad"}}`
	if out = callTool(t, h, nil, "create_benchmark", `{"benchmark":{}}`); !out.IsError || !strings.Contains(out.Content[0].Text, "HTTP 422") {
		t.Errorf("422: %+v", out)
	}
	f.status, f.body = 204, ``
	if out = callTool(t, h, nil, "delete_suite", `{"id":"s"}`); out.IsError || !strings.Contains(out.Content[0].Text, "no content") {
		t.Errorf("204: %+v", out)
	}
}

func TestGetProfileFallsBackToProfileList(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/config/") {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"message":"no such path: `+r.URL.Path+`"}}`)
			return
		}
		io.WriteString(w, `{"profiles":[{"id":"alpha","model":"a.gguf"},{"id":"beta"}]}`)
	})
	h := newTest(inner)
	out := callTool(t, h, nil, "get_profile", `{"id":"alpha"}`)
	if out.IsError || !strings.Contains(out.Content[0].Text, "a.gguf") {
		t.Fatalf("%+v", out)
	}
	if out = callTool(t, h, nil, "get_profile", `{"id":"zzz"}`); !out.IsError {
		t.Fatalf("%+v", out)
	}
}

func TestSourceAndActor(t *testing.T) {
	if ok, _ := SourceFromContext(context.Background()); ok {
		t.Fatal("a plain context must not look like MCP")
	}
	f := &fakeInner{}
	var bridged []string
	h := New(f, Options{Version: "1", Bridge: func(ctx context.Context, actor string) context.Context {
		bridged = append(bridged, actor)
		return ctx
	}})
	resp, _ := rpc(t, h, nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code","version":"2.1.0"}}}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	hdr := map[string]string{"Mcp-Session-Id": sid}

	callTool(t, h, hdr, "load_model", `{"model":"m"}`)
	if g := f.last(); !g.IsMCP || g.Actor != "claude-code 2.1.0" {
		t.Errorf("actor from clientInfo: %+v", g)
	}
	callTool(t, h, hdr, "load_model", `{"model":"m","actor":"nightly-bench"}`)
	if g := f.last(); g.Actor != "nightly-bench (via claude-code 2.1.0)" {
		t.Errorf("actor argument: %q", g.Actor)
	}
	callTool(t, h, nil, "get_status", `{}`)
	if g := f.last(); !g.IsMCP || g.Actor != "mcp-client" {
		t.Errorf("no session: %+v", g)
	}
	if len(bridged) != 3 || bridged[0] != "claude-code 2.1.0" {
		t.Errorf("bridge calls: %v", bridged)
	}

	// Headers a REST client sends cannot fake the marker: the inner handler
	// only trusts the context, and a request that did not come through the
	// MCP server has none.
	plain := httptest.NewRequest("POST", "/api/load", nil)
	plain.Header.Set("X-Source", "mcp")
	plain.Header.Set("X-Actor", "someone")
	if ok, _ := SourceFromContext(plain.Context()); ok {
		t.Error("headers must not set the marker")
	}
	// The session store is bounded.
	small := New(f, Options{MaxSessions: 2})
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, small.newSession("c"))
	}
	if len(small.sess) != 2 || small.clientOf(ids[0]) != "mcp-client" || small.clientOf(ids[3]) != "c" {
		t.Errorf("session bound: %d", len(small.sess))
	}
	small.dropSession(ids[3])
	if small.clientOf(ids[3]) != "mcp-client" {
		t.Error("DELETE did not drop the session")
	}
}

func TestStatusSummary(t *testing.T) {
	full := `{"time":"t","shutting_down":false,"instances":[{"profile":"gemma","state":"starting","model_file":"g.gguf","cmd":"HUGE","in_flight":0,"load_progress":0.4,"load_eta_s":30,
	 "nodes":[{"id":"aa","addr":"x","budget_mib":9000,"memory":[{"name":"RPC0","total_mib":4000},{"name":"CPU","total_mib":500}]}]}],
	 "queue":[{"profile":"big","age":3,"waiters":2,"claimed":false,"wait_reason":"does not fit","created":"c"}],
	 "quarantines":[],"backoffs":[],"recent_loads":[1,2,3,4,5,6,7],"learned_needs":{"gemma":{}},
	 "view":{"age":1.5,"connected":true,"stale":false,"nodes":[{"id":"aa","hostname":"ps5-1","online":true,"unplaced":"agent_unreachable","unplaced_message":"cannot reach 192.0.2.2:50060","total_mib":12000,"free_mib":11000,"addr":"192.0.2.2","priority":1}]}}`
	h := newTest(&fakeInner{body: full})
	out := callTool(t, h, nil, "get_status", `{}`)
	var st map[string]any
	if err := json.Unmarshal([]byte(out.Content[0].Text), &st); err != nil {
		t.Fatal(err)
	}
	in := st["instances"].([]any)[0].(map[string]any)
	n0 := in["nodes"].([]any)[0].(map[string]any)
	if in["profile"] != "gemma" || in["load_progress"] != 0.4 || in["cmd"] != nil || n0["hostname"] != "ps5-1" || n0["memory_mib"] != 4500.0 {
		t.Errorf("instance: %v", in)
	}
	if len(st["recent_loads"].([]any)) != 5 || st["learned_needs"] != nil || len(st["queue"].([]any)) != 1 {
		t.Errorf("summary: %v", st)
	}
	if nd := st["nodes"].([]any)[0].(map[string]any); nd["addr"] != nil || nd["hostname"] != "ps5-1" ||
		nd["unplaced"] != "agent_unreachable" || nd["unplaced_message"] != "cannot reach 192.0.2.2:50060" {
		t.Errorf("node: %v", nd)
	}
	if len(out.Content[0].Text) >= len(full) {
		t.Error("summary is not smaller than the full status")
	}
	var verbose map[string]any
	json.Unmarshal([]byte(callTool(t, h, nil, "get_status", `{"verbose":true}`).Content[0].Text), &verbose)
	if verbose["learned_needs"] == nil {
		t.Error("verbose must return the full object")
	}
	var q map[string]any
	json.Unmarshal([]byte(callTool(t, h, nil, "get_queue", `{}`).Content[0].Text), &q)
	if len(q["queue"].([]any)) != 1 || q["instances"] != nil {
		t.Errorf("queue: %v", q)
	}
	var nodes map[string]any
	json.Unmarshal([]byte(callTool(t, h, nil, "list_nodes", `{}`).Content[0].Text), &nodes)
	if nodes["nodes"].([]any)[0].(map[string]any)["addr"] != "192.0.2.2" {
		t.Errorf("nodes: %v", nodes)
	}
}

func TestResources(t *testing.T) {
	h := newTest(&fakeInner{body: `{"x":1}`})
	list := call(t, h, nil, "resources/list", "")
	for _, u := range []string{uriStatus, uriConfig, uriGuide} {
		if !strings.Contains(string(list.Result), u) {
			t.Errorf("%s missing from %s", u, list.Result)
		}
	}
	for u, want := range map[string]string{uriStatus: `{\"x\":1}`, uriConfig: `{\"x\":1}`, uriGuide: "# guide"} {
		o := call(t, h, nil, "resources/read", `{"uri":"`+u+`"}`)
		if o.Error != nil || !strings.Contains(string(o.Result), want) {
			t.Errorf("%s: %+v %s", u, o.Error, o.Result)
		}
	}
	if o := call(t, h, nil, "resources/read", `{"uri":"llm-scheduler://nope"}`); o.Error == nil {
		t.Error("unknown resource must error")
	}
	_ = url.Values{}
}
