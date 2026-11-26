package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// gemma4Cell is Gemma 4 12B's proven bound at f16: 320.00 MiB.
var gemma4Cell = estimate.Ckpt{Family: gguf.FamilyWindowed, CellBytes: 335544320, BoundBytes: 335544320, Estimate: true}

// A proven window model is counted as "up to": the plan's note on the
// scheduler's machine names the checkpoints' figure (32 x 320 MiB).
func TestHostMemory_ProvenWindowIsUpTo(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Defaults.CacheRAMMiB, h.cfg.Defaults.CtxCheckpoints = 2048, 32
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs, ck: gemma4Cell}
	h.llama("g", 4000)
	r := h.c.dryPlan("g")
	if r.HostMemory == nil || r.HostMemory.SizeUnknown || !r.HostMemory.Estimate || r.HostMemory.CellMiB != 320 || r.HostMemory.CheckpointsMiB != 10240 {
		t.Fatalf("%+v", r.HostMemory)
	}
	want := "on " + r.HostMemory.Machine + ": up to 12288 MiB, not placed (context checkpoints 10240 MiB, prompt cache 2048 MiB)"
	if r.HostMemoryNote != want {
		t.Fatalf("got %q\nwant %q", r.HostMemoryNote, want)
	}
}

// The log check for a window model compares the LARGEST checkpoint created
// with the bound: smaller ones are normal while a conversation is shorter
// than the window (no warning); within 1% above (320.013 against 320.00) is
// the server's own overhead (no warning); more than 1% above is one WARN.
func TestHostMemory_WindowLogCheckUsesTheLargest(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.c.needs = ckptNeeds{NeedProvider: h.c.needs, ck: gemma4Cell}
	h.llama("g", 4000)
	h.release(h.granted(func() *waiter { w := h.request("g"); h.healthy("g"); return w }()))
	ch := h.child("g")
	set := func(last, largest float64) {
		ch.mu.Lock()
		ch.load = instance.LoadStats{HostMem: instance.HostMem{CheckpointMiB: last, LargestCheckpointMiB: largest, CheckpointsCreated: 3, CheckpointsHeld: 3}}
		ch.mu.Unlock()
		h.step(evTick{})
	}
	set(12.5, 12.5)
	set(94, 320.013)
	if ev := h.histTypes(history.TLearnedNeed); len(ev) != 0 {
		t.Fatalf("smaller and within 1%%: %+v", ev)
	}
	set(94, 330)
	set(94, 340)
	ev := h.histTypes(history.TLearnedNeed)
	if len(ev) != 1 || ev[0].Severity != history.Warn || !strings.Contains(ev[0].Summary, "330.00 MiB") || !strings.Contains(ev[0].Summary, "320.00 MiB") {
		t.Fatalf("%+v", ev)
	}
}
