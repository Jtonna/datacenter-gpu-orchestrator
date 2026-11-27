package scheduler

import (
	"reflect"
	"strings"
	"testing"
)

// count_host_memory off (the default) is opt-in per profile: only a profile
// whose extra_args carry --ctx-checkpoints or --cache-ram in an accepted
// form is counted against the machine that hosts it, with its own value and
// the cluster value for the flag it does not set (countsHost, hostmem.go).
// On counts every profile (hostbudget_test.go's flip tests).

// withArgs sets p's extra_args.
func withArgs(h *harness, p string, args ...string) {
	h.t.Helper()
	h.cfg.Profiles[p].ExtraArgs = args
	h.viewChanged()
}

// Off, a console-hosted Qwen 3.5 4B with --ctx-checkpoints 3 (one slot) is
// counted on its host console: 3 x 50.25 = 150.75, shown 151 MiB, plus the
// console's prompt cache (node_host_cache_ram_mib 0). The other node's share
// does not move.
func TestOptIn_ConsoleProfileWithFlagCountsOnItsConsole(t *testing.T) {
	mk := func() *harness {
		h := withCheckpoints(hostHarness(t))
		h.cfg.Defaults.CountHostMemory = false
		h.viewChanged()
		return h
	}
	plainH := mk()
	before := planFigures(plainH, "m")
	if r := plainH.c.dryPlan("m"); r.HostMemory == nil || r.HostMemory.Counted || r.Memory.Nodes[0].HostMemoryMiB != 0 {
		t.Fatalf("no flag, off: counted %+v row %+v", r.HostMemory, r.Memory.Nodes[0])
	}
	h := mk()
	withArgs(h, "m", "--ctx-checkpoints", "3")
	after := planFigures(h, "m")
	if d := after["m"].shares[0] - before["m"].shares[0]; d != 151 || after["m"].shares[1] != before["m"].shares[1] {
		t.Fatalf("shares %v -> %v, want +151 on the host node only", before["m"].shares, after["m"].shares)
	}
	r := h.c.dryPlan("m")
	hm := r.HostMemory
	if hm == nil || !hm.Counted || hm.Checkpoints != 3 || hm.PerSlot != 3 || hm.CheckpointsMiB != 151 || hm.PromptCacheMiB != 0 ||
		hm.CheckpointsFrom != "from this profile's extra arguments" || hm.PromptCacheFrom != "the cluster default" {
		t.Fatalf("term %+v", hm)
	}
	if row := r.Memory.Nodes[0]; row.HostMemoryMiB != 151 {
		t.Fatalf("host row %+v", row)
	}
}

// blankShown drops what a plan only shows of the host term (host_memory and
// its note): everything else is what places and decides.
func blankShown(r PlanReport) PlanReport {
	r.HostMemory, r.HostMemoryNote = nil, ""
	return r
}

// The gate's assertion: with NO profile carrying a flag and the setting
// false (or absent: the default), every plan, action, victims, reason,
// memory rows, host room, is the same as with no host term at all (no
// checkpoint sizer: the image before p33 with counting off), idle and with
// a model loaded on the scheduler's machine, with or without a reading.
// Only host_memory and host_memory_note (shown, never counted) differ.
func TestOptIn_NoFlagsOffEveryPlanAsBefore(t *testing.T) {
	for _, mem := range []int{0, 15400, 9000} {
		with := budgetFleet(t, mem)
		with.cfg.Defaults.CountHostMemory = false
		without := budgetFleet(t, mem)
		without.cfg.Defaults.CountHostMemory = false
		without.c.needs = without.c.needs.(modelCkpts).NeedProvider
		for _, loaded := range []bool{false, true} {
			if loaded {
				loadIdle(with, gemmaA, mem)
				loadIdle(without, gemmaA, mem)
			}
			for _, p := range budgetProfiles {
				a, b := with.c.dryPlan(p), without.c.dryPlan(p)
				if a.HostRoom != nil {
					t.Errorf("mem %d loaded %v %s: a host room off without flags: %+v", mem, loaded, p, a.HostRoom)
				}
				if a.HostMemory != nil && a.HostMemory.Counted {
					t.Errorf("mem %d loaded %v %s: counted", mem, loaded, p)
				}
				if !reflect.DeepEqual(blankShown(a), blankShown(b)) {
					t.Errorf("mem %d loaded %v %s: plan differs:\n with    %+v\n without %+v", mem, loaded, p, blankShown(a), blankShown(b))
				}
			}
		}
	}
	// a console-hosted profile without flags, off: rows identical too
	mk := func(sizer bool) *harness {
		h := hostHarness(t)
		if sizer {
			h = withCheckpoints(h)
		}
		h.viewChanged()
		return h
	}
	a, b := planFigures(mk(true), "m"), planFigures(mk(false), "m")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("console plan differs:\n%+v\n%+v", a, b)
	}
}

// Off, a scheduler-machine profile with -cram 0 is checked against the
// budget (its own cache 0, the cluster's 32 restore points); a profile
// without flags beside it is not. The plan's counted field and the note's
// word follow the profile.
func TestOptIn_ServerProfileWithCramIsChecked(t *testing.T) {
	h := budgetFleet(t, 15400)
	h.cfg.Defaults.CountHostMemory = false
	withArgs(h, gemmaP2, "-cram", "0")
	r := h.c.dryPlan(gemmaP2) // 2 slots x 32 x 320 = 20,480 > 14,200
	if r.Action != PlanQueue || r.HostRoom == nil || r.HostRoom.ThisMiB != 20480 || r.HostRoom.BudgetMiB != 14200 || r.HostRoom.Fits {
		t.Fatalf("p2: %s %+v", r.Action, r.HostRoom)
	}
	if r.HostMemory == nil || !r.HostMemory.Counted || !strings.Contains(r.HostMemoryNote, "up to 20480 MiB, counted") ||
		!strings.Contains(r.HostMemoryNote, "32 restore points per slot the cluster default, prompt cache from this profile's extra arguments") {
		t.Fatalf("p2 term %+v %q", r.HostMemory, r.HostMemoryNote)
	}
	g := h.c.dryPlan(gemmaA)
	if g.Action != PlanStart || g.HostRoom != nil || g.HostMemory == nil || g.HostMemory.Counted || !strings.Contains(g.HostMemoryNote, "up to 12288 MiB, not placed") {
		t.Fatalf("gemma-4-12b without flags: %s %+v %+v %q", g.Action, g.HostRoom, g.HostMemory, g.HostMemoryNote)
	}
}

// Off, a loaded instance without flags holds nothing in the budget: a
// second Gemma with --cache-ram 0 (32 x 320 = 10,240 MiB) starts beside an
// idle Gemma at the defaults. On, the loaded one holds its 12,288 MiB and
// is unloaded first.
func TestOptIn_LoadedWithoutFlagsHoldsNothing(t *testing.T) {
	h := budgetFleet(t, 15400)
	h.cfg.Defaults.CountHostMemory = false
	withArgs(h, gemmaB, "--cache-ram", "0")
	loadIdle(h, gemmaA, 15400)
	r := h.c.dryPlan(gemmaB)
	if r.Action != PlanStart || r.HostRoom == nil || len(r.HostRoom.Held) != 0 || r.HostRoom.HeldMiB != 0 || r.HostRoom.ThisMiB != 10240 || !r.HostRoom.Fits {
		t.Fatalf("off: %s %+v", r.Action, r.HostRoom)
	}

	on := budgetFleet(t, 15400)
	withArgs(on, gemmaB, "--cache-ram", "0")
	loadIdle(on, gemmaA, 15400)
	r = on.c.dryPlan(gemmaB)
	if r.Action != PlanEvict || r.HostRoom == nil || len(r.HostRoom.Held) != 1 || r.HostRoom.Held[0].Profile != gemmaA {
		t.Fatalf("on: %s %+v", r.Action, r.HostRoom)
	}
}

// Off, a loaded instance WITH flags holds its own maximum: a plain profile
// opted in beside it sees it held.
func TestOptIn_LoadedWithFlagsIsHeld(t *testing.T) {
	h := budgetFleet(t, 15400)
	h.cfg.Defaults.CountHostMemory = false
	withArgs(h, gemmaA, "--ctx-checkpoints", "30")
	withArgs(h, qwen, "-cram", "0")
	loadIdle(h, gemmaA, 15400)
	r := h.c.dryPlan(qwen)
	if r.HostRoom == nil || len(r.HostRoom.Held) != 1 || r.HostRoom.Held[0].Profile != gemmaA || r.HostRoom.Held[0].MaxMiB != 30*320+2048 {
		t.Fatalf("%s %+v", r.Action, r.HostRoom)
	}
}

// On counts every profile, flags or not: the console profile without flags
// is counted at the cluster default (1,608 MiB).
func TestOptIn_OnCountsEveryProfile(t *testing.T) {
	h := withCheckpoints(hostHarness(t))
	h.cfg.Defaults.CountHostMemory = true
	h.viewChanged()
	r := h.c.dryPlan("m")
	if r.HostMemory == nil || !r.HostMemory.Counted || r.Memory.Nodes[0].HostMemoryMiB != 1608 {
		t.Fatalf("%+v %+v", r.HostMemory, r.Memory.Nodes[0])
	}
}
