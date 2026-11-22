package scheduler

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// exclude_nodes (design 6): an excluded node is never used, never counted,
// never pre-flighted; it still shows in the status view.

func (h *harness) nodeStatus(id core.NodeID) NodeStatus {
	h.t.Helper()
	for _, n := range h.c.statusDoc().View.Nodes {
		if n.ID == id {
			return n
		}
	}
	h.t.Fatalf("node %s not in status view", id)
	return NodeStatus{}
}

func TestExclude_AutoPlacementSkipsExcludedNode(t *testing.T) {
	// a is the tightest fit (the placer's pick), but excluded: the model lands on b.
	h := newHarness(t).nodes(ps5("a", 2, 8500), ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"A"} // id, case-insensitive
	h.llama("m", 6000)
	h.ready("m")
	h.wantNodes("m", "b")
	if slices.Contains(h.prober.calls, "192.0.2.2:50052") {
		t.Fatalf("excluded node was pre-flighted: %v", h.prober.calls)
	}
}

func TestExclude_ByHostnameGlob(t *testing.T) {
	a := ps5("02:00:00:00:00:01", 2, 15000)
	a.Hostname = "ps5-test7"
	a.FreeMiB = 8500
	h := newHarness(t).nodes(a, ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"PS5-TEST*"}
	h.llama("m", 6000)
	h.ready("m")
	h.wantNodes("m", "b")
}

func TestExclude_OnlyFittingNodeExcluded_RequestWaitsWithReason(t *testing.T) {
	// Only a has room (b is nearly full). a is excluded, so m waits; it never lands on a.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 3000))
	h.cfg.ExcludeNodes = []string{"a"}
	h.cfg.Defaults.QueueTimeout = 5 * time.Minute
	h.llama("m", 8000)
	h.step(evTick{})
	r := h.c.dryPlan("m")
	if r.Action != PlanQueue || !strings.Contains(r.Reason, "node a is excluded by config") {
		t.Fatalf("plan = %+v", r)
	}
	w := h.request("m")
	h.pending(w)
	h.wantState("m", "absent")
	if got := h.waitReasonOf("m"); !strings.Contains(got, "node a is excluded by config") {
		t.Fatalf("status wait reason = %q", got)
	}
	h.advance(5 * time.Minute)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonNodeExcluded)
	if e.RetryAfter == 0 || !strings.Contains(e.Message, "node a is excluded by config") {
		t.Fatalf("error = %+v", e)
	}
	if len(h.prober.calls) != 0 {
		t.Fatalf("pre-flight ran: %v", h.prober.calls)
	}
}

func TestExclude_CapacityCheckIgnoresExcludedNode(t *testing.T) {
	// a+b together could hold 20000 (15000 each by plan budget); without a they cannot.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 20000)
	if err := h.c.capacity("m"); err != nil {
		t.Fatalf("fixture: both nodes should hold it: %v", err)
	}
	h.cfg.ExcludeNodes = []string{"a"}
	w := h.request("m")
	e := h.failed(w, http.StatusServiceUnavailable, ReasonDoesNotFit)
	if e.RetryAfter != 0 || e.CapacityMiB != 15000 {
		t.Fatalf("error = %+v, want permanent with capacity 15000", e)
	}
}

func TestExclude_PlanDoesNotEvictOntoExcludedNode(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"a"}
	h.llama("one", 12000)
	h.llama("two", 12000)
	h.ready("one")
	h.wantNodes("one", "b")
	w := h.request("two") // only b could hold it, and one is on b; a is not an option
	h.pending(w)
	h.wantState("two", "absent")
	if ns := h.c.nodes["a"]; ns != nil && (ns.holder != "" || ns.claim != "") {
		t.Fatalf("excluded node held or claimed: %+v", ns)
	}
}

func TestExclude_LocalNodeNeverUsed(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000), ps5("a", 2, 15000))
	h.cfg.ExcludeNodes = []string{"host-1"}
	h.llama("m", 6000)
	h.ready("m")
	h.wantNodes("m", "a")
	h.command("whisper", 1000)
	w := h.request("whisper") // command profiles run on the local node only
	e := h.failed(w, http.StatusServiceUnavailable, ReasonNodeExcluded)
	if e.RetryAfter != 0 || !strings.Contains(e.Message, "excluded by config") || !strings.Contains(e.Message, "host-1") ||
		!strings.Contains(e.Message, "exclude_nodes") {
		t.Fatalf("error = %+v", e)
	}
}

func TestExclude_ForcedProfileNamingExcludedNodeFailsPermanently(t *testing.T) {
	// The exclusion is a glob on the hostname; the profile names the node by id,
	// so config load could not tell. The request fails with no Retry-After.
	a := ps5("02:00:00:00:00:01", 2, 15000)
	a.Hostname = "ps5-test7"
	h := newHarness(t).nodes(a, ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"ps5-test*"}
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"02:00:00:00:00:01", "b"} })
	h.step(evTick{})
	r := h.c.dryPlan("f")
	if r.Action != PlanReject || !strings.Contains(r.Reason, "node ps5-test7 is excluded by config") {
		t.Fatalf("plan = %+v", r)
	}
	w := h.request("f")
	e := h.failed(w, http.StatusServiceUnavailable, ReasonNodeExcluded)
	if e.RetryAfter != 0 || !strings.Contains(e.Message, "profile f") || !strings.Contains(e.Message, "node ps5-test7 is excluded by config") {
		t.Fatalf("error = %+v", e)
	}
	h.wantState("f", "absent")
	h.noEntry("f")
	if len(h.prober.calls) != 0 {
		t.Fatalf("pre-flight ran: %v", h.prober.calls)
	}
}

func TestExclude_ForcedNodeExcludedAfterQueueingFailsPermanently(t *testing.T) {
	// The forced node is absent at first (queues, by design), then appears and is excluded.
	h := newHarness(t).nodes(ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"ps5-new"}
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"ps5-new"} })
	w := h.request("f")
	h.pending(w)
	n := ps5("ps5-new", 2, 15000)
	h.view.set(ps5("b", 4, 15000), n)
	h.viewChanged()
	e := h.failed(w, http.StatusServiceUnavailable, ReasonNodeExcluded)
	if e.RetryAfter != 0 {
		t.Fatalf("want permanent, got %+v", e)
	}
	h.noEntry("f")
}

func TestExclude_ForcedNodeAbsentStillQueues(t *testing.T) {
	h := newHarness(t).nodes(ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"ps5-other"}
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"ghost"} })
	w := h.request("f")
	h.pending(w)
	if h.entry("f") == nil {
		t.Fatal("entry missing")
	}
}

func TestExclude_StatusShowsExcludedNode(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"a"}
	h.step(evTick{})
	a, b := h.nodeStatus("a"), h.nodeStatus("b")
	if !a.Excluded || a.Eligible || a.ExcludedBy != "a" {
		t.Fatalf("a: excluded=%v eligible=%v", a.Excluded, a.Eligible)
	}
	if b.Excluded || !b.Eligible || b.ExcludedBy != "" {
		t.Fatalf("b: excluded=%v eligible=%v", b.Excluded, b.Eligible)
	}
}

func TestExclude_LoggedOnceWhenFirstSeen(t *testing.T) {
	h := newHarness(t).nodes(ps5("b", 4, 15000))
	h.cfg.ExcludeNodes = []string{"ps5-*"}
	h.step(evTick{})
	h.view.set(ps5("ps5-new", 2, 15000), ps5("b", 4, 15000))
	h.viewChanged()
	h.viewChanged()
	h.step(evTick{})
	recs := h.logs.find("excluded node in fleet view", "node", "ps5-new")
	if len(recs) != 1 {
		t.Fatalf("logged %d times for ps5-new, want 1", len(recs))
	}
	if recs[0].Level.String() != "INFO" || recs[0].Attrs["entry"] != "ps5-*" {
		t.Fatalf("record %+v", recs[0])
	}
}

func TestExclude_NoExclusionsNoLog(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.step(evTick{})
	if n := len(h.logs.find("excluded node in fleet view")); n != 0 {
		t.Fatalf("logged %d times", n)
	}
}
