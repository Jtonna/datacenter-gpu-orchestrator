package scheduler

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The stable placement order (stable.go, placer/stable.go).

const stableTol = 128

// stableFleet: six consoles whose idle figures differ by a few MiB, and the
// local host. jitter moves every free reading by up to tol either way.
func stableFleet(r *rand.Rand, tol int) []core.Node {
	base := []int{13000, 13015, 13040, 13022, 13008, 13060}
	var ns []core.Node
	for i, b := range base {
		j := 0
		if r != nil {
			j = r.Intn(2*tol+1) - tol
		}
		ns = append(ns, ps5(fmt.Sprintf("ps5-%c", 'a'+i), 10+i, b+j))
	}
	return append(ns, hostNode(7000))
}

func stableHarness(t *testing.T, tol int) *harness {
	h := newHarness(t)
	h.cfg.Defaults.PlacementToleranceMiB = tol
	h.nodes(stableFleet(nil, 0)...)
	h.llama("small", 4000)
	h.llama("mid", 16000)
	h.llama("big", 30000)
	h.llama("est", 0)
	h.needs["est"] = NeedInfo{Need: placer.Estimated(9000), FileBytes: 1 << 30}
	h.llama("huge", 200000)
	h.command("cmd", 1000)
	h.llama("forced", 16000, func(p *core.Profile) { p.ForceNodes = []string{"ps5-e", "ps5-c"} })
	h.viewChanged()
	return h
}

// stableRows is what an idle plan must keep from one read to the next: the
// action, the placement's nodes in order, and per memory row everything but
// the live readings (free now, average, available, spare, remains, the
// console's MemAvailable figures), which are readings by design.
func stableRows(r PlanReport) string {
	type row struct {
		ID                                             core.NodeID
		Weights, Context, Fill, WillTake, Margin, Host int
		Basis                                          string
		DeviceOverhead                                 int
	}
	var out struct {
		Action PlanAction
		Nodes  []core.NodeID
		Rows   []row
	}
	out.Action = r.Action
	for _, pn := range r.Placement {
		out.Nodes = append(out.Nodes, pn.ID)
	}
	if r.Memory != nil {
		for _, m := range r.Memory.Nodes {
			out.Rows = append(out.Rows, row{m.ID, m.WeightsMiB, m.ContextMiB, m.FillReserveMiB, m.WillTakeMiB, m.MarginMiB, m.HostOverheadMiB, m.Basis, m.DeviceOverheadMiB})
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// differingIdleRows reads every profile's plan reads times, the free
// figures jittering by up to jitter either way between reads, and counts the
// profiles whose rows ever differ from the first read.
func differingIdleRows(t *testing.T, tol, jitter, reads int) (map[string]bool, map[string]string) {
	h := stableHarness(t, tol)
	r := rand.New(rand.NewSource(11))
	first := map[string]string{}
	differ := map[string]bool{}
	names := []string{"small", "mid", "big", "est", "huge", "cmd", "forced"}
	for i := 0; i < reads; i++ {
		h.nodes(stableFleet(r, jitter)...)
		h.viewChanged()
		for _, n := range names {
			got := stableRows(h.c.dryPlan(n))
			if i == 0 {
				first[n] = got
			} else if got != first[n] {
				if !differ[n] && testing.Verbose() {
					t.Logf("read %d %s: %s; first %s", i, n, got, first[n])
				}
				differ[n] = true
			}
		}
	}
	return differ, first
}

// The lead's proof shape: 20 idle plans of every profile over a fleet whose
// free readings jitter by the tolerance either way give 0 differing rows.
// The one exception is a forced profile: its plan must stay byte-identical
// to the plan without the tolerance (TestStable_ForcedPlansByteIdentical),
// and that plan splits by the live budgets, so its rows still move.
func TestStable_TwentyIdlePlansZeroDiffering(t *testing.T) {
	differ, first := differingIdleRows(t, stableTol, stableTol, 20)
	delete(differ, "forced")
	if len(differ) != 0 {
		t.Fatalf("%d profiles' rows differ across 20 idle reads: %v", len(differ), differ)
	}
	// The plans are the expected ones: one console by name, an even split.
	if want := `{"Action":"start","Nodes":["ps5-a"]`; first["small"][:len(want)] != want {
		t.Fatalf("small: %s", first["small"])
	}
	var mid struct{ Rows []struct{ WillTake int } }
	_ = json.Unmarshal([]byte(first["mid"]), &mid)
	if len(mid.Rows) != 2 || mid.Rows[0].WillTake != mid.Rows[1].WillTake {
		t.Fatalf("mid: %s", first["mid"])
	}
	for _, p := range []string{"huge", "cmd", "forced", "big", "est"} {
		if first[p] == "" || first[p] == "{}" {
			t.Fatalf("%s: no plan", p)
		}
	}
	// Without the tolerance the same reads do differ (the test bites).
	if d, _ := differingIdleRows(t, 0, stableTol, 20); len(d) < 3 {
		t.Fatalf("tolerance 0: no row differs; the jitter does not reach the plans")
	}
}

// Forced profiles and chosen-zones plans are byte-identical with and without
// the tolerance, on every read.
func TestStable_ForcedPlansByteIdentical(t *testing.T) {
	on, off := stableHarness(t, stableTol), stableHarness(t, 0)
	r := rand.New(rand.NewSource(5))
	for i := 0; i < 20; i++ {
		fleet := stableFleet(r, stableTol)
		on.nodes(fleet...)
		on.viewChanged()
		off.nodes(fleet...)
		off.viewChanged()
		a, b := on.c.dryPlan("forced"), off.c.dryPlan("forced")
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("read %d forced:\n%+v\n%+v", i, a, b)
		}
		p := on.cfg.Profiles["mid"]
		a = on.c.dryPlanOf("mid", pinCopy(p, []string{"ps5-f", "ps5-b"}))
		b = off.c.dryPlanOf("mid", pinCopy(p, []string{"ps5-f", "ps5-b"}))
		if !reflect.DeepEqual(a, b) || len(a.Placement) != 2 {
			t.Fatalf("read %d chosen zones:\n%+v\n%+v", i, a, b)
		}
	}
}

// A console that does not fit is never chosen because it sorts first: the
// warm console named first is short right now, so the next one is taken.
func TestStable_ShortWarmConsoleNotChosen(t *testing.T) {
	h := stableHarness(t, stableTol)
	h.c.ns("ps5-a").lastFile = "small.gguf"
	h.c.ns("ps5-b").lastFile = "small.gguf"
	if got := h.c.dryPlan("small"); len(got.Placement) != 1 || got.Placement[0].ID != "ps5-a" {
		t.Fatalf("warm by name: %+v", got.Placement)
	}
	h.c.ns("ps5-c").lastFile = "small.gguf"
	h.c.ns("ps5-a").lastFile = "mid.gguf"
	if got := h.c.dryPlan("small"); got.Placement[0].ID != "ps5-b" {
		t.Fatalf("warm first: %+v", got.Placement)
	}
	fleet := stableFleet(nil, 0)
	fleet[1].FreeMiB = 4500 // ps5-b: 3500 usable, short of 4000
	h.nodes(fleet...)
	h.viewChanged()
	if got := h.c.dryPlan("small"); got.Placement[0].ID != "ps5-c" {
		t.Fatalf("short warm console chosen: %+v", got.Placement)
	}
}

// A launch splits as its plan said, and the pre-flight drops the even split
// when the exact figures no longer hold it (the split by exact budget then).
func TestStable_LaunchSplitFollowsPlan(t *testing.T) {
	h := stableHarness(t, stableTol)
	h.load("mid")
	in := h.inst("mid")
	if in == nil || len(in.placement.Nodes) != 2 {
		t.Fatalf("mid not started: %+v", in)
	}
	_, a, b := instanceWeights(in)
	if a == 0 || a != b {
		t.Fatalf("launch split not even: %+v", in.placement.Nodes)
	}

	// The pre-flight's exact figures against the held ones.
	h2 := stableHarness(t, stableTol)
	req, _, _ := h2.c.request(h2.cfg.Profiles["mid"])
	heldA, heldB := h2.c.steady["ps5-a"], h2.c.steady["ps5-b"]
	if heldA < 11900 || heldB < 11900 {
		t.Fatalf("held figures %d %d", heldA, heldB)
	}
	keep := func(budgetB int) []int {
		pl := core.Placement{Nodes: []core.PlacedNode{{ID: "ps5-a", BudgetMiB: heldA, SplitMiB: 12000}, {ID: "ps5-b", BudgetMiB: budgetB, SplitMiB: 12000}}}
		var cands []placer.Candidate
		for _, pn := range pl.Nodes {
			n, _ := h2.c.node(pn.ID)
			cands = append(cands, placer.FromNode(n, pn.BudgetMiB))
		}
		h2.c.keepSteadySplit(req, &pl, cands)
		return []int{pl.Nodes[0].SplitMiB, pl.Nodes[1].SplitMiB}
	}
	if got := keep(heldB - 2*stableTol); !slices.Equal(got, []int{12000, 12000}) {
		t.Fatalf("noise only: even split dropped: %v", got)
	}
	if got := keep(heldB - 2*stableTol - 1); !slices.Equal(got, []int{0, 0}) {
		t.Fatalf("pre-flight found more than noise: even split kept: %v", got)
	}
	if got := keep(6500); !slices.Equal(got, []int{0, 0}) {
		t.Fatalf("even split kept on a short console: %v", got)
	}
}

func instanceWeights(in *inst) (n, a, b int) {
	if len(in.placement.Nodes) != 2 {
		return 0, 0, 0
	}
	return len(in.placement.Nodes), in.placement.Nodes[0].SplitMiB, in.placement.Nodes[1].SplitMiB
}

// seededFleet is a fleet shaped like the live one: twelve consoles of two
// sizes, two laptops and the host; seededProfiles 311 profiles, 207 of
// which start and 104 are refused (too large for any four nodes).
func seededFleet(r *rand.Rand, jitter int) []core.Node {
	var ns []core.Node
	for i := 0; i < 12; i++ {
		free := 13000 + (i%5)*9
		if i >= 8 {
			free = 9000 + (i%3)*31
		}
		if r != nil {
			free += r.Intn(2*jitter+1) - jitter
		}
		ns = append(ns, ps5(fmt.Sprintf("ps5-%02d", i), 20+i, free))
	}
	for i := 0; i < 2; i++ {
		l := laptop(fmt.Sprintf("laptop-%d", i), 7000+i*40)
		l.Addr = fmt.Sprintf("192.0.2.%d:50052", 40+i)
		if r != nil {
			l.FreeMiB += r.Intn(2*jitter+1) - jitter
		}
		ns = append(ns, l)
	}
	if r != nil {
		return append(ns, hostNode(7000+r.Intn(2*jitter+1)-jitter))
	}
	return append(ns, hostNode(7000))
}

func seededHarness(t *testing.T, tol int) (*harness, []string) {
	h := newHarness(t)
	h.cfg.Defaults.PlacementToleranceMiB = tol
	h.nodes(seededFleet(nil, 0)...)
	r := rand.New(rand.NewSource(2026))
	var names []string
	for i := 0; i < 311; i++ {
		name := fmt.Sprintf("p%03d", i)
		names = append(names, name)
		switch {
		case i < 207 && i%23 == 0:
			h.command(name, 500+r.Intn(3000))
		case i < 207 && i%17 == 0:
			h.llama(name, 6000+r.Intn(6000), func(p *core.Profile) { p.ForceNodes = []string{fmt.Sprintf("ps5-%02d", i%8)} })
		case i < 207 && i%5 == 0:
			h.llama(name, 0)
			h.needs[name] = NeedInfo{Need: placer.Estimated(1000 + r.Intn(30000)), FileBytes: 1 << 30}
		case i < 207:
			h.llama(name, 500+r.Intn(38000))
		default:
			h.llama(name, 60000+r.Intn(140000))
		}
	}
	h.viewChanged()
	return h, names
}

// The action of every profile is unchanged by the tolerance on a seeded
// fleet like live's, on every one of 20 jittered reads; the profiles whose
// placement changes are listed with why (the flip list).
func TestStable_ActionsUnchangedOnSeededFleet(t *testing.T) {
	on, names := seededHarness(t, stableTol)
	off, _ := seededHarness(t, 0)
	r := rand.New(rand.NewSource(9))
	flips := map[string]string{}
	for i := 0; i < 20; i++ {
		fleet := seededFleet(r, 25)
		on.nodes(fleet...)
		on.viewChanged()
		off.nodes(fleet...)
		off.viewChanged()
		count := map[PlanAction]int{}
		for _, n := range names {
			a, b := on.c.dryPlan(n), off.c.dryPlan(n)
			count[b.Action]++
			if a.Action != b.Action {
				t.Fatalf("read %d %s: action %s with the tolerance, %s without", i, n, a.Action, b.Action)
			}
			if len(on.cfg.Profiles[n].ForceNodes) > 0 && !reflect.DeepEqual(a, b) {
				t.Fatalf("read %d forced %s differs", i, n)
			}
			if i == 0 {
				if why := flipWhy(a, b); why != "" {
					flips[n] = why
				}
			}
		}
		if i == 0 && (count[PlanStart] != 207 || count[PlanReject] != 104) {
			t.Fatalf("fleet shape: %v, want 207 start and 104 reject", count)
		}
	}
	kinds := map[string]int{}
	for _, why := range flips {
		kinds[why]++
	}
	t.Logf("flip list on the first read: %d of %d profiles change placement or split: %v", len(flips), len(names), kinds)
}

// flipWhy says how a plan with the tolerance (a) differs from one without
// (b): "" when the same nodes, order and rows.
func flipWhy(a, b PlanReport) string {
	ids := func(r PlanReport) []core.NodeID {
		var out []core.NodeID
		for _, pn := range r.Placement {
			out = append(out, pn.ID)
		}
		return out
	}
	ai, bi := ids(a), ids(b)
	switch {
	case stableRows(a) == stableRows(b):
		return ""
	case len(ai) != len(bi):
		return "other node count"
	case !slices.Equal(slices.Sorted(slices.Values(ai)), slices.Sorted(slices.Values(bi))):
		return "other nodes of the same class (by name)"
	case !slices.Equal(ai, bi):
		return "same nodes, device order by name"
	default:
		return "same nodes, even split in place of the split by live budget"
	}
}

// liveShapeFleet is live's shape: six equal consoles, one laptop with memory
// of its own, and the local node.
// lower takes that many MiB off console-3's free memory.
func liveShapeFleet(r *rand.Rand, jitter, lower int) []core.Node {
	j := func() int {
		if r == nil {
			return 0
		}
		return r.Intn(2*jitter+1) - jitter
	}
	var ns []core.Node
	for i, b := range []int{13010, 13024, 12998, 13031, 13005, 13017} {
		if i == 2 {
			b -= lower
		}
		ns = append(ns, ps5(fmt.Sprintf("console-%d", i+1), 10+i, b+j()))
	}
	return append(ns, laptop("laptop-1", 7600+j()), hostNode(7000+j()))
}

// For live's shape, between tolerance 128 and 0 on every one of 20 jittered
// reads: no profile's action, need, fit need, device count or tiers used
// changes. What changes is listed: which console a single load lands on,
// the device order and the split.
func TestStable_LiveShapeFlipList(t *testing.T) {
	liveShapeCompare(t, 0)
}

// The same with console-3 lower by 60 and 127 MiB (still of the class: the
// even split against unequal real memory) and by 700 and 2,500 MiB (a class
// of its own): no profile that starts at tolerance 0 queues or is refused
// at 128, and nothing else in the list above changes either.
func TestStable_LiveShapeOneConsoleLower(t *testing.T) {
	for _, d := range []int{60, 127, 700, 2500} {
		t.Run(fmt.Sprintf("lower_%d", d), func(t *testing.T) { liveShapeCompare(t, d) })
	}
}

func liveShapeCompare(t *testing.T, lower int) {
	t.Helper()
	liveShapeDiff(t, lower, stableTol, false, 0, false)
}

// The preference for consoles with stored figures (after warm ones) against
// the order before it, both at tolerance 128, on live's shape with stored
// per-node figures and host readings: nothing but the consoles chosen, the
// device order and the rows that follow them changes.
func TestStable_PreferStoredFlipList(t *testing.T) {
	for _, d := range []int{0, 700} {
		t.Run(fmt.Sprintf("lower_%d", d), func(t *testing.T) { liveShapeDiff(t, d, stableTol, false, stableTol, true) })
	}
}

// liveShapeDiff compares the plans of two harnesses on live's shape (the
// first at tolA, the second at tolB; noPrefer drops the preference for
// stored figures), with stored per-node figures on some profiles.
func liveShapeDiff(t *testing.T, lower, tolA int, noPreferA bool, tolB int, noPreferB bool) {
	t.Helper()
	storedSet := map[string][]core.NodeID{}
	build := func(tol int, noPrefer bool) (*harness, []string) {
		h := newHarness(t)
		h.cfg.Defaults.PlacementToleranceMiB = tol
		h.c.noPreferStored = noPrefer
		h.nodes(liveShapeFleet(nil, 0, lower)...)
		stored := rand.New(rand.NewSource(77))
		r := rand.New(rand.NewSource(55))
		var names []string
		for i := 0; i < 120; i++ {
			name := fmt.Sprintf("p%03d", i)
			names = append(names, name)
			switch {
			case i%11 == 0:
				h.command(name, 500+r.Intn(4000))
			case i%13 == 0:
				h.llama(name, 3000+r.Intn(8000), func(p *core.Profile) { p.ForceNodes = []string{fmt.Sprintf("console-%d", 1+i%6)} })
			case i%4 == 0:
				h.llama(name, 0)
				h.needs[name] = NeedInfo{Need: placer.Estimated(800 + r.Intn(40000)), FileBytes: 1 << 30}
			case i%7 == 0:
				h.llama(name, 60000+r.Intn(60000)) // too large: refused
			default:
				need := 500 + r.Intn(44000)
				p := h.llama(name, need)
				if i%3 == 0 { // measured with per-node figures on random consoles
					k := min((need+11000)/12000, 4)
					perm := stored.Perm(6)
					l := &learnedNeed{TotalMiB: need, key: needKeyOf(p), profile: name, devices: k}
					for j := 0; j < k; j++ {
						id := core.NodeID(fmt.Sprintf("console-%d", 1+perm[j]))
						l.placement = append(l.placement, id)
						l.split = append(l.split, 1/float64(k))
						l.Nodes = append(l.Nodes, learnedNode{ID: id, Fraction: 1 / float64(k), MiB: need / k, PeakMiB: need/k + 150})
					}
					h.c.putLearned(l)
					storedSet[name] = slices.Sorted(slices.Values(l.placement))
				}
			}
		}
		h.c.needs_ = map[string]NeedInfo{}
		h.viewChanged()
		return h, names
	}
	on, names := build(tolA, noPreferA)
	off, _ := build(tolB, noPreferB)
	prios := func(h *harness, r PlanReport) []int {
		var out []int
		for _, pn := range r.Placement {
			n, _ := h.c.node(pn.ID)
			out = append(out, n.Priority)
		}
		slices.Sort(out)
		return out
	}
	rng := rand.New(rand.NewSource(3))
	kinds := map[string]int{}
	actions := map[PlanAction]int{}
	onStored := [2]int{} // profiles placed on exactly their stored set: first harness, second
	sameSet := func(r PlanReport, want []core.NodeID) bool {
		var got []core.NodeID
		for _, pn := range r.Placement {
			got = append(got, pn.ID)
		}
		return want != nil && slices.Equal(slices.Sorted(slices.Values(got)), want)
	}
	for i := 0; i < 20; i++ {
		fleet := liveShapeFleet(rng, 25, lower)
		on.nodes(fleet...)
		on.viewChanged()
		off.nodes(fleet...)
		off.viewChanged()
		for _, n := range names {
			a, b := on.c.dryPlan(n), off.c.dryPlan(n)
			if a.Action != b.Action || a.NeedMiB != b.NeedMiB || a.FitNeedMiB != b.FitNeedMiB || len(a.Placement) != len(b.Placement) ||
				!slices.Equal(prios(on, a), prios(off, b)) || a.TightFit != b.TightFit {
				t.Fatalf("read %d %s: action %s/%s need %d/%d fit %d/%d devices %d/%d tiers %v/%v tight %v/%v", i, n,
					a.Action, b.Action, a.NeedMiB, b.NeedMiB, a.FitNeedMiB, b.FitNeedMiB, len(a.Placement), len(b.Placement),
					prios(on, a), prios(off, b), a.TightFit, b.TightFit)
			}
			if i == 0 {
				if sameSet(a, storedSet[n]) {
					onStored[0]++
				}
				if sameSet(b, storedSet[n]) {
					onStored[1]++
				}
				actions[b.Action]++
				if why := flipWhy(a, b); why != "" {
					kinds[fmt.Sprintf("%d device(s): %s", max(len(a.Placement), len(a.Memory.Nodes)), why)]++
				}
			}
		}
	}
	t.Logf("live shape, console-3 lower by %d MiB, tolerance %d (no preference %v) against %d (no preference %v), %d profiles %v, %d with stored per-node figures (on exactly that set: %d against %d): on the first read %v",
		lower, tolA, noPreferA, tolB, noPreferB, len(names), actions, len(storedSet), onStored[0], onStored[1], kinds)
}

// (Reviewer 1) A profile measured on two consoles stays measured on any two
// consoles: the measured need is kept per shape and device count, not per
// machine set (needstore needs_by_placement, PRIMARY KEY (shape, devices)).
// Per-node figures and peaks apply only to the very set and split they were
// measured on. Among equal consoles the ones with per-node figures of the
// shape go first (after warm ones), so here the plan comes back to
// console-4 and console-5 and their own peaks apply; without that order it
// takes console-1 and console-2, where the peak total still holds.
func TestStable_MeasuredOnOtherConsolesStaysMeasured(t *testing.T) {
	plan := func(tol int, set []core.NodeID, noPrefer ...bool) PlanReport {
		h := newHarness(t)
		h.c.noPreferStored = len(noPrefer) > 0
		h.cfg.Defaults.PlacementToleranceMiB = tol
		h.cfg.Defaults.FillReserveKiBPerToken = 8
		h.nodes(liveShapeFleet(nil, 0, 0)...)
		p := h.llama("m", 0, func(p *core.Profile) { p.Ctx = 4096 })
		h.needs["m"] = NeedInfo{Need: placer.Estimated(17000), FileBytes: 8 << 30}
		h.c.putLearned(&learnedNeed{TotalMiB: 19000, key: needKeyOf(p), profile: "m", placement: set, devices: 2,
			split: []float64{0.5, 0.5}, Nodes: []learnedNode{{ID: set[0], Fraction: 0.5, MiB: 9500, PeakMiB: 9800}, {ID: set[1], Fraction: 0.5, MiB: 9500, PeakMiB: 9800}}})
		h.c.needs_ = map[string]NeedInfo{}
		h.viewChanged()
		return h.c.dryPlan("m")
	}
	rows := func(r PlanReport) (ids []core.NodeID, take int, basis []string) {
		for _, n := range r.Memory.Nodes {
			ids, take, basis = append(ids, n.ID), take+n.WillTakeMiB, append(basis, n.Basis)
		}
		return
	}
	measuredOn := []core.NodeID{"console-4", "console-5"}
	on, off := plan(stableTol, measuredOn), plan(0, measuredOn)
	for _, r := range []PlanReport{on, off} {
		if r.Action != PlanStart || !r.Measured || r.NeedMiB != 19000 || len(r.Placement) != 2 {
			t.Fatalf("%s measured %v need %d on %d", r.Action, r.Measured, r.NeedMiB, len(r.Placement))
		}
	}
	if on.FitNeedMiB != off.FitNeedMiB {
		t.Fatalf("fit need %d at 128, %d at 0", on.FitNeedMiB, off.FitNeedMiB)
	}
	ids, take, basis := rows(on)
	if !slices.Equal(ids, measuredOn) || take != 2*9800 || basis[0] != BasisPeak {
		t.Fatalf("at 128 on %v: will take %d, basis %v (want the measured set with its own peaks, 19600)", ids, take, basis)
	}
	plain := plan(stableTol, measuredOn, true)
	if pids, ptake, _ := rows(plain); !slices.Equal(pids, []core.NodeID{"console-1", "console-2"}) || ptake < 2*9800 || !plain.Measured || plain.NeedMiB != 19000 {
		t.Fatalf("without the preference on %v: will take %d, measured %v need %d", pids, ptake, plain.Measured, plain.NeedMiB)
	}
	oids, otake, obasis := rows(off)
	t.Logf("measured on %v: at 128 on %v will take %d (%v); at 0 on %v will take %d (%v)", measuredOn, ids, take, basis, oids, otake, obasis)

	// measured on the set the stable order takes: its own per-node peaks
	same := plan(stableTol, []core.NodeID{"console-1", "console-2"})
	ids, take, basis = rows(same)
	if !same.Measured || same.NeedMiB != 19000 || take != 2*9800 || basis[0] != BasisPeak {
		t.Fatalf("on its own set %v: measured %v need %d will take %d basis %v", ids, same.Measured, same.NeedMiB, take, basis)
	}
}

// A host: node profile placed automatically on equal consoles is hosted by
// the console with a measured host overhead of its shape (the first node
// of the placement), so its host row is measured, not the setting.
func TestStable_HostReadingConsoleHosts(t *testing.T) {
	h := newHarness(t)
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.cfg.Defaults.PlacementToleranceMiB = stableTol
	h.nodes(liveShapeFleet(nil, 0, 0)...)
	q := h.llama("q", 16000, func(p *core.Profile) { p.Host = core.HostNode })
	h.c.putHostOverhead(measured(shapeOf(q), "console-5", 2, 700, h.clk.Now(), h.c.llamaBuild))
	h.viewChanged()
	r := h.c.dryPlan("q")
	if r.Action != PlanStart || len(r.Placement) != 2 || r.Placement[0].ID != "console-5" {
		t.Fatalf("%s %+v", r.Action, r.Placement)
	}
	var host *PlanMemoryNode
	for i := range r.Memory.Nodes {
		if r.Memory.Nodes[i].ID == "console-5" {
			host = &r.Memory.Nodes[i]
		}
	}
	if host == nil || host.HostOverheadMiB != 700 || host.HostOverheadSource != HostOverheadMeasured {
		t.Fatalf("host row %+v", host)
	}
	h.c.noPreferStored = true
	if r := h.c.dryPlan("q"); r.Placement[0].ID != "console-1" {
		t.Fatalf("without the preference the host is the first by name: %+v", r.Placement)
	}
}
