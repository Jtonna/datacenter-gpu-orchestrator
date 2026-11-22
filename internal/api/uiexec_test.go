package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
