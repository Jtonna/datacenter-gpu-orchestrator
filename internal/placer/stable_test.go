package placer

import (
	"math/rand"
	"reflect"
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
