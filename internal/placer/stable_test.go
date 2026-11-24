package placer

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// console is a shared-memory candidate with a name and a steady figure.
func console(name string, live, steady int) Candidate {
	return Candidate{ID: core.NodeID("id-" + name), Name: name, Addr: "192.0.2.1:50052", Priority: 1, BudgetMiB: live, SteadyMiB: steady, Shared: true}
}

func stableReq(need int) Request {
	return Request{Need: Measured(need), MaxPool: 4, MinShareMiB: 1024, SkewPermille: 150, EqualMiB: 128}
}

// Equal consoles (steady figures within the tolerance) are taken by name,
// whatever their live readings say; a warm console goes first.
func TestStable_SingleByName(t *testing.T) {
	nodes := []Candidate{console("ps5-c", 13990, 14000), console("ps5-a", 14020, 14010), console("ps5-b", 13950, 13960)}
	pl, err := Place(stableReq(8000), nodes)
	if err != nil || len(pl.Nodes) != 1 || pl.Nodes[0].ID != "id-ps5-a" {
		t.Fatalf("%+v %v", pl, err)
	}
	nodes[0].Warm = true
	pl, _ = Place(stableReq(8000), nodes)
	if pl.Nodes[0].ID != "id-ps5-c" {
		t.Fatalf("warm first: %+v", pl)
	}
	// Today's rule (EqualMiB 0): the smallest live budget, ps5-b.
	old := stableReq(8000)
	old.EqualMiB = 0
	if pl, _ := Place(old, nodes); pl.Nodes[0].ID != "id-ps5-b" {
		t.Fatalf("old rule: %+v", pl)
	}
}

// A console that does not fit is never chosen because it sorts first.
func TestStable_NeverAConsoleThatDoesNotFit(t *testing.T) {
	nodes := []Candidate{console("ps5-a", 7000, 14000), console("ps5-b", 13950, 13960)}
	nodes[0].Warm = true
	pl, err := Place(stableReq(8000), nodes)
	if err != nil || pl.Nodes[0].ID != "id-ps5-b" {
		t.Fatalf("%+v %v", pl, err)
	}
}

// A consoles' class is decided by its steady figure; a console clearly
// smaller (one tolerance or more) is a class of its own and, for a single
// node, still preferred as the tighter fit.
func TestStable_ClearlySmallerIsItsOwnClass(t *testing.T) {
	nodes := []Candidate{console("ps5-a", 14000, 14000), console("ps5-b", 12000, 12000)}
	if pl, _ := Place(stableReq(8000), nodes); pl.Nodes[0].ID != "id-ps5-b" {
		t.Fatalf("%+v", pl)
	}
}

// Pools: the same set and the same even split on every read while the live
// readings jitter by up to the tolerance either way; input order does not
// matter.
func TestStable_PoolIgnoresJitter(t *testing.T) {
	steady := []int{14000, 14030, 13980, 14010, 14005, 13990}
	names := []string{"ps5-f", "ps5-b", "ps5-e", "ps5-a", "ps5-d", "ps5-c"}
	r := rand.New(rand.NewSource(7))
	var first core.Placement
	for i := 0; i < 50; i++ {
		var nodes []Candidate
		for j, s := range steady {
			nodes = append(nodes, console(names[j], s+r.Intn(257)-128, s))
		}
		r.Shuffle(len(nodes), func(a, b int) { nodes[a], nodes[b] = nodes[b], nodes[a] })
		pl, err := Place(stableReq(20000), nodes)
		if err != nil {
			t.Fatal(err)
		}
		for k := range pl.Nodes {
			pl.Nodes[k].BudgetMiB = 0 // a live reading, carried for the status only
		}
		if i == 0 {
			first = pl
			if len(pl.Nodes) != 2 || pl.Nodes[0].ID != "id-ps5-a" || pl.Nodes[1].ID != "id-ps5-b" || pl.Nodes[0].SplitMiB != pl.Nodes[1].SplitMiB || pl.Nodes[0].SplitMiB == 0 {
				t.Fatalf("first %+v", pl)
			}
			continue
		}
		if !reflect.DeepEqual(pl, first) {
			t.Fatalf("read %d: %+v, first %+v", i, pl, first)
		}
	}
}

// When the even split would overfill a console that is short right now,
// the placement keeps today's split by live budget (no SplitMiB), so a set
// that fits today still fits.
func TestStable_SplitFallsBackWhenEvenDoesNotFit(t *testing.T) {
	nodes := []Candidate{console("ps5-a", 8800, 14000), console("ps5-b", 13900, 14000)}
	req := stableReq(17000)
	req.MaxPool = 2
	pl, err := Place(req, nodes)
	if err != nil || len(pl.Nodes) != 2 {
		t.Fatalf("%+v %v", pl, err)
	}
	for _, n := range pl.Nodes {
		if n.SplitMiB != 0 {
			t.Fatalf("even split kept although ps5-a cannot take half: %+v", pl)
		}
	}
	old := req
	old.EqualMiB = 0
	if _, err := Place(old, nodes); err != nil {
		t.Fatalf("today's rule: %v", err)
	}
}

// Forced placements are untouched by the tolerance.
func TestStable_ForcedUntouched(t *testing.T) {
	nodes := []Candidate{console("ps5-b", 13000, 14000), console("ps5-a", 14000, 14000)}
	req := stableReq(20000)
	req.Force = []ForcedNode{{Name: "ps5-b", ID: "id-ps5-b"}, {Name: "ps5-a", ID: "id-ps5-a"}}
	a, _ := Place(req, nodes)
	req.EqualMiB = 0
	b, _ := Place(req, nodes)
	if !reflect.DeepEqual(a, b) || a.Nodes[0].ID != "id-ps5-b" || a.Nodes[0].SplitMiB != 0 {
		t.Fatalf("%+v %+v", a, b)
	}
}

// Whatever the need, a request that places at tolerance 0 places at 128 on
// as many nodes, and every share fits its node's EXACT budget: with equal
// consoles, with one console lower inside the class (60 and 127 MiB, where
// the even split meets unequal real memory) and outside it (700 and 2,500
// MiB), and with the live reading of one console below its steady figure.
func TestStable_NeverWorseThanToleranceZero(t *testing.T) {
	for _, d := range []int{0, 60, 127, 700, 2500} {
		for _, liveShort := range []int{0, 90} {
			var nodes []Candidate
			for i, name := range []string{"ps5-a", "ps5-b", "ps5-c", "ps5-d"} {
				steady := 12010 - i*3
				if i == 2 {
					steady -= d
				}
				live := steady
				if i == 1 {
					live -= liveShort
				}
				nodes = append(nodes, console(name, live, steady))
			}
			placed := 0
			for need := 1000; need <= 49000; need += 37 {
				req := stableReq(need)
				old := req
				old.EqualMiB = 0
				b, errB := Place(old, nodes)
				a, errA := Place(req, nodes)
				if errB != nil {
					continue
				}
				placed++
				if errA != nil || len(a.Nodes) != len(b.Nodes) {
					t.Fatalf("lower %d, live short %d, need %d: tolerance 0 places on %d, 128 gives %+v %v", d, liveShort, need, len(b.Nodes), a, errA)
				}
				if len(a.Nodes) > 1 {
					w := make([]int, len(a.Nodes))
					for i, n := range a.Nodes {
						w[i] = n.BudgetMiB
						if n.SplitMiB > 0 {
							w[i] = n.SplitMiB
						}
					}
					sum := 0
					for _, x := range w {
						sum += x
					}
					for i, n := range a.Nodes {
						if share := need * w[i] / sum; share > n.BudgetMiB {
							t.Fatalf("lower %d, live short %d, need %d: %s takes %d of its exact budget %d (%+v)", d, liveShort, need, n.ID, share, n.BudgetMiB, a)
						}
					}
				}
			}
			if placed < 1000 {
				t.Fatalf("lower %d: only %d needs placed; the sweep is too narrow", d, placed)
			}
		}
	}
}

// Among equal consoles: warm first, then one with a host reading, then one
// with stored per-node figures, then by name. A pool takes the set with
// more of them in that order, and the host reading's node leads the device
// order (it hosts a host: node profile). Tolerance 0 ignores all of it.
func TestStable_HostReadingAndMeasuredOrder(t *testing.T) {
	mk := func(name string, warm, host, measured bool) Candidate {
		c := console(name, 13000, 13000)
		c.Warm, c.HostReading, c.Measured = warm, host, measured
		return c
	}
	ids := func(pl core.Placement) (out []string) {
		for _, n := range pl.Nodes {
			out = append(out, string(n.ID))
		}
		return
	}
	// single loads
	for _, c := range []struct {
		nodes []Candidate
		want  string
	}{
		{[]Candidate{mk("ps5-a", false, false, false), mk("ps5-b", false, false, true)}, "id-ps5-b"},
		{[]Candidate{mk("ps5-a", false, false, true), mk("ps5-b", false, true, false)}, "id-ps5-b"},
		{[]Candidate{mk("ps5-a", false, true, true), mk("ps5-b", true, false, false)}, "id-ps5-b"},
		{[]Candidate{mk("ps5-a", false, false, true), mk("ps5-b", false, false, true)}, "id-ps5-a"},
	} {
		pl, err := Place(stableReq(8000), c.nodes)
		if err != nil || len(pl.Nodes) != 1 || string(pl.Nodes[0].ID) != c.want {
			t.Errorf("%+v: %v %v, want %s", c.nodes, ids(pl), err, c.want)
		}
	}
	// pools of two out of four
	nodes := []Candidate{mk("ps5-a", false, false, false), mk("ps5-b", false, false, false), mk("ps5-c", false, false, true), mk("ps5-d", false, true, true)}
	pl, err := Place(stableReq(20000), nodes)
	if err != nil || !slices.Equal(ids(pl), []string{"id-ps5-d", "id-ps5-c"}) || pl.Nodes[0].SplitMiB != pl.Nodes[1].SplitMiB {
		t.Fatalf("pool: %v %v %+v", ids(pl), err, pl)
	}
	// tolerance 0: today's rule, untouched
	old := stableReq(20000)
	old.EqualMiB = 0
	plain := []Candidate{console("ps5-a", 13000, 13000), console("ps5-b", 13000, 13000), console("ps5-c", 13000, 13000), console("ps5-d", 13000, 13000)}
	a, _ := Place(old, nodes)
	b, _ := Place(old, plain)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("tolerance 0 moved: %+v %+v", a, b)
	}
}
