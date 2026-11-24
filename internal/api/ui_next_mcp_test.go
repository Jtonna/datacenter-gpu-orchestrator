package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// the fixture's full keys (scripts/ui-fixture/keys.mjs): fixture text, shown
// only by the Created and Rotated panels
const (
	mcpCreatedKey = "ptl_fixture0example0key0not0real0001"
	mcpRotatedKey = "ptl_fixture0example0key0rotated00002"
)

// mcpCopy is a deep copy of a fixture answer, to change one field.
func mcpCopy(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// mcpAgent is one key of the fixture's GET /api/agents answer.
func mcpAgent(t *testing.T, list any, id string) map[string]any {
	t.Helper()
	for _, a := range list.(map[string]any)["agents"].([]any) {
		if m := a.(map[string]any); m["id"] == id {
			return mcpCopy(t, m)
		}
	}
	t.Fatalf("no key %s in the fixture", id)
	return nil
}

// The MCP access-key pages (boards MCP, MCP-New, MCP-Created, MCP-Manage):
// the list with its statuses, New key, the key shown once, Manage in the
// side panel; every write with a comment in X-Comment and no key or actor
// sent; the server's sentence on a refusal; the full key gone once its
// panel closes.
func TestUINextMCP(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--keys", "normal"}, "/api/agents", "/api/status")
	st := fetch["/api/status"]
	noKey := []string{"X-Actor", "X-Agent-Key", "Authorization"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "mcp: the list", "route": "#/mcp", "status": st,
			"expect": []string{"Access keys", "5 active", "New key", "Name", "Key", "Can", "Expires", "Last used", "Status",
				"ops-bot", "ptl_••••7c2e", "Observe · switch zones", "Desktop assistant", "Never", "Home dashboard", "Expires in 3 days",
				"test-script", "Expired", "old-laptop", "Revoked", "report writer", "legacy", "lls_3f9a01••••", "Observe · switch zones · load and unload · change settings"},
			"has": []string{"tr.gone[data-key=ag_000000000005]", "tr.gone[data-key=ag_000000000006]", "a.go[href='#/mcp?key=ag_000000000001']"}},
		map[string]any{"name": "mcp: a row opens Manage", "route": "#/mcp", "status": st, "click": []string{"tr[data-key=ag_000000000002]"}, "hash": "#/mcp?key=ag_000000000002"},
		map[string]any{"name": "mcp: New key", "route": "#/mcp?new=1", "status": st,
			"page": []string{"New key", "Name", "Key expires", "30 days", "90 days", "Custom date", "Never", "Permissions", "Observe", "zones, runs, tokens",
				"Switch zones on or off", "Load and unload models", "Change settings", "Allow", "Ask me", "Off", "Cancel", "Create key"},
			"pageHas": []string{"aside.side input#mcp-name", "aside.side div[data-perm=observe] div.seg", "aside.side button[data-act=create]"}},
		map[string]any{"name": "mcp: New key, Ask me noted", "route": "#/mcp?new=1", "status": st, "click": []string{"button[data-level=ask]"},
			"page": []string{"Ask me is kept, but approvals are not built yet"}},
		map[string]any{"name": "mcp: Manage", "route": "#/mcp?key=ag_000000000001", "status": st,
			"page": []string{"Manage access", "ops-bot", "Active", "last used", "requests in 7 days, 3 refused", "Key expires", "Permissions",
				"MCP settings for the agent", "<paste your key>", "ptl_••••7c2e", "Rotate key", "Revoke key", "Save", "Ask me is kept"},
			"has": []string{"tr.sel[data-key=ag_000000000001]"}, "notOnPage": []string{mcpCreatedKey, "ptl_fixture"}},
		map[string]any{"name": "mcp: Manage a revoked key", "route": "#/mcp?key=ag_000000000006", "status": st,
			"page": []string{"was revoked", "cannot be changed", "Close"}, "pageNone": []string{"button[data-act=save]", "button[data-act=rotate]", "input#mcp-name"}},
		map[string]any{"name": "mcp: Manage an expired key", "route": "#/mcp?key=ag_000000000005", "status": st, "page": []string{"This key expired", "Rotate key"}},
		map[string]any{"name": "mcp: Manage a legacy key", "route": "#/mcp?key=ag_000000000007", "status": st, "page": []string{"Made before permissions existed", "4 requests in 7 days"}},
	}})
	none := fixtureAnswers(t, []string{"--keys", "none"}, "/api/agents")
	none["/api/status"] = st
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": none, "cases": []any{
		map[string]any{"name": "mcp: no key yet", "route": "#/mcp", "status": st, "expect": []string{"Access keys", "0 active", "No access keys yet", "New key"}, "none": []string{"table"}},
	}})

	// the writes, each in a run of its own
	post := fixturePost(t, "--keys", "normal")
	_, created := post("/api/agents", `{"name":"grafana-reader"}`)
	_, rotated := post("/api/agents/ag_000000000001/rotate", "")
	// after a create the server lists the new key too
	withNew := mcpCopy(t, fetch["/api/agents"])
	withNew["agents"] = append([]any{created["agent"]}, withNew["agents"].([]any)...)
	madeIt := map[string]any{"POST /api/agents": created, "/api/agents": withNew}
	run := func(name string, extra map[string]any, c map[string]any) {
		t.Helper()
		f := map[string]any{}
		for k, v := range fetch {
			f[k] = v
		}
		for k, v := range extra {
			f[k] = v
		}
		c["name"], c["status"] = name, st
		if c["noHeader"] == nil {
			c["noHeader"] = noKey
		}
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{c}})
	}
	comment := func(text, button string) []any {
		return []any{button, map[string]any{"type": "textarea.dlg-comment", "value": text}, "div.dlg-foot button.pri"}
	}
	typeName := func(v string) map[string]any { return map[string]any{"type": "input#mcp-name", "value": v} }

	run("mcp: create, the key shown once", map[string]any{"POST /api/agents": created}, map[string]any{"route": "#/mcp?new=1",
		"click":        append([]any{typeName("grafana-reader")}, comment("for the dashboard", "button[data-act=create]")...),
		"page":         []string{"grafana-reader", "Created", "shown once", mcpCreatedKey, "MCP settings for the agent", "Bearer " + mcpCreatedKey, "Manage access", "Done"},
		"fetchedQuery": []string{`^POST /api/agents \{"name":"grafana-reader","expires_at":\d{13},"permissions":\{"observe":"allow","zones":"off","models":"off","settings":"off"\}\}$`},
		"sentHeader":   []string{`^X-Comment: for the dashboard$`},
		"noHeaderLine": []string{"ptl_"}})
	run("mcp: Done drops the key", madeIt, map[string]any{"route": "#/mcp?new=1",
		"click":     append(append([]any{typeName("grafana-reader")}, comment("for the dashboard", "button[data-act=create]")...), "button[data-act=done]"),
		"notOnPage": []string{mcpCreatedKey, "shown once"}, "expect": []string{"grafana-reader"}, "hash": "#/mcp"})
	run("mcp: Manage access after Created shows no key", madeIt, map[string]any{"route": "#/mcp?new=1",
		"click":     append(append([]any{typeName("grafana-reader")}, comment("for the dashboard", "button[data-act=create]")...), "button[data-act=manage]"),
		"page":      []string{"Manage access", "<paste your key>", "Rotate key"},
		"notOnPage": []string{mcpCreatedKey}})
	run("mcp: create refused, the server's sentence", map[string]any{"POST /api/agents": map[string]any{"$status": 409,
		"$body": map[string]any{"error": map[string]any{"message": "an agent named ops-bot already exists: two agents with one name could not be told apart in the audit log"}}}},
		map[string]any{"route": "#/mcp?new=1", "click": append([]any{typeName("ops-bot")}, comment("again", "button[data-act=create]")...),
			"page": []string{"an agent named ops-bot already exists", "Create key"}, "notOnPage": []string{"shown once"}})
	run("mcp: create without a name", nil, map[string]any{"route": "#/mcp?new=1", "click": []any{"button[data-act=create]"},
		"page": []string{"The key needs a name"}, "notFetched": []string{"^POST"}})

	renamed := mcpAgent(t, fetch["/api/agents"], "ag_000000000001")
	renamed["name"] = "ops-bot-2"
	run("mcp: save a rename", map[string]any{"PATCH /api/agents/ag_000000000001": map[string]any{"agent": renamed, "changed": true}}, map[string]any{"route": "#/mcp?key=ag_000000000001",
		"click":        append([]any{typeName("ops-bot-2")}, comment("clearer name", "button[data-act=save]")...),
		"fetchedQuery": []string{`^PATCH /api/agents/ag_000000000001 \{"name":"ops-bot-2"\}$`},
		"sentHeader":   []string{`^X-Comment: clearer name$`}, "page": []string{"Saved ops-bot-2"}})
	run("mcp: save never", map[string]any{"PATCH /api/agents/ag_000000000001": map[string]any{"agent": renamed, "changed": true}}, map[string]any{"route": "#/mcp?key=ag_000000000001",
		"click":        append([]any{"button[data-exp=never]"}, comment("keep it", "button[data-act=save]")...),
		"fetchedQuery": []string{`^PATCH /api/agents/ag_000000000001 \{"expires_at":0\}$`}})
	run("mcp: save refused, the server's sentence", map[string]any{"PATCH /api/agents/ag_000000000001": map[string]any{"$status": 400,
		"$body": map[string]any{"error": map[string]any{"message": "expires_at must be in the future (or 0 for never)"}}}},
		map[string]any{"route": "#/mcp?key=ag_000000000001", "click": append([]any{typeName("ops-bot-2")}, comment("x", "button[data-act=save]")...),
			"page": []string{"expires_at must be in the future (or 0 for never)"}})
	run("mcp: rotate, the new key shown once", map[string]any{"POST /api/agents/ag_000000000001/rotate": rotated}, map[string]any{"route": "#/mcp?key=ag_000000000001",
		"click":        comment("leaked in a log", "button[data-act=rotate]"),
		"fetchedQuery": []string{`^POST /api/agents/ag_000000000001/rotate$`},
		"sentHeader":   []string{`^X-Comment: leaked in a log$`},
		"page":         []string{"Key rotated", "Rotated", mcpRotatedKey, "The old key stopped working"}})
	revoked := mcpAgent(t, fetch["/api/agents"], "ag_000000000001")
	revoked["status"], revoked["revoked"], revoked["revoked_at"] = "revoked", true, 1.7955e12
	run("mcp: revoke", map[string]any{"DELETE /api/agents/ag_000000000001": map[string]any{"agent": revoked, "revoked": true}}, map[string]any{"route": "#/mcp?key=ag_000000000001",
		"click":        []any{"button[data-act=revoke]", map[string]any{"type": "textarea.dlg-comment", "value": "laptop sold"}, "div.dlg-foot button.dng"},
		"fetchedQuery": []string{`^DELETE /api/agents/ag_000000000001$`},
		"sentHeader":   []string{`^X-Comment: laptop sold$`},
		"page":         []string{"Revoked ops-bot"}}) // the revoked panel itself: "Manage a revoked key"
}

// The MCP page's Tools section (not on the boards; the classic MCP page
// listed them): every tool the server's MCP part offers, by group, with
// what it does and the permission it needs; read-only. Read from a real
// scheduler's GET /api/mcp/tools.
func TestUINextMCPTools(t *testing.T) {
	e, _, _ := accessEnv(t)
	front := httptest.NewServer(e.api)
	defer front.Close()
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": front.URL, "cases": []any{
		map[string]any{"name": "mcp tools: the list", "route": "#/mcp", "status": "fetch",
			"expect": []string{"Access keys", "Tools", "tools an agent can call", "version test", "Read", "Configuration", "Benchmarks", "Load and unload",
				"get_status", "load_model", "pause_node", "update_settings", "What it does", "Needs", "Observe", "Switch zones on or off", "Load and unload models", "Change settings", "Changes or removes"},
			"has":  []string{"section.mcp-tools details.mcp-tg[data-group] summary", "tr[data-tool=load_model]", "tr[data-tool=get_status]"},
			"none": []string{"section.mcp-tools button", "section.mcp-tools input"}},
	}})
	// a scheduler without the MCP part answers 503: the section says so, the keys still draw
	fetch := fixtureAnswers(t, []string{"--keys", "normal"}, "/api/agents", "/api/status")
	fetch["/api/mcp/tools"] = map[string]any{"$status": 503, "$body": map[string]any{"error": map[string]any{"message": "the MCP server is not part of this process"}}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "mcp tools: not available", "route": "#/mcp", "status": fetch["/api/status"],
			"expect": []string{"ops-bot", "Tools", "The tool list could not be read", "the MCP server is not part of this process"}},
	}})
}
