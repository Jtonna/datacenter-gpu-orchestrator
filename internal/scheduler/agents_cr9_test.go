package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

func targets(l effLaunchWorkers) map[core.NodeID]WorkerTarget {
	out := map[core.NodeID]WorkerTarget{}
	for _, wt := range l.spec.Workers {
		out[wt.Node] = wt
	}
	return out
}

// CR-9, 1a: every worker launch carries the node's share of the need. The
// harness profile needs 8000 MiB, split 1:1 over a (shared memory) and b
// (dedicated): a's share takes the split skew margin, as the fit rules do.
func TestAgents_LaunchCarriesTheNodesShareAsExpectMiB(t *testing.T) {
	h := agentsHarness(t)
	h.request("m")
	ts := targets(h.lastLaunch())
	if ts["a"].ExpectMiB != 4600 || ts["b"].ExpectMiB != 4000 {
		t.Fatalf("expect_mib a %d b %d, want 4600 and 4000", ts["a"].ExpectMiB, ts["b"].ExpectMiB)
	}

	// A single node is expected to take the whole need.
	h = newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.llama("solo", 8000)
	h.request("solo")
	if ts := targets(h.lastLaunch()); ts["a"].ExpectMiB != 8000 {
		t.Fatalf("single node expect_mib %d, want 8000", ts["a"].ExpectMiB)
	}
}

// CR-9, 2: min_mem_available_mib is built with the node's own watchdog floor
// when its agent reports one.
func TestAgents_MinMemUsesTheNodesReportedFloor(t *testing.T) {
	h := agentsHarness(t)
	h.request("m")
	base := targets(h.lastLaunch())

	h = agentsHarness(t)
	h.view.modify("a", func(n *core.Node) { n.WatchdogFloorMiB = 2500 })
	h.viewChanged()
	h.request("m")
	ts := targets(h.lastLaunch())
	if got, want := ts["a"].MinMemAvailableMiB, base["a"].MinMemAvailableMiB+1500; got != want {
		t.Fatalf("a: min_mem_available_mib %d, want %d (floor 2500 in place of 1000)", got, want)
	}
	if ts["b"].MinMemAvailableMiB != base["b"].MinMemAvailableMiB {
		t.Fatalf("b reports no floor and must keep the setting: %d", ts["b"].MinMemAvailableMiB)
	}
}

// CR-9, 1c: would_not_fit is a clean failed load: 502 with the agent's reason
// and message, in history, no quarantine of the node, nothing learned.
func TestAgents_WouldNotFitIsACleanFailedLoad(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m")
	before, _ := h.c.need(h.cfg.Profiles["m"])
	msg := "MemAvailable 5000 MiB minus the expected 4600 MiB leaves less than the watchdog floor of 1000 MiB"
	h.launched(WorkerResult{Node: "a", Reason: agentproto.ReasonWouldNotFit, Message: msg})
	e := h.failed(w, 502, ReasonLoadFailed)
	if !strings.Contains(e.Message, "would_not_fit") || !strings.Contains(e.Message, msg) {
		t.Fatalf("message %q", e.Message)
	}
	h.wantState("m", "absent")
	if h.c.quarantined("a") || h.c.quarantined("b") {
		t.Fatal("a would_not_fit refusal quarantined a node")
	}
	after, _ := h.c.need(h.cfg.Profiles["m"])
	if after.Need.MiB != before.Need.MiB || after.Source == NeedLearned || len(h.histTypes(history.TLearnedNeed)) != 0 {
		t.Fatalf("the need changed: %d -> %d (%v)", before.Need.MiB, after.Need.MiB, after.Source)
	}
	ev := h.histTypes(history.TWorkerLaunchFailed)
	if len(ev) != 1 || ev[0].Data["reason"] != agentproto.ReasonWouldNotFit || ev[0].Data["message"] != msg {
		t.Fatalf("history %+v", ev)
	}
	if len(h.wstops) != 1 {
		t.Fatalf("the other launches are stopped: %+v", h.wstops)
	}
}

// CR-9, 1d: a forced placement whose share of a node exceeds what the node's
// agent reports as usable (available minus the watchdog floor on a
// shared-memory node, the device's free memory otherwise) is refused when it
// is placed, whatever forced_fit_check says; no launch is sent.
func TestAgents_ForcedPlacementThatWouldNotFitIsRefusedWhenPlaced(t *testing.T) {
	for _, tc := range []struct {
		name, node string
		free       int
		parts      []string
	}{
		{"shared memory", "a", 4599, []string{"a", "4600", "4599", "watchdog floor"}},
		{"dedicated memory", "b", 3999, []string{"b", "4000", "3999"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := agentsHarness(t) // forced_fit_check is off
			h.view.modify(core.NodeID(tc.node), func(n *core.Node) { n.FreeMiB = tc.free })
			h.viewChanged()
			w := h.request("m")
			e := h.failed(w, 503, ReasonForcedDoesNotFit)
			for _, p := range tc.parts {
				if !strings.Contains(e.Message, p) {
					t.Fatalf("message %q does not name %q", e.Message, p)
				}
			}
			if len(h.wlaunches) != 0 || len(h.starts) != 0 {
				t.Fatalf("something was sent: %d launches, %d starts", len(h.wlaunches), len(h.starts))
			}
			h.wantState("m", "absent")
			if rep := h.c.dryPlan("m"); rep.Action != PlanReject || !strings.Contains(rep.Reason, tc.parts[1]) {
				t.Fatalf("plan %+v", rep)
			}
		})
	}
	// Exactly the usable memory is placed.
	h := agentsHarness(t)
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 4600 })
	h.view.modify("b", func(n *core.Node) { n.FreeMiB = 4000 })
	h.viewChanged()
	h.request("m")
	if len(h.wlaunches) != 1 {
		t.Fatalf("%d launches, want 1", len(h.wlaunches))
	}
}

// CR-9 review, A1: a forced node whose memory figure is unknown (its agent
// reported no usable device) is refused under the fit check, never sent
// unchecked. The refusal names the node and says its figure is unknown.
func TestAgents_ForcedNodeWithUnknownFigureIsRefused(t *testing.T) {
	h := agentsHarness(t) // forced_fit_check is off
	h.view.modify("a", func(n *core.Node) { n.FreeUnknown = true })
	h.viewChanged()
	w := h.request("m")
	e := h.failed(w, 503, ReasonForcedDoesNotFit)
	if !strings.Contains(e.Message, "node a") || !strings.Contains(e.Message, "memory figure is unknown") {
		t.Fatalf("message %q", e.Message)
	}
	if len(h.wlaunches) != 0 || len(h.starts) != 0 {
		t.Fatalf("something was sent: %d launches, %d starts", len(h.wlaunches), len(h.starts))
	}
	if rep := h.c.dryPlan("m"); rep.Action != PlanReject || !strings.Contains(rep.Reason, "unknown") {
		t.Fatalf("plan %+v", rep)
	}
}

// CR-9 review, A3: in agents membership the forced-placement refusal does not
// depend on defaults.forced_fit_check: with the setting "off" a forced
// placement that does not fit is still refused, and nothing is sent.
func TestAgents_ForcedRefusalIgnoresForcedFitCheckOff(t *testing.T) {
	h := agentsHarness(t)
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitOff
	h.view.modify("a", func(n *core.Node) { n.FreeMiB = 4599 })
	h.viewChanged()
	w := h.request("m")
	h.failed(w, 503, ReasonForcedDoesNotFit)
	if len(h.wlaunches) != 0 || len(h.starts) != 0 {
		t.Fatalf("something was sent: %d launches, %d starts", len(h.wlaunches), len(h.starts))
	}
}
