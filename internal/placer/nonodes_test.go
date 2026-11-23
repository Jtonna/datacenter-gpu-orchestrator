package placer

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// No candidate node at all (every node excluded, or no node in the profile's
// tiers) with every margin setting and both kinds of need: no entry point
// panics. CheckCapacity keeps the reason it gives an empty pool without
// margins, ExceedsCapacity with a capacity of 0; Place and Plan say NoFit; a
// forced request names its nodes as unavailable; Fits and FitsNodes say no.
// (The 0.1.7 readiness crash: a divide by zero in CheckCapacity's
// configured-margin branch.)
func TestNoCandidateNodes(t *testing.T) {
	needs := map[string]Need{"measured": Measured(7600), "estimated": Estimated(7600), "estimated 10%": EstimatedMargin(7600, 100)}
	margins := map[string]func(*Request){
		"none":         func(*Request) {},
		"margin mib":   func(r *Request) { r.MarginMiB = 300 },
		"margin pct":   func(r *Request) { r.MarginPermille, r.MarginMinMiB = 100, 300 },
		"skew":         func(r *Request) { r.SkewPermille = 50 },
		"fill reserve": func(r *Request) { r.PerNodeReserveMiB = 500; r.MarginMiB = 300 },
		"by count": func(r *Request) {
			r.MarginMiB = 300
			r.ByCount = []CountNeed{{}, {Need: Measured(8000), MarginMiB: 300}, {Need: Measured(8400), MarginMiB: 300}}
		},
	}
	for nn, need := range needs {
		for mn, m := range margins {
			req := Request{Need: need, MaxPool: 4, MinShareMiB: 1024, DeviceOverheadMiB: 600}
			m(&req)
			name := nn + "/" + mn
			call := func(what string, f func()) {
				t.Helper()
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s: %s panicked: %v", name, what, r)
					}
				}()
				f()
			}
			for _, cands := range [][]Candidate{nil, {}} {
				call("CheckCapacity", func() {
					np := reasonOf(t, CheckCapacity(req, cands))
					if np.Reason != ExceedsCapacity || np.CapacityMiB != 0 || np.RequiredMiB < need.MiB {
						t.Fatalf("%s: CheckCapacity = %+v", name, np)
					}
				})
				call("Place", func() {
					_, err := Place(req, cands)
					if np := reasonOf(t, err); np.Reason != NoFit {
						t.Fatalf("%s: Place = %+v", name, np)
					}
				})
				for _, ev := range [][]Evictable{nil, {{Instance: "v"}}} {
					call(fmt.Sprintf("Plan with %d evictable", len(ev)), func() {
						_, err := Plan(req, cands, ev)
						if np := reasonOf(t, err); np.Reason != NoFit {
							t.Fatalf("%s: Plan = %+v", name, np)
						}
					})
				}
				call("Fits", func() {
					if Fits(req, nil) || Fits(req, []int{}) {
						t.Fatalf("%s: Fits with no budgets", name)
					}
				})
				call("FitsNodes", func() {
					if FitsNodes(req, cands) {
						t.Fatalf("%s: FitsNodes with no nodes", name)
					}
				})
				forced := req
				forced.Force = []ForcedNode{{Name: "a", ID: core.NodeID("a")}}
				call("forced", func() {
					if err := CheckCapacity(forced, cands); err != nil {
						t.Fatalf("%s: forced CheckCapacity = %v", name, err)
					}
					_, err := Place(forced, cands)
					if np := reasonOf(t, err); np.Reason != ForcedUnavailable || len(np.Missing) != 1 {
						t.Fatalf("%s: forced Place = %+v", name, np)
					}
					_, err = Plan(forced, cands, nil)
					if np := reasonOf(t, err); np.Reason != ForcedUnavailable {
						t.Fatalf("%s: forced Plan = %+v", name, np)
					}
				})
			}
		}
	}
}

// Property: candidates offered but every one filtered out (allow_tiers or
// min_share), over random needs and margin settings: no entry point panics,
// and nothing is placed.
func TestNoCandidateLeftAfterFilters_Property(t *testing.T) {
	r := rand.New(rand.NewPCG(17, 7))
	for i := 0; i < 5000; i++ {
		need := Measured(1 + r.IntN(40000))
		if r.IntN(2) == 0 {
			need = EstimatedMargin(1+r.IntN(40000), r.IntN(300))
		}
		req := Request{Need: need, MaxPool: 1 + r.IntN(6), MinShareMiB: 1024, DeviceOverheadMiB: r.IntN(1000),
			SkewPermille: r.IntN(200), PerNodeReserveMiB: r.IntN(800)}
		switch r.IntN(3) {
		case 1:
			req.MarginMiB = 1 + r.IntN(600)
		case 2:
			req.MarginPermille, req.MarginMinMiB = 1+r.IntN(200), r.IntN(600)
		}
		var cands []Candidate
		for k := r.IntN(5); k > 0; k-- {
			c := Candidate{ID: core.NodeID(fmt.Sprintf("n%d", k)), BudgetMiB: 1 + r.IntN(20000), Priority: 2, Shared: r.IntN(2) == 0}
			if r.IntN(2) == 0 {
				c.BudgetMiB = r.IntN(req.MinShareMiB) // below min_share
			}
			cands = append(cands, c)
		}
		req.AllowTiers = []int{1} // no candidate is in tier 1
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("case %d %+v with %d filtered candidates: panic %v", i, req, len(cands), p)
				}
			}()
			if err := CheckCapacity(req, cands); err == nil {
				t.Fatalf("case %d: capacity with no candidate left", i)
			}
			if _, err := Place(req, cands); err == nil {
				t.Fatalf("case %d: placed with no candidate left", i)
			}
			if _, err := Plan(req, cands, nil); err == nil {
				t.Fatalf("case %d: planned with no candidate left", i)
			}
		}()
	}
}
