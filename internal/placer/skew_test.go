package placer

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Per-node fit on shared-memory nodes: a split model does not land evenly
// (11% over an even share was measured), so each shared-memory node of a
// multi-node placement must hold need x fraction x (1 + split_skew_margin).

func ps(id string, budget int) Candidate {
	c := nd(id, 1, budget)
	c.Shared = true
	return c
}

func skewReq(need Need) Request { return with(base(need), func(r *Request) { r.SkewPermille = 150 }) }

func TestSkew_SharedPoolNeedsTheMarginOnEveryNode(t *testing.T) {
	free := []Candidate{ps("a", 12400), ps("b", 12400), ps("c", 12400)}
	// 27793 MiB over three PS5s: 10653 per node with the margin, under 12400.
	if p, err := Place(skewReq(Measured(27793)), free); err != nil || len(p.Nodes) != 3 {
		t.Fatalf("27793 on three: %v %v", p, err)
	}
	// The summed budgets (37200) hold 33000, but 33000 x 1.15 = 37950 does not.
	_, err := Place(skewReq(Measured(33000)), free)
	if np := reasonOf(t, err); np.Reason != NoFit {
		t.Fatalf("reason %v, want NoFit", np.Reason)
	}
	// Without the rule the same placement is accepted: the rule is what refuses it.
	if _, err := Place(base(Measured(33000)), free); err != nil {
		t.Fatalf("no skew: %v", err)
	}
}

func TestSkew_AddsANodeInsteadOfRefusing(t *testing.T) {
	free := []Candidate{ps("a", 12400), ps("b", 12400), ps("c", 12400), ps("d", 12400)}
	p, err := Place(skewReq(Measured(33000)), free)
	if err != nil || len(p.Nodes) != 4 {
		t.Fatalf("want a 4-node placement, got %v %v", placedIDs(p), err)
	}
	// With max_pool 3 nothing fits.
	_, err = Place(with(skewReq(Measured(33000)), func(r *Request) { r.MaxPool = 3 }), free)
	reasonOf(t, err)
}

func TestSkew_TwoWayRealCases(t *testing.T) {
	two := []Candidate{ps("a", 12400), ps("b", 12400)}
	// 14248 MiB 50/50: 8185 each with the margin: fits.
	if _, err := Place(skewReq(Measured(14248)), two); err != nil {
		t.Fatalf("14248: %v", err)
	}
	// 16442 MiB 50/50: 9454 each: fits. The summed rule needs 16442 of 24800.
	if _, err := Place(skewReq(Measured(16442)), two); err != nil {
		t.Fatalf("16442: %v", err)
	}
}

func TestSkew_SingleNodeTakesNoMargin(t *testing.T) {
	if _, err := Place(skewReq(Measured(12400)), []Candidate{ps("a", 12400)}); err != nil {
		t.Fatalf("exact fit on one node must stay accepted: %v", err)
	}
}

func TestSkew_DedicatedNodesTakeNoMargin(t *testing.T) {
	free := []Candidate{nd("a", 1, 12400), nd("b", 1, 12400), nd("c", 1, 12400)}
	if _, err := Place(skewReq(Measured(33000)), free); err != nil {
		t.Fatalf("dedicated VRAM nodes: %v", err)
	}
}

func TestSkew_MixedSetChecksOnlyTheSharedNode(t *testing.T) {
	// One shared node of 6000 and a big dedicated node of 20000: the shared
	// node's fraction is 6000/26000, its share 20000 x (6000/26000) x 1.15 =
	// 5307 <= 6000: fits. At need 24000 its share is 6369 > 6000: the
	// summed budgets hold it (26000) but the shared node would be overfilled.
	free := []Candidate{ps("s", 6000), nd("d", 2, 20000)}
	if _, err := Place(skewReq(Measured(20000)), free); err != nil {
		t.Fatalf("need 20000: %v", err)
	}
	// 24000 fits the dedicated node alone, so it is placed there.
	p, err := Place(skewReq(Measured(24000)), free)
	if err == nil && len(p.Nodes) != 1 {
		t.Fatalf("placed %v: the shared node would be overfilled", placedIDs(p))
	}
	// Both nodes are needed at 26000 (the dedicated one alone is too small):
	// the shared node's share is 6900 > 6000.
	if _, err := Place(skewReq(Measured(25000)), free); err == nil {
		t.Fatalf("25000 over a 6000 shared and a 20000 dedicated node must be refused")
	}
}

func TestSkew_EstimatedNeedUsesTheEstimateMarginAndOverhead(t *testing.T) {
	free := []Candidate{ps("a", 12400), ps("b", 12400)}
	req := with(skewReq(EstimatedMargin(20000, 100)), func(r *Request) { r.DeviceOverheadMiB = 600 })
	// need 20000 x 1.10 = 22000; per node 11000 x 1.15 = 12650 + 600 > 12400.
	if _, err := Place(req, free); err == nil {
		t.Fatalf("expected a refusal")
	}
	req.Need = EstimatedMargin(17000, 100) // 18700: 9350 x 1.15 = 10752 + 600 <= 12400
	if _, err := Place(req, free); err != nil {
		t.Fatalf("17000: %v", err)
	}
}

func TestSkew_CapacityCheckIncludesTheMargin(t *testing.T) {
	cands := []Candidate{ps("a", 12400), ps("b", 12400)}
	err := CheckCapacity(skewReq(Measured(24000)), cands)
	np := reasonOf(t, err)
	if np.Reason != ExceedsCapacity || np.RequiredMiB != 27600 || np.CapacityMiB != 24800 {
		t.Fatalf("%+v", np)
	}
	// One node of 12400 holds 12400 with no margin: not a capacity refusal.
	if err := CheckCapacity(skewReq(Measured(12400)), []Candidate{ps("a", 12400), ps("b", 100)}); err != nil {
		t.Fatalf("single node fits: %v", err)
	}
}

func TestSkew_FitsNodesMatchesPlace(t *testing.T) {
	req := skewReq(Measured(33000))
	three := []Candidate{ps("a", 12400), ps("b", 12400), ps("c", 12400)}
	if FitsNodes(req, three) {
		t.Fatal("33000 on three must not fit")
	}
	if !FitsNodes(skewReq(Measured(27793)), three) {
		t.Fatal("27793 on three must fit")
	}
}

// Property: whatever the placer returns, no shared-memory node's expected
// share (need x budget/sum x (1+skew)) exceeds its budget in a multi-node
// placement, and a refusal is never wrong in the other direction for the
// simple all-shared case.
func TestSkew_NoPlacementOverfillsASharedNode(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 3000; i++ {
		n := 1 + r.Intn(6)
		var free []Candidate
		for j := 0; j < n; j++ {
			c := nd(fmt.Sprintf("n%d", j), 1+r.Intn(3), 1024+r.Intn(14000))
			c.Shared = r.Intn(4) != 0
			free = append(free, c)
		}
		need := Measured(1000 + r.Intn(60000))
		if r.Intn(2) == 0 {
			need = EstimatedMargin(1000+r.Intn(60000), r.Intn(200))
		}
		req := with(base(need), func(q *Request) { q.SkewPermille = r.Intn(400); q.MaxPool = 1 + r.Intn(5) })
		p, err := Place(req, free)
		if err != nil {
			continue
		}
		if len(p.Nodes) < 2 {
			continue
		}
		shared := map[core.NodeID]bool{}
		for _, c := range free {
			shared[c.ID] = c.Shared
		}
		var sum float64
		for _, pn := range p.Nodes {
			sum += float64(pn.BudgetMiB)
		}
		for _, pn := range p.Nodes {
			if !shared[pn.ID] {
				continue
			}
			share := float64(req.Need.WithMargin()) * float64(pn.BudgetMiB) / sum * (1 + float64(req.SkewPermille)/1000)
			if !req.Need.Measured {
				share += float64(req.DeviceOverheadMiB)
			}
			if share > float64(pn.BudgetMiB)+1e-6 {
				t.Fatalf("case %d: node %s share %.0f > budget %d (need %+v skew %d)", i, pn.ID, share, pn.BudgetMiB, req.Need, req.SkewPermille)
			}
		}
	}
}
