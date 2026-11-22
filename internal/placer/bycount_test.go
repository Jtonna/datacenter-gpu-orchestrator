package placer

import "testing"

// A need per node count (review SB1): a set of n nodes is held to its
// ByCount entry, with that entry's margins; a count without an entry keeps
// the request's need.
func TestByCount_ASetIsHeldToItsCountsNeed(t *testing.T) {
	req := Request{Need: Measured(7600), MaxPool: 3, MarginMiB: 300}
	req.ByCount = make([]CountNeed, 4)
	req.ByCount[3] = CountNeed{Need: Measured(13000), MarginMiB: 300}
	two := []Candidate{ps("a", 5000), ps("b", 5000)}
	three := []Candidate{ps("a", 4000), ps("b", 4000), ps("c", 4000)}
	if !FitsNodes(req, two) {
		t.Fatal("2 nodes: 7600 should fit 2 x 5000")
	}
	if FitsNodes(req, three) {
		t.Fatal("3 nodes: fits on 3 x 4000, so the 3-node need 13000 was not used")
	}
	if r := req.For(3); r.Need != Measured(13000) || r.MarginMiB != 300 {
		t.Fatalf("For(3) %+v", r)
	}
	if r := req.For(5); r.Need != Measured(7600) {
		t.Fatalf("For(5) past the table %+v", r)
	}
}
