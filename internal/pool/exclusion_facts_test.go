package pool

import "testing"

// An agent node in exclude_nodes (pinning today's behaviour): the pool does
// not read exclude_nodes. The agent keeps registering and heartbeating, its
// node is in the scheduler's view (the scheduler leaves it out of every
// decision and marks it excluded in /api/status), and its own facts
// (/api/nodes) say nothing of the exclusion: active and placeable, with no
// unplaced reason. It gets its programs as any agent does. Its start-up
// record is kept.
func TestExcludedAgentFacts(t *testing.T) {
	r, _, ms := startupRig(t)
	r.cfg.ExcludeNodes = []string{upID}
	ans := r.p.Register(startupReq(upID, "s1", "b1", 20, map[string]float64{"agent_start": 20}))
	// Accepted with the release's programs to fetch, as any agent.
	if !ans.Accepted || len(ans.Programs) == 0 || ans.Release == "" {
		t.Fatalf("registration %+v", ans)
	}
	if res := r.heartbeat(upID); res.Answer.Paused {
		t.Fatalf("heartbeat answer %+v", res.Answer)
	}
	if _, ok := r.node(upID); !ok {
		t.Fatal("the excluded node is not in the scheduler's view")
	}
	f := r.fact(upID)
	if f.State != StateActive || !f.Placeable || f.Unplaced != "" || f.LastStartup == nil {
		t.Fatalf("facts %+v", f)
	}
	if recs, _ := ms.List(upID, 0); len(recs) != 1 {
		t.Fatalf("start-up records %d", len(recs))
	}
	if r.unplaced(upID) != "" {
		t.Fatalf("unplaced %q", r.unplaced(upID))
	}
}
