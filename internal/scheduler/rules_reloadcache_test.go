package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// reloadConsole is agents membership with one 14 GB console "a" (tier headroom
// 1000 MiB, watchdog floor 1000 MiB) and a profile "m" whose file is fileGB
// and whose loaded model is modelGB.
func reloadConsole(t *testing.T, fileDir bool, fileGB, modelGB float64) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 13000))
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.cfg.Defaults.WorkerModelDir = fileDir
	h.c.agents = true
	h.view.modify("a", func(n *core.Node) { n.SystemMiB, n.WatchdogFloorMiB = gbMiB(14), 1000 })
	h.viewChanged()
	h.llama("m", gbMiB(modelGB))
	h.needs["m"] = NeedInfo{Need: placer.Measured(gbMiB(modelGB)), FileBytes: int64(fileGB * 1e9)}
	return h
}

// The measured case that reloads slowly: 7.0 GB file + 7.5 GB model on a 14
// GB console. The plan says partial, with the share of the file that stays.
func TestReloadPlan_PartialOnTheConsole(t *testing.T) {
	h := reloadConsole(t, true, 7.0, 7.5)
	rep := h.c.dryPlan("m")
	if rep.Action != PlanStart || rep.LoadMode != LoadModeFileRead {
		t.Fatalf("plan %s, mode %q", rep.Action, rep.LoadMode)
	}
	if len(rep.ReloadFromCache) != 1 {
		t.Fatalf("reload %+v", rep.ReloadFromCache)
	}
	r := rep.ReloadFromCache[0]
	if r.ID != "a" || r.Verdict != ReloadPartial || r.CachedPct == nil || *r.CachedPct != 63 {
		t.Fatalf("reload %+v (pct %v)", r, r.CachedPct)
	}
	if r.FileMiB != gbMiB(7.0)+1 && r.FileMiB != gbMiB(7.0) || r.ModelMiB != gbMiB(7.5) || r.MemoryMiB != gbMiB(14) || r.MarginMiB != 2000 || !r.Shared {
		t.Fatalf("figures %+v", r)
	}
}

// The measured case that reloads fast: 2.7 GB + 2.9 GB.
func TestReloadPlan_FullOnTheConsole(t *testing.T) {
	h := reloadConsole(t, true, 2.7, 2.9)
	rep := h.c.dryPlan("m")
	if len(rep.ReloadFromCache) != 1 || rep.ReloadFromCache[0].Verdict != ReloadFull || rep.ReloadFromCache[0].CachedPct != nil {
		t.Fatalf("reload %+v", rep.ReloadFromCache)
	}
}

// Pushed weights: the worker does not read the file, so the figure is unknown.
func TestReloadPlan_PushedIsUnknown(t *testing.T) {
	h := reloadConsole(t, false, 2.7, 2.9)
	rep := h.c.dryPlan("m")
	if rep.LoadMode != LoadModePushed || len(rep.ReloadFromCache) != 1 || rep.ReloadFromCache[0].Verdict != ReloadUnknown {
		t.Fatalf("mode %q reload %+v", rep.LoadMode, rep.ReloadFromCache)
	}
}

// The GPU alone (dedicated memory, no remote worker): local load mode; the
// file share has to fit in the scheduler machine's system memory only.
func TestReloadPlan_LocalDedicatedGPU(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7800))
	h.viewChanged()
	h.c.localSysMiB = 32000
	h.llama("m", 7000)
	h.needs["m"] = NeedInfo{Need: placer.Measured(7000), FileBytes: 7e9}
	rep := h.c.dryPlan("m")
	if rep.LoadMode != LoadModeLocal || len(rep.ReloadFromCache) != 1 {
		t.Fatalf("mode %q reload %+v", rep.LoadMode, rep.ReloadFromCache)
	}
	if r := rep.ReloadFromCache[0]; r.Verdict != ReloadFull || r.MemoryMiB != 32000 || r.Shared {
		t.Fatalf("reload %+v", r)
	}
	// Unknown system memory: unknown.
	h.c.localSysMiB = 0
	if r := h.c.dryPlan("m").ReloadFromCache[0]; r.Verdict != ReloadUnknown {
		t.Fatalf("reload %+v", r)
	}
}

// A plan with no placement carries neither figure; a loaded instance's plan
// and its status carry the load mode.
func TestReloadPlan_InstanceAndQueue(t *testing.T) {
	h := reloadConsole(t, true, 2.7, 2.9)
	h.llama("huge", 99000)
	if rep := h.c.dryPlan("huge"); rep.LoadMode != "" || rep.ReloadFromCache != nil {
		t.Fatalf("plan without placement: %+v", rep)
	}
	w := h.request("m")
	h.launched(WorkerResult{LaunchIDs: map[core.NodeID]string{"a": "l-a"}})
	h.healthy("m")
	h.release(h.granted(w))
	h.advance(time.Second)
	rep := h.c.dryPlan("m")
	if rep.Action != PlanServe || rep.LoadMode != LoadModeFileRead || len(rep.ReloadFromCache) != 1 {
		t.Fatalf("plan %+v", rep)
	}
	st := h.c.statusDoc()
	if len(st.Instances) != 1 || st.Instances[0].LoadMode != LoadModeFileRead {
		t.Fatalf("status %+v", st.Instances)
	}
}

func TestParseMemTotal(t *testing.T) {
	if got := parseMemTotalMiB("MemFree:  100 kB\nMemTotal:       32768000 kB\n"); got != 32000 {
		t.Fatalf("got %d", got)
	}
	if got := parseMemTotalMiB("nothing here"); got != 0 {
		t.Fatalf("got %d", got)
	}
}
