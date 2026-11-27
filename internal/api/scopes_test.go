package api

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/access"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

var updateDocs = flag.Bool("update-docs", false, "rewrite the access table in docs/runbook.md from routeScopes")

// registeredPatterns reads every pattern the api package registers on its
// mux (m.HandleFunc / m.Handle with a literal, or a literal plus an
// agentproto path constant).
func registeredPatterns(t *testing.T) []string {
	t.Helper()
	consts := map[string]string{"PathRegister": agentproto.PathRegister, "PathRelease": agentproto.PathRelease,
		"PathClientLogs": agentproto.PathClientLogs, "PathClientLogsConfig": agentproto.PathClientLogsConfig,
		"PathLogsQuery": agentproto.PathLogsQuery, "PathLogsPages": agentproto.PathLogsPages}
	var eval func(ast.Expr) (string, bool)
	eval = func(x ast.Expr) (string, bool) {
		switch v := x.(type) {
		case *ast.BasicLit:
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		case *ast.BinaryExpr:
			a, ok1 := eval(v.X)
			b, ok2 := eval(v.Y)
			return a + b, ok1 && ok2
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && id.Name == "agentproto" {
				s, ok := consts[v.Sel.Name]
				return s, ok
			}
		}
		return "", false
	}
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			p, ok := eval(call.Args[0])
			if !ok {
				t.Errorf("%s: a route pattern this test cannot read; add its constant to registeredPatterns", fset.Position(call.Pos()))
				return true
			}
			seen[p] = true
			return true
		})
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Every route the API registers has an entry in the scope table, and every
// entry is a registered route: a new route cannot be added without saying
// what a key needs for it.
func TestRouteScopes_EveryRouteHasAnEntry(t *testing.T) {
	reg := registeredPatterns(t)
	if len(reg) < 150 {
		t.Fatalf("only %d patterns found", len(reg))
	}
	isReg := map[string]bool{}
	for _, p := range reg {
		isReg[p] = true
		if _, ok := routeScopes[p]; !ok {
			t.Errorf("route %q has no entry in routeScopes (scopes.go)", p)
		}
	}
	for p := range routeScopes {
		if !isReg[p] {
			t.Errorf("routeScopes has %q, which is not a registered route", p)
		}
	}
	// the live mux matches each pattern to itself, so ServeHTTP finds the entry
	e := newEnv(t, okUpstream())
	for _, c := range [][3]string{{"POST", "/api/nodes/{id}/pause", "zones"}, {"GET", "/api/config/profiles/{id...}", "observe"},
		{"POST", "/api/plan", "observe"}, {"DELETE", "/api/models/{key...}", "settings"}, {"POST", "/api/bench/suites/{id}/run", "models"},
		{"GET", "/api/agents", "owner"}, {"POST", "/v1/chat/completions", "model_api"}, {"POST", "/api/pool/nodes/{id}/heartbeat", "agents"}} {
		if got := e.api.ScopeOf(c[0], c[1]); string(got) != c[2] {
			t.Errorf("%s %s: %s, want %s", c[0], c[1], got, c[2])
		}
	}
	// a pattern missing from the table fails closed
	if scopeOf("GET /api/new", "GET") != access.Observe || scopeOf("POST /api/new", "POST") != access.Settings {
		t.Error("a missing pattern does not fail closed")
	}
}

// accessDocTable is the runbook's table of what each permission covers,
// written from routeScopes.
func accessDocTable() string {
	var b strings.Builder
	b.WriteString("| Permission | Levels | Routes |\n|---|---|---|\n")
	for _, sc := range append(append([]access.Scope{}, access.Permissions...), access.Owner, access.Agents, access.ModelAPI, access.Open) {
		levels := "allow, ask, off"
		switch sc {
		case access.Observe:
			levels = "allow, off"
		case access.Owner:
			levels = "no key may (the default user only)"
		case access.Agents, access.ModelAPI, access.Open:
			levels = "not checked"
		}
		routes := routesOf(sc)
		for i, r := range routes {
			routes[i] = "`" + r + "`"
		}
		label := access.Label(sc)
		switch sc {
		case access.Agents:
			label = "Agents' protocol"
		case access.ModelAPI:
			label = "Model API (a key there is checked for validity only)"
		case access.Open:
			label = "Always open"
		}
		b.WriteString("| " + label + " (`" + string(sc) + "`) | " + levels + " | " + strings.Join(routes, ", ") + " |\n")
	}
	return b.String()
}

const docBegin, docEnd = "<!-- access-table:begin (written by TestRouteScopes_DocsTable -update-docs) -->\n", "<!-- access-table:end -->"

// The runbook's access table is the code's table: run the test with
// -update-docs to rewrite it after a change.
func TestRouteScopes_DocsTable(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "runbook.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i, j := strings.Index(doc, docBegin), strings.Index(doc, docEnd)
	if i < 0 || j < i {
		t.Fatalf("the runbook has no access table markers")
	}
	want := doc[:i+len(docBegin)] + accessDocTable() + doc[j:]
	if want == doc {
		return
	}
	if *updateDocs {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("docs/runbook.md's access table differs from routeScopes: run go test ./internal/api -run TestRouteScopes_DocsTable -update-docs")
}
