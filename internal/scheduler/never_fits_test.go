package scheduler

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
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

// SS3 (sixth review): a request already waiting when its forced node is
// paused keeps waiting, as in 0.1.6, and is served after the unpause; a new
// request during the pause is refused at once, as the plan says.
func TestNeverFits_APauseKeepsTheWaitingRequest(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12643))
	h.llama("other", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.viewChanged()
	loadSmallAs(h, "fleet", "other")
	h.llama("m", 9000, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
	w := h.request("m")
	if _, ok := h.peek(w); ok {
		t.Fatalf("answered before the pause")
	}
	h.view.modify("a", func(n *core.Node) { n.Paused = true })
	h.viewChanged()
	h.advance(time.Minute)
	if a, ok := h.peek(w); ok {
		t.Fatalf("the waiting request was answered during the pause: %+v", a.err)
	}
	if r := h.c.dryPlan("m"); r.Action != PlanReject || !strings.Contains(r.Reason, "paused by an operator") {
		t.Fatalf("plan during the pause: %s %s", r.Action, r.Reason)
	}
	w2 := h.request("m")
	if a, ok := h.peek(w2); !ok || a.err == nil || !strings.Contains(a.err.Message, "paused by an operator") || a.err.RetryAfter != queueRetryAfter {
		t.Fatalf("a new request during the pause: answered %v %+v", ok, a.err)
	}
	h.view.modify("a", func(n *core.Node) { n.Paused = false })
	h.viewChanged()
	h.advance(time.Minute)
	if a, ok := h.peek(w); ok && a.err != nil {
		t.Fatalf("refused after the unpause: %v", a.err)
	}
	if h.c.insts["m"] == nil {
		t.Fatalf("m was not started after the unpause (wait: %s)", h.waitReasonOf("m"))
	}
}

// SS1 (sixth review): a just-released forced node whose figure still shows
// what left it is not counted short by the owner's-case refusal (gate on)
// nor by the older refusal before eviction (agents, also with the
// defaults), when another forced node has a holder. "small" (6000) leaves
// a with the view still showing 7000 used; "x" (4000) is idle on b; "two"
// (16000, forced onto a and b, 1:1) fits once a's memory is back and x is
// stopped.
func TestNeverFits_JustReleasedNodeWithAHolderOnAnother(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, tight := range []bool{true, false} {
			name := fmt.Sprintf("%s tight %v", mode, tight)
			h := inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
			if tight {
				liveTight(h)
			}
			h.llama("x", 4000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
			h.viewChanged()
			if mode == "fleet" {
				h.release(h.ready("x"))
			} else {
				h.release(h.agentReady("x"))
			}
			h.view.modify("b", func(n *core.Node) { n.FreeMiB = n.TotalMiB - 4500 })
			h.advance(time.Minute) // x idle past min_hold
			unloadLagging(h, mode)
			h.llama("two", 0, func(p *core.Profile) {
				p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{1, 1}, time.Hour
			})
			h.needs["two"] = NeedInfo{Need: placer.Measured(16000), FileBytes: 8 << 30}
			if r := h.c.dryPlan("two"); r.Action == PlanReject {
				t.Fatalf("%s: plan reject right after the unload: %s", name, r.Reason)
			}
			w := h.request("two")
			if a, ok := h.peek(w); ok && a.err != nil {
				t.Fatalf("%s: refused right after the unload: %v", name, a.err)
			}
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB })
			h.viewChanged()
			if a, ok := h.peek(w); ok && a.err != nil {
				t.Fatalf("%s: refused once the view caught up: %v", name, a.err)
			}
		}
	}
}

// SS2 (sixth review): when the set the gate refused holds a just-released
// node, the placer is not asked for another set: the request waits for the
// next view. Once nothing there can change any more, the retry runs.
func TestNeverFits_NoRetryWhileTheRefusedSetWasJustReleased(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		h := skewedPeaks(t, mode, "a", "b", "c")
		// something of ours just left b (its figure is already back)
		h.llama("small", 3000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
		h.viewChanged()
		if mode == "fleet" {
			h.release(h.ready("small"))
		} else {
			h.release(h.agentReady("small"))
		}
		h.step(evUnload{profile: "small", force: true})
		h.advance(10 * time.Second)
		if !h.c.justReleased("b") {
			t.Fatalf("%s: b is not counted as just released", mode)
		}
		w := h.request("m")
		if a, ok := h.peek(w); ok {
			t.Fatalf("%s: answered right after the unload: %+v", mode, a.err)
		}
		if h.c.insts["m"] != nil {
			t.Fatalf("%s: started on %v instead of waiting for the next view", mode, h.nodesOfAny("m"))
		}
		h.advance(30 * time.Second)
		h.viewChanged()
		if a, ok := h.peek(w); ok && a.err != nil {
			t.Fatalf("%s: refused after the window: %v", mode, a.err)
		}
		if got := h.nodesOfAny("m"); slices.Contains(got, "a") || len(got) != 2 {
			t.Fatalf("%s: after the window placed on %v, want a set without a", mode, got)
		}
	}
}

// SS1, the all-free case of the same check, with the defaults: in agents
// membership a forced load right after an unload on its one node is held
// to the node's figure (agentForcedFit), which may still show the unloaded
// model. 0.1.6 refused it at once; now it waits and starts once the view
// catches up. The plan says queue meanwhile.
func TestNeverFits_AgentsForcedFitWaitsOnAJustReleasedNode(t *testing.T) {
	h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), "agents")
	unloadLagging(h, "agents")
	h.llama("mid", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
	h.needs["mid"] = NeedInfo{Need: placer.Measured(9000), FileBytes: 5 << 30}
	if r := h.c.dryPlan("mid"); r.Action != PlanQueue || !strings.Contains(r.Reason, "just left") {
		t.Fatalf("plan %s (%s), want queue", r.Action, r.Reason)
	}
	w := h.request("mid")
	if a, ok := h.peek(w); ok {
		t.Fatalf("answered right after the unload: %+v", a.err)
	}
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB })
	h.viewChanged()
	if h.c.insts["mid"] == nil {
		t.Fatalf("mid did not start once the view caught up (wait: %s)", h.waitReasonOf("mid"))
	}
	// a real misfit is still refused once the window is over
	h2 := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), "agents")
	unloadLagging(h2, "agents")
	h2.llama("big", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
	h2.needs["big"] = NeedInfo{Need: placer.Measured(19266), FileBytes: 10 << 30}
	w = h2.request("big")
	h2.advance(31 * time.Second)
	h2.viewChanged()
	if a, ok := h2.peek(w); !ok || a.err == nil || a.err.Reason != ReasonForcedDoesNotFit {
		t.Fatalf("a real misfit after the window: answered %v %+v", ok, a.err)
	}
}

// AB1 (seventh review): a forced request that can never fit, made within the
// window after an unload on one of its nodes, evicts nothing: it waits
// while the figure may lag, then is refused before any eviction. x (4000)
// is idle on b; "small" left a with the view still showing it; "two" is
// forced onto a and b with a need of 2 x 19266. Both modes, both settings.
func TestNeverFits_NoEvictionForANeverFitAfterAnUnload(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, tight := range []bool{false, true} {
			name := fmt.Sprintf("%s tight %v", mode, tight)
			h := inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
			if tight {
				liveTight(h)
			}
			h.llama("x", 4000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
			h.viewChanged()
			if mode == "fleet" {
				h.release(h.ready("x"))
			} else {
				h.release(h.agentReady("x"))
			}
			h.view.modify("b", func(n *core.Node) { n.FreeMiB = n.TotalMiB - 4500 })
			h.advance(time.Minute)
			unloadLagging(h, mode)
			h.llama("two", 0, func(p *core.Profile) {
				p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{1, 1}, time.Hour
			})
			h.needs["two"] = NeedInfo{Need: placer.Measured(2 * 19266), FileBytes: 20 << 30}
			legacy := mode == "fleet" && !tight // nothing refuses before eviction there
			if r := h.c.dryPlan("two"); r.Action == PlanEvict && !legacy {
				t.Fatalf("%s: plan evict within the window: %s", name, r.Reason)
			}
			w := h.request("two")
			h.advance(5 * time.Second)
			if !legacy && (len(h.of(history.TEviction)) != 0 || h.c.insts["x"] == nil || h.c.insts["x"].state != stReady) {
				t.Fatalf("%s: x was evicted for a request that cannot fit", name)
			}
			// the view catches up and the window ends: refused, x kept. In
			// fleet membership with the defaults a forced placement has no
			// fit check before its launch (design 7.3, forced_fit_check
			// warn): x is evicted and two launched, as in 0.1.6; never an
			// eviction followed by a refusal.
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB })
			h.advance(30 * time.Second)
			h.viewChanged()
			a, ok := h.peek(w)
			refused, evicted := ok && a.err != nil, len(h.of(history.TEviction))
			if refused && evicted > 0 {
				t.Fatalf("%s: x was evicted and the request refused anyway", name)
			}
			if mode == "fleet" && !tight {
				if refused || h.c.insts["two"] == nil {
					t.Fatalf("%s: want the 0.1.6 fleet path (evict, launch): answered %v %+v", name, ok, a.err)
				}
				continue
			}
			if !refused || evicted != 0 || h.c.insts["x"] == nil {
				t.Fatalf("%s: once the figures are fresh: answered %v %+v, x %v, evictions %d", name, ok, a.err, h.c.insts["x"] != nil, evicted)
			}
		}
	}
}

// AS1 (seventh review): after an eviction the claimed node is just released
// and its figure may still show the victim. The request waits on its claim
// (no refusal, no claim dropped, no empty figure learned from the lagging
// view) and starts once the view catches up. Both modes, both settings.
func TestNeverFits_StartOnClaimWaitsForAFreshFigure(t *testing.T) {
	for _, mode := range []string{"fleet", "agents"} {
		for _, tight := range []bool{false, true} {
			name := fmt.Sprintf("%s tight %v", mode, tight)
			h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), mode)
			if tight {
				liveTight(h)
			}
			h.llama("x", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
			h.viewChanged()
			if mode == "fleet" {
				h.release(h.ready("x"))
			} else {
				h.release(h.agentReady("x"))
			}
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB - 7000 })
			h.advance(time.Minute)
			h.llama("mid", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
			h.needs["mid"] = NeedInfo{Need: placer.Measured(9000), FileBytes: 5 << 30}
			h.viewChanged()
			w := h.request("mid")
			h.advance(10 * time.Second) // x stops, the node settles; the view still shows x
			if h.c.insts["x"] != nil {
				t.Fatalf("%s: x was not evicted (plan %+v)", name, h.c.dryPlan("mid"))
			}
			if a, ok := h.peek(w); ok && a.err != nil {
				t.Fatalf("%s: refused after the eviction on the lagging figure: %v", name, a.err)
			}
			if ns := h.c.nodes["a"]; ns != nil && ns.hasClaim {
				t.Fatalf("%s: an empty figure %d was learned from the lagging view", name, ns.emptyClaim)
			}
			h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB })
			h.viewChanged()
			h.advance(time.Second)
			if a, ok := h.peek(w); ok && a.err != nil {
				t.Fatalf("%s: refused once the view caught up: %v", name, a.err)
			}
			if h.c.insts["mid"] == nil {
				t.Fatalf("%s: mid did not start once the view caught up (wait: %s)", name, h.waitReasonOf("mid"))
			}
		}
	}
}

// AS2 (seventh review): a load cancelled in the worker phase leaves its
// nodes just released (the workers may have held memory there).
func TestNeverFits_ACancelledWorkerLoadLeavesTheNodeJustReleased(t *testing.T) {
	h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), "agents")
	h.llama("small", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.viewChanged()
	h.request("small")
	if in := h.c.insts["small"]; in == nil || in.phase != phWorkers || !in.workers {
		t.Fatalf("small is not in its worker phase")
	}
	h.step(evUnload{profile: "small", force: true})
	if h.c.insts["small"] != nil {
		t.Fatalf("small is still there")
	}
	if !h.c.justReleased("a") {
		t.Fatalf("a is not just released after a cancelled worker load")
	}
}

// runTo moves the clock by d the way production drives the core: a step
// only at each core deadline on the way, none at the end (advance also
// ticks at its target, which hides a missing deadline).
func (h *harness) runTo(d time.Duration) {
	h.t.Helper()
	target := h.clk.Now().Add(d)
	for i := 0; i < 100000; i++ {
		dl, ok := h.c.nextDeadline()
		if !ok || dl.After(target) {
			break
		}
		h.clk.Set(dl)
		h.step(evTick{})
	}
	h.clk.Set(target)
}

// neverFitAfterUnload is the AB1 situation: x idle on b, small just left a
// with the view still showing it, "two" (2 x 19266, forced onto a and b)
// requested. It returns the waiter.
func neverFitAfterUnload(t *testing.T, mode string, tight bool) (*harness, *waiter) {
	t.Helper()
	h := inMode(newHarness(t).nodes(psNode("a", 2, 12643), psNode("b", 4, 12643)), mode)
	if tight {
		liveTight(h)
	}
	h.llama("x", 4000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
	h.viewChanged()
	if mode == "fleet" {
		h.release(h.ready("x"))
	} else {
		h.release(h.agentReady("x"))
	}
	h.view.modify("b", func(n *core.Node) { n.FreeMiB = n.TotalMiB - 4500 })
	h.advance(time.Minute)
	unloadLagging(h, mode)
	h.llama("two", 0, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{1, 1}, time.Hour
	})
	h.needs["two"] = NeedInfo{Need: placer.Measured(2 * 19266), FileBytes: 20 << 30}
	w := h.request("two")
	if _, ok := h.peek(w); ok {
		t.Fatalf("answered right after the unload")
	}
	return h, w
}

// ES1 (eighth review): the end of the window is a deadline. With no event
// at all (a quiet view), the waiting request is decided when the window
// ends, not at the next view change.
func TestNeverFits_TheWindowEndIsADeadline(t *testing.T) {
	for _, tight := range []bool{false, true} {
		h, w := neverFitAfterUnload(t, "agents", tight)
		end, _ := h.c.releaseWindowEnd("a")
		h.runTo(end.Sub(h.clk.Now()) - time.Second)
		if _, ok := h.peek(w); ok {
			t.Fatalf("tight %v: decided before the window ended", tight)
		}
		h.runTo(2 * time.Second)
		a, ok := h.peek(w)
		if !ok || a.err == nil {
			t.Fatalf("tight %v: not decided when the window ended (no event came)", tight)
		}
		if len(h.of(history.TEviction)) != 0 {
			t.Fatalf("tight %v: evicted for it", tight)
		}
	}
}

// heartbeat models a heartbeat of node id arriving now: a reading, after
// the worker stop when stopped (the first of those also sees the stop),
// with the old figure unless caughtUp.
func (h *harness) heartbeat(id core.NodeID, stopped, caughtUp bool) {
	h.t.Helper()
	now := h.clk.Now()
	h.view.modify(id, func(n *core.Node) {
		n.FigureAt = now
		if stopped {
			if n.StopSeenAt.IsZero() {
				n.StopSeenAt, n.FiguresSinceStop = now, 1 // the stop, then this reading
			} else {
				n.FiguresSinceStop++
			}
		}
		if caughtUp {
			n.FreeMiB = n.TotalMiB
		}
	})
	h.viewChanged()
}

// ES2 and NB1 (eighth and ninth review): the wait on a just-released node
// with worker launches ends only with a reading certainly measured after
// the workers stopped: the second one after the view saw their launch end.
// Agents membership, both settings, both outcomes.
func TestNeverFits_AFreshFigureEndsTheWait(t *testing.T) {
	for _, tight := range []bool{false, true} {
		// (a) and the refusal: readings before the stop, and the first one
		// after it, do not end the wait; the second ends it, no eviction
		h, w := neverFitAfterUnload(t, "agents", tight)
		h.view.mu.Lock()
		asked, ok := h.view.wantFig["a"]
		h.view.mu.Unlock()
		if !ok || !asked.Equal(h.c.nodes["a"].releasedAt) {
			t.Fatalf("tight %v: the view was asked for readings of a after %v (%v)", tight, asked, ok)
		}
		h.runTo(time.Second)
		h.heartbeat("a", false, false) // received after the release, measured before the stop
		h.runTo(time.Second)
		h.heartbeat("a", true, false) // the stop is seen; this reading may predate it
		if _, ok := h.peek(w); ok {
			t.Fatalf("tight %v: a reading before or at the worker stop ended the wait", tight)
		}
		h.runTo(2 * time.Second)
		h.heartbeat("a", true, true)
		if a, ok := h.peek(w); !ok || a.err == nil {
			t.Fatalf("tight %v: the second reading after the stop did not end the wait (refusal)", tight)
		}
		if len(h.of(history.TEviction)) != 0 {
			t.Fatalf("tight %v: evicted for it", tight)
		}

		// (c) the eviction probe: no 503 after the eviction, start once the
		// reading after the stop shows the memory back
		h = inMode(newHarness(t).nodes(psNode("a", 2, 12643)), "agents")
		if tight {
			liveTight(h)
		}
		h.llama("x", 6000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
		h.viewChanged()
		h.release(h.agentReady("x"))
		h.view.modify("a", func(n *core.Node) { n.FreeMiB = n.TotalMiB - 7000 })
		h.advance(time.Minute)
		h.llama("mid", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
		h.needs["mid"] = NeedInfo{Need: placer.Measured(9000), FileBytes: 5 << 30}
		h.viewChanged()
		w = h.request("mid")
		h.runTo(5 * time.Second) // x stops, the node settles; the view lags
		if h.c.insts["x"] != nil || h.c.insts["mid"] != nil {
			t.Fatalf("tight %v: x %v mid %v after 5 s", tight, h.c.insts["x"] != nil, h.c.insts["mid"] != nil)
		}
		h.heartbeat("a", false, false)
		h.runTo(time.Second)
		h.heartbeat("a", true, false)
		if a, ok := h.peek(w); ok && a.err != nil {
			t.Fatalf("tight %v: refused after the eviction on a reading from before the stop: %v", tight, a.err)
		}
		h.runTo(2 * time.Second)
		h.heartbeat("a", true, true)
		if h.c.insts["mid"] == nil {
			t.Fatalf("tight %v: the reading after the stop did not let mid start", tight)
		}
		if a, ok := h.peek(w); ok && a.err != nil {
			t.Fatalf("tight %v: refused %v", tight, a.err)
		}

		// (d) the stop is never seen: readings alone do not end the wait;
		// the window does
		h, w = neverFitAfterUnload(t, "agents", tight)
		end, _ := h.c.releaseWindowEnd("a")
		for h.clk.Now().Add(2 * time.Second).Before(end) {
			h.runTo(2 * time.Second)
			h.heartbeat("a", false, true)
		}
		if _, ok := h.peek(w); ok {
			t.Fatalf("tight %v: decided before the window ended without a stop seen", tight)
		}
		h.runTo(end.Sub(h.clk.Now()) + time.Second)
		if a, ok := h.peek(w); !ok || a.err == nil {
			t.Fatalf("tight %v: not decided at the window's end", tight)
		}
	}
}

// NB1 (b): unload, then at once a forced load with the tight values: the
// stale readings around the worker stop do not refuse it; it is served once
// the reading after the stop shows the memory back.
func TestNeverFits_UnloadThenForcedLoadIsServed(t *testing.T) {
	h := inMode(newHarness(t).nodes(psNode("a", 2, 12643)), "agents")
	liveTight(h)
	unloadLagging(h, "agents")
	h.llama("mid", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
	h.needs["mid"] = NeedInfo{Need: placer.Measured(9000), FileBytes: 5 << 30}
	w := h.request("mid")
	h.heartbeat("a", false, false)
	h.runTo(time.Second)
	h.heartbeat("a", true, false)
	if a, ok := h.peek(w); ok && a.err != nil {
		t.Fatalf("refused on a reading from before the stop: %v", a.err)
	}
	h.runTo(2 * time.Second)
	h.heartbeat("a", true, true)
	if h.c.insts["mid"] == nil {
		t.Fatalf("mid was not served (wait: %s)", h.waitReasonOf("mid"))
	}
}

// fleet membership reports no reading times: a fleet wait lasts the window
// (documented); a local node's reading after settle + 2 heartbeats counts.
func TestNeverFits_NoStopToSeeUsesTheFallback(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12643))
	liveTight(h)
	unloadLagging(h, "fleet")
	if h.c.nodes["a"].expectStop {
		t.Fatal("fleet: a stop is expected where no workers ran")
	}
	if !h.c.justReleased("a") {
		t.Fatal("a is not just released")
	}
	h.view.modify("a", func(n *core.Node) { n.FigureAt = h.c.freshAfter("a") }) // not after it
	h.viewChanged()
	if !h.c.justReleased("a") {
		t.Fatal("a reading at freshAfter counted as fresh")
	}
	h.view.modify("a", func(n *core.Node) { n.FigureAt = h.c.freshAfter("a").Add(time.Millisecond) })
	h.viewChanged()
	if h.c.justReleased("a") {
		t.Fatal("a reading after freshAfter did not count")
	}
}
