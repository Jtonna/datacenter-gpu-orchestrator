package scheduler

import (
	"net/http"
	"slices"
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
func loadSmall(h *harness, mode string) { h.t.Helper(); loadSmallAs(h, mode, "small") }

func loadSmallAs(h *harness, mode, name string) {
	h.t.Helper()
	if mode == "fleet" {
		h.release(h.ready(name))
		return
	}
	h.request(name)
	if h.c.insts[name] == nil {
		h.t.Fatalf("%s did not start", name)
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

// unloadLagging loads "small" (6000) on a, shows it in the view as 7000 MiB
// used, unloads it and lets the settle complete while the view still shows
// the old figure (heartbeat lag, or a worker that has not returned its
// buffers yet): the fifth review's FB1.
func unloadLagging(h *harness, mode string) {
	h.t.Helper()
	h.llama("small", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.viewChanged()
	if mode == "fleet" {
		h.release(h.ready("small"))
	} else {
		h.release(h.agentReady("small"))
	}
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB - 7000 })
	h.viewChanged()
	h.step(evUnload{profile: "small", force: true})
	h.advance(10 * time.Second)
	if h.c.insts["small"] != nil {
		h.t.Fatalf("small is still there after its unload")
	}
}

// FB1, the reviewer's probe and the benchmark sequence: load A on a,
// unload A, at once request a forced B on a that fits only when A's memory
// is back. B waits (never refused) and starts once the view catches up.
func TestNeverFits_ForcedLoadRightAfterAnUnloadWaits(t *testing.T) {
	for _, window := range []time.Duration{30 * time.Second, 0} {
		for _, mode := range []string{"fleet", "agents"} {
			h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), mode)
			liveTight(h)
			h.cfg.Defaults.FreeAverageWindow = window
			unloadLagging(h, mode)
			h.llama("mid", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
			h.needs["mid"] = NeedInfo{Need: placer.Measured(9000), FileBytes: 5 << 30}
			if r := h.c.dryPlan("mid"); r.Action == PlanReject {
				t.Fatalf("%s: plan reject right after the unload: %s", mode+" window "+window.String(), r.Reason)
			}
			w := h.request("mid")
			if a, ok := h.peek(w); ok && a.err != nil {
				t.Fatalf("%s: refused right after the unload: %v", mode+" window "+window.String(), a.err)
			}
			// the view catches up: mid starts
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB })
			h.viewChanged()
			if a, ok := h.peek(w); ok && a.err != nil {
				t.Fatalf("%s: refused once the view caught up: %v", mode+" window "+window.String(), a.err)
			}
			if h.c.insts["mid"] == nil {
				t.Fatalf("%s: mid did not start once the view caught up (wait: %s)", mode+" window "+window.String(), h.waitReasonOf("mid"))
			}
		}
	}
}

// skewedPeaks stores a 2-device measurement of "m" on a and b, 10000 MiB on
// each, whose peak on a (12500) the placer does not see: the gate refuses
// the placer's pick a, b; any other node set has no per-node peaks.
func skewedPeaks(t *testing.T, mode string, ids ...string) *harness {
	t.Helper()
	var ns []core.Node
	for i, id := range ids {
		ns = append(ns, psNode(id, 2+2*i, 12643))
	}
	h := inMode(newHarness(t).nodes(ns...), mode)
	liveTight(h)
	h.cfg.Defaults.MaxPool = 3
	p := h.llama("m", 0, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	h.needs["m"] = NeedInfo{Need: placer.Measured(20000), FileBytes: 10 << 30}
	h.c.llamaBuild, h.c.servedBuild = "aaaaaaa1", "aaaaaaa1"
	h.c.putLearned(&learnedNeed{TotalMiB: 20000, key: needKeyOf(p), profile: "m", devices: 2, at: h.clk.Now(),
		placement: []core.NodeID{"a", "b"}, split: []float64{0.5, 0.5},
		Nodes: []learnedNode{{ID: "a", Fraction: 0.5, MiB: 10000, PeakMiB: 12500}, {ID: "b", Fraction: 0.5, MiB: 10000, PeakMiB: 10000}}})
	h.viewChanged()
	return h
}

// FS1: the gate refuses the placer's first choice a, b, but b, c passes:
// the profile starts there instead of being refused (or waiting).
func TestNeverFits_AnotherPlacementIsTriedBeforeRefusing(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		h := skewedPeaks(t, mode, "a", "b", "c")
		p := h.c.cfg.Profiles["m"]
		req, _, _ := h.c.request(p)
		first, err := placer.Place(req, h.c.freeCandidates(p))
		if err != nil || h.c.reserveGate(p, first, nil) == "" || first.Nodes[0].ID != "a" {
			t.Fatalf("%s: the setup does not make the gate refuse a, b: %v %v", mode, placementIDs(first), err)
		}
		r := h.c.dryPlan("m")
		if r.Action != PlanStart {
			t.Fatalf("%s: plan %s (%s), want start on another set", mode, r.Action, r.Reason)
		}
		w := h.request("m")
		if a, ok := h.peek(w); ok && a.err != nil {
			t.Fatalf("%s: refused %v", mode, a.err)
		}
		if got := h.nodesOfAny("m"); slices.Contains(got, "a") || len(got) != 2 {
			t.Fatalf("%s: placed on %v, want a set without a", mode, got)
		}
	}
}

// FS1 and FS2: when no placement passes, the refusal names what was tried
// and carries Retry-After as the forced one does.
func TestNeverFits_AutomaticRefusalNamesTheTriedAndRetryAfter(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		h := skewedPeaks(t, mode, "a", "b")
		e := refusedAtOnce(t, h, "m", ReasonDoesNotFit)
		for _, s := range []string{"tried 1: a, b", "node a", "short by", "waiting cannot help"} {
			if !strings.Contains(e.Message, s) {
				t.Errorf("%s: the message lacks %q: %s", mode, s, e.Message)
			}
		}
		if e.RetryAfter != queueRetryAfter {
			t.Errorf("%s: Retry-After %v, want %v", mode, e.RetryAfter, queueRetryAfter)
		}
	}
}

// The owner's case (fifth review): forced onto a, "small" (6000) loaded and
// idle there, "big" needs 19266. Even with small stopped a is short, so big
// is refused at once in both membership modes, small is not stopped, and
// the plan says the same. With small stopped it would fit: big evicts or
// waits as before.
func TestNeverFits_OwnersCaseShortEvenWithOursStopped(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, need := range []int{19266, 9000} {
			h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), mode)
			liveTight(h)
			h.llama("small", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
			h.viewChanged()
			if mode == "fleet" {
				h.release(h.ready("small"))
			} else {
				h.release(h.agentReady("small"))
			}
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB - 7000 })
			h.advance(time.Minute) // past min_hold and the release window
			h.llama("big", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
			h.needs["big"] = NeedInfo{Need: placer.Measured(need), FileBytes: 10 << 30}
			h.viewChanged()
			if need == 9000 {
				if r := h.c.dryPlan("big"); r.Action == PlanReject {
					t.Fatalf("%s 9000: fits once small is stopped, plan reject: %s", mode, r.Reason)
				}
				w := h.request("big")
				if a, ok := h.peek(w); ok && a.err != nil {
					t.Fatalf("%s 9000: refused %v", mode, a.err)
				}
				continue
			}
			e := refusedAtOnce(t, h, "big", ReasonForcedDoesNotFit)
			if e.RetryAfter != queueRetryAfter {
				t.Errorf("%s: Retry-After %v", mode, e.RetryAfter)
			}
			if mode == "fleet" {
				for _, s := range []string{"small stopped", "smaller of what it reported and what is reserved", "short by", "after freeing"} {
					if !strings.Contains(e.Message, s) {
						t.Errorf("%s: the message lacks %q: %s", mode, s, e.Message)
					}
				}
			}
			if in := h.c.insts["small"]; in == nil || in.state != stReady {
				t.Fatalf("%s: small was stopped for a profile that cannot fit", mode)
			}
		}
	}
}

// A forced node an operator paused or excluded refuses the request at once
// with the true reason (Retry-After for a pause: it can end); the plan says
// the same. An absent (offline) forced node keeps the request waiting: it
// may come back; so does one whose unpause has not reached its agent yet. Both membership modes, defaults and the tight values.
func TestNeverFits_PausedExcludedAbsentForcedNodes(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, tight := range []bool{false, true} {
			for _, kind := range []string{"paused", "unpause-pending", "excluded", "absent"} {
				name := mode + " " + kind
				if tight {
					name += " tight"
				}
				h := inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
				if tight {
					liveTight(h)
				}
				h.llama("m", 6000, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
				switch kind {
				case "paused":
					h.view.modify("a", func(n *core.Node) { n.Paused = true })
				case "unpause-pending":
					h.view.modify("a", func(n *core.Node) { n.Paused, n.UnpausePending = true, true })
					h.view.setUnplaced("a", UnplacedPaused) // as the agents pool lists it
				case "excluded":
					h.cfg.ExcludeNodes = []string{"a"}
				case "absent":
					h.view.modify("a", func(n *core.Node) { n.Online = false })
				}
				h.viewChanged()
				switch kind {
				case "absent", "unpause-pending":
					if r := h.c.dryPlan("m"); r.Action == PlanReject {
						t.Fatalf("%s: plan reject (%s), want a wait", name, r.Reason)
					}
					w := h.request("m")
					if a, ok := h.peek(w); ok && a.err != nil {
						t.Fatalf("%s: refused at once %v, want a wait", name, a.err)
					}
				case "excluded":
					e := refusedAtOnce(t, h, "m", ReasonNodeExcluded)
					if !strings.Contains(e.Message, "excluded by config") || strings.Contains(e.Message, "in use") {
						t.Fatalf("%s: %s", name, e.Message)
					}
				default:
					e := refusedAtOnce(t, h, "m", ReasonForcedUnavailable)
					if !strings.Contains(e.Message, "node a is paused") || strings.Contains(e.Message, "in use") {
						t.Fatalf("%s: %s", name, e.Message)
					}
					if e.RetryAfter != queueRetryAfter {
						t.Fatalf("%s: Retry-After %v", name, e.RetryAfter)
					}
				}
			}
		}
	}
}

// A request already waiting when its forced node is paused is refused on
// the next queue pass, with the same text.
func TestNeverFits_APauseRefusesTheWaitingRequest(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), mode)
		h.llama("other", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
		h.viewChanged()
		loadSmallAs(h, mode, "other")
		h.llama("m", 9000, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
		w := h.request("m")
		if _, ok := h.peek(w); ok {
			t.Fatalf("%s: answered before the pause", mode)
		}
		h.view.modify("a", func(n *core.Node) { n.Paused = true })
		h.viewChanged()
		a, ok := h.peek(w)
		if !ok || a.err == nil || a.err.Reason != ReasonForcedUnavailable || !strings.Contains(a.err.Message, "paused") {
			t.Fatalf("%s: after the pause answered %v %+v", mode, ok, a.err)
		}
	}
}
