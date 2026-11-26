package scheduler

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
)

// modelCkpts sizes each profile's checkpoint by its name.
type modelCkpts struct {
	NeedProvider
	by map[string]estimate.Ckpt
}

func (n modelCkpts) CheckpointOf(p *core.Profile) (estimate.Ckpt, error) { return n.by[p.Name], nil }

// A Gemma-like window model: a checkpoint is at most 320 MiB (an estimate).
var gemmaCell = estimate.Ckpt{Family: gguf.FamilyWindowed, CellBytes: 320 << 20, Estimate: true}

const (
	gemmaA  = "gemma-4-12b"
	gemmaB  = "gemma-4-12b-b"
	gemmaP2 = "gemma-4-12b-p2"
	qwen    = "qwen3.5-4b"
	plain   = "plain-8b"
	console = "qwen3.5-4b-console"
)

// budgetFleet is a fleet shaped like live: four consoles, the scheduler's
// own machine (host-1, no room for a share: it only hosts), the cluster
// defaults (ctx_checkpoints 32, cache_ram_mib 2048, watchdog_floor_mib
// 1000), and these profiles, each held to its own console so the device
// fit never decides:
//
//	gemma-4-12b, gemma-4-12b-b   window, 1 slot: 32 x 320 + 2048 = 12,288 MiB
//	gemma-4-12b-p2               window, 2 slots: 64 x 320 + 2048 = 22,528 MiB
//	qwen3.5-4b                   recurrent: 32 x 50.25 + 2048 = 3,656 MiB
//	plain-8b                     no checkpoints: the cache, 2,048 MiB
//	qwen3.5-4b-console           host: node (its console counts it)
//
// mem is the scheduler machine's MemAvailable; 0 leaves it unread. host-1's
// tier keeps headroom_mib 200, so 15,400 MiB available is a budget of
// 15,400 - 1,000 - 200 = 14,200 MiB for conversation memory.
func budgetFleet(t *testing.T, mem int, extra ...string) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("ps5-1", 2, 15000), ps5("ps5-2", 4, 15000), ps5("ps5-3", 5, 15000), ps5("ps5-4", 6, 15000), hostNode(0))
	h.c.needs = modelCkpts{NeedProvider: h.c.needs, by: map[string]estimate.Ckpt{
		gemmaA: gemmaCell, gemmaB: gemmaCell, gemmaP2: gemmaCell, qwen: qwen4BCell, console: qwen4BCell,
		plain: {Family: gguf.FamilyPlain},
	}}
	on := func(node string, mod ...func(*core.Profile)) func(*core.Profile) {
		return func(p *core.Profile) {
			p.ForceNodes, p.ExtraArgs = []string{node}, extra
			for _, m := range mod {
				m(p)
			}
		}
	}
	h.llama(gemmaA, 6000, on("ps5-1"))
	h.llama(gemmaB, 6000, on("ps5-2"))
	h.llama(gemmaP2, 6000, on("ps5-3", func(p *core.Profile) { p.Parallel = 2 }))
	h.llama(qwen, 3000, on("ps5-4"))
	h.llama(plain, 3000, on("ps5-3"))
	h.viewChanged()
	if mem > 0 {
		h.memAvailable(mem)
	}
	return h
}

func (h *harness) memAvailable(mib int) {
	h.t.Helper()
	h.step(evLocalMem{availMiB: mib, at: h.clk.Now()})
}

var budgetProfiles = []string{gemmaA, gemmaB, gemmaP2, qwen, plain}

func actions(h *harness) map[string]PlanAction {
	out := map[string]PlanAction{}
	for _, p := range budgetProfiles {
		out[p] = h.c.dryPlan(p).Action
	}
	return out
}

// (a) Idle fleet: the only profile whose action changes is the one whose
// own maximum is over the budget: gemma-4-12b-p2 (22,528 MiB > 14,200)
// waits instead of starting; it says what would make it fit.
func TestHostBudget_FlipA_Idle(t *testing.T) {
	off := budgetFleet(t, 15400)
	off.cfg.Defaults.CountHostMemory = false
	before := actions(off)
	h := budgetFleet(t, 15400)
	after := actions(h)
	var changed []string
	for _, p := range budgetProfiles {
		if before[p] != after[p] {
			changed = append(changed, p+": "+string(before[p])+" -> "+string(after[p]))
		}
	}
	if want := []string{gemmaP2 + ": start -> queue"}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed %q, want %q", changed, want)
	}
	r := h.c.dryPlan(gemmaP2)
	for _, want := range []string{
		"has 14,200 MiB available for conversation memory (MemAvailable 15,400 MiB minus watchdog_floor_mib 1,000 MiB and the tier's headroom_mib 200 MiB)",
		"this model may hold up to 22,528 MiB of conversation memory there",
		"nothing hosted there is idle to unload, so it waits",
		"with --ctx-checkpoints 3 --cache-ram 0 on this profile it would hold up to 1,920 MiB",
	} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("reason %q\nwant %q", r.Reason, want)
		}
	}
	if g := h.c.dryPlan(gemmaA); g.HostRoom == nil || !g.HostRoom.Fits || g.HostRoom.BudgetMiB != 14200 || g.HostRoom.ThisMiB != 12288 ||
		!strings.Contains(g.HostMemoryNote, "up to 12288 MiB, counted") || !strings.Contains(g.HostMemoryNote, "restore points and prompt cache the cluster default") {
		t.Fatalf("gemma-4-12b: %+v %q", g.HostRoom, g.HostMemoryNote)
	}
}

// loadIdle loads p, releases it and waits past its hold: idle, evictable;
// then a fresh reading of mem MiB (the sampler posts at least every 30 s;
// 0 posts none, and the last reading is two minutes old).
func loadIdle(h *harness, p string, mem int) {
	h.t.Helper()
	h.release(h.ready(p))
	h.advance(2 * time.Minute)
	if mem > 0 {
		h.memAvailable(mem)
	}
}

// (b) One window model loaded at the cluster default, idle: a second
// Gemma, Qwen and the plain model each unload it first (each with its
// reason); busy, they wait.
func TestHostBudget_FlipB_OneLoaded(t *testing.T) {
	h := budgetFleet(t, 15400)
	loadIdle(h, gemmaA, 15400)
	want := map[string]string{
		gemmaB: "this model may hold up to 12,288 MiB of conversation memory there; gemma-4-12b, idle, would be unloaded first; with --ctx-checkpoints 3 --cache-ram 0 on this profile it would hold up to 960 MiB",
		qwen:   "this model may hold up to 3,656 MiB of conversation memory there; gemma-4-12b, idle, would be unloaded first; with --ctx-checkpoints 3 --cache-ram 0 on this profile it would hold up to 151 MiB",
		plain:  "this model may hold up to 2,048 MiB of conversation memory there; gemma-4-12b, idle, would be unloaded first; with --cache-ram 0 on this profile it would hold none",
	}
	for p, w := range want {
		r := h.c.dryPlan(p)
		if r.Action != PlanEvict || len(r.Victims) != 1 || r.Victims[0].Profile != gemmaA || !strings.Contains(r.Reason, w) ||
			!strings.Contains(r.Reason, "the models hosted there (gemma-4-12b) may hold up to 12,288 MiB") {
			t.Errorf("%s: %s %+v\n%q\nwant %q", p, r.Action, r.Victims, r.Reason, w)
		}
	}
	if r := h.c.dryPlan(gemmaP2); r.Action != PlanQueue || !strings.Contains(r.Reason, "nothing hosted there is idle to unload") {
		t.Errorf("p2: %s %q", r.Action, r.Reason)
	}

	// busy: nothing to unload, every one of them waits
	h2 := budgetFleet(t, 15400)
	g := h2.ready(gemmaA)
	h2.advance(2 * time.Minute)
	h2.memAvailable(15400)
	for p := range want {
		if r := h2.c.dryPlan(p); r.Action != PlanQueue || !strings.Contains(r.Reason, "nothing hosted there is idle to unload, so it waits") {
			t.Errorf("busy %s: %s %q", p, r.Action, r.Reason)
		}
	}
	_ = g
}

// The loaded case for real: Gemma loaded and idle, a second Gemma is
// asked for: the first is unloaded (an eviction that names the
// conversation memory) and the second starts; while the first is busy the
// second waits, and its queue reason says why.
func TestHostBudget_LoadedGemmaSecondModel(t *testing.T) {
	h := budgetFleet(t, 15400)
	loadIdle(h, gemmaA, 15400)
	w := h.request(gemmaB)
	h.wantState(gemmaA, "absent")
	h.healthy(gemmaB)
	h.release(h.granted(w))
	if len(h.logs.find("idle models unloaded first for conversation memory")) == 0 {
		t.Fatal("no log of the host unload")
	}

	h2 := budgetFleet(t, 15400)
	g := h2.ready(gemmaA)
	h2.advance(2 * time.Minute)
	h2.memAvailable(15400)
	h2.request(gemmaB)
	h2.wantState(gemmaA, "ready")
	if h2.c.insts[gemmaB] != nil {
		t.Fatal("the second Gemma started beside a busy first one")
	}
	if _, why := h2.c.waitWhy(h2.c.profile(gemmaB)); !strings.Contains(why, "this model may hold up to 12,288 MiB of conversation memory there") {
		t.Fatalf("why %q", why)
	}
	h2.release(g) // idle now, past its hold: unloaded first
	h2.wantState(gemmaA, "absent")
	h2.wantState(gemmaB, "starting")
}

// (c) The same with --ctx-checkpoints 3 --cache-ram 0 on the profiles:
// 960 MiB each, back to side by side.
func TestHostBudget_FlipC_SmallerLimitsSideBySide(t *testing.T) {
	h := budgetFleet(t, 15400, "--ctx-checkpoints", "3", "--cache-ram", "0")
	loadIdle(h, gemmaA, 15400)
	for _, p := range []string{gemmaB, qwen, plain, gemmaP2} {
		if r := h.c.dryPlan(p); r.Action != PlanStart || len(r.Victims) != 0 {
			t.Errorf("%s: %s %q", p, r.Action, r.Reason)
		}
	}
	r := h.c.dryPlan(gemmaB)
	if r.HostRoom.ThisMiB != 960 || r.HostRoom.HeldMiB != 960 || !strings.Contains(r.HostMemoryNote, "restore points and prompt cache from this profile's extra arguments") {
		t.Fatalf("%+v %q", r.HostRoom, r.HostMemoryNote)
	}
	w := h.request(gemmaB)
	h.healthy(gemmaB)
	h.granted(w)
	h.wantState(gemmaA, "ready")
}

// (d) The reservation is released at unload.
func TestHostBudget_FlipD_ReleasedAtUnload(t *testing.T) {
	h := budgetFleet(t, 15400)
	loadIdle(h, gemmaA, 15400)
	if r := h.c.dryPlan(gemmaB); r.HostRoom.HeldMiB != 12288 {
		t.Fatalf("held %+v", r.HostRoom)
	}
	h.step(evUnload{profile: gemmaA})
	h.wantState(gemmaA, "absent")
	r := h.c.dryPlan(gemmaB)
	if r.Action != PlanStart || r.HostRoom.HeldMiB != 0 || len(r.HostRoom.Held) != 0 {
		t.Fatalf("after unload: %s %+v", r.Action, r.HostRoom)
	}
}

// (e) A console-hosted profile is unchanged from today's counting: the
// scheduler machine's reading plays no part.
func TestHostBudget_FlipE_ConsoleHostedUnchanged(t *testing.T) {
	plan := func(mem int) PlanReport {
		h := withCheckpoints(hostHarness(t))
		h.viewChanged()
		if mem > 0 {
			h.memAvailable(mem)
		}
		return h.c.dryPlan("m")
	}
	none, tiny := plan(0), plan(1)
	if none.HostRoom != nil || tiny.HostRoom != nil || none.Action != tiny.Action || !reflect.DeepEqual(none.Memory, tiny.Memory) {
		t.Fatalf("console-hosted changed:\n%+v\n%+v", none, tiny)
	}
	if tiny.Memory.Nodes[0].HostMemoryMiB != 1608 {
		t.Fatalf("console term %+v", tiny.Memory.Nodes[0])
	}
}

// Unknown: no reading (or a stale one) checks nothing, never refuses,
// says so in the plan and WARNs once.
func TestHostBudget_UnknownIsNotChecked(t *testing.T) {
	h := budgetFleet(t, 0)
	loadIdle(h, gemmaA, 0)
	r := h.c.dryPlan(gemmaB)
	if r.Action != PlanStart || !strings.Contains(r.HostMemoryNote, "host memory not checked: available memory of host-1 unknown (no reading yet)") {
		t.Fatalf("%s %q", r.Action, r.HostMemoryNote)
	}
	w := h.request(gemmaB)
	h.healthy(gemmaB)
	h.granted(w)
	h.wantState(gemmaA, "ready")
	if n := len(h.logs.find("host memory not checked: the scheduler's machine's available memory is unknown; loads hosted there are not held to it")); n != 1 {
		t.Fatalf("%d warnings, want 1", n)
	}

	h2 := budgetFleet(t, 15400)
	loadIdle(h2, gemmaA, 0) // two minutes: the reading is stale
	if r := h2.c.dryPlan(gemmaB); r.Action != PlanStart || !strings.Contains(r.HostMemoryNote, "the last reading is 2m0s old") {
		t.Fatalf("stale: %s %q", r.Action, r.HostMemoryNote)
	}
}

// The two flags are not part of the shape: a profile that adds them keeps
// its measured need.
func TestHostBudget_ShapeKeyLeavesTheFlagsOut(t *testing.T) {
	p := &core.Profile{Name: "x", Kind: core.KindLlama, Model: "m.gguf", Ctx: 8192, Parallel: 1, ExtraArgs: []string{"--temp", "0.7"}}
	q := *p
	q.ExtraArgs = []string{"--ctx-checkpoints", "3", "--temp", "0.7", "-cram", "0"}
	if needKeyOf(p) != needKeyOf(&q) {
		t.Fatalf("%+v != %+v", needKeyOf(p), needKeyOf(&q))
	}
	q.ExtraArgs = []string{"--temp", "0.8"}
	if needKeyOf(p) == needKeyOf(&q) {
		t.Fatal("another argument is no longer in the shape")
	}
}
