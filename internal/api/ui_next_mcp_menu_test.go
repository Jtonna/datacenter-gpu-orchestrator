package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The MCP page with a docked panel (Manage, New key, Created): no section
// menu beside it, as the MCP boards draw it and the health popup does; the
// menu comes back as it was when the panel closes or moves over the page.
func TestUINextMCPPanelClosesTheMenu(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--keys", "normal"}, "/api/agents", "/api/status")
	st := fetch["/api/status"]
	post := fixturePost(t, "--keys", "normal")
	_, created := post("/api/agents", `{"name":"grafana-reader"}`)
	withNew := mcpCopy(t, fetch["/api/agents"])
	withNew["agents"] = append([]any{created["agent"]}, withNew["agents"].([]any)...)
	madeF := map[string]any{"POST /api/agents": created, "/api/agents": withNew, "/api/status": st}
	comment := []any{"button[data-act=create]", map[string]any{"type": "textarea.dlg-comment", "value": "for the dashboard"}, "div.dlg-foot button.pri"}
	typeName := map[string]any{"type": "input#mcp-name", "value": "grafana-reader"}

	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "mcp menu: open beside the list", "route": "#/mcp", "status": st, "width": 1536,
			"pageHas": []string{"div.menu-head"}, "pageNone": []string{"div.app.menu-closed", "aside.side"}},
		map[string]any{"name": "mcp menu: closed beside Manage", "route": "#/mcp?key=ag_000000000001", "status": st, "width": 1536,
			"pageHas": []string{"div.app.menu-closed", "aside.side.docked"}},
		map[string]any{"name": "mcp menu: closed beside New key", "route": "#/mcp?new=1", "status": st, "width": 1536,
			"pageHas": []string{"div.app.menu-closed", "aside.side.docked"}},
		map[string]any{"name": "mcp menu: back when the panel closes", "route": "#/mcp?key=ag_000000000001", "status": st, "width": 1536,
			"click": []string{"aside.side button.x"}, "pageNone": []string{"div.app.menu-closed", "aside.side"}},
		map[string]any{"name": "mcp menu: a row opens Manage, the menu closes", "route": "#/mcp", "status": st, "width": 1536,
			"click": []string{"tr[data-key=ag_000000000002]"}, "pageHas": []string{"div.app.menu-closed", "aside.side.docked"}},
		map[string]any{"name": "mcp menu: over the page (narrow) it stays", "route": "#/mcp?key=ag_000000000001", "status": st, "width": 1280,
			"pageHas": []string{"aside.side"}, "pageNone": []string{"div.app.menu-closed", "aside.side.docked"}},
		map[string]any{"name": "mcp menu: back when the window narrows", "route": "#/mcp?key=ag_000000000001", "status": st, "width": 1536, "widthAfter": 1280,
			"pageHas": []string{"aside.side"}, "pageNone": []string{"div.app.menu-closed"}},
	}})

	// the Created panel (the key shown once) closes it too, and Done brings it back
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": madeF, "cases": []any{
		map[string]any{"name": "mcp menu: closed beside Created", "route": "#/mcp?new=1", "status": st, "width": 1536,
			"click": append([]any{typeName}, comment...), "page": []string{"shown once"}, "pageHas": []string{"div.app.menu-closed", "aside.side.docked"}},
	}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": madeF, "cases": []any{
		map[string]any{"name": "mcp menu: Done brings it back", "route": "#/mcp?new=1", "status": st, "width": 1536,
			"click": append(append([]any{typeName}, comment...), "button[data-act=done]"), "pageNone": []string{"div.app.menu-closed", "aside.side"}},
	}})

	// closed by hand before: it stays closed after the panel (as it was)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "mcp menu: closed by hand stays closed", "route": "#/mcp", "status": st, "width": 1536,
			"click":   []string{"button.menu-close", "tr[data-key=ag_000000000002]", "aside.side button.x"},
			"pageHas": []string{"div.app.menu-closed"}, "pageNone": []string{"aside.side"}},
	}})
}

// The list's Can column is never cut with an ellipsis: beside a docked
// panel at 1536 (menu closed) the list is wider than the narrow rules, and
// in a narrower list the cell wraps.
func TestUINextMCPCanNeverCut(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("ui", "next", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	js, err := os.ReadFile(filepath.Join("ui", "next", "views", "mcp.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"h('th.c-can'", "h('td.c-can'"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("mcp.js: %s missing (the Can column's class)", want)
		}
	}
	// everywhere: the cell wraps rather than widen the table past the list
	// (the list scrolls sideways then, and Status is cut at its edge)
	if !regexp.MustCompile(`
\.mcp-tbl td\.c-can\{[^}]*white-space:normal`).MatchString(css) {
		t.Error("app.css: .mcp-tbl td.c-can does not wrap (white-space:normal) outside the narrow block")
	}
	// the narrow block: td.c-can wraps and shows everything
	m := regexp.MustCompile(`(?s)@container \(max-width:600px\)\{(.*?)\n\}`).FindStringSubmatch(css)
	if m == nil {
		t.Fatal("app.css: the narrow MCP list block (@container max-width:600px) is gone")
	}
	rule := regexp.MustCompile(`\.mcp-tbl td\.c-can\{([^}]*)\}`).FindStringSubmatch(m[1])
	if rule == nil {
		t.Fatal("app.css: no .mcp-tbl td.c-can rule in the narrow block")
	}
	for _, want := range []string{"white-space:normal", "overflow:visible", "text-overflow:clip"} {
		if !strings.Contains(rule[1], want) {
			t.Errorf("td.c-can in the narrow block: %s missing (got %q)", want, rule[1])
		}
	}
	// nothing else gives the Can cell an ellipsis
	for _, r := range regexp.MustCompile(`([^{}]*c-can[^{}]*)\{([^}]*)\}`).FindAllStringSubmatch(css, -1) {
		if strings.Contains(r[2], "ellipsis") {
			t.Errorf("app.css: %s gives the Can column an ellipsis", strings.TrimSpace(r[1]))
		}
	}
	// 1536 wide, menu closed, panel docked: the list's container is wider than
	// 600 (1536 - rail - panel - the page's side padding), so the narrow rules
	// do not apply there
	px := func(name string) int {
		m := regexp.MustCompile(`--` + name + `:(\d+)px`).FindStringSubmatch(css)
		if m == nil {
			t.Fatalf("app.css: --%s is gone", name)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if w := 1536 - px("rail-w") - px("side-w") - 2*32; w <= 600 {
		t.Errorf("at 1536 the list would be %d wide: the narrow rules would cut it", w)
	}
}
