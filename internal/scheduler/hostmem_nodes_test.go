package scheduler

import (
	"reflect"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
)

// The status's host-memory sum is the scheduler's machine only, and says
// so; beside it, one row per console that hosts a loaded model, with that
// console's term (checkpoints, prompt cache, the instances, "at least" when
// a size is unknown), so a load hosted on a console never reads as nothing.
func TestHostMemory_StatusHasEveryMachineThatHoldsIt(t *testing.T) {
	h := hostHarness(t)
	h.nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 6, 15000))
	h.viewChanged()
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs, ck: qwen4BCell}
	h.cfg.Defaults.CacheRAMMiB, h.cfg.Defaults.NodeHostCacheRAMMiB, h.cfg.Defaults.CtxCheckpoints = 2048, 256, 32
	h.cfg.Defaults.CountHostMemory = true // counting every profile (off counts only profiles with their own flags)
	h.llama("s", 4000, func(p *core.Profile) { p.ForceNodes, p.TTL = []string{"c"}, ptrDur(0) })

	st := h.c.statusDoc()
	if st.ServerHostMemory.Scope != "on the scheduler's machine only" || len(st.NodeHostMemory) != 0 {
		t.Fatalf("nothing loaded: %+v %+v", st.ServerHostMemory, st.NodeHostMemory)
	}

	h.hostReady() // m, hosted on console a
	h.release(h.granted(func() *waiter { w := h.request("s"); h.workersUpAll(); h.healthy("s"); return w }()))
	st = h.c.statusDoc()
	sv := st.ServerHostMemory
	if sv.Scope != "on the scheduler's machine only" || !reflect.DeepEqual(sv.Instances, []string{"s"}) || sv.CheckpointsMiB != 1608 || sv.PromptCacheMiB != 2048 || sv.Note != "up to 3656 MiB" {
		t.Fatalf("server: %+v", sv)
	}
	want := []NodeHostMemory{{Node: "a", Name: "a", CheckpointsMiB: 1608, PromptCacheMiB: 256, Instances: []string{"m"}, Counted: true, Note: "up to 1864 MiB"}}
	if !reflect.DeepEqual(st.NodeHostMemory, want) {
		t.Fatalf("nodes:\n got %+v\nwant %+v", st.NodeHostMemory, want)
	}

	// a size not known yet: "at least", the instance named
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs.(ckptNeeds).NeedProvider, ck: estimate.Ckpt{Family: gguf.FamilyWindowed, Unknown: true}}
	st = h.c.statusDoc()
	n := st.NodeHostMemory[0]
	if n.CheckpointsMiB != 0 || !reflect.DeepEqual(n.SizeUnknown, []string{"m"}) || n.Note != "at least 256 MiB; 1 loaded model with checkpoints of unknown size not included" {
		t.Fatalf("unknown: %+v", n)
	}
}
