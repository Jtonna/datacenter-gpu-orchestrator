package api

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
)

// A planned or blocked scenario is a suite with a status and a reason and no
// benchmark yet. REST and MCP accept it, list it with its status and reason,
// and refuse to run it with 409 and the reason. A normal suite still needs a
// benchmark.
func TestPlannedSuiteIsListedButNotRun(t *testing.T) {
	e := benchEnv(t)
	planned := `{"id":"five-consoles","name":"Consoles only, 5","status":"planned","reason":"the fifth console is not set up yet","benchmarks":[]}`
	if st, b, _ := e.do("POST", "/api/bench/suites", planned); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/suites", `{"id":"empty","benchmarks":[]}`); st != 422 || !strings.Contains(b, "benchmarks") {
		t.Fatalf("a normal empty suite: %d %s", st, b)
	}
	if st, b, _ := e.do("POST", "/api/bench/suites", `{"id":"noreason","status":"blocked","benchmarks":[]}`); st != 422 || !strings.Contains(b, "reason") {
		t.Fatalf("without a reason: %d %s", st, b)
	}
	st, b, _ := e.do("GET", "/api/bench/suites", "")
	if st != 200 || !strings.Contains(b, `"status":"planned"`) || !strings.Contains(b, "the fifth console is not set up yet") {
		t.Fatalf("%d %s", st, b)
	}
	st, b, _ = e.do("POST", "/api/bench/suites/five-consoles/run", "")
	if st != 409 || !strings.Contains(b, "planned") || !strings.Contains(b, "the fifth console is not set up yet") {
		t.Fatalf("run: %d %s", st, b)
	}
	if st, b, _ := e.do("GET", "/api/bench/runs", ""); st != 200 || strings.Contains(b, "five-consoles") {
		t.Fatalf("a run was queued: %s", b)
	}

	// The same through the MCP tools.
	h := mcpserver.New(e.api, mcpserver.Options{Version: "test"})
	mcpJSON(t, h, "create_suite", `{"suite":{"id":"gbe","status":"blocked","reason":"waiting for the switch"}}`)
	o := mcpTool(t, h, "run_suite", `{"id":"gbe"}`)
	if !o.IsError || !strings.Contains(o.Text, "waiting for the switch") {
		t.Fatalf("run_suite: %+v", o)
	}
}
