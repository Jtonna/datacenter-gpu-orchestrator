package scheduler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// A forced node that is absent from or offline in a live fleet view for
// defaults.forced_node_grace fails the waiting requests at once, instead of
// waiting for queue_timeout.

func forcedProfile(h *harness, names ...string) {
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = names })
}

func TestForcedGone_AbsentNodeFailsAfterGrace(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	forcedProfile(h, "a", "ghost")
	w := h.request("f")
	h.advance(29 * time.Second)
	h.pending(w)
	h.advance(time.Second)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedUnavailable)
	if e.RetryAfter != queueRetryAfter {
		t.Fatalf("Retry-After = %v", e.RetryAfter)
	}
	if !strings.Contains(e.Message, "ghost") || !strings.Contains(e.Message, "absent from the fleet") {
		t.Fatalf("message = %q", e.Message)
	}
	h.noEntry("f")
	if len(h.logs.find("forced node unavailable; failing the request", "node", "ghost", "state", forcedAbsent)) == 0 {
		t.Fatalf("no log line for the fail-fast")
	}
}

func TestForcedGone_OfflineNodeFailsAfterGrace(t *testing.T) {
	b := ps5("b", 3, 15000)
	b.Online = false
	h := newHarness(t).nodes(ps5("a", 2, 15000), b)
	forcedProfile(h, "a", "b")
	w := h.request("f")
	h.advance(29 * time.Second)
	h.pending(w)
	h.advance(time.Second)
	e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedUnavailable)
	if !strings.Contains(e.Message, "b is offline") {
		t.Fatalf("message = %q", e.Message)
	}
}

func TestForcedGone_NodeBackWithinGraceProceeds(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	forcedProfile(h, "a", "b")
	w := h.request("f")
	h.advance(20 * time.Second)
	h.pending(w)
	h.view.set(ps5("b", 3, 15000))
	h.viewChanged()
	h.advance(60 * time.Second) // past where the grace would have ended
	h.wantState("f", "starting")
	h.pending(w)
}

func TestForcedGone_GraceRestartsWhenNodeFlaps(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	forcedProfile(h, "a", "b")
	w := h.request("f")
	h.advance(20 * time.Second)
	h.view.set(ps5("b", 3, 100)) // back online (too full to place on), then gone again
	h.viewChanged()
	h.view.remove("b")
	h.viewChanged()
	h.advance(25 * time.Second)
	h.pending(w) // 45 s since the first sighting, but only 25 s since the last
	h.advance(5 * time.Second)
	h.failed(w, http.StatusServiceUnavailable, ReasonForcedUnavailable)
}

func TestForcedGone_StaleViewDoesNotFailAndDoesNotCount(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Fleet.StaleAfter = 5 * time.Second
	forcedProfile(h, "a", "ghost")
	w := h.request("f")
	h.advance(20 * time.Second)
	h.view.stale()
	h.advance(120 * time.Second) // stale the whole time: nothing fails
	h.pending(w)
	h.view.fresh()
	h.viewChanged()
	h.advance(29 * time.Second) // the stale time did not count
	h.pending(w)
	h.advance(time.Second)
	h.failed(w, http.StatusServiceUnavailable, ReasonForcedUnavailable)
}

func TestForcedGone_BusyNodeIsNotFailFast(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("other", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	forcedProfile(h, "a")
	h.ready("other") // a stays occupied: the grant is never released
	w := h.request("f")
	h.advance(5 * time.Minute)
	h.pending(w)
}

func TestForcedGone_RequestAfterGraceFailsAtOnce(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	forcedProfile(h, "a", "ghost")
	h.step(evTick{}) // the scheduler sees the node missing
	h.advance(31 * time.Second)
	w := h.request("f")
	h.failed(w, http.StatusServiceUnavailable, ReasonForcedUnavailable)
}

func TestForcedGone_DryPlanRejects(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	forcedProfile(h, "a", "ghost")
	h.step(evTick{})
	h.advance(31 * time.Second)
	r := h.c.dryPlan("f")
	if r.Action != PlanReject || !strings.Contains(r.Reason, "ghost is absent from the fleet") {
		t.Fatalf("plan = %+v", r)
	}
}

// ---- child log file path ----

func TestChildLog_PathInStatusAndStartLog(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("gemma:48k", 8000)
	h.ready("gemma:48k")
	in := h.inst("gemma:48k")
	want := "/logs/gemma:48k-" + fmt.Sprint(in.iid) + ".log"
	var got string
	for _, s := range h.c.statusDoc().Instances {
		if s.Profile == "gemma:48k" {
			got = s.LogFile
		}
	}
	if got != want {
		t.Fatalf("status log_file = %q, want %q", got, want)
	}
	if len(h.logs.find("child started", "profile", "gemma:48k", "log_file", want)) == 0 {
		t.Fatalf("the child started line does not carry log_file %q", want)
	}
}
