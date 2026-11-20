package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// agentsHarness is a harness in agents membership with two remote nodes
// and a profile split over both.
func agentsHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), laptop("b", 7000))
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	return h
}

func (h *harness) lastLaunch() effLaunchWorkers {
	h.t.Helper()
	if len(h.wlaunches) == 0 {
		h.t.Fatalf("no worker launch requested")
	}
	return h.wlaunches[len(h.wlaunches)-1]
}

func (h *harness) launched(res WorkerResult) {
	h.t.Helper()
	h.step(evWorkersLaunched{iid: h.lastLaunch().spec.IID, res: res})
}

func readyIDs() WorkerResult {
	return WorkerResult{LaunchIDs: map[core.NodeID]string{"a": "l-a", "b": "l-b"}}
}

func (h *harness) histTypes(typ string) []history.Event {
	var out []history.Event
	for _, e := range h.histLog {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestAgents_WorkerLaunchesReplaceThePreflight(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m")
	if len(h.probes) != 0 || len(h.starts) != 0 {
		t.Fatal("no probe and no child before the worker launches are ready")
	}
	l := h.lastLaunch()
	if len(l.spec.Workers) != 2 || l.spec.Label != "m.gguf" {
		t.Fatalf("spec %+v", l.spec)
	}
	for _, wt := range l.spec.Workers {
		switch wt.Node {
		case "a": // shared memory: budget plus the watchdog floor
			if wt.MinMemAvailableMiB <= 1000 || wt.Addr != "192.0.2.2:50052" {
				t.Fatalf("a %+v", wt)
			}
		case "b":
			if wt.MinMemAvailableMiB != 1000 {
				t.Fatalf("b %+v", wt)
			}
		}
	}
	if h.inst("m").phase != phWorkers {
		t.Fatalf("phase %v", h.inst("m").phase)
	}
	h.launched(readyIDs())
	h.healthy("m")
	g := h.granted(w)
	if len(h.wstops) != 0 {
		t.Fatal("stopped while running")
	}
	h.release(g)
	if err := h.c.unload("m", false); err != nil {
		t.Fatal(err)
	}
	h.step(evTick{})
	h.wantState("m", "absent")
	if len(h.wstops) != 1 || h.wstops[0].iid != l.spec.IID || len(h.wstops[0].force) != 0 {
		t.Fatalf("stops %+v", h.wstops)
	}
}

func TestAgents_RefusalFailsTheLoadWithTheAgentsReason(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m")
	h.launched(WorkerResult{Node: "b", Reason: agentproto.ReasonLowMemory, Message: "MemAvailable 900 MiB is below 1000"})
	e := h.failed(w, 502, ReasonLoadFailed)
	if !strings.Contains(e.Message, "low_memory") || !strings.Contains(e.Message, "MemAvailable 900") {
		t.Fatalf("message %q", e.Message)
	}
	h.wantState("m", "absent")
	if len(h.wstops) != 1 {
		t.Fatalf("the other launches are stopped: %+v", h.wstops)
	}
	if len(h.histTypes(history.TWorkerLaunchFailed)) != 1 {
		t.Fatalf("history %+v", h.histLog)
	}
}

func TestAgents_ModelSourceUnreachableKeepsTheRequestQueued(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m")
	h.launched(WorkerResult{Node: "a", Reason: agentproto.ReasonModelSourceUnreachable, Message: "share probe failed"})
	h.pending(w)
	h.wantState("m", "absent")
	n := len(h.wlaunches)
	h.advance(4 * time.Second)
	if len(h.wlaunches) != n {
		t.Fatal("tried again before the hold ended")
	}
	h.advance(2 * time.Second)
	if len(h.wlaunches) != n+1 {
		t.Fatalf("not tried again after 5 s: %d launches", len(h.wlaunches))
	}
	h.launched(readyIDs())
	h.healthy("m")
	h.granted(w)
}

func TestAgents_ExitedLaunchFailsTheInstanceAndRaisesTheNeed(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m")
	h.launched(readyIDs())
	in := h.inst("m")
	before, _ := h.c.need(in.prof)
	h.step(evWorkerReport{r: WorkerReport{IID: in.iid, Node: "a", Addr: "192.0.2.2:50052",
		Exit: &agentproto.ExitInfo{Reason: agentproto.ExitWatchdog, Message: "MemAvailable fell below 1000 MiB"}}})
	h.failed(w, 502, ReasonLoadFailed)
	h.step(evTick{})
	h.wantState("m", "absent")
	if len(h.wstops) != 1 {
		t.Fatalf("stops %+v", h.wstops)
	}
	after, _ := h.c.need(h.cfg.Profiles["m"])
	if after.Need.MiB <= before.Need.MiB || after.Source != NeedLearned {
		t.Fatalf("need %d -> %d (%v)", before.Need.MiB, after.Need.MiB, after.Source)
	}
}

func TestAgents_ExitWhileReadyStopsTheInstance(t *testing.T) {
	h := agentsHarness(t)
	g := h.agentReady("m")
	in := h.inst("m")
	h.step(evWorkerReport{r: WorkerReport{IID: in.iid, Node: "b", Exit: &agentproto.ExitInfo{Reason: agentproto.ExitCrashed}}})
	h.release(g)
	h.step(evTick{})
	h.wantState("m", "absent")
	if len(h.histTypes(history.TWorkerExited)) != 1 || len(h.wstops) != 1 {
		t.Fatalf("history %v stops %v", h.histLog, h.wstops)
	}
}

func TestAgents_LostNodeEndsTheInstanceAtOnce(t *testing.T) {
	h := agentsHarness(t)
	h.release(h.agentReady("m"))
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.step(evTick{})
	h.wantState("m", "absent")
	if len(h.wstops) != 1 {
		t.Fatalf("stops %+v", h.wstops)
	}
	if len(h.rposter.posts) != 0 {
		t.Fatal("no recovery request for agents")
	}
}

func TestAgents_PausedNodeTakesNoNewLoad(t *testing.T) {
	h := agentsHarness(t)
	h.view.modify("a", func(n *core.Node) { n.Paused = true })
	w := h.request("m")
	h.pending(w)
	if len(h.wlaunches) != 0 {
		t.Fatal("launched on a paused node")
	}
	h.view.modify("a", func(n *core.Node) { n.Paused = false })
	h.viewChanged()
	if len(h.wlaunches) != 1 {
		t.Fatal("not launched after unpause")
	}
}

func TestAgents_ForceUnloadKillsAndForcesTheNode(t *testing.T) {
	h := agentsHarness(t)
	h.release(h.agentReady("m"))
	var r forceResult
	h.step(evQuery{fn: func(c *schedCore) { r = c.forceUnload("a") }})
	h.step(evTick{})
	if r.profile != "m" {
		t.Fatalf("result %+v", r)
	}
	h.wantState("m", "absent")
	if len(h.wstops) != 1 || len(h.wstops[0].force) != 1 || h.wstops[0].force[0] != "a" {
		t.Fatalf("stops %+v", h.wstops)
	}
	h.step(evQuery{fn: func(c *schedCore) { r = c.forceUnload("b") }})
	if r.profile != "" || len(h.wforce) != 1 || h.wforce[0] != "b" {
		t.Fatalf("idle node: %+v %v", r, h.wforce)
	}
}

func TestAgents_StopDuringWorkerLaunchStopsTheLaunches(t *testing.T) {
	h := agentsHarness(t)
	h.load("m")
	if err := h.c.unload("m", true); err != nil {
		t.Fatal(err)
	}
	h.step(evTick{})
	h.wantState("m", "absent")
	if len(h.wstops) != 1 {
		t.Fatalf("stops %+v", h.wstops)
	}
	h.launched(readyIDs()) // arrives late: ignored
	h.wantState("m", "absent")
}

func TestAgents_PerWorkerProgressFromTheAgent(t *testing.T) {
	h := agentsHarness(t)
	h.request("m")
	h.launched(readyIDs())
	in := h.inst("m")
	total, loaded, frac := int64(4<<30), int64(1<<30), 0.25
	h.step(evWorkerReport{r: WorkerReport{IID: in.iid, Node: "a", Addr: "192.0.2.2:50052",
		Load: &agentproto.LoadProgress{Fraction: &frac, BytesTotal: &total, BytesLoaded: &loaded, State: agentproto.LoadReceiving}}})
	ws := h.c.loadWorkers(in, false)
	var got *LoadWorker
	for i := range ws {
		if ws[i].Addr == "192.0.2.2:50052" {
			got = &ws[i]
		}
	}
	if got == nil || got.State != WorkerReceiving || got.BytesSent == nil || *got.BytesSent != loaded || got.BytesTotal != total {
		t.Fatalf("workers %+v", ws)
	}
}

// agentReady runs a request through the worker launches to ready.
func (h *harness) agentReady(profile string) *grant {
	h.t.Helper()
	w := h.request(profile)
	h.launched(readyIDs())
	h.healthy(profile)
	return h.granted(w)
}

func TestAgents_LoadGateRefusesTheLoad(t *testing.T) {
	h := agentsHarness(t)
	h.c.loadGate = func() error { return errors.New("the host program is build abc1234, the release needs a01665d30") }
	w := h.request("m")
	e := h.failed(w, 502, ReasonLoadFailed)
	if !strings.Contains(e.Message, "a01665d30") || len(h.wlaunches) != 0 {
		t.Fatalf("%q %d", e.Message, len(h.wlaunches))
	}
}

// Force unload clears a bad state: requests waiting for the instance fail at
// once with an error naming the force unload and the node (they do not go
// back to the queue). Node loss, by contrast, requeues them.
func TestAgents_ForceUnloadFailsTheWaitersOfAStartingInstance(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m") // the instance waits for its worker launches
	h.pending(w)
	var r forceResult
	h.step(evQuery{fn: func(c *schedCore) { r = c.forceUnload("a") }})
	h.step(evTick{})
	if r.profile != "m" {
		t.Fatalf("result %+v", r)
	}
	err := h.failed(w, 502, ReasonLoadFailed)
	if !strings.Contains(err.Message, "force unload of node a") {
		t.Fatalf("message %q", err.Message)
	}
	h.wantState("m", "absent")
	if n := len(h.wlaunches); n != 1 {
		t.Fatalf("the failed request was put back and launched again: %d launches", n)
	}
	// Not a load failure of the profile: the next request starts a load at once.
	w2 := h.request("m")
	h.pending(w2)
	if n := len(h.wlaunches); n != 2 {
		t.Fatalf("profile held back after a force unload: %d launches", n)
	}
}

func TestAgents_LostNodeRequeuesTheWaitersOfAStartingInstance(t *testing.T) {
	h := agentsHarness(t)
	w := h.request("m")
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.step(evTick{})
	h.pending(w)
}
