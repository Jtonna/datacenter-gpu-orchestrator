package scheduler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The live finding after 0.1.6: with the reservation settings on, a profile
// that can never fit (a forced one-node profile needing 19266 MiB on a node
// with about 12643 MiB usable) was queued (plan "queue", the request waited
// up to queue_timeout) instead of being refused at once. When the gate
// refuses, waiting only helps if something of ours holds, claims or settles
// on the nodes the profile could use, or one of them may come back; else
// the request is refused at once and the plan says reject, with the same
// text.

func liveTight(h *harness) {
	d := &h.cfg.Defaults
	d.FillReserveKiBPerToken, d.PlacementUsesReserved, d.MarginMeasuredMiB = 8, true, 300
	d.MarginEstimatedPercent, d.MarginEstimatedMinMiB = 5, 500
}

func inMode(h *harness, mode string) *harness {
	if mode == "agents" {
		h.cfg.Defaults.Membership, h.cfg.Defaults.WorkerProbeInterval = config.MembershipAgents, 0
		h.c.agents = true
	}
	return h
}

// refusedAtOnce checks that the request was answered in its own step with
// the plan's reason, and returns the error.
func refusedAtOnce(t *testing.T, h *harness, profile string, reason Reason) *Error {
	t.Helper()
	r := h.c.dryPlan(profile)
	if r.Action != PlanReject {
		t.Fatalf("%s: plan %s (%s), want reject", profile, r.Action, r.Reason)
	}
	e := h.failed(h.request(profile), http.StatusServiceUnavailable, reason)
	if e.Message != r.Reason {
		t.Fatalf("%s: the request says %q, the plan %q", profile, e.Message, r.Reason)
	}
	if h.c.entries[profile] != nil {
		t.Fatalf("%s: a queue entry was left", profile)
	}
	return e
}

// The live example, in both membership modes, single node and split.
func TestNeverFits_ForcedIsRefusedAtOnce(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, split := range []bool{false, true} {
			h := inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
			liveTight(h)
			nodes, need := []string{"a"}, 19266
			if split {
				nodes, need = []string{"a", "b"}, 2*19266
			}
			h.llama("big", 0, func(p *core.Profile) {
				p.ForceNodes, p.LoadTimeout = nodes, time.Hour
				if split {
					p.TensorSplit = []int{1, 1}
				}
			})
			h.needs["big"] = NeedInfo{Need: placer.Measured(need), FileBytes: 10 << 30}
			h.viewChanged()
			e := refusedAtOnce(t, h, "big", ReasonForcedDoesNotFit)
			for _, s := range []string{"node a", "would take", "fill reserve", "margin", "short by", "waiting cannot help"} {
				if !strings.Contains(e.Message, s) {
					t.Errorf("%s split %v: the message lacks %q: %s", mode, split, s, e.Message)
				}
			}
			if e.NeedMiB <= need || e.CapacityMiB <= 0 {
				t.Errorf("%s split %v: figures need %d capacity %d", mode, split, e.NeedMiB, e.CapacityMiB)
			}
		}
	}
}

// loadSmall loads "small": ready and idle (evictable) in fleet membership;
// in agents membership its load is under way (the worker launch answer is
// not given), which also holds its node.
func loadSmall(h *harness, mode string) {
	h.t.Helper()
	if mode == "fleet" {
		h.release(h.ready("small"))
		return
	}
	h.request("small")
	if h.c.insts["small"] == nil {
		h.t.Fatalf("small did not start")
	}
}

// The same node busy with an evictable instance: a profile that fits once
// it is stopped is not refused (it evicts or waits as before).
func TestNeverFits_ABusyNodeStillEvictsOrWaits(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), mode)
		liveTight(h)
		h.llama("small", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
		h.viewChanged()
		loadSmall(h, mode)
		h.llama("mid", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
		h.needs["mid"] = NeedInfo{Need: placer.Measured(9000), FileBytes: 5 << 30}
		h.viewChanged()
		r := h.c.dryPlan("mid")
		if r.Action == PlanReject {
			t.Fatalf("%s: plan reject (%s) while small is loaded and evictable", mode, r.Reason)
		}
		w := h.request("mid")
		if a, ok := h.peek(w); ok && a.err != nil {
			t.Fatalf("%s: refused %v while small is loaded and evictable", mode, a.err)
		}
	}
}

// An automatic profile too big for the whole idle pool is refused; one too
// big only while another profile is loaded waits (or evicts).
func TestNeverFits_Automatic(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		h := inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
		liveTight(h)
		h.cfg.Defaults.MaxPool = 2
		h.llama("huge", 0, func(p *core.Profile) { p.LoadTimeout = time.Hour })
		h.needs["huge"] = NeedInfo{Need: placer.Measured(25000), FileBytes: 12 << 30}
		h.viewChanged()
		r := h.c.dryPlan("huge")
		if r.Action != PlanReject {
			t.Fatalf("%s: too big for the idle pool: plan %s (%s)", mode, r.Action, r.Reason)
		}
		w := h.request("huge")
		// (refused by the capacity check at arrival, as with the defaults)
		if a, ok := h.peek(w); !ok || a.err == nil || a.err.Reason != ReasonDoesNotFit {
			t.Fatalf("%s: too big for the idle pool: answered %v %+v, plan %q", mode, ok, a.err, r.Reason)
		}

		h = inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
		liveTight(h)
		h.cfg.Defaults.MaxPool = 2
		h.llama("small", 6000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
		h.viewChanged()
		loadSmall(h, mode)
		h.llama("big", 0, func(p *core.Profile) { p.LoadTimeout = time.Hour })
		h.needs["big"] = NeedInfo{Need: placer.Measured(16000), FileBytes: 8 << 30}
		h.viewChanged()
		if r := h.c.dryPlan("big"); r.Action == PlanReject {
			t.Fatalf("%s: too big only while small is loaded: plan reject (%s)", mode, r.Reason)
		}
		w = h.request("big")
		if a, ok := h.peek(w); ok && a.err != nil {
			t.Fatalf("%s: too big only while small is loaded: refused %v", mode, a.err)
		}
	}
}

// A command profile too big for the idle local node is refused at once.
func TestNeverFits_Command(t *testing.T) {
	for _, tight := range []bool{false, true} {
		h := newHarness(t).nodes(hostNode(7000), psNode("a", 2, 12643))
		if tight {
			liveTight(h)
		}
		h.command("whisper", 9000)
		h.viewChanged()
		r := h.c.dryPlan("whisper")
		if r.Action != PlanReject {
			t.Fatalf("tight %v: plan %s (%s)", tight, r.Action, r.Reason)
		}
		w := h.request("whisper")
		if a, ok := h.peek(w); !ok || a.err == nil {
			t.Fatalf("tight %v: not refused at once", tight)
		}
	}
}

// The default settings: the new rule never acts (the gate is off), so the
// outcomes stay those of 0.1.6.
func TestNeverFits_DefaultsUnchanged(t *testing.T) {
	want := map[string]PlanAction{"fleet": PlanStart, "agents": PlanReject}
	for _, mode := range []string{"fleet", "agents"} {
		h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), mode)
		h.llama("big", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
		h.needs["big"] = NeedInfo{Need: placer.Measured(19266), FileBytes: 10 << 30}
		h.viewChanged()
		if r := h.c.dryPlan("big"); r.Action != want[mode] || strings.Contains(r.Reason, "waiting cannot help") {
			t.Fatalf("%s defaults: plan %s (%s), want %s as in 0.1.6", mode, r.Action, r.Reason, want[mode])
		}
	}
}
