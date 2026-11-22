package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	agenttest.Eventually(t, 20*time.Second, what, f)
}

// mcpPost sends one JSON-RPC message to the real binary's /mcp endpoint.
func mcpPost(t *testing.T, c *cluster, sid, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", c.url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// An agent's path: initialize, list tools, read status and profiles, and plan
// a load, all over HTTP against the real binary; REST on the same port keeps
// working.
func TestMCPEndToEnd(t *testing.T) {
	c := withPS5(t, nil)
	resp, b := mcpPost(t, c, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`)
	var init struct {
		Result struct {
			ProtocolVersion string
			ServerInfo      struct{ Name, Version string }
			Instructions    string
		}
	}
	if err := json.Unmarshal([]byte(b), &init); err != nil || init.Result.ServerInfo.Name != "llm-scheduler" || init.Result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("initialize: %v %s", err, b)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no session id")
	}
	if r, _ := mcpPost(t, c, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.StatusCode != 202 {
		t.Fatalf("initialized: %d", r.StatusCode)
	}
	_, b = mcpPost(t, c, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if n := strings.Count(b, `"inputSchema"`); n < 46 {
		t.Fatalf("only %d tools", n)
	}

	tool := func(id int, name, args string) string {
		_, b := mcpPost(t, c, sid, `{"jsonrpc":"2.0","id":`+string(rune('0'+id))+`,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
		var o struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
			Error any
		}
		if err := json.Unmarshal([]byte(b), &o); err != nil || o.Error != nil || o.Result.IsError || len(o.Result.Content) == 0 {
			t.Fatalf("%s: %v %s", name, err, b)
		}
		return o.Result.Content[0].Text
	}
	var st struct {
		Instances []any
		Nodes     []struct {
			Hostname string
			Online   bool
		}
	}
	if err := json.Unmarshal([]byte(tool(3, "get_status", `{}`)), &st); err != nil || len(st.Nodes) == 0 {
		t.Fatalf("get_status: %v %+v", err, st)
	}
	var profs struct{ Profiles []struct{ ID string } }
	if err := json.Unmarshal([]byte(tool(4, "list_profiles", `{}`)), &profs); err != nil || len(profs.Profiles) != 4 {
		t.Fatalf("list_profiles: %v %+v", err, profs)
	}
	if p := tool(5, "plan_load", `{"model":"tiny:2k"}`); !strings.Contains(p, `"would_start":true`) && !strings.Contains(p, `"action"`) {
		t.Fatalf("plan_load: %s", p)
	}
	// a write tool: load, then watch the instance become ready in the summary
	l := tool(6, "load_model", `{"model":"tiny:2k","actor":"e2e"}`)
	t.Logf("load_model -> %s", l)
	if !strings.Contains(l, `"loading"`) {
		t.Fatalf("load_model: %s", l)
	}
	waitFor(t, "tiny:2k ready via get_status", func() bool {
		var s struct {
			Instances []struct{ Profile, State string }
		}
		json.Unmarshal([]byte(tool(7, "get_status", `{}`)), &s)
		return len(s.Instances) == 1 && s.Instances[0].Profile == "tiny:2k" && s.Instances[0].State == "ready"
	})
	t.Logf("get_status -> %s", tool(8, "get_status", `{}`))
	// the same data over REST, same port
	if s, _ := c.get("/api/status"); s != 200 {
		t.Fatalf("REST status: %d", s)
	}
	// GET on /mcp is not an event stream
	if s, _ := c.get("/mcp"); s != 405 {
		t.Fatalf("GET /mcp: %d", s)
	}
}
