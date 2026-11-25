package scheduler

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// ckptNeeds adds a checkpoint sizer to the harness's need provider: every
// llama profile is a Qwen 3.5 4B (one cell 52,690,944 bytes = 50.25 MiB).
type ckptNeeds struct {
	NeedProvider
	ck estimate.Ckpt
}

func (n ckptNeeds) CheckpointOf(p *core.Profile) (estimate.Ckpt, error) { return n.ck, nil }

var qwen4BCell = estimate.Ckpt{Family: gguf.FamilyRecurrent, CellBytes: 52690944}

func withCheckpoints(h *harness) *harness {
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs, ck: qwen4BCell}
	return h
}

// hostPlanFigs is every plan figure the term may move: the shares and the plan.
type hostPlanFigs struct {
	shares []int
	plan   PlanReport
}

func planFigures(h *harness, names ...string) map[string]hostPlanFigs {
	out := map[string]hostPlanFigs{}
	for _, n := range names {
		p := h.cfg.Profiles[n]
		pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.2:50052"}, {ID: "b", Addr: "192.0.2.4:50052"}}}
		r := h.c.dryPlan(n)
		r.HostMemory, r.HostMemoryNote = nil, "" // shown only
		out[n] = hostPlanFigs{shares: h.c.expectShares(p, pl), plan: r}
	}
	return out
}

// With count_host_memory off (the default) the term changes no plan: the
// plans and shares of a node-hosted and a server-hosted profile are the
// same as without any checkpoint sizer; the term is only shown.
func TestHostMemory_DarkSettingLeavesEveryPlanUnchanged(t *testing.T) {
	mk := func() *harness {
		h := hostHarness(t)
		h.llama("s", 4000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
		h.viewChanged()
		return h
	}
	before := planFigures(mk(), "m", "s")
	h := withCheckpoints(mk())
	if h.cfg.Defaults.CountHostMemory {
		t.Fatal("count_host_memory is on by default")
	}
	after := planFigures(h, "m", "s")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a plan changed:\n before %+v\n after  %+v", before, after)
	}
	r := h.c.dryPlan("m")
	if r.HostMemory == nil || r.HostMemory.Counted || r.HostMemory.CheckpointsMiB != 1608 || r.HostMemory.Checkpoints != 32 {
		t.Fatalf("node-hosted term shown: %+v", r.HostMemory)
	}
	if r.Memory.Nodes[0].HostMemoryMiB != 0 {
		t.Fatalf("host row counts it: %+v", r.Memory.Nodes[0])
	}
}

// With count_host_memory on, a node-hosted Qwen 3.5 4B profile's host node
// reservation grows by 32 × 50.25 = 1,608 MiB (node prompt cache 0), the
// other node's does not, and the host row names it apart from the host
// overhead; a server-hosted profile's plan does not change and says what
// its host process may take on the scheduler's machine.
func TestHostMemory_CountedJoinsTheHostNodeOnly(t *testing.T) {
	mk := func() *harness {
		h := withCheckpoints(hostHarness(t))
		h.llama("s", 4000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
		h.viewChanged()
		return h
	}
	off := planFigures(mk(), "m", "s")
	h := mk()
	h.cfg.Defaults.CountHostMemory = true
	on := planFigures(h, "m", "s")
	if d := on["m"].shares[0] - off["m"].shares[0]; d != 1608 || on["m"].shares[1] != off["m"].shares[1] {
		t.Fatalf("shares off %v on %v, want +1608 on the host node only", off["m"].shares, on["m"].shares)
	}
	if !reflect.DeepEqual(off["s"], on["s"]) {
		t.Fatalf("a server-hosted plan changed:\n%+v\n%+v", off["s"], on["s"])
	}
	r := h.c.dryPlan("m")
	row := r.Memory.Nodes[0]
	if row.HostMemoryMiB != 1608 || row.HostOverheadMiB != h.cfg.Defaults.HostOverheadMiB || !r.HostMemory.Counted {
		t.Fatalf("host row %+v term %+v", row, r.HostMemory)
	}
	s := h.c.dryPlan("s")
	if s.HostMemory == nil || s.HostMemory.OnNode || !strings.Contains(s.HostMemoryNote, "up to 3656 MiB, not placed") {
		t.Fatalf("server-hosted: %+v %q", s.HostMemory, s.HostMemoryNote)
	}
}

// /api/status sums the term over the instances loaded on the scheduler's
// machine; node-hosted instances are not in it.
func TestHostMemory_StatusSumsTheServerHostedInstances(t *testing.T) {
	h := withCheckpoints(newHarness(t).nodes(ps5("a", 2, 15000)))
	h.llama("s", 4000)
	if got := h.c.statusDoc().ServerHostMemory; got.CheckpointsMiB != 0 || len(got.Instances) != 0 {
		t.Fatalf("nothing loaded: %+v", got)
	}
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	got := h.c.statusDoc().ServerHostMemory
	if got.CheckpointsMiB != 1608 || got.PromptCacheMiB != h.cfg.Defaults.CacheRAMMiB || len(got.Instances) != 1 {
		t.Fatalf("%+v", got)
	}
}

// After a load the server log's figures are compared with the formula: a
// difference of more than 5% is a WARN event, once per source; within 5%
// nothing is said.
func TestHostMemory_LoggedSizeAgainstTheFormula(t *testing.T) {
	h := withCheckpoints(newHarness(t).nodes(ps5("a", 2, 15000)))
	h.llama("s", 4000)
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	ch := h.child("s")
	ch.mu.Lock()
	ch.load = instance.LoadStats{HostMem: instance.HostMem{RecurrentMiB: 50.25}}
	ch.mu.Unlock()
	h.step(evTick{})
	if ev := h.histTypes(history.TLearnedNeed); len(ev) != 0 {
		t.Fatalf("within 5%%: %+v", ev)
	}
	ch.mu.Lock()
	ch.load.HostMem.CheckpointMiB, ch.load.HostMem.CheckpointsCreated = 60.0, 1
	ch.mu.Unlock()
	h.step(evTick{})
	h.step(evTick{})
	ev := h.histTypes(history.TLearnedNeed)
	if len(ev) != 1 || ev[0].Severity != history.Warn || !strings.Contains(ev[0].Summary, "60.00 MiB") || !strings.Contains(ev[0].Summary, "50.25 MiB") {
		t.Fatalf("%+v", ev)
	}
}
