package placer

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// nd builds a remote candidate.
func nd(id string, prio, budget int) Candidate {
	return Candidate{ID: core.NodeID(id), Addr: id + ":50052", Priority: prio, BudgetMiB: budget}
}

// local builds the local candidate.
func local(id string, prio, budget int) Candidate {
	return Candidate{ID: core.NodeID(id), Local: true, Priority: prio, BudgetMiB: budget}
}

func placedIDs(p core.Placement) []string {
	out := []string{}
	for _, n := range p.Nodes {
		out = append(out, string(n.ID))
	}
	return out
}

func reasonOf(t *testing.T, err error) *NoPlacement {
	t.Helper()
	var np *NoPlacement
	if !errors.As(err, &np) {
		t.Fatalf("error %v is not a *NoPlacement", err)
	}
	return np
}

// base is a request with the config defaults (max_pool 4, overhead 600,
// min_share 1024).
func base(need Need) Request {
	return Request{Need: need, MaxPool: 4, MinShareMiB: 1024, DeviceOverheadMiB: 600}
}

func with(r Request, f func(*Request)) Request { f(&r); return r }

type placeCase struct {
	name        string
	req         Request
	free        []Candidate
	want        []string // placed IDs in device order; nil means no placement
	wantReason  Reason
	wantMissing []string
	wantSplit   []int
}

func runPlaceCases(t *testing.T, cases []placeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Place(tc.req, tc.free)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("got placement %v, want reason %v", placedIDs(p), tc.wantReason)
				}
				np := reasonOf(t, err)
				if np.Reason != tc.wantReason {
					t.Fatalf("reason %v, want %v (%v)", np.Reason, tc.wantReason, err)
				}
				if tc.wantMissing != nil && !reflect.DeepEqual(np.Missing, tc.wantMissing) {
					t.Fatalf("missing %v, want %v", np.Missing, tc.wantMissing)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := placedIDs(p); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("placed %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(p.TensorSplit, tc.wantSplit) {
				t.Fatalf("tensor split %v, want %v", p.TensorSplit, tc.wantSplit)
			}
		})
	}
}

// Golden: design 7.2 "Worked checks" (host machine 8151, Nvidia laptop 7885, headroom
// 256 -> live budgets 7895 and 7629).
func TestPlaceWorkedChecks(t *testing.T) {
	omen := local("host-1", 3, 8151-256)
	alien := nd("laptop-1", 2, 7885-256)
	runPlaceCases(t, []placeCase{
		{name: "gemma 7800 on host machine alone fits", req: base(Measured(7800)), free: []Candidate{omen}, want: []string{"host-1"}},
		{name: "gemma 7800 on Nvidia laptop alone does not fit", req: base(Measured(7800)), free: []Candidate{alien}, wantReason: NoFit},
		{name: "devstral 14828 estimated on both laptops does not fit", req: base(Estimated(14828)), free: []Candidate{omen, alien}, wantReason: NoFit},
		{name: "gemma 7800 with both laptops free goes to the host machine", req: base(Measured(7800)), free: []Candidate{alien, omen}, want: []string{"host-1"}},
	})
}

func TestPlaceTierWalk(t *testing.T) {
	runPlaceCases(t, []placeCase{
		{
			name: "pool of PS5s before a single Nvidia laptop that fits",
			req:  base(Measured(12000)),
			free: []Candidate{nd("ps5-a", 1, 7000), nd("ps5-b", 1, 7000), nd("alien", 2, 12500)},
			want: []string{"ps5-a", "ps5-b"},
		},
		{
			name: "host machine reached only when tiers 1 and 2 cannot hold the model",
			req:  base(Measured(7700)),
			free: []Candidate{nd("ps5-a", 1, 3000), nd("alien", 2, 7629), local("host-1", 3, 7895)},
			want: []string{"ps5-a", "alien"},
		},
		{
			name: "host machine used when tiers 1 and 2 together are too small",
			req:  base(Measured(11000)),
			free: []Candidate{nd("ps5-a", 1, 3000), nd("alien", 2, 7629), local("host-1", 3, 7895)},
			want: []string{"alien", "host-1"},
		},
		{
			name: "single node in tier 1 beats everything",
			req:  base(Measured(5000)),
			free: []Candidate{local("host-1", 3, 7895), nd("alien", 2, 7629), nd("ps5-a", 1, 13000)},
			want: []string{"ps5-a"},
		},
		{
			name: "single: smallest budget that still fits",
			req:  base(Measured(5000)),
			free: []Candidate{nd("ps5-a", 1, 13000), nd("ps5-b", 1, 6000), nd("ps5-c", 1, 4000)},
			want: []string{"ps5-b"},
		},
		{
			name: "single: equal budgets broken by ID",
			req:  base(Measured(5000)),
			free: []Candidate{nd("ps5-c", 1, 6000), nd("ps5-b", 1, 6000)},
			want: []string{"ps5-b"},
		},
		{
			name: "pool: fewest nodes wins over larger pools",
			req:  base(Measured(10000)),
			free: []Candidate{nd("a", 1, 4000), nd("b", 1, 4000), nd("c", 1, 4000), nd("d", 1, 6000)},
			want: []string{"d", "a"},
		},
		{
			name: "pool: equal size, largest total budget",
			req:  base(Measured(9000)),
			free: []Candidate{nd("a", 1, 5000), nd("b", 1, 4500), nd("c", 1, 6000), nd("d", 1, 3000)},
			want: []string{"c", "a"},
		},
		{
			name: "pool: equal size and total broken by ID",
			req:  base(Measured(9000)),
			free: []Candidate{nd("d", 1, 5000), nd("b", 1, 5000), nd("c", 1, 5000)},
			want: []string{"b", "c"},
		},
		{
			name: "pool: a smaller pool with a lower total wins over a larger pool",
			req:  base(Measured(8000)),
			free: []Candidate{nd("a", 1, 4000), nd("b", 1, 4000), nd("c", 1, 3000), nd("d", 1, 3000), nd("e", 1, 3000)},
			want: []string{"a", "b"},
		},
		{
			name: "device order: better tier first, then larger budget, then ID",
			req:  with(base(Measured(16000)), func(r *Request) { r.MaxPool = 5 }),
			free: []Candidate{local("host-1", 3, 7895), nd("alien", 2, 2000), nd("ps5-b", 1, 2000), nd("ps5-a", 1, 2000), nd("ps5-c", 1, 3000)},
			want: []string{"ps5-c", "ps5-a", "ps5-b", "alien", "host-1"},
		},
		{
			name: "estimated rule: overhead and 5% margin applied",
			// (6000-600)+(5000-600) = 9800 >= 9333*1.05 = 9799.65
			req:  base(Estimated(9333)),
			free: []Candidate{nd("a", 1, 6000), nd("b", 1, 5000)},
			want: []string{"a", "b"},
		},
		{
			name:       "estimated rule: one MiB more does not fit",
			req:        base(Estimated(9334)),
			free:       []Candidate{nd("a", 1, 6000), nd("b", 1, 5000)},
			wantReason: NoFit,
		},
		{
			name: "measured rule: exact sum fits",
			req:  base(Measured(11000)),
			free: []Candidate{nd("a", 1, 6000), nd("b", 1, 5000)},
			want: []string{"a", "b"},
		},
	})
}

func TestPlaceLimits(t *testing.T) {
	four := []Candidate{nd("a", 1, 3000), nd("b", 1, 3000), nd("c", 1, 3000), nd("d", 1, 3000)}
	runPlaceCases(t, []placeCase{
		{name: "max_pool reached exactly", req: base(Measured(12000)), free: four, want: []string{"a", "b", "c", "d"}},
		{name: "max_pool 3 forbids the 4-node pool", req: with(base(Measured(12000)), func(r *Request) { r.MaxPool = 3 }), free: four, wantReason: NoFit},
		{name: "max_pool 1 allows single nodes only", req: with(base(Measured(5000)), func(r *Request) { r.MaxPool = 1 }), free: four, wantReason: NoFit},
		{
			name: "min_share drops a node below it",
			req:  base(Measured(6000)),
			free: []Candidate{nd("a", 1, 5000), nd("b", 1, 1023), nd("c", 2, 1024)},
			want: []string{"a", "c"},
		},
		{
			name: "min_share boundary: exactly min_share is usable",
			req:  base(Measured(1024)),
			free: []Candidate{nd("a", 1, 1024)},
			want: []string{"a"},
		},
		{
			name:       "allow_tiers excludes the host machine",
			req:        with(base(Measured(7800)), func(r *Request) { r.AllowTiers = []int{1, 2} }),
			free:       []Candidate{local("host-1", 3, 7895), nd("alien", 2, 7629)},
			wantReason: NoFit,
		},
		{
			name: "allow_tiers can skip a preferred tier",
			req:  with(base(Measured(5000)), func(r *Request) { r.AllowTiers = []int{2, 3} }),
			free: []Candidate{nd("ps5-a", 1, 13000), nd("alien", 2, 7629)},
			want: []string{"alien"},
		},
		{name: "empty input", req: base(Measured(1)), free: nil, wantReason: NoFit},
		{name: "zero need still needs one usable node", req: base(Measured(0)), free: []Candidate{nd("a", 1, 500)}, wantReason: NoFit},
	})
}

func TestPlaceForced(t *testing.T) {
	forced := func(names ...string) Request {
		r := base(Measured(999999)) // forced placement skips the fit check
		for _, n := range names {
			r.Force = append(r.Force, ForcedNode{Name: n, ID: core.NodeID(n)})
		}
		return r
	}
	free := []Candidate{nd("ps5-a", 1, 13000), nd("alien", 2, 7629), local("host-1", 3, 7895)}
	runPlaceCases(t, []placeCase{
		{name: "forced nodes present, config order kept", req: forced("alien", "ps5-a"), free: free, want: []string{"alien", "ps5-a"}},
		{name: "forced nodes in a different order", req: forced("host-1", "alien", "ps5-a"), free: free, want: []string{"host-1", "alien", "ps5-a"}},
		{
			name: "forced tensor split copied",
			req:  with(forced("ps5-a", "alien"), func(r *Request) { r.TensorSplit = []int{60, 40} }),
			free: free, want: []string{"ps5-a", "alien"}, wantSplit: []int{60, 40},
		},
		{name: "forced node missing", req: forced("ps5-a", "ps5-zz", "alien"), free: free, wantReason: ForcedUnavailable, wantMissing: []string{"ps5-zz"}},
		{
			name: "forced name that resolved to nothing",
			req: with(forced("alien"), func(r *Request) {
				r.Force = append(r.Force, ForcedNode{Name: "ghost"})
			}),
			free: free, wantReason: ForcedUnavailable, wantMissing: []string{"ghost"},
		},
		{
			name: "forced node below min_share is not usable",
			req:  forced("alien"),
			free: []Candidate{nd("alien", 2, 500)}, wantReason: ForcedUnavailable, wantMissing: []string{"alien"},
		},
		{name: "forced with empty input", req: forced("alien"), free: nil, wantReason: ForcedUnavailable, wantMissing: []string{"alien"}},
		{name: "forced ignores max_pool", req: with(forced("host-1", "alien", "ps5-a"), func(r *Request) { r.MaxPool = 1 }), free: free, want: []string{"host-1", "alien", "ps5-a"}},
	})
}

func TestPlaceCarriesNodeDetails(t *testing.T) {
	p, err := Place(base(Measured(10000)), []Candidate{local("host-1", 3, 7895), nd("ps5-a", 1, 3000)})
	if err != nil {
		t.Fatal(err)
	}
	want := []core.PlacedNode{
		{ID: "ps5-a", Addr: "ps5-a:50052", BudgetMiB: 3000},
		{ID: "host-1", Local: true, BudgetMiB: 7895},
	}
	if !reflect.DeepEqual(p.Nodes, want) {
		t.Fatalf("nodes %+v, want %+v", p.Nodes, want)
	}
}

func TestPlaceInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		free []Candidate
	}{
		{"duplicate node id", base(Measured(1)), []Candidate{nd("a", 1, 2000), nd("a", 1, 3000)}},
		{"empty node id", base(Measured(1)), []Candidate{nd("", 1, 2000)}},
		{"max_pool below 1", with(base(Measured(1)), func(r *Request) { r.MaxPool = 0 }), []Candidate{nd("a", 1, 2000)}},
		{"negative need", base(Measured(-1)), []Candidate{nd("a", 1, 2000)}},
		{"forced node twice", with(base(Measured(1)), func(r *Request) {
			r.Force = []ForcedNode{{Name: "a", ID: "a"}, {Name: "a-host", ID: "a"}}
		}), []Candidate{nd("a", 1, 2000)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Place(tc.req, tc.free)
			if err == nil {
				t.Fatal("want an error")
			}
			if np := reasonOf(t, err); np.Reason != InvalidInput || np.Detail == "" {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestNoPlacementMessages(t *testing.T) {
	_, err := Place(with(base(Measured(1)), func(r *Request) {
		r.Force = []ForcedNode{{Name: "ps5-x", ID: "x"}, {Name: "laptop-1", ID: "y"}}
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "ps5-x, laptop-1") {
		t.Fatalf("message %v should name the missing forced nodes", err)
	}
}
