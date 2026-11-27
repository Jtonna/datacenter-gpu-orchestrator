package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
)

// The MCP tools run through the real REST handler of a real scheduler on
// fakes: the tools whose endpoints exist on this branch behave like REST.

type mcpOut struct {
	Text    string
	IsError bool
}

func mcpTool(t *testing.T, h http.Handler, name, args string) mcpOut {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var o struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
		Error *struct{ Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil || o.Error != nil || len(o.Result.Content) == 0 {
		t.Fatalf("%s: %v %s", name, err, rec.Body.String())
	}
	return mcpOut{o.Result.Content[0].Text, o.Result.IsError}
}

func mcpJSON(t *testing.T, h http.Handler, name, args string) map[string]any {
	t.Helper()
	o := mcpTool(t, h, name, args)
	if o.IsError {
		t.Fatalf("%s %s: %s", name, args, o.Text)
	}
	return jsonOf(t, o.Text)
}

func TestMCPReadAndControlToolsThroughTheRealAPI(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	h := mcpserver.New(e.api, mcpserver.Options{Version: "test"})

	// Nothing loaded yet: the summary is empty but complete.
	s0 := mcpJSON(t, h, "get_status", `{}`)
	if len(s0["instances"].([]any)) != 0 || s0["queue"] == nil || s0["nodes"] == nil {
		t.Fatalf("%v", s0)
	}
	profs := mcpJSON(t, h, "list_profiles", `{}`)["profiles"].([]any)
	if len(profs) != 3 {
		t.Fatalf("%v", profs)
	}
	if m := mcpJSON(t, h, "list_models", `{}`); m["data"] == nil {
		t.Fatalf("%v", m)
	}
	// without the configuration service the config tools say so (503), they do not crash
	if o := mcpTool(t, h, "get_profile", `{"id":"alpha"}`); !o.IsError || !strings.Contains(o.Text, "HTTP 503") {
		t.Fatalf("%+v", o)
	}

	// plan, then load, then the instance shows up in the model-centric status.
	plan := mcpJSON(t, h, "plan_load", `{"model":"alpha"}`)
	if plan["model"] != "alpha" || plan["action"] == nil {
		t.Fatalf("%v", plan)
	}
	if o := mcpTool(t, h, "plan_load", `{"model":"nonexistent"}`); !o.IsError || !strings.Contains(o.Text, "HTTP 4") {
		t.Fatalf("unknown model: %+v", o)
	}
	if l := mcpJSON(t, h, "load_model", `{"model":"alpha","actor":"test"}`); l["msg"] != "loading" {
		t.Fatalf("%v", l)
	}
	eventually(t, "alpha ready in the summary", func() bool {
		ins := mcpJSON(t, h, "get_status", `{}`)["instances"].([]any)
		return len(ins) == 1 && ins[0].(map[string]any)["state"] == "ready"
	})
	ins := mcpJSON(t, h, "get_status", `{}`)["instances"].([]any)[0].(map[string]any)
	if ins["profile"] != "alpha" || ins["model_file"] != "a.gguf" || ins["in_flight"] == nil {
		t.Fatalf("%v", ins)
	}
	if q := mcpJSON(t, h, "get_queue", `{}`); q["queue"] == nil {
		t.Fatalf("%v", q)
	}
	if n := mcpJSON(t, h, "list_nodes", `{}`); n["nodes"] == nil {
		t.Fatalf("%v", n)
	}
	if u := mcpJSON(t, h, "unload_model", `{"model":"alpha"}`); u == nil {
		t.Fatal("no unload answer")
	}
	eventually(t, "alpha unloaded", func() bool {
		return len(mcpJSON(t, h, "get_status", `{}`)["instances"].([]any)) == 0
	})

	// history tools read the real store
	seed(st)
	hist := mcpJSON(t, h, "query_history", `{"type":"nodeLost","severity":"error"}`)
	if evs := hist["events"].([]any); len(evs) != 1 || evs[0].(map[string]any)["type"] != "nodeLost" {
		t.Fatalf("%v", hist)
	}
	if sum := mcpJSON(t, h, "get_history_summary", `{}`); sum["by_type"] == nil {
		t.Fatalf("%v", sum)
	}
	if sys := mcpJSON(t, h, "get_system_info", `{}`); sys["version"] == nil && sys["listen"] == nil {
		t.Fatalf("%v", sys)
	}

	// the configuration, audit and bench endpoints answer 503 with their reason when their service is off
	for _, tool := range []string{"get_config", "list_config_revisions", "query_audit", "get_bench_aggregates"} {
		o := mcpTool(t, h, tool, `{}`)
		if !o.IsError || !strings.Contains(o.Text, "HTTP 503") {
			t.Errorf("%s: %+v", tool, o)
		}
	}
	if o := mcpTool(t, h, "get_config_schema", `{}`); o.IsError || !strings.Contains(o.Text, `"fields"`) {
		t.Errorf("the settings schema needs no service: %+v", o)
	}
	if o := mcpTool(t, h, "set_tiers", `{"value":[]}`); !o.IsError || !strings.Contains(o.Text, "HTTP 503") {
		t.Errorf("set_tiers: %+v", o)
	}
	// benchmarks are off without data_dir: the real 503 text reaches the agent
	if o := mcpTool(t, h, "list_benchmarks", `{}`); !o.IsError || !strings.Contains(o.Text, "data_dir") {
		t.Errorf("bench off: %+v", o)
	}
}

func TestMCPBenchmarkToolsThroughTheRealAPI(t *testing.T) {
	e := benchEnv(t)
	h := mcpserver.New(e.api, mcpserver.Options{Version: "test"})

	if o := mcpTool(t, h, "create_benchmark", `{"comment":"test","benchmark":{"id":"a","profile":"ghost","requests":[{"prompt":"x","max_tokens":1}]}}`); !o.IsError || !strings.Contains(o.Text, "HTTP 422") {
		t.Fatalf("validation error must reach the agent: %+v", o)
	}
	mcpJSON(t, h, "create_benchmark", `{"comment":"test","benchmark":`+apiBench+`}`)
	if b := mcpJSON(t, h, "get_benchmark", `{"id":"alpha-cold"}`); b["profile"] != "alpha" || b["source"] != "api" {
		t.Fatalf("%v", b)
	}
	if l := mcpJSON(t, h, "list_benchmarks", `{}`); len(l["benchmarks"].([]any)) != 1 {
		t.Fatalf("%v", l)
	}
	mcpJSON(t, h, "create_suite", `{"comment":"test","suite":{"id":"nightly","name":"Nightly","benchmarks":["alpha-cold"],"cooldown_s":0,"only_when_idle":false}}`)
	if s := mcpJSON(t, h, "get_suite", `{"id":"nightly"}`); s["id"] != "nightly" {
		t.Fatalf("%v", s)
	}
	mcpJSON(t, h, "update_suite", `{"comment":"test","id":"nightly","suite":{"id":"nightly","name":"Nightly 2","benchmarks":["alpha-cold"],"cooldown_s":0,"only_when_idle":false}}`)
	if l := mcpJSON(t, h, "list_suites", `{}`)["suites"].([]any); len(l) != 1 || l[0].(map[string]any)["name"] != "Nightly 2" {
		t.Fatalf("%v", l)
	}

	run := mcpJSON(t, h, "run_suite", `{"id":"nightly","note":"via mcp","comment":"test"}`)
	id := run["id"].(string)
	if run["state"] != "queued" || run["note"] != "via mcp" {
		t.Fatalf("%v", run)
	}
	if st := mcpJSON(t, h, "get_bench_state", `{}`); st["enabled"] != true {
		t.Fatalf("%v", st)
	}
	eventually(t, "run completed", func() bool {
		return mcpJSON(t, h, "get_run", `{"id":"`+id+`"}`)["state"] == "completed"
	})
	if l := mcpJSON(t, h, "list_runs", `{"suite":"nightly","state":"completed","limit":5}`)["runs"].([]any); len(l) != 1 {
		t.Fatalf("%v", l)
	}
	if md := mcpTool(t, h, "export_run", `{"id":"`+id+`","format":"markdown"}`); md.IsError || !strings.Contains(md.Text, "alpha") || !strings.Contains(md.Text, "|") {
		t.Fatalf("%+v", md)
	}
	if csv := mcpTool(t, h, "export_run", `{"id":"`+id+`","format":"csv"}`); csv.IsError || !strings.HasPrefix(csv.Text, "run_id,") {
		t.Fatalf("%+v", csv)
	}
	if o := mcpTool(t, h, "cancel_run", `{"id":"`+id+`","comment":"test"}`); !o.IsError || !strings.Contains(o.Text, "HTTP 409") {
		t.Fatalf("cancel finished run: %+v", o)
	}
	if tr := mcpJSON(t, h, "get_bench_trends", `{"benchmark":"alpha-cold","metric":"load_s"}`); tr["points"] == nil {
		t.Fatalf("%v", tr)
	}
	// a benchmark in use cannot be deleted; after the suite goes, it can
	if o := mcpTool(t, h, "delete_benchmark", `{"id":"alpha-cold","comment":"done"}`); !o.IsError || !strings.Contains(o.Text, "HTTP 409") {
		t.Fatalf("%+v", o)
	}
	mcpJSON(t, h, "update_benchmark", `{"comment":"test","id":"alpha-cold","benchmark":{"id":"alpha-cold","profile":"alpha","repeats":1,"requests":[{"name":"q","prompt":"hi","max_tokens":4}]}}`)
	if o := mcpTool(t, h, "delete_suite", `{"id":"nightly","comment":"done"}`); o.IsError {
		t.Fatalf("%+v", o)
	}
	if o := mcpTool(t, h, "delete_benchmark", `{"id":"alpha-cold","comment":"done"}`); o.IsError {
		t.Fatalf("%+v", o)
	}
}
