package scheduler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// no_auto_nodes (CR-9 review, C): automatic placement never picks such a node
// and never evicts what runs on it; a profile that names it in force_nodes
// still uses it. exclude_nodes wins over both.

func TestNoAuto_AutoPlacementSkipsTheOnlyNodeWithRoom(t *testing.T) {
	// Only a has room now (b is nearly full); a is no_auto, so the automatic
	// request waits and never lands there. The wait reason names no_auto_nodes.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 3000))
	h.cfg.NoAutoNodes = []string{"A"}
	h.cfg.Defaults.QueueTimeout = 5 * time.Minute
	h.llama("m", 8000)
	h.step(evTick{})
	if r := h.c.dryPlan("m"); r.Action != PlanQueue || !strings.Contains(r.Reason, "node a is in no_auto_nodes") {
		t.Fatalf("plan = %+v", r)
	}
	w := h.request("m")
	h.pending(w)
	h.wantState("m", "absent")
	h.advance(5 * time.Minute)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonClusterBusy)
	if !strings.Contains(e.Message, "no_auto_nodes") {
		t.Fatalf("error = %+v", e)
	}
	if ns := h.c.nodes["a"]; ns != nil && (ns.holder != "" || ns.claim != "") {
		t.Fatalf("no_auto node held or claimed: %+v", ns)
	}
	if len(h.prober.calls) != 0 {
		t.Fatalf("pre-flight ran: %v", h.prober.calls)
	}
}

func TestNoAuto_CapacityCheckIgnoresTheNode(t *testing.T) {
	// a+b together could hold 20000; without a they cannot, so the automatic
	// request fails at once and the message names the no_auto node.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.NoAutoNodes = []string{"a"}
	h.llama("m", 20000)
	w := h.request("m")
	e := h.failed(w, http.StatusServiceUnavailable, ReasonDoesNotFit)
	if e.CapacityMiB != 15000 || !strings.Contains(e.Message, "no_auto_nodes") {
		t.Fatalf("error = %+v", e)
	}
}

func TestNoAuto_AutoPlacementPrefersAnotherNode(t *testing.T) {
	// a is the tightest fit (the placer's pick) but no_auto: the model lands on b.
	h := newHarness(t).nodes(ps5("a", 2, 8500), ps5("b", 4, 15000))
	h.cfg.NoAutoNodes = []string{"a"}
	h.llama("m", 6000)
	h.ready("m")
	h.wantNodes("m", "b")
	if r := h.c.dryPlan("m"); r.Action == PlanReject {
		t.Fatalf("plan = %+v", r)
	}
}

func TestNoAuto_ForcedProfileStarts(t *testing.T) {
	a := ps5("02:00:00:00:00:01", 2, 15000)
	a.Hostname = "laptop-1"
	h := newHarness(t).nodes(a, ps5("b", 4, 15000))
	h.cfg.NoAutoNodes = []string{"laptop-*"}
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"laptop-1"} })
	h.ready("f")
	h.wantNodes("f", "02:00:00:00:00:01")
}

func TestNoAuto_NeverAVictimOfAutomaticEviction(t *testing.T) {
	// f runs on the no_auto node a (forced). An automatic m that only a could
	// hold does not evict f; it fails as not fitting the automatic nodes.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.NoAutoNodes = []string{"a"}
	h.llama("f", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("one", 12000)
	h.llama("two", 12000)
	h.release(h.ready("f"))
	h.wantNodes("f", "a")
	h.release(h.ready("one"))
	h.wantNodes("one", "b")
	h.advance(h.c.cfg.Defaults.MinHold + time.Second) // past every hold; f and one are idle
	h.pending(h.request("two"))
	if in := h.c.insts["f"]; in == nil || in.victimOf != "" || in.state != stReady {
		t.Fatalf("f was touched: %+v", in)
	}
	if ns := h.c.nodes["a"]; ns != nil && ns.claim != "" {
		t.Fatalf("no_auto node claimed by %s", ns.claim)
	}
	// The automatic profile may still evict one on b, and takes b.
	if in := h.c.insts["one"]; in != nil && in.victimOf != "two" {
		t.Fatalf("one on b should be the victim: %+v", in)
	}
	if got := append(h.nodesOfAny("two"), h.claimOf("two")...); len(got) != 1 || got[0] != "b" {
		t.Fatalf("two placed or claimed on %v, want b", got)
	}
}

func TestNoAuto_ExcludeStillWins(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.NoAutoNodes = []string{"a"}
	h.cfg.ExcludeNodes = []string{"a"}
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	w := h.request("f")
	h.failed(w, http.StatusServiceUnavailable, ReasonNodeExcluded)
}

func TestNoAuto_StatusShowsAutoFalse(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.NoAutoNodes = []string{"a"}
	h.step(evTick{})
	if s := h.nodeStatus("a"); s.Auto {
		t.Fatalf("a: %+v", s)
	}
	if s := h.nodeStatus("b"); !s.Auto {
		t.Fatalf("b: %+v", s)
	}
}

// nodesOfAny is the nodes of the profile's instance, or none.
func (h *harness) nodesOfAny(profile string) []core.NodeID {
	var out []core.NodeID
	if in := h.c.insts[profile]; in != nil {
		for _, n := range in.placement.Nodes {
			out = append(out, n.ID)
		}
	}
	return out
}

// claimOf is the claim of the profile's queue entry, or none.
func (h *harness) claimOf(profile string) []core.NodeID {
	if e := h.c.entries[profile]; e != nil {
		return e.claim
	}
	return nil
}
