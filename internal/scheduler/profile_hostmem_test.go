package scheduler

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// p33 flip: with no profile setting ctx_checkpoints or cache_ram_mib, every
// plan, share and launch line is what it was; a profile that sets them to
// the cluster values (32; 2,048 MiB on the scheduler's machine, 0 on a
// node) plans and launches identically, and only says "set on this
// profile". count_host_memory on, so the term reaches the reservation.
func TestProfileHostMemory_FlipChangesNoPlan(t *testing.T) {
	mk := func(own bool) *harness {
		h := withCheckpoints(hostHarness(t))
		h.cfg.Defaults.CountHostMemory = true
		h.llama("s", 4000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
		if own {
			h.cfg.Profiles["s"].CtxCheckpoints, h.cfg.Profiles["s"].CacheRAMMiB = ptrInt(32), ptrInt(2048)
			h.cfg.Profiles["m"].CtxCheckpoints, h.cfg.Profiles["m"].CacheRAMMiB = ptrInt(32), ptrInt(0)
		}
		h.viewChanged()
		return h
	}
	launch := func(h *harness) (string, string) {
		pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.2:50052"}}}
		s, err := instance.BuildArgs(*h.cfg.Profiles["s"], pl, 9000, instance.OptionsFromConfig(h.cfg))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(s, " "), strings.Join(instance.HostLaunchArgs(*h.cfg.Profiles["m"], instance.HostOptionsFromConfig(h.cfg)), " ")
	}
	a, b := mk(false), mk(true)
	if fa, fb := planFigures(a, "m", "s"), planFigures(b, "m", "s"); !reflect.DeepEqual(fa, fb) {
		t.Fatalf("a plan changed:\n absent %+v\n set    %+v", fa, fb)
	}
	for _, n := range []string{"m", "s"} {
		ra, rb := a.c.dryPlan(n), b.c.dryPlan(n)
		ha, hb := *ra.HostMemory, *rb.HostMemory
		if ha.CheckpointsSet || ha.PromptCacheSet || ha.SetNote != "" || !hb.CheckpointsSet || !hb.PromptCacheSet || !strings.HasPrefix(hb.SetNote, "set on this profile: 32 restore points per slot, prompt cache") {
			t.Fatalf("%s: marks %+v / %+v", n, ha, hb)
		}
		hb.CheckpointsSet, hb.PromptCacheSet, hb.SetNote = false, false, ""
		if !reflect.DeepEqual(ha, hb) {
			t.Fatalf("%s: the term differs:\n %+v\n %+v", n, ha, hb)
		}
	}
	sa, ma := launch(a)
	sb, mb := launch(b)
	if sa != sb || ma != mb || !strings.Contains(sa, "--cache-ram 2048 --ctx-checkpoints 32") || !strings.Contains(ma, "--cache-ram 0 --ctx-checkpoints 32") {
		t.Fatalf("launch lines:\n %s\n %s\n %s\n %s", sa, sb, ma, mb)
	}
}

// A profile's own values are the term's and the note says so; on a node
// with count_host_memory on, the reservation takes the profile's figure.
func TestProfileHostMemory_TheTermUsesTheProfileValue(t *testing.T) {
	h := withCheckpoints(hostHarness(t))
	h.cfg.Defaults.CountHostMemory = true
	h.llama("s", 4000, func(p *core.Profile) { p.ForceNodes = []string{"a"}; p.CtxCheckpoints = ptrInt(3) })
	h.cfg.Profiles["m"].CtxCheckpoints, h.cfg.Profiles["m"].CacheRAMMiB = ptrInt(1), ptrInt(256)
	h.viewChanged()

	r := h.c.dryPlan("s")
	hm := r.HostMemory
	if hm.Checkpoints != 3 || hm.PerSlot != 3 || hm.CheckpointsMiB != 151 || hm.PromptCacheMiB != 2048 || !hm.CheckpointsSet || hm.PromptCacheSet {
		t.Fatalf("server-hosted: %+v", hm)
	}
	if !strings.Contains(r.HostMemoryNote, "context checkpoints 151 MiB, prompt cache 2048 MiB") || !strings.HasSuffix(r.HostMemoryNote, "; set on this profile: 3 restore points per slot") {
		t.Fatalf("note %q", r.HostMemoryNote)
	}
	r = h.c.dryPlan("m")
	hm = r.HostMemory
	if hm.Checkpoints != 1 || hm.CheckpointsMiB != 51 || hm.PromptCacheMiB != 256 || hm.TermMiB() != 307 || hm.SetNote != "set on this profile: 1 restore points per slot, prompt cache 256 MiB" {
		t.Fatalf("node-hosted: %+v", hm)
	}
	if got := r.Memory.Nodes[0].HostMemoryMiB; got != 307 {
		t.Fatalf("host node reservation term %d, want 307", got)
	}
}

// Changing either field makes a loaded instance stale (replaced at its next
// load: the launch line changes); the need's shape key does not include
// them (they are the host process's memory, not the devices').
func TestProfileHostMemory_StaleButSameShape(t *testing.T) {
	for _, set := range []func(p *core.Profile){
		func(p *core.Profile) { p.CtxCheckpoints = ptrInt(3) },
		func(p *core.Profile) { p.CacheRAMMiB = ptrInt(0) },
	} {
		h := newHarness(t).nodes(hostNode(16000))
		h.llama("m", 4000)
		h.ready("m")
		in := h.inst("m")
		before := shapeOf(h.cfg.Profiles["m"])
		p := *h.cfg.Profiles["m"]
		set(&p)
		h.cfg.Profiles["m"] = &p
		if !h.c.staleProfile(in) {
			t.Fatal("not stale after a host memory change")
		}
		if shapeOf(&p) != before {
			t.Fatalf("the shape changed: %s / %s", before, shapeOf(&p))
		}
	}
}

// The dropped-flag line names the profile's own value when it has one.
func TestProfileHostMemory_DroppedFlagNamesTheField(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("s", 4000, func(p *core.Profile) { p.ExtraArgs = []string{"--ctx-checkpoints", "9"}; p.CtxCheckpoints = ptrInt(2) })
	h.release(h.granted(func() *waiter { w := h.request("s"); h.healthy("s"); return w }()))
	recs := h.logs.find(msgCkptFlagDropped)
	if len(recs) != 1 || recs[0].Attrs["used"] != "--ctx-checkpoints 2 (set on this profile)" {
		t.Fatalf("%+v", recs)
	}
}
