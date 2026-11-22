package scheduler

import (
	"net/http"
	"strings"
	"testing"
)

// A command profile runs on the local node alone, by definition, so
// no_auto_nodes does not apply to it: the local node in no_auto_nodes still
// runs it. exclude_nodes does apply: an excluded local node rejects it with a
// reason naming exclude_nodes. The dry-run plan says the same as the request.

func TestCommandProfile_LocalNodeInNoAutoNodesStillRunsIt(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000), ps5("a", 2, 15000))
	h.cfg.NoAutoNodes = []string{"host-1"}
	h.command("whisper", 1000)
	h.step(evTick{})
	if r := h.c.dryPlan("whisper"); r.Action != PlanStart || len(r.Placement) != 1 || r.Placement[0].ID != "host-1" {
		t.Fatalf("plan = %+v", r)
	}
	h.ready("whisper")
	h.wantNodes("whisper", "host-1")
	// An automatic llama profile still keeps off the no_auto local node.
	h.llama("m", 6000)
	h.ready("m")
	h.wantNodes("m", "a")
}

func TestCommandProfile_LocalNodeInNoAutoNodesMayBeEvictedFor(t *testing.T) {
	// The local node holds an idle command profile; another command profile
	// that needs the room may evict it, although the node is in no_auto_nodes.
	h := newHarness(t).nodes(hostNode(7000))
	h.cfg.NoAutoNodes = []string{"host-1"}
	h.command("whisper", 5000)
	h.command("flux", 5000)
	h.release(h.ready("whisper"))
	h.advance(h.c.cfg.Defaults.MinHold + 1)
	if r := h.c.dryPlan("flux"); r.Action != PlanEvict {
		t.Fatalf("plan = %+v", r)
	}
}

func TestCommandProfile_ExcludedLocalNodeRejectsIt(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000), ps5("a", 2, 15000))
	h.cfg.ExcludeNodes = []string{"host-1"}
	h.cfg.NoAutoNodes = []string{"host-1"}
	h.command("whisper", 1000)
	h.step(evTick{})
	r := h.c.dryPlan("whisper")
	if r.Action != PlanReject || !strings.Contains(r.Reason, "exclude_nodes") || !strings.Contains(r.Reason, "host-1") ||
		strings.Contains(r.Reason, "no_auto_nodes") {
		t.Fatalf("plan = %+v", r)
	}
	e := h.failed(h.request("whisper"), http.StatusServiceUnavailable, ReasonNodeExcluded)
	if e.RetryAfter != 0 || e.Message != r.Reason || strings.Contains(e.Message, "no_auto_nodes") {
		t.Fatalf("error = %+v", e)
	}
}
