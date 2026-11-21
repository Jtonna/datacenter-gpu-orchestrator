package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The agents scenario shows the four machine states the Nodes screen draws,
// and its scripted actions change them.
func TestAgentsScenario(t *testing.T) {
	sc, ok := scenarioByName("agents")
	if !ok {
		t.Fatal("no agents scenario")
	}
	doc := func() (map[string]map[string]any, map[string]any) {
		v := sc.Status(time.Now(), 0).finish()
		sc.Decorate(v)
		if v["membership"] != "agents" || len(v["agents"].([]any)) != len(agentHosts) {
			t.Fatalf("membership %v, agents %v", v["membership"], v["agents"])
		}
		nodes := map[string]map[string]any{}
		for _, x := range v["view"].(map[string]any)["nodes"].([]any) {
			n := x.(map[string]any)
			nodes[n["hostname"].(string)] = n
		}
		return nodes, v
	}
	agent := func(n map[string]any) map[string]any { a, _ := n["agent"].(map[string]any); return a }
	nodes, _ := doc()
	if a := agent(nodes[consoleA.Host]); a["placeable"] != true || a["launch_id"] == nil || nodes[consoleA.Host]["holder"] != agentsDemoProfile {
		t.Errorf("console-1 should hold a model: %v", nodes[consoleA.Host])
	}
	if a := agent(nodes[consoleB.Host]); a["paused"] != true || a["placeable"] != false {
		t.Errorf("console-2 should be paused: %v", a)
	}
	if a := agent(nodes[consoleC.Host]); a["unplaced"] != "no_tier_match" || a["placeable"] != false || nodes[consoleC.Host]["unplaced"] != "no_tier_match" {
		t.Errorf("console-3 should match no tier: %v", nodes[consoleC.Host])
	}
	if a := agent(nodes[laptop.Host]); a["placeable"] != true || a["paused"] != false || a["launch_id"] != nil || nodes[laptop.Host]["excluded"] == true {
		t.Errorf("laptop-2 should be free and placeable: %v", nodes[laptop.Host])
	}
	if agent(nodes[hostNode.Host]) != nil {
		t.Errorf("the scheduler's own machine has no agent")
	}

	s := &scenarioState{}
	s.set("agents")
	act := func(id, verb, body, header string) int {
		r := httptest.NewRequest("POST", "/api/nodes/x/"+verb, strings.NewReader(body))
		r.SetPathValue("id", id)
		if header != "" {
			r.Header.Set("X-Comment", header)
		}
		w := httptest.NewRecorder()
		if !s.handleNodeAction(w, r, verb) {
			t.Fatalf("%s was not handled in the agents scenario", verb)
		}
		return w.Code
	}
	t.Cleanup(func() { agentsDemo.paused, agentsDemo.unloaded = map[string]bool{consoleB.ID: true}, false })
	if c := act(consoleB.ID, "unpause", `{}`, ""); c != 400 {
		t.Errorf("no comment: %d", c)
	}
	if c := act("nope", "pause", `{"comment":"x"}`, ""); c != 404 {
		t.Errorf("unknown node: %d", c)
	}
	if c := act(consoleB.ID, "unpause", `{"comment":"back"}`, ""); c != 200 {
		t.Errorf("unpause: %d", c)
	}
	if c := act(laptop.ID, "pause", ``, "bench work"); c != 200 {
		t.Errorf("pause: %d", c)
	}
	if c := act(consoleA.ID, "force-unload", `{"comment":"stuck"}`, ""); c != 200 {
		t.Errorf("force unload: %d", c)
	}
	nodes, v := doc()
	if agent(nodes[consoleB.Host])["paused"] != false || agent(nodes[laptop.Host])["paused"] != true {
		t.Errorf("pause state not changed: %v %v", agent(nodes[consoleB.Host]), agent(nodes[laptop.Host]))
	}
	if agent(nodes[consoleA.Host])["launch_id"] != nil || nodes[consoleA.Host]["holder"] != "" || len(v["instances"].([]any)) != 0 {
		t.Errorf("console-1 still holds its model after the force unload: %v", nodes[consoleA.Host])
	}
	s.set("demo")
	if s.handleNodeAction(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/nodes/x/pause", nil), "pause") {
		t.Error("the node actions are scripted outside the agents scenario")
	}
}
