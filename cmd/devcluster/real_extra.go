package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
)

// What the "real" scenarios add to the recording, DEV-ONLY.
//
// The recording was made before some answers existed. They are worked out
// from what was recorded, with the real code:
//
//   - the pass/fail history of every suite and benchmark (recent[] of GET
//     /api/bench/aggregates) and GET /api/bench/benchmarks/{id}/results, from
//     the recorded run details (bench.RecentOf, bench.ResultsOf);
//
// and some parts are not a recording at all: the agents, the model icons and
// the MCP tool list are the harness's real server with its own database, so
// they can be tried out. What is done there is recorded in the real audit
// log; GET /api/audit shows those entries in front of the recorded trail.

// runs are the recorded runs with their steps, oldest first.
func (rp *replay) runs() []bench.Run {
	v, _ := rp.doc("/api/bench/runs")
	m, _ := v.(map[string]any)
	list, _ := m["runs"].([]any)
	var out []bench.Run
	for _, x := range list {
		id, _ := x.(map[string]any)["id"].(string)
		d, ok := rp.doc("/api/bench/runs/" + id)
		if !ok {
			d = x
		}
		b, _ := json.Marshal(d)
		var r bench.Run
		if json.Unmarshal(b, &r) == nil && r.ID != "" {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].QueuedAt < out[j].QueuedAt })
	return out
}

// realAggregates is the recorded aggregates document with recent[] added.
func (h *harness) realAggregates() map[string]any {
	v, ok := h.rp.doc("/api/bench/aggregates")
	doc, _ := v.(map[string]any)
	if !ok || doc == nil {
		return nil
	}
	suites, benches := bench.RecentOf(h.rp.runs())
	add := func(list any, idKey string, marks map[string][]bench.RecentMark) {
		items, _ := list.([]any)
		for _, it := range items {
			e, _ := it.(map[string]any)
			if e == nil {
				continue
			}
			if _, has := e["recent"]; has {
				continue // a newer recording has it already
			}
			ms := marks[fmt.Sprint(e[idKey])]
			if ms == nil {
				ms = []bench.RecentMark{}
			}
			var arr []any
			b, _ := json.Marshal(ms)
			json.Unmarshal(b, &arr)
			e["recent"] = arr
		}
	}
	add(doc["suites"], "suite_id", suites)
	add(doc["benchmarks"], "benchmark_id", benches)
	return doc
}

// ownAuditBase keeps the ids of the harness's own entries apart from the recorded ones.
const ownAuditBase = 1000000

// ownAudit lists the audit entries of the harness's own database that are
// about agents, icons, or were made with an agent key, newest first, as JSON
// values with their ids moved out of the way of the recorded ones.
func (h *harness) ownAudit(ctx context.Context) []any {
	es, _, err := h.c.cs.Query(ctx, cfgstore.Filter{Limit: 500})
	if err != nil {
		return nil
	}
	out := []any{}
	for _, e := range es {
		if e.Section != "agents" && e.Section != "models" && e.AgentID == "" {
			continue
		}
		e.ID += ownAuditBase
		b, _ := json.Marshal(e)
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// seedAgents makes two agents through the real API, as the web UI does, and
// lets each do one audited thing, so the MCP and Audit screens have something
// to show. Their keys are thrown away: make a new agent to try one.
func seedAgents(c *cluster) error {
	sc := newSeedClient(c.url)
	asJacob := who{source: "ui", actor: "alex"}
	mk := func(name, desc string) (string, error) {
		out, err := sc.do("POST", "/api/agents", asJacob, map[string]any{"name": name, "description": desc})
		if err != nil {
			return "", err
		}
		key, _ := out["key"].(string)
		return key, nil
	}
	k1, err := mk("claude-code-laptop", "Claude Code on the laptop: day-to-day changes")
	if err != nil {
		return err
	}
	k2, err := mk("nightly-bench-agent", "Runs the nightly suite and reads the results")
	if err != nil {
		return err
	}
	// one over MCP, one over REST: both are recorded under the agent's name
	m, err := newMCPWithKey(c.url, "claude-code", "2.1.0", k1)
	if err != nil {
		return err
	}
	if _, err := m.call("update_settings", map[string]any{"settings": map[string]any{"queue_timeout": "900s"}, "comment": "longer queue timeout for the big models"}); err != nil {
		return err
	}
	if _, err := sc.doKey("PATCH", "/api/config/settings", k2, map[string]any{"ws_max_clients": 48, "comment": "room for the dashboards during the nightly run"}); err != nil {
		return err
	}
	return nil
}
