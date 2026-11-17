package placer

import (
	"reflect"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

func idle(name string, load, idleFor time.Duration, nodes ...Candidate) Evictable {
	return Evictable{Instance: name, Nodes: nodes, LoadDuration: load, IdleFor: idleFor}
}

func busy(name string, load time.Duration, nodes ...Candidate) Evictable {
	return Evictable{Instance: name, Nodes: nodes, Busy: true, LoadDuration: load}
}

func TestPlan(t *testing.T) {
	m := time.Minute
	cases := []struct {
		name        string
		req         Request
		free        []Candidate
		ev          []Evictable
		want        []string
		wantVictims []string
		wantReason  Reason
	}{
		{
			name: "fewest victims wins over a larger total budget",
			req:  base(Measured(20000)),
			ev: []Evictable{
				idle("i1", m, m, nd("a", 1, 10000), nd("b", 1, 10000)),
				idle("i2", m, m, nd("c", 1, 11000)),
				idle("i3", m, m, nd("d", 1, 11000)),
			},
			want: []string{"a", "b"}, wantVictims: []string{"i1"},
		},
		{
			name: "idle victim preferred over busy, despite the smaller budget",
			req:  base(Measured(12000)),
			ev: []Evictable{
				busy("i1", m, nd("p1", 1, 13000)),
				idle("i2", m, m, nd("p2", 1, 14000)),
			},
			want: []string{"p2"}, wantVictims: []string{"i2"},
		},
		{
			name: "shortest load_duration",
			req:  base(Measured(12000)),
			ev: []Evictable{
				idle("i1", 10*m, m, nd("p1", 1, 13000)),
				idle("i2", 2*m, m, nd("p2", 1, 14000)),
			},
			want: []string{"p2"}, wantVictims: []string{"i2"},
		},
		{
			name: "longest idle",
			req:  base(Measured(12000)),
			ev: []Evictable{
				idle("i1", m, 5*m, nd("p1", 1, 13000)),
				idle("i2", m, 30*m, nd("p2", 1, 14000)),
			},
			want: []string{"p2"}, wantVictims: []string{"i2"},
		},
		{
			name: "all victim criteria equal: smallest budget",
			req:  base(Measured(12000)),
			ev: []Evictable{
				idle("i1", m, m, nd("p1", 1, 14000)),
				idle("i2", m, m, nd("p2", 1, 13000)),
			},
			want: []string{"p2"}, wantVictims: []string{"i2"},
		},
		{
			name: "busy count decides between pools with equal victim counts",
			req:  base(Measured(20000)),
			ev: []Evictable{
				busy("i1", m, nd("a", 1, 10000)),
				idle("i2", m, m, nd("b", 1, 10000)),
				idle("i3", m, m, nd("c", 1, 10000)),
			},
			want: []string{"b", "c"}, wantVictims: []string{"i2", "i3"},
		},
		{
			name: "instance spanning several nodes: one node claimed, whole instance a victim",
			req:  base(Measured(12000)),
			ev:   []Evictable{idle("i1", m, m, nd("p1", 1, 13000), nd("p2", 1, 13000))},
			want: []string{"p1"}, wantVictims: []string{"i1"},
		},
		{
			name: "free nodes combined with victim nodes",
			req:  base(Measured(12000)),
			free: []Candidate{nd("f", 1, 6000)},
			ev: []Evictable{
				idle("i1", 5*m, m, nd("p", 1, 7000)),
				idle("i2", m, m, nd("q", 1, 7000)),
			},
			want: []string{"q", "f"}, wantVictims: []string{"i2"},
		},
		{
			name: "tier rules before victims: two-victim PS5 pool over one-victim pool with the Alienware",
			req:  base(Measured(20000)),
			free: []Candidate{nd("p3", 1, 8000)},
			ev: []Evictable{
				idle("i1", m, m, nd("p1", 1, 10000)),
				idle("i2", m, m, nd("p2", 1, 10000)),
				idle("i3", m, m, nd("alien", 2, 12000)),
			},
			want: []string{"p1", "p2"}, wantVictims: []string{"i1", "i2"},
		},
		{
			name: "pool size before victims: one 2-victim pair over a 1-victim triple",
			req:  base(Measured(20000)),
			ev: []Evictable{
				idle("i1", m, m, nd("a", 1, 7000), nd("b", 1, 7000), nd("c", 1, 7000)),
				idle("i2", m, m, nd("d", 1, 10000)),
				idle("i3", m, m, nd("e", 1, 10000)),
			},
			want: []string{"d", "e"}, wantVictims: []string{"i2", "i3"},
		},
		{
			name: "no evictables behaves like Place",
			req:  base(Measured(9000)),
			free: []Candidate{nd("a", 1, 5000), nd("b", 1, 4500), nd("c", 1, 6000)},
			want: []string{"c", "a"}, wantVictims: nil,
		},
		{
			name: "forced: victims are the holders of the forced nodes",
			req: with(base(Measured(1)), func(r *Request) {
				r.Force = []ForcedNode{{Name: "p1", ID: "p1"}, {Name: "f", ID: "f"}}
			}),
			free: []Candidate{nd("f", 1, 6000)},
			ev:   []Evictable{idle("i1", m, m, nd("p1", 1, 7000), nd("p2", 1, 7000)), idle("i2", m, m, nd("q", 1, 7000))},
			want: []string{"p1", "f"}, wantVictims: []string{"i1"},
		},
		{
			name:       "no plan possible",
			req:        base(Measured(50000)),
			free:       []Candidate{nd("f", 1, 6000)},
			ev:         []Evictable{idle("i1", m, m, nd("p1", 1, 13000))},
			wantReason: NoFit,
		},
		{
			name:       "allow_tiers and min_share apply to held nodes",
			req:        with(base(Measured(5000)), func(r *Request) { r.AllowTiers = []int{1} }),
			ev:         []Evictable{idle("i1", m, m, nd("alien", 2, 7629), nd("p1", 1, 1000))},
			wantReason: NoFit,
		},
		{
			name:       "node both free and held is invalid",
			req:        base(Measured(1)),
			free:       []Candidate{nd("a", 1, 6000)},
			ev:         []Evictable{idle("i1", m, m, nd("a", 1, 7000))},
			wantReason: InvalidInput,
		},
		{
			name:       "instance listed twice is invalid",
			req:        base(Measured(1)),
			ev:         []Evictable{idle("i1", m, m, nd("a", 1, 7000)), idle("i1", m, m, nd("b", 1, 7000))},
			wantReason: InvalidInput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Plan(tc.req, tc.free, tc.ev)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("got plan %v victims %v, want reason %v", placedIDs(res.Placement), res.Victims, tc.wantReason)
				}
				if np := reasonOf(t, err); np.Reason != tc.wantReason {
					t.Fatalf("reason %v, want %v", np.Reason, tc.wantReason)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := placedIDs(res.Placement); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("placed %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(res.Victims, tc.wantVictims) {
				t.Fatalf("victims %v, want %v", res.Victims, tc.wantVictims)
			}
		})
	}
}

func TestCheckCapacity(t *testing.T) {
	omen := local("omen", 3, 8151-256)
	alien := nd("alienware", 2, 7885-256)
	three := []Candidate{nd("a", 1, 5000), nd("b", 1, 4000), nd("c", 1, 3000)}
	cases := []struct {
		name     string
		req      Request
		eligible []Candidate
		wantOK   bool
		want     NoPlacement // figures when not OK
	}{
		{
			name: "worked check: Devstral 14K on both laptops exceeds capacity",
			req:  base(Estimated(14828)), eligible: []Candidate{omen, alien},
			want: NoPlacement{Reason: ExceedsCapacity, NeedMiB: 14828, RequiredMiB: 15570, CapacityMiB: 14324},
		},
		{name: "worked check: gemma 7800 within capacity", req: base(Measured(7800)), eligible: []Candidate{alien, omen}, wantOK: true},
		{name: "best max_pool of them: fits", req: with(base(Measured(9000)), func(r *Request) { r.MaxPool = 2 }), eligible: three, wantOK: true},
		{
			name: "best max_pool of them: one more MiB exceeds",
			req:  with(base(Measured(9001)), func(r *Request) { r.MaxPool = 2 }), eligible: three,
			want: NoPlacement{Reason: ExceedsCapacity, NeedMiB: 9001, RequiredMiB: 9001, CapacityMiB: 9000},
		},
		{
			name: "allow_tiers applied",
			req:  with(base(Measured(7800)), func(r *Request) { r.AllowTiers = []int{1, 2} }), eligible: []Candidate{omen, alien},
			want: NoPlacement{Reason: ExceedsCapacity, NeedMiB: 7800, RequiredMiB: 7800, CapacityMiB: 7629},
		},
		{
			name: "min_share applied",
			req:  base(Measured(8000)), eligible: []Candidate{nd("a", 1, 7000), nd("b", 1, 1000)},
			want: NoPlacement{Reason: ExceedsCapacity, NeedMiB: 8000, RequiredMiB: 8000, CapacityMiB: 7000},
		},
		{
			name: "empty input",
			req:  base(Measured(1)), eligible: nil,
			want: NoPlacement{Reason: ExceedsCapacity, NeedMiB: 1, RequiredMiB: 1, CapacityMiB: 0},
		},
		{
			name: "forced request always passes",
			req: with(base(Measured(99999)), func(r *Request) {
				r.Force = []ForcedNode{{Name: "x", ID: "x"}}
			}),
			eligible: nil, wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckCapacity(tc.req, tc.eligible)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want ExceedsCapacity")
			}
			if np := reasonOf(t, err); !reflect.DeepEqual(*np, tc.want) {
				t.Fatalf("got %+v, want %+v", *np, tc.want)
			}
		})
	}
}

func TestFits(t *testing.T) {
	cases := []struct {
		name    string
		req     Request
		budgets []int
		want    bool
	}{
		{"measured fits", base(Measured(7800)), []int{7895}, true},
		{"measured does not fit", base(Measured(7800)), []int{7629}, false},
		{"estimated uses overhead and margin", base(Estimated(14828)), []int{7895, 7629}, false},
		{"forced always fits", with(base(Measured(99999)), func(r *Request) { r.Force = []ForcedNode{{Name: "x", ID: "x"}} }), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Fits(tc.req, tc.budgets); got != tc.want {
				t.Fatalf("Fits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFromNode(t *testing.T) {
	n := core.Node{ID: "x", Addr: "10.0.0.2:50052", Priority: 1, FreeMiB: 14000, HeadroomMiB: 1024}
	got := FromNode(n, n.LiveBudgetMiB())
	want := Candidate{ID: "x", Addr: "10.0.0.2:50052", Priority: 1, BudgetMiB: 12976}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
