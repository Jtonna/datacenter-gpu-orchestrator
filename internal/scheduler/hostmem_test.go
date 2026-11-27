package scheduler

import (
	"reflect"
	"strconv"
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

// With count_host_memory off (the default) and no profile carrying its own
// flags, the term changes no plan: the
// plans and shares of a node-hosted and a server-hosted profile are the
// same as without any checkpoint sizer; the term is only shown.
func TestHostMemory_DarkSettingLeavesEveryPlanUnchanged(t *testing.T) {
	mk := func() *harness {
		h := hostHarness(t)
		h.cfg.Defaults.CountHostMemory = false
		h.llama("s", 4000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
		h.viewChanged()
		return h
	}
	before := planFigures(mk(), "m", "s")
	h := withCheckpoints(mk())
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
		h.cfg.Defaults.CountHostMemory = false
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
	// the server-hosted plan shows the machine's budget now (hostbudget.go);
	// without a reading nothing else of it changes
	so, sf := on["s"], off["s"]
	so.plan.HostRoom, so.plan.HostMemoryNote, sf.plan.HostMemoryNote = nil, "", ""
	if !reflect.DeepEqual(sf, so) {
		t.Fatalf("a server-hosted plan changed:\n%+v\n%+v", off["s"], on["s"])
	}
	r := h.c.dryPlan("m")
	row := r.Memory.Nodes[0]
	if row.HostMemoryMiB != 1608 || row.HostOverheadMiB != h.cfg.Defaults.HostOverheadMiB || !r.HostMemory.Counted {
		t.Fatalf("host row %+v term %+v", row, r.HostMemory)
	}
	s := h.c.dryPlan("s")
	if s.HostMemory == nil || s.HostMemory.OnNode || !strings.Contains(s.HostMemoryNote, "up to 3656 MiB, host memory not checked: available memory of the scheduler's machine unknown (no reading yet)") {
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

// 1% is the threshold: the recurrent formula is exact, so a figure 1.5%
// off (which the 5% of before let through) is a WARN.
func TestHostMemory_OnePercentIsEnough(t *testing.T) {
	h := withCheckpoints(newHarness(t).nodes(ps5("a", 2, 15000)))
	h.llama("s", 4000)
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	ch := h.child("s")
	ch.mu.Lock()
	ch.load = instance.LoadStats{HostMem: instance.HostMem{RecurrentMiB: 51.0}} // 1.5% above 50.25
	ch.mu.Unlock()
	h.step(evTick{})
	if ev := h.histTypes(history.TLearnedNeed); len(ev) != 1 || ev[0].Severity != history.Warn {
		t.Fatalf("%+v", ev)
	}
}

// A window model whose window layers the header does not tell: no figure
// (SizeUnknown, 0 MiB of checkpoints, the plan's note says so); once a
// load's log shows a checkpoint, the largest logged is the size for that
// model file for the run.
func TestHostMemory_WindowSizeUnknownUntilALoadLogsIt(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs, ck: estimate.Ckpt{Family: gguf.FamilyWindowed, Unknown: true, FileBytes: 7000000000}}
	h.llama("s", 4000)
	r := h.c.dryPlan("s")
	if r.HostMemory == nil || !r.HostMemory.SizeUnknown || r.HostMemory.CheckpointsMiB != 0 || r.HostMemory.CellMiB != 0 ||
		!strings.Contains(r.HostMemoryNote, "size not yet known, read at the first load") {
		t.Fatalf("%+v %q", r.HostMemory, r.HostMemoryNote)
	}
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	ch := h.child("s")
	ch.mu.Lock()
	ch.load = instance.LoadStats{HostMem: instance.HostMem{CheckpointMiB: 40.5, CheckpointsCreated: 1, CheckpointsHeld: 1}}
	ch.mu.Unlock()
	h.step(evTick{})
	r = h.c.dryPlan("s")
	hm := r.HostMemory
	if hm.SizeUnknown || !hm.FromLog || hm.CellMiB != 40.5 || hm.CheckpointsMiB != 1296 {
		t.Fatalf("after the log: %+v", hm)
	}
	// the figure says which file (version) and which context it was read at
	if hm.LoggedFile != h.cfg.Profiles["s"].Model || hm.LoggedFileBytes != 7000000000 || hm.LoggedCtx != h.cfg.Profiles["s"].Ctx || hm.LoggedAt.IsZero() {
		t.Fatalf("read at: %+v", hm)
	}
	if !strings.Contains(r.HostMemoryNote, "read at a load of") || !strings.Contains(r.HostMemoryNote, "ctx "+itoa(h.cfg.Profiles["s"].Ctx)) {
		t.Fatalf("note %q", r.HostMemoryNote)
	}
	if ev := h.histTypes(history.TLearnedNeed); len(ev) != 0 {
		t.Fatalf("no comparison without a formula: %+v", ev)
	}
	// another context, or another version of the file: not known again
	h.llama("s2", 4000, func(p *core.Profile) { p.Model, p.Ctx = h.cfg.Profiles["s"].Model, h.cfg.Profiles["s"].Ctx*2 })
	if r2 := h.c.dryPlan("s2"); r2.HostMemory == nil || !r2.HostMemory.SizeUnknown || r2.HostMemory.CheckpointsMiB != 0 {
		t.Fatalf("other ctx: %+v", r2.HostMemory)
	}
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs.(ckptNeeds).NeedProvider, ck: estimate.Ckpt{Family: gguf.FamilyWindowed, Unknown: true, FileBytes: 7100000000}}
	if r3 := h.c.dryPlan("s"); !r3.HostMemory.SizeUnknown || r3.HostMemory.CheckpointsMiB != 0 {
		t.Fatalf("new file version: %+v", r3.HostMemory)
	}
}

// A model of an architecture the code has no exact rule for (recurrent
// but unknown: estimate.Checkpoint gives Unknown) shows no number and adds
// nothing, like a window model.
func TestHostMemory_MadeUpArchitectureIsUnknown(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs, ck: estimate.Ckpt{Family: gguf.FamilyRecurrent, Unknown: true}}
	h.llama("s", 4000)
	r := h.c.dryPlan("s")
	if r.HostMemory == nil || !r.HostMemory.SizeUnknown || r.HostMemory.CheckpointsMiB != 0 || r.HostMemory.CellMiB != 0 {
		t.Fatalf("%+v", r.HostMemory)
	}
}

// The scheduler's machine is named the same on every read when several
// nodes are local (the lowest id), so plans and the status do not change
// between two reads of the same state.
func TestHostMemory_ServerNameIsStable(t *testing.T) {
	h := withCheckpoints(newHarness(t).nodes(hostNode(7000), func() core.Node { n := hostNode(7000); n.ID, n.Hostname = "host-0", "host-0"; return n }()))
	h.viewChanged()
	for i := 0; i < 50; i++ {
		if got := h.c.serverName(); got != "host-0" {
			t.Fatalf("read %d: %q", i, got)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
