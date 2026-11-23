package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// uiExec runs scripts/ui-exec.mjs under node with these cases: it executes
// the UI's screens on a fake DOM and fails on any runtime error (`node
// --check` only parses). It skips when node is not installed.
func uiExec(t *testing.T, spec map[string]any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the UI execution test (scripts/ui-exec.mjs) needs it; install Node.js 18 or later to run it")
	}
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	spec["ui"] = filepath.Join(dir, "ui")
	b, _ := json.Marshal(spec)
	file := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, filepath.Join(dir, "..", "..", "scripts", "ui-exec.mjs"), file).CombinedOutput()
	t.Logf("ui-exec:\n%s", out)
	if err != nil {
		t.Fatalf("a UI screen failed at run time: %v", err)
	}
}

// statusWithAgentNodes reads GET /api/status of a real API in agents
// membership and gives it a view node per agent: the test scheduler's view
// knows only the local node, so each agent's view node is that node's real
// entry with the agent's id and name, and excluded/excluded_by as the
// scheduler sets them (view.go, NodeStatus.Excluded).
func statusWithAgentNodes(t *testing.T, e *env, cfg func() *config.Config) map[string]any {
	t.Helper()
	var st map[string]any
	if err := json.Unmarshal([]byte(body(t, e.get("/api/status"))), &st); err != nil {
		t.Fatal(err)
	}
	view, _ := st["view"].(map[string]any)
	nodes, _ := view["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatalf("no view node: %v", st["view"])
	}
	base, _ := json.Marshal(nodes[0])
	agents, _ := st["agents"].([]any)
	for _, x := range agents {
		a := x.(map[string]any)
		var n map[string]any
		json.Unmarshal(base, &n)
		id, _ := a["id"].(string)
		name, _ := a["name"].(string)
		n["id"], n["hostname"], n["local"] = id, name, false
		delete(n, "excluded")
		delete(n, "excluded_by")
		if entry, ok := cfg().ExcludedBy(coreID(id), name); ok {
			n["excluded"], n["excluded_by"] = true, entry
		}
		nodes = append(nodes, n)
	}
	view["nodes"] = nodes
	return st
}

// The live 0.1.6 Nodes page threw a ReferenceError in every tile (an
// undeclared name read for the excluding entry). This draws the page from a
// real agents-membership status with an active, a paused and an excluded
// machine, and fails on any runtime error.
func TestUINodesPageExecutes(t *testing.T) {
	const active, paused, excluded = "02:00:00:00:00:01", "02:00:00:00:00:02", "02:00:00:00:00:03"
	var conf *config.Config
	e, p, _ := feedEnv(t, func(c *config.Config) { c.ExcludeNodes = []string{excluded}; conf = c })
	for _, id := range []string{active, paused, excluded} {
		register(t, e, id)
		feedHeartbeat(t, p, id, "")
	}
	if _, err := p.SetPaused(paused, true); err != nil {
		t.Fatal(err)
	}
	st := statusWithAgentNodes(t, e, func() *config.Config { return conf })
	if s, _ := json.Marshal(st); !strings.Contains(string(s), `"excluded_by":"`+excluded+`"`) || !strings.Contains(string(s), `"paused":true`) {
		t.Fatalf("the status lacks the excluded or the paused machine: %s", s)
	}
	cases := []any{}
	for _, c := range []struct {
		name string
		keep string
	}{{"active node", active}, {"paused node", paused}, {"excluded node", excluded}} {
		one := copyStatus(st, c.keep)
		cases = append(cases, map[string]any{"name": "nodes: " + c.name, "route": "#/nodes", "status": one})
	}
	cases = append(cases, map[string]any{"name": "nodes: all three and the local node", "route": "#/nodes", "status": st,
		"expect": []string{"Excluded in the configuration", "(exclude_nodes: " + excluded + ")", "Paused by an operator"}})
	// 0.1.7 review, ES1: excluded while a model still runs there.
	busy := copyStatus(st, excluded)
	for _, n := range busy["view"].(map[string]any)["nodes"].([]any) {
		n.(map[string]any)["holder"] = "f1"
	}
	for _, a := range busy["agents"].([]any) {
		a.(map[string]any)["running_while_excluded"] = map[string]any{"profile": "f1", "state": "ready", "since": 1763800000000}
	}
	cases = append(cases, map[string]any{"name": "nodes: excluded and still busy", "route": "#/nodes", "status": busy,
		"expect": []string{"Excluded, busy", "Excluded, but still busy.", "keeps running and serving until it is unloaded"}})
	uiExec(t, map[string]any{"modules": []string{"views/nodes.js"}, "cases": cases})
}

// copyStatus is st with only the agent and the view node of id.
func copyStatus(st map[string]any, id string) map[string]any {
	b, _ := json.Marshal(st)
	var c map[string]any
	json.Unmarshal(b, &c)
	keep := func(list []any) []any {
		out := []any{}
		for _, x := range list {
			if m, _ := x.(map[string]any); m != nil && m["id"] == id {
				out = append(out, m)
			}
		}
		return out
	}
	c["agents"] = keep(c["agents"].([]any))
	v := c["view"].(map[string]any)
	v["nodes"] = keep(v["nodes"].([]any))
	return c
}

func coreID(s string) core.NodeID { return core.NodeID(s) }

// Every view module that app.js imports loads as an ES module, with all it
// imports: one that does not parse stops the whole page, every screen
// included (release 0.1.6 shipped a quote inside a single-quoted string in
// views/benchmarks/editor.js). `node --check` on a .js file passes it. Each
// screen's first page is drawn on an empty status as well.
func TestUIAllModulesLoad(t *testing.T) {
	app := uiText(t, "app.js")
	var mods []string
	for _, line := range strings.Split(app, "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), "import './"); ok {
			if i := strings.Index(s, "'"); i > 0 {
				mods = append(mods, s[:i])
			}
		}
	}
	if len(mods) < 10 {
		t.Fatalf("app.js imports only %v", mods)
	}
	cases := []any{}
	for _, id := range []string{"models", "nodes", "queue", "activity", "placement", "benchmarks", "profiles", "tiers", "settings", "audit", "mcp", "about"} {
		cases = append(cases, map[string]any{"name": id + " with no status yet", "route": "#/" + id})
	}
	uiExec(t, map[string]any{"modules": mods, "cases": cases, "fetch": map[string]any{}})
}

// The screens release 0.1.6 changed, on answers shaped by the server's own
// types: a run with a verified and a not-guaranteed cold load and a reload
// (the cold badge, the machine table and the result row), and the placement
// page's memory table from a real POST /api/plan.
func TestUI016ScreensExecute(t *testing.T) {
	e := newEnv(t, chatOK)
	var plan, profiles map[string]any
	if err := json.Unmarshal([]byte(body(t, e.post("/api/plan", `{"model":"alpha"}`))), &plan); err != nil || plan["memory"] == nil {
		t.Fatalf("plan without a memory section: %v %v", plan, err)
	}
	json.Unmarshal([]byte(body(t, e.get("/api/profiles"))), &profiles)
	var status map[string]any
	json.Unmarshal([]byte(body(t, e.get("/api/status"))), &status)
	const gib = int64(1) << 30
	cold := func(verified bool, reason string) *bench.ColdResult {
		c := &bench.ColdResult{Requested: true, Verified: verified, Reason: reason,
			Local: &bench.ColdFigures{FileBytes: 8 * gib, CachedBefore: 8 * gib, CachedAfter: 0},
			Nodes: []bench.ColdNode{{Node: "console-1", ID: "02:00:00:00:00:01", ColdFigures: bench.ColdFigures{FileBytes: 8 * gib, CachedBefore: 2 * gib}}}}
		if !verified {
			c.Nodes = append(c.Nodes, bench.ColdNode{Node: "console-2", ID: "02:00:00:00:00:02", ColdFigures: bench.ColdFigures{FileBytes: 8 * gib, Error: "busy"}})
		}
		return c
	}
	load := func(s float64) *bench.LoadResult {
		return &bench.LoadResult{Outcome: "ready", LoadS: s, Bytes: 8 * gib, AvgMBs: 320, MemoryMiB: 8200, Workers: []string{"02:00:00:00:00:01"}, LoadMode: bench.LoadModeFileRead}
	}
	step := func(idx int, verified bool, reason string) bench.Step {
		return bench.Step{Idx: idx, BenchmarkID: "cold-reload", Name: "Cold, reload, loaded", Profile: "alpha", State: bench.StepDone, Outcome: bench.OutcomeReady,
			StartedAt: 1795400000000, FinishedAt: 1795400090000, Load: load(26.6), NodeCount: 1,
			Expect: &bench.ExpectResult{Pass: true},
			Phases: []bench.PhaseResult{
				{Idx: 0, Type: "load", Label: "cold load", State: "done", Outcome: "ready", DurationS: 26.6, Load: load(26.6), Cold: cold(verified, reason)},
				{Idx: 1, Type: "unload", State: "done", Outcome: "ready", DurationS: 1.2, Unload: &bench.UnloadResult{Loaded: true, Gone: true, UnloadS: 1.2}},
				{Idx: 2, Type: "load", Label: "reload", State: "done", Outcome: "ready", DurationS: 1.8, Load: load(1.8)},
			}}
	}
	run := bench.Run{ID: "run-1", SuiteID: "s", SuiteName: "Cold suite", State: bench.RunCompleted, Trigger: "manual", QueuedAt: 1795400000000,
		StartedAt: 1795400000000, FinishedAt: 1795400200000, Summary: &bench.RunSummary{Steps: 2, Passed: 2},
		Steps: []bench.Step{step(0, true, ""), step(1, false, "not guaranteed: console-2 is busy")}}
	fetch := map[string]any{"/api/bench/runs/run-1": run, "/api/bench/runs": map[string]any{"runs": []any{run}}, "/api/profiles": profiles}
	cases := []any{
		map[string]any{"name": "run with cold loads", "route": "#/benchmarks/runs/run-1", "expect": []string{"cold: verified", "cold: not guaranteed"}},
		map[string]any{"name": "run with cold loads, steps tab", "route": "#/benchmarks/runs/run-1?tab=steps"},
		map[string]any{"name": "runs list", "route": "#/benchmarks/runs"},
		map[string]any{"name": "placement with the memory table", "route": "#/placement/alpha", "status": status, "fetch": map[string]any{"/api/plan": plan},
			"expect": []string{"Memory, with the context filled"}},
	}
	uiExec(t, map[string]any{"modules": []string{"views/benchmarks.js", "views/placement.js"}, "fetch": fetch, "cases": cases})
}

// uiExecBase runs `scripts/ui-exec.mjs --base URL` and returns its output and
// exit code.
func uiExecBase(t *testing.T, url string) (string, int) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the UI execution test (scripts/ui-exec.mjs) needs it; install Node.js 18 or later to run it")
	}
	_, here, _, _ := runtime.Caller(0)
	cmd := exec.Command(node, filepath.Join(filepath.Dir(here), "..", "..", "scripts", "ui-exec.mjs"), "--base", url)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// --base takes the page from the server, not from the checkout: the page
// this API serves loads and every screen draws (exit 0); the same server
// with one module that does not parse, or one module it does not serve,
// fails (exit 1) and the output names that module.
func TestUIExecBaseUsesTheServersPage(t *testing.T) {
	e := newEnv(t, chatOK)
	out, code := uiExecBase(t, e.front.URL)
	if code != 0 || !strings.Contains(out, "modules from "+e.front.URL+"/ui/") || !strings.Contains(out, "ok   start module app.js") ||
		!strings.Contains(out, "PASS: 14 screens") {
		t.Fatalf("good page: exit %d\n%s", code, out)
	}
	for _, c := range []struct{ name, path, body, want string }{
		{"does not parse", "/ui/views/benchmarks/editor.js", "export const x = 'users' idle';\n", "views/benchmarks/editor.js:1: does not parse as an ES module"},
		{"not served", "/ui/core/reserve.js", "", "core/reserve.js: the server answered 404"},
	} {
		t.Run(c.name, func(t *testing.T) {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == c.path {
					if c.body == "" {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "text/javascript")
					w.Write([]byte(c.body))
					return
				}
				e.api.ServeHTTP(w, r)
			}))
			defer bad.Close()
			out, code := uiExecBase(t, bad.URL)
			if code != 1 || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, want 1 naming %q:\n%s", code, c.want, out)
			}
		})
	}
}
