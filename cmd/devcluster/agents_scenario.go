package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	agentpool "github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The agents scenario: the status of a scheduler whose machines join through
// agents (defaults.membership: agents), for the Nodes screen. DEV-ONLY: the
// harness's real scheduler runs in fleet membership, so the agent facts of
// the status (membership, agents[], view.nodes[].agent and .unplaced) and the
// three operator actions (pause, unpause, force unload) are scripted here.
// The actions change the scripted status only; they write no audit entry.
//
// The machines: console-1 holds a model, console-2 is paused, console-3
// matches no tier, laptop-2 is free and placeable, host-1 is the machine the
// scheduler runs on (no agent, so no controls).

func init() {
	registerScenario(scenario{Name: "agents", Description: "Agents membership (scripted): gemma-4-12b on console-1, console-2 paused, console-3 not placeable (no tier matches), laptop-2 free. Pause, Unpause and Force unload on the Nodes screen change this scripted status.",
		Status: scAgents, Decorate: agentsDemo.decorate})
}

// agentsDemoState is what the operator actions of the scenario changed.
type agentsDemoState struct {
	mu       sync.Mutex
	paused   map[string]bool // by node id
	unloaded bool            // console-1 was force unloaded
}

var agentsDemo = &agentsDemoState{paused: map[string]bool{consoleB.ID: true}}

// agentHosts are the nodes of the scenario that have an agent.
var agentHosts = []nodeDef{consoleA, consoleB, consoleC, laptop}

const agentsDemoProfile = "gemma-4-12b"

func scAgents(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	agentsDemo.mu.Lock()
	unloaded := agentsDemo.unloaded
	agentsDemo.mu.Unlock()
	// the laptop is an ordinary machine of the pool here, not an excluded one
	lp := b.nodes[laptop.Host]
	lp.Excluded, lp.Eligible, lp.Addr = false, true, laptop.Addr()
	// console-3 matches no tier: outside the pool, never eligible
	c3 := b.nodes[consoleC.Host]
	c3.Unplaced, c3.Eligible, c3.Priority = scheduler.UnplacedNoTierMatch, false, 0
	if !unloaded {
		b.ready(agentsDemoProfile, []string{consoleA.Host}, instOpts{readyAgo: 38 * time.Minute, served: 212, lastTPS: 14.2, lastAgo: 3 * time.Minute})
		b.learned(agentsDemoProfile, []string{consoleA.Host})
	}
	return b
}

// decorate adds the agent facts to the scripted status document, as
// api.agentStatus does on a scheduler in agents membership.
func (d *agentsDemoState) decorate(v map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	byID := map[string]any{}
	var facts []any
	for _, n := range agentHosts {
		f := agentpool.NodeFacts{ID: n.ID, Name: n.Host, Addr: n.IP, State: agentpool.StateActive, Platform: "linux-vulkan",
			ProgramsReady: true, Paused: d.paused[n.ID], Role: "idle"}
		avail := int64(n.TotalMiB)
		if n.ID == consoleA.ID && !d.unloaded {
			lid := "ln-demo-1"
			f.LaunchID, f.LaunchState, f.Role = &lid, "running", "worker"
			avail -= 7900
		}
		f.MemAvailMiB = &avail
		if n.ID == consoleC.ID {
			f.Unplaced = scheduler.UnplacedNoTierMatch
		}
		f.Placeable = !f.Paused && f.Unplaced == ""
		b, _ := json.Marshal(f)
		var m map[string]any
		json.Unmarshal(b, &m)
		byID[n.ID] = m
		facts = append(facts, m)
	}
	if view, ok := v["view"].(map[string]any); ok {
		nodes, _ := view["nodes"].([]any)
		for _, x := range nodes {
			if nm, ok := x.(map[string]any); ok {
				if f, ok := byID[fmt.Sprint(nm["id"])]; ok {
					nm["agent"] = f
				}
			}
		}
	}
	v["agents"], v["membership"] = facts, "agents"
}

// handleNodeAction answers POST /api/nodes/{id}/{pause|unpause|force-unload}
// in the agents scenario, with the answers of internal/api/nodes.go: a
// comment is required (body "comment" or X-Comment), an unknown node is 404.
// It reports false in every other scenario (the real server answers).
func (s *scenarioState) handleNodeAction(w http.ResponseWriter, r *http.Request, action string) bool {
	if sc, _ := s.get(); sc.Name != "agents" {
		return false
	}
	id := r.PathValue("id")
	var body struct {
		Comment string `json:"comment"`
	}
	if b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); len(b) > 0 {
		if err := json.Unmarshal(b, &body); err != nil {
			writeErr(w, 400, "the body is not a JSON object: "+err.Error(), "bad request")
			return true
		}
	}
	if body.Comment == "" {
		body.Comment = r.Header.Get("X-Comment")
	}
	if body.Comment == "" {
		writeErr(w, 400, `a comment is required (body {"comment": "..."} or the X-Comment header)`, "bad request")
		return true
	}
	known := false
	for _, n := range agentHosts {
		known = known || n.ID == id
	}
	if !known {
		writeErr(w, 404, "no agent node "+id, "not found")
		return true
	}
	d := agentsDemo
	d.mu.Lock()
	defer d.mu.Unlock()
	switch action {
	case "pause", "unpause":
		d.paused[id] = action == "pause"
		writeJSON(w, 200, map[string]any{"node": id, "paused": d.paused[id], "audit_id": 0})
	default:
		prof := ""
		if id == consoleA.ID && !d.unloaded {
			d.unloaded, prof = true, agentsDemoProfile
		}
		writeJSON(w, 200, map[string]any{"node": id, "stopped_profile": prof, "audit_id": 0})
	}
	return true
}
