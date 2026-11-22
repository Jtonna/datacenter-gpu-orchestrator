package pool

import "testing"

// An agent node in exclude_nodes: the agent keeps registering and
// heartbeating, gets its programs as any agent does, and its node is in the
// scheduler's view (the scheduler leaves it out of every decision). Its facts
// (/api/nodes and the agents of /api/status) say it is excluded, by which
// entry, and not placeable, with the reason; the scheduler's own unplaced
// map does not carry it (exclusion is the scheduler's own rule). Its start-up
// record is kept.
func TestExcludedAgentFacts(t *testing.T) {
	r, _, ms := startupRig(t)
	r.cfg.ExcludeNodes = []string{"other", upID}
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
	if f.State != StateActive || f.Placeable || !f.Excluded || f.ExcludedBy != upID || f.Unplaced != UnplacedExcluded ||
		f.UnplacedMessage != "excluded by exclude_nodes: "+upID || f.LastStartup == nil || f.Release == "" {
		t.Fatalf("facts %+v", f)
	}
	if recs, _ := ms.List(upID, 0); len(recs) != 1 {
		t.Fatalf("start-up records %d", len(recs))
	}
	if r.unplaced(upID) != "" {
		t.Fatalf("unplaced %q", r.unplaced(upID))
	}
	// Exclusion wins over a pause: still excluded, still the same reason.
	r.p.SetPaused(upID, true)
	if f := r.fact(upID); f.Unplaced != UnplacedExcluded || f.Placeable {
		t.Fatalf("paused %+v", f)
	}
	// Taken out of exclude_nodes: placeable again, no exclusion fields.
	r.p.SetPaused(upID, false)
	r.cfg.ExcludeNodes = nil
	r.heartbeat(upID)
	if f := r.fact(upID); f.Excluded || f.ExcludedBy != "" || f.Unplaced == UnplacedExcluded {
		t.Fatalf("not excluded %+v", f)
	}
}
