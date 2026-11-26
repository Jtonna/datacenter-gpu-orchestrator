package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
)

// When the checkpoint size is unknown the figure is a floor, never a
// ceiling: the plan's note on the scheduler's machine and on a host node,
// and the status sum, say "at least" and that the checkpoints are NOT
// included.
func TestHostMemory_UnknownSizeReadsAsAFloor(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Defaults.CacheRAMMiB, h.cfg.Defaults.NodeHostCacheRAMMiB = 2048, 512
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs, ck: estimate.Ckpt{Family: gguf.FamilyWindowed, Unknown: true}}
	h.llama("s", 4000)
	h.llama("n", 4000, func(p *core.Profile) { p.Host = core.HostNode })
	r := h.c.dryPlan("s")
	want := "on " + r.HostMemory.Machine + ": at least 2048 MiB, not placed (prompt cache 2048 MiB; context checkpoints NOT included: size not yet known, read at the first load)"
	if r.HostMemoryNote != want {
		t.Fatalf("server note\n got %q\nwant %q", r.HostMemoryNote, want)
	}
	rn := h.c.dryPlan("n")
	if rn.HostMemory == nil || !rn.HostMemory.OnNode {
		t.Fatalf("node-hosted: %+v", rn.HostMemory)
	}
	want = "on the host node: at least 512 MiB, not yet counted (prompt cache 512 MiB; context checkpoints NOT included: size not yet known, read at the first load)"
	if rn.HostMemoryNote != want {
		t.Fatalf("node note\n got %q\nwant %q", rn.HostMemoryNote, want)
	}
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	sum := h.c.statusDoc().ServerHostMemory
	want = "at least 2048 MiB; 1 loaded model with checkpoints of unknown size not included"
	if sum.Note != want {
		t.Fatalf("sum note\n got %q\nwant %q", sum.Note, want)
	}
}

// A known size keeps "up to" (a ceiling: every checkpoint full).
func TestHostMemory_KnownSizeKeepsUpTo(t *testing.T) {
	h := withCheckpoints(newHarness(t).nodes(ps5("a", 2, 15000)))
	h.cfg.Defaults.CacheRAMMiB = 2048
	h.llama("s", 4000)
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	if got := h.c.statusDoc().ServerHostMemory.Note; got != "up to 3656 MiB" {
		t.Fatalf("%q", got)
	}
}
