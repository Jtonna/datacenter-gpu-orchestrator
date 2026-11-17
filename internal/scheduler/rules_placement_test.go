package scheduler

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/fleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
)

// Design 7.1, 7.2, 7.5, 7.6 as they apply to the stateful core.

func argValue(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func TestPreflight_AllSucceed_OverrideAndSplitFromProbe(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 9000), ps5("b", 4, 9000))
	h.llama("m", 14000)
	h.prober.free["10.0.0.2:50052"] = 8500
	h.prober.free["10.0.0.4:50052"] = 9500
	h.request("m")
	h.wantNodes("m", "a", "b")
	argv := h.child("m").Argv()
	if got := argValue(argv, "--tensor-split"); got != "7500,8500" && got != "8500,7500" {
		t.Fatalf("--tensor-split %q, want the pre-flight budgets 7500 and 8500", got)
	}
	if got := argValue(argv, "--rpc"); !strings.Contains(got, "10.0.0.2:50052") || !strings.Contains(got, "10.0.0.4:50052") {
		t.Fatalf("--rpc %q", got)
	}
	ns := h.c.nodes["a"]
	if ns.ovUntil.IsZero() || ns.ovFree != 8500 || !ns.hasPF || ns.emptyPF != 8500 {
		t.Fatalf("override/empty not recorded: %+v", ns)
	}
}

func TestPreflight_NodeFails_QuarantinedAndReturned(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("big", 12000)
	h.llama("m", 12000)
	h.ready("big") // a
	if h.nodesOf("big")[0] != "a" {
		t.Fatalf("fixture: big on %v", h.nodesOf("big"))
	}
	h.cfg.Defaults.DrainAfter = time.Hour
	h.prober.fail["10.0.0.4:50052"] = probeFail("10.0.0.4:50052")
	h.autoProbe = false
	h.view.modify("b", func(n *core.Node) { n.Online = false })
	w := h.request("m") // queued: b offline, a busy
	h.advance(20 * time.Second)
	created, clock := h.entry("m").created, h.entry("m").clock
	h.view.modify("b", func(n *core.Node) { n.Online = true })
	h.viewChanged()
	h.wantState("m", "starting")
	h.completeProbes()
	h.wantState("m", "absent")
	e := h.entry("m")
	if !e.created.Equal(created) || e.clock != clock || len(e.waiters) != 1 || e.waiters[0] != w {
		t.Fatalf("returned entry created %v clock %v waiters %d; want %v %v 1", e.created, e.clock, len(e.waiters), created, clock)
	}
	if !h.c.quarantined("b") || h.c.nodes["b"].holder != "" || h.c.nodes["b"].settle != setNone {
		t.Fatalf("b: quarantined=%v %+v", h.c.quarantined("b"), h.c.nodes["b"])
	}
	h.pending(w)
}

func TestPreflight_ExactFiguresDoNotFit_ReturnedAndOverrideStopsRepeat(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 12000)
	h.prober.free["10.0.0.2:50052"] = 9000 // fleet says 15000; a is really fuller
	h.autoProbe = false
	w := h.request("m")
	first := h.nodesOf("m")
	if !slices.Equal(first, []core.NodeID{"a"}) {
		t.Fatalf("fixture: first placement %v", first)
	}
	h.completeProbes()
	// Returned, then placed again at once on b: the override on a stops a repeat.
	h.wantState("m", "starting")
	h.wantNodes("m", "b")
	h.completeProbes()
	h.healthy("m")
	h.granted(w)
	if h.c.quarantined("a") {
		t.Fatalf("a quarantined for a fit failure")
	}
}

func TestPreflight_LocalNotProbed(t *testing.T) {
	h := newHarness(t).nodes(omen(8000))
	h.llama("m", 6000)
	h.request("m")
	if len(h.prober.calls) != 0 {
		t.Fatalf("probed %v", h.prober.calls)
	}
	argv := h.child("m").Argv()
	if argValue(argv, "--device") != "CUDA0" || argValue(argv, "--rpc") != "" {
		t.Fatalf("argv %v", argv)
	}
}

func TestOverride_ExpiresAfter10Minutes(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 12000)
	h.prober.free["10.0.0.2:50052"] = 9000
	w := h.request("m") // returned: does not fit on the exact figure
	h.wantState("m", "absent")
	if got := h.c.freeMiB("a", h.viewNode("a")); got != 9000 {
		t.Fatalf("free = %d, want override 9000", got)
	}
	delete(h.prober.free, "10.0.0.2:50052")
	h.advance(10*time.Minute - time.Second)
	h.wantState("m", "absent")
	h.advance(time.Second)
	h.wantState("m", "starting") // fleet figure used again
	_ = w
}

func TestOverride_ClearedWhenInstanceReleasedAfterChildRan(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.prober.free["10.0.0.2:50052"] = 14000
	h.release(h.ready("m"))
	if h.c.nodes["a"].ovUntil.IsZero() {
		t.Fatalf("no override after pre-flight")
	}
	h.step(evUnload{profile: "m"})
	if !h.c.nodes["a"].ovUntil.IsZero() {
		t.Fatalf("override kept after release")
	}
}

func TestSettle_NodeUnusableThenFetchedFigures(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.llama("n", 8000)
	h.autoFetch = false
	h.release(h.ready("m"))
	h.step(evUnload{profile: "m"})
	if h.c.nodes["a"].settle != setWaiting {
		t.Fatalf("a not settling")
	}
	w := h.request("n")
	h.advance(2 * time.Second)
	h.wantState("n", "absent")
	h.advance(time.Second)
	if h.c.nodes["a"].settle != setFetching || len(h.fetches) != 1 {
		t.Fatalf("no fetch after settle: %v", h.c.nodes["a"].settle)
	}
	h.wantState("n", "absent")
	h.completeFetches()
	h.wantState("n", "starting")
	_ = w
}

func TestSettle_FetchFails_TreatedOffline(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.llama("n", 8000)
	h.release(h.ready("m"))
	h.view.fetchErr["a"] = errors.New("console down")
	h.step(evUnload{profile: "m"})
	w := h.request("n")
	h.advance(3 * time.Second)
	if h.c.nodes["a"].settle != setFailed || h.c.eligible("a") {
		t.Fatalf("a after failed fetch: %v eligible=%v", h.c.nodes["a"].settle, h.c.eligible("a"))
	}
	h.wantState("n", "absent")
	delete(h.view.fetchErr, "a")
	h.advance(3 * time.Second) // fetched again
	h.wantState("n", "starting")
	_ = w
}

func TestEmpty_ForgottenOnSuccessfulLoad(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	ns := h.c.ns("a")
	ns.emptyClaim, ns.hasClaim = 9000, true
	h.prober.free["10.0.0.2:50052"] = 14000
	h.ready("m")
	if ns.hasClaim || ns.hasPF {
		t.Fatalf("empty figures kept after a successful load: %+v", ns)
	}
	if got := h.c.planBudget("a", h.viewNode("a")); got != 15000 {
		t.Fatalf("plan budget %d, want total-headroom 15000", got)
	}
}

func TestCapacity_NeverFits_503AtOnceWithFigures(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), alien("al", 7000))
	h.llama("huge", 40000)
	w := h.request("huge")
	e := h.failed(w, http.StatusServiceUnavailable, ReasonDoesNotFit)
	if e.NeedMiB != 40000 || e.RequiredMiB != 40000 || e.CapacityMiB != 15000+7800 {
		t.Fatalf("figures need %d required %d capacity %d", e.NeedMiB, e.RequiredMiB, e.CapacityMiB)
	}
	h.noEntry("huge")
}

func TestCapacity_CountsQuarantinedAndSettlingNodes(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("pair", 25000)
	h.llama("m", 8000)
	h.autoFetch = false
	h.release(h.ready("m"))
	h.step(evUnload{profile: "m"}) // its node settles
	h.c.quarantine("b", errBoom)
	w := h.request("pair")
	h.pending(w)
	h.entry("pair")
}

func TestCapacity_SkippedWhileStale_TimesOutStale(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("huge", 40000)
	h.view.stale()
	h.advance(61 * time.Second)
	w := h.request("huge")
	h.pending(w)
	h.advance(600 * time.Second)
	h.failed(w, http.StatusServiceUnavailable, ReasonFleetStale)
}

func TestCapacity_EstimatedNeedUsesOverheadAndMargin(t *testing.T) {
	h := newHarness(t).nodes(omen(8151+200-256), alien("al", 7885-200+256))
	// 7.2 worked check: Devstral 14K, estimated 14828, both laptops do not fit.
	h.llama("devstral", 0)
	h.needs["devstral"] = NeedInfo{Need: placer.Estimated(14828)}
	h.c.nodes = map[core.NodeID]*nodeState{}
	n1, _ := h.view.get("omen")
	n1.TotalMiB, n1.FreeMiB, n1.HeadroomMiB = 8151, 8151, 256
	n2, _ := h.view.get("al")
	n2.TotalMiB, n2.FreeMiB, n2.HeadroomMiB = 7885, 7885, 256
	h.view.set(n1, n2)
	w := h.request("devstral")
	e := h.failed(w, http.StatusServiceUnavailable, ReasonDoesNotFit)
	if e.CapacityMiB != 14324 || e.RequiredMiB != 15570 {
		t.Fatalf("capacity %d required %d, want 14324 and 15570", e.CapacityMiB, e.RequiredMiB)
	}
}

func TestEligibility_StaleView_RemoteIneligibleLocalOK(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), omen(8000))
	h.llama("m", 6000)
	h.view.stale()
	h.advance(61 * time.Second)
	h.request("m")
	h.wantNodes("m", "omen")
}

func TestEligibility_NoAddressNotUsed(t *testing.T) {
	n := ps5("a", 2, 15000)
	n.Addr = ""
	h := newHarness(t).nodes(n, ps5("b", 4, 15000))
	h.llama("m", 6000)
	h.request("m")
	h.wantNodes("m", "b")
}

func TestEligibility_AllowTiers(t *testing.T) {
	h := newHarness(t).nodes(omen(8000), alien("al", 7800))
	h.llama("m", 6000, func(p *core.Profile) { p.AllowTiers = []int{3} })
	h.request("m")
	h.wantNodes("m", "omen")
}

func TestForced_UnresolvedAndAmbiguousNamesUnavailable(t *testing.T) {
	a := ps5("a", 2, 15000)
	b := ps5("b", 4, 15000)
	a.Hostname, b.Hostname = "twin", "twin"
	h := newHarness(t).nodes(a, b)
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"twin"} })
	if _, err := h.view.Resolve("twin"); !errors.As(err, new(*fleet.AmbiguousError)) {
		t.Fatalf("fixture: %v", err)
	}
	w := h.request("f")
	h.pending(w)
	h.wantState("f", "absent")
	h.view.modify("b", func(n *core.Node) { n.Online = false }) // now unambiguous
	h.viewChanged()
	h.wantState("f", "starting")
	h.wantNodes("f", "a")
}

func TestForced_PlacedInConfigOrderWithProfileSplit(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), alien("al", 7800))
	h.llama("bench", 1, func(p *core.Profile) {
		p.ForceNodes = []string{"al", "a"}
		p.TensorSplit = []int{40, 60}
	})
	h.request("bench")
	argv := h.child("bench").Argv()
	if argValue(argv, "--rpc") != "10.0.0.3:50052,10.0.0.2:50052" || argValue(argv, "--tensor-split") != "40,60" {
		t.Fatalf("argv %v", argv)
	}
}

func TestLoadTimeout_FormulaForLlama(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: 11e9}
	h.request("m")
	// 120 s + 2 × 11e9 / 11e6 s = 2120 s
	if got := h.inst("m").loadTimeout; got != 2120*time.Second {
		t.Fatalf("load timeout %v", got)
	}
}

func TestLoadTimeout_CommandUsesCommandLoadTimeout(t *testing.T) {
	h := newHarness(t).nodes(omen(8000))
	h.command("whisper", 5000)
	h.request("whisper")
	if got := h.inst("whisper").loadTimeout; got != 900*time.Second {
		t.Fatalf("load timeout %v", got)
	}
	argv := h.child("whisper").Argv()
	if argv[0] != "whisper" || argv[2] != "5801" || h.child("whisper").health != "/ready" {
		t.Fatalf("argv %v health %q", argv, h.child("whisper").health)
	}
}

func TestCommand_LocalNodeOnly(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), omen(4000))
	h.command("whisper", 5000)
	w := h.request("whisper") // live budget 3800 on the local node; the PS5 is never used
	h.pending(w)
	h.wantState("whisper", "absent")
	h.command("flux", 9000)
	w2 := h.request("flux") // capacity: the local node alone, 8000-200 < 9000
	h.failed(w2, http.StatusServiceUnavailable, ReasonDoesNotFit)
}

// Regression (review item 1): the local node keeps one id, so an instance on
// it stays its holder, and stays evictable, when the view's local entry is
// refreshed or replaced; the settle fetch then targets that same id.
func TestLocalNode_HolderAndEvictionSurviveViewRefresh(t *testing.T) {
	h := newHarness(t).nodes(omen(8000))
	h.llama("m", 6000)
	g := h.ready("m")
	h.release(g)
	h.wantNodes("m", "omen")

	// refreshed figures, then replaced (removed and re-added) entry
	h.view.modify("omen", func(n *core.Node) { n.FreeMiB = 1500; n.Hostname = "omen-runner" })
	h.viewChanged()
	h.view.remove("omen")
	h.viewChanged()
	h.view.set(omen(7800))
	h.viewChanged()
	if got := h.c.nodes["omen"].holder; got != "m" {
		t.Fatalf("holder of omen = %q, want m", got)
	}
	if len(h.c.nodes) != 1 {
		t.Fatalf("ghost node ids: %v", h.c.nodes)
	}

	h.command("whisper", 5000)
	w := h.request("whisper")
	h.pending(w)
	h.advance(h.cfg.Defaults.MaxHold)
	h.wantState("m", "absent")  // evicted: it was still associated with the node
	h.advance(10 * time.Second) // settle, then the fetch of the same id
	h.wantState("whisper", "starting")
	if len(h.c.nodes) != 1 {
		t.Fatalf("ghost node ids: %v", h.c.nodes)
	}
}

// Forced placement has no fit check (7.3): a node whose pre-flight free
// figure is below its headroom must not make the load fail with a negative
// tensor-split weight.
func TestForced_NegativeBudgetStillLoadsWithClampedSplit(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), alien("al", 7800))
	h.llama("bench", 1, func(p *core.Profile) { p.ForceNodes = []string{"al", "a"} })
	h.prober.free["10.0.0.2:50052"] = 600 // below the PS5 tier's 1000 MiB headroom
	w := h.request("bench")
	h.wantState("bench", "starting")
	h.healthy("bench")
	h.granted(w)
	if got := argValue(h.child("bench").Argv(), "--tensor-split"); got != "7600,1" {
		t.Fatalf("--tensor-split %q, want 7600,1", got)
	}
}
