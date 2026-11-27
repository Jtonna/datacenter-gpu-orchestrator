package scheduler

import (
	"encoding/json"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// build-90: count_host_memory is false by default, and a stored document
// WITHOUT the key behaves as false: opt-in per profile. A plain profile
// counts 0; a profile carrying --ctx-checkpoints counts its own value (and
// the cluster prompt cache), on the scheduler's machine and on a console.
func TestBuild90_CountHostMemoryDefaultFalseAndAbsentKeyIsFalse(t *testing.T) {
	if config.DefaultDefaults().CountHostMemory {
		t.Fatal("count_host_memory defaults to true")
	}
	// a stored document as build-89 writes it: no count_host_memory key
	var doc config.Document
	if err := json.Unmarshal([]byte(`{"settings":{"cache_ram_mib":2048},"profiles":{
		"plain":{"model":"q.gguf","ctx":4096,"needs_mib":100},
		"own":{"model":"q.gguf","ctx":4096,"needs_mib":100,"extra_args":["--ctx-checkpoints","3"]}}}`), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Normalize()
	if _, ok := doc.Settings["count_host_memory"]; ok {
		t.Fatal("the document carries the key")
	}
	built, err := config.Build(&doc, config.Bootstrap{}, config.Options{FileExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if built.Defaults.CountHostMemory {
		t.Fatal("a document without count_host_memory reads as true")
	}

	// scheduler's machine
	h := withCheckpoints(newHarness(t).nodes(ps5("a", 2, 15000)))
	h.cfg.Defaults.CountHostMemory = built.Defaults.CountHostMemory
	h.cfg.Defaults.CacheRAMMiB = built.Defaults.CacheRAMMiB
	h.llama("plain", 4000, func(p *core.Profile) { p.ExtraArgs = built.Profiles["plain"].ExtraArgs })
	h.llama("own", 4000, func(p *core.Profile) { p.ExtraArgs = built.Profiles["own"].ExtraArgs })
	h.viewChanged()
	h.memAvailable(15400)
	if r := h.c.dryPlan("plain"); r.HostMemory == nil || r.HostMemory.Counted || r.HostRoom != nil {
		t.Fatalf("plain: %+v %+v", r.HostMemory, r.HostRoom)
	}
	r := h.c.dryPlan("own") // 3 x 50.25 = 151 MiB, plus cache_ram_mib 2048
	if r.HostMemory == nil || !r.HostMemory.Counted || r.HostMemory.CheckpointsMiB != 151 || r.HostMemory.PromptCacheMiB != 2048 ||
		r.HostRoom == nil || r.HostRoom.ThisMiB != 2199 {
		t.Fatalf("own: %+v %+v", r.HostMemory, r.HostRoom)
	}

	// a console host: the flagged profile counts 151 MiB there, the plain 0
	for name, want := range map[string]int{"plain": 0, "own": 151} {
		hc := withCheckpoints(hostHarness(t))
		hc.cfg.Defaults.CountHostMemory = built.Defaults.CountHostMemory
		withArgs(hc, "m", built.Profiles[name].ExtraArgs...)
		if got := hc.c.dryPlan("m").Memory.Nodes[0].HostMemoryMiB; got != want {
			t.Errorf("console, %s: host_memory_mib %d, want %d", name, got, want)
		}
	}
}
