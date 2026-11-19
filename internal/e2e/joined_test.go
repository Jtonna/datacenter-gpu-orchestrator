package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The joined system through the real binary: a seed configuration imported at
// start, then everything an agent does over MCP (a profile, a load-only
// benchmark, a suite), the run watched on the WebSocket, its results and the
// aggregates read back, and the audit log showing every step with the right
// source and actor.
func TestJoinedSystemEndToEnd(t *testing.T) {
	c := benchCluster(t) // seed config imported with import-config, then the binary started on the database

	resp, b := mcpPost(t, c, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e-agent","version":"3"}}}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("no session: %s", b)
	}
	mcpPost(t, c, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	n := 1
	tool := func(name, args string) map[string]any {
		t.Helper()
		n++
		_, b := mcpPost(t, c, sid, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"%s","arguments":%s}}`, n, name, args))
		var o struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
			Error any
		}
		if err := json.Unmarshal([]byte(b), &o); err != nil || o.Error != nil || o.Result.IsError || len(o.Result.Content) == 0 {
			t.Fatalf("%s %s: %v %s", name, args, err, b)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(o.Result.Content[0].Text), &m); err != nil {
			m = map[string]any{"text": o.Result.Content[0].Text}
		}
		return m
	}

	// the seed was imported as revision 1; a profile through MCP is revision 2
	if cfg := tool("get_config", `{}`); cfg["revision"] != 1.0 || cfg["configured"] != true {
		t.Fatalf("seed not imported: %v", cfg["revision"])
	}
	if r := tool("create_profile", `{"profile":{"id":"joined","model":"tiny.gguf","ctx":1024,"needs_mib":500,"allow_tiers":[1]},"comment":"e2e","actor":"joiner"}`); r["revision"] != 2.0 {
		t.Fatalf("create_profile: %v", r)
	}
	if st, body := c.get("/api/config/profiles"); st != 200 || !strings.Contains(body, `"joined"`) {
		t.Fatalf("GET /api/config/profiles: %d %s", st, body)
	}
	c.waitFor("the new profile live", 10*time.Second, func() bool { return c.jsonGet("/api/status")["config_revision"] == 2.0 })
	tool("create_benchmark", `{"benchmark":{"id":"joined-load","name":"Joined load","description":"cold load of the new profile","profile":"joined","phases":[{"type":"load"},{"type":"unload"}]}}`)
	tool("create_suite", `{"suite":{"id":"joined-suite","name":"Joined","benchmarks":["joined-load"],"cooldown_s":0}}`)

	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(c.url, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	var mu sync.Mutex
	var bench []map[string]any
	go func() {
		for {
			_, b, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			if m["type"] == "bench" {
				mu.Lock()
				bench = append(bench, m["data"].(map[string]any))
				mu.Unlock()
			}
		}
	}()

	run := tool("run_suite", `{"id":"joined-suite","note":"joined e2e","actor":"joiner"}`)
	id, _ := run["id"].(string)
	if id == "" {
		t.Fatalf("run_suite: %v", run)
	}
	c.waitFor("the run to finish", 60*time.Second, func() bool {
		s := tool("get_run", `{"id":"`+id+`"}`)["state"]
		return s == "completed" || s == "failed" || s == "cancelled"
	})
	got := tool("get_run", `{"id":"`+id+`"}`)
	if got["state"] != "completed" {
		t.Fatalf("run ended %v: %v", got["state"], got["error"])
	}
	if env, _ := got["env"].(map[string]any); env == nil || env["config_revision"] != "2" {
		t.Fatalf("the environment record must carry the real config revision (2): %v", got["env"])
	}
	step := got["steps"].([]any)[0].(map[string]any)
	if step["outcome"] != "ready" || step["load"] == nil {
		t.Fatalf("step: %v", step)
	}

	// the WebSocket carried overall percent and the current benchmark, monotonic, 100 at the end
	c.waitFor("the final progress message", 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range bench {
			if r, _ := m["run"].(map[string]any); m["kind"] == "progress" && r != nil && r["percent"] == 100.0 {
				return true
			}
		}
		return false
	})
	mu.Lock()
	msgs := append([]map[string]any(nil), bench...)
	mu.Unlock()
	last, count, sawCurrent := -1.0, 0, false
	for _, m := range msgs {
		if m["kind"] != "progress" {
			continue
		}
		r := m["run"].(map[string]any)
		p := r["percent"].(float64)
		if p < last {
			t.Fatalf("progress went back: %v after %v", p, last)
		}
		last = p
		count++
		if cur, _ := r["current"].(map[string]any); cur != nil && cur["benchmark_id"] == "joined-load" {
			sawCurrent = true
		}
	}
	if count < 2 || !sawCurrent {
		t.Fatalf("%d progress messages, current benchmark seen: %v", count, sawCurrent)
	}

	// aggregates and schema through MCP
	ag := tool("get_bench_aggregates", `{"suite":"joined-suite","env":"same"}`)
	if ag["runs_considered"] != 1.0 || len(ag["benchmarks"].([]any)) != 1 {
		t.Fatalf("aggregates: %v", ag)
	}
	if sc := tool("get_bench_schema", `{}`); sc["phase_types"] == nil {
		t.Fatalf("schema: %v", sc)
	}

	// the load the run made is tagged in the history and audited as the system, not as a person
	c.waitFor("a history record of the load tagged with the run", 10*time.Second, func() bool {
		h := c.jsonGet("/api/history?type=loadStarted,loadFinished&limit=200")
		for _, e := range h["events"].([]any) {
			if d, _ := e.(map[string]any)["data"].(map[string]any); d["bench_run"] == id {
				return true
			}
		}
		return false
	})

	// the audit log: each step, with the right source
	type row struct{ source, actor, section, action, target string }
	var rows []row
	c.waitFor("the audit log to hold every step", 10*time.Second, func() bool {
		rows = nil
		for _, x := range c.jsonGet("/api/audit?order=asc&limit=500")["entries"].([]any) {
			e := x.(map[string]any)
			rows = append(rows, row{e["source"].(string), e["actor"].(string), e["section"].(string), e["action"].(string), e["target"].(string)})
		}
		return len(rows) >= 7
	})
	has := func(w row) bool {
		for _, r := range rows {
			if r == w {
				return true
			}
		}
		return false
	}
	want := []row{
		{"mcp", "joiner (via e2e-agent 3)", "profiles", "create", "joined"},
		{"mcp", "e2e-agent 3", "bench", "create", "benchmark:joined-load"},
		{"mcp", "e2e-agent 3", "bench", "create", "suite:joined-suite"},
		{"mcp", "joiner (via e2e-agent 3)", "bench", "run_start", "suite:joined-suite"},
		{"system", "bench run " + id, "runtime", "load", "joined"},
		{"system", "bench run " + id, "runtime", "unload", "joined"},
	}
	for _, w := range want {
		if !has(w) {
			t.Errorf("audit log lacks %+v\n%+v", w, rows)
		}
	}
	if rows[0].source != "import" || rows[0].action != "import" {
		t.Errorf("the first entry should be the import: %+v", rows[0])
	}
	for _, r := range rows {
		if r.section == "runtime" && r.source != "system" {
			t.Errorf("a benchmark load is audited as a person's action: %+v", r)
		}
	}
	// a person's load over REST is rest, through MCP is mcp
	c.do("POST", "/api/load", `{"model":"tiny:2k"}`)
	tool("load_model", `{"model":"tiny:2k"}`)
	c.waitFor("rest and mcp loads audited", 10*time.Second, func() bool {
		return len(c.jsonGet("/api/audit?section=runtime&action=load&source=rest")["entries"].([]any)) == 1 &&
			len(c.jsonGet("/api/audit?section=runtime&action=load&source=mcp")["entries"].([]any)) == 1
	})
}
