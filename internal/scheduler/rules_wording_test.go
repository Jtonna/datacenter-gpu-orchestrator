package scheduler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Messages people see say "pool" with membership agents and keep the fleet
// wording with membership fleet (config.PoolText); reason codes are the same
// in both.

func wordingHarness(t *testing.T, agents bool) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	if agents {
		h.cfg.Defaults.Membership = config.MembershipAgents
		h.cfg.Defaults.WorkerProbeInterval = 0
		h.c.agents = true
	}
	return h
}

func TestWording_ForcedNodeAbsent(t *testing.T) {
	for _, agents := range []bool{false, true} {
		h := wordingHarness(t, agents)
		forcedProfile(h, "a", "ghost")
		w := h.request("f")
		h.advance(30 * time.Second)
		e := h.failed(w, http.StatusServiceUnavailable, ReasonForcedUnavailable)
		want, other := "forced node ghost is absent from the fleet (for 30s)", "pool"
		if agents {
			want, other = "forced node ghost is absent from the pool (for 30s)", "fleet"
		}
		if !strings.Contains(e.Message, want) || strings.Contains(e.Message, other) {
			t.Errorf("agents %v: message %q, want %q", agents, e.Message, want)
		}
		state := forcedAbsent
		if agents {
			state = "absent from the pool"
		}
		if len(h.logs.find("forced node unavailable; failing the request", "node", "ghost", "state", state)) == 0 {
			t.Errorf("agents %v: no log line with state %q", agents, state)
		}
	}
}

func TestWording_ForcedNodeNotInView(t *testing.T) {
	for _, agents := range []bool{false, true} {
		h := wordingHarness(t, agents)
		h.cfg.Defaults.ForcedNodeGrace = time.Hour
		h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a", "ghost"} })
		h.step(evTick{})
		r := h.c.dryPlan("f")
		want := "forced node ghost is not in the fleet view"
		if agents {
			want = "forced node ghost is not in the pool"
		}
		if r.Action != PlanQueue || !strings.Contains(r.Reason, want) {
			t.Errorf("agents %v: plan %+v, want %q", agents, r, want)
		}
	}
}

func TestWording_SharedBudgetSource(t *testing.T) {
	for _, agents := range []bool{false, true} {
		h := wordingHarness(t, agents)
		h.step(evTick{})
		n, _ := h.c.node("a")
		_, how := h.c.sharedBudget("a", n)
		want := "shared memory, headroom kept, from fleet figures"
		if agents {
			want = "shared memory, headroom kept, from agent figures"
		}
		if how != want {
			t.Errorf("agents %v: %q, want %q", agents, how, want)
		}
	}
}

func TestWording_ExcludedNodeLog(t *testing.T) {
	for _, agents := range []bool{false, true} {
		h := wordingHarness(t, agents)
		h.cfg.ExcludeNodes = []string{"a"}
		h.step(evTick{})
		msg := "excluded node in fleet view"
		if agents {
			msg = "excluded node in pool"
		}
		if len(h.logs.find(msg, "node", "a")) == 0 {
			t.Errorf("agents %v: no log line %q", agents, msg)
		}
	}
}
