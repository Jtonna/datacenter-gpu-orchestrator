package placer

import (
	"testing"
)

// The memory reservation rule in the placer: the fill reserve is taken off
// every node, and a configured per-node margin replaces the skew margin on
// shared-memory nodes. Zero values keep the old rules (the other tests).

func TestReserve_FitTable(t *testing.T) {
	three := func(b int) []Candidate { return []Candidate{ps("a", b), ps("b", b), ps("c", b)} }
	tests := []struct {
		name  string
		req   Request
		nodes []Candidate
		fits  bool
	}{
		// 27000 measured over three of 10000: 9000 a node.
		{"skew 15%: 10350 > 10000", skewReq(Measured(27000)), three(10000), false},
		{"fixed 300: 9300 <= 10000", with(skewReq(Measured(27000)), func(r *Request) { r.MarginMiB = 300 }), three(10000), true},
		{"fixed 1001: 10001 > 10000", with(skewReq(Measured(27000)), func(r *Request) { r.MarginMiB = 1001 }), three(10000), false},
		{"fixed 300 + reserve 700: 10000", with(skewReq(Measured(27000)), func(r *Request) { r.MarginMiB, r.PerNodeReserveMiB = 300, 700 }), three(10000), true},
		{"fixed 300 + reserve 701", with(skewReq(Measured(27000)), func(r *Request) { r.MarginMiB, r.PerNodeReserveMiB = 300, 701 }), three(10000), false},
		// The reserve alone, on the skew rule: 10350 + 1 > 10000 even before.
		{"reserve on dedicated nodes: sum 30000 - 3x1000 >= 27000", with(base(Measured(27000)), func(r *Request) { r.PerNodeReserveMiB = 1000 }),
			[]Candidate{nd("a", 1, 10000), nd("b", 1, 10000), nd("c", 1, 10000)}, true},
		{"reserve on dedicated nodes: 3x1001 too much", with(base(Measured(27000)), func(r *Request) { r.PerNodeReserveMiB = 1001 }),
			[]Candidate{nd("a", 1, 10000), nd("b", 1, 10000), nd("c", 1, 10000)}, false},
		// An estimate: 20000 x 1.05 = 21000 over three: 7000 + overhead 600 = 7600 a node.
		{"percent 15: 7600 + 1140 <= 10000", with(skewReq(Estimated(20000)), func(r *Request) { r.MarginPermille = 150 }), three(10000), true},
		{"percent 15: 7600 + 1140 > 8700", with(skewReq(Estimated(20000)), func(r *Request) { r.MarginPermille = 150 }), three(8700), false},
		{"percent 15 min 2400: 7600 + 2400 = 10000", with(skewReq(Estimated(20000)), func(r *Request) { r.MarginPermille, r.MarginMinMiB = 150, 2400 }), three(10000), true},
		{"percent 15 min 2401", with(skewReq(Estimated(20000)), func(r *Request) { r.MarginPermille, r.MarginMinMiB = 150, 2401 }), three(10000), false},
		// A configured margin holds a single shared-memory node too.
		{"single node, fixed 300: 9800 + 300 > 10000", with(skewReq(Measured(9800)), func(r *Request) { r.MarginMiB, r.MaxPool = 300, 1 }), three(10000)[:1], false},
		{"single node, skew rule: no margin", with(skewReq(Measured(9800)), func(r *Request) { r.MaxPool = 1 }), three(10000)[:1], true},
		{"single node, reserve 201: 9800 + 201", with(skewReq(Measured(9800)), func(r *Request) { r.PerNodeReserveMiB, r.MaxPool = 201, 1 }), three(10000)[:1], false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Place(tc.req, tc.nodes)
			if (err == nil) != tc.fits {
				t.Fatalf("fits %v, want %v (err %v)", err == nil, tc.fits, err)
			}
			if got := FitsNodes(tc.req, tc.nodes); got != tc.fits && len(tc.nodes) <= tc.req.MaxPool {
				t.Fatalf("FitsNodes %v, want %v", got, tc.fits)
			}
		})
	}
}

func TestReserve_NodeMarginMiB(t *testing.T) {
	for _, tc := range []struct {
		req   Request
		share int
		want  int
	}{
		{Request{}, 9000, 0},
		{Request{MarginMiB: 300}, 9000, 300},
		{Request{MarginPermille: 150}, 9000, 1350},
		{Request{MarginPermille: 150}, 1, 1},
		{Request{MarginPermille: 150, MarginMinMiB: 2000}, 9000, 2000},
		{Request{MarginMiB: 300, MarginPermille: 150}, 9000, 300},
	} {
		if got := tc.req.NodeMarginMiB(tc.share); got != tc.want {
			t.Errorf("%+v share %d: %d, want %d", tc.req, tc.share, got, tc.want)
		}
	}
}

// N4: the capacity refusal's required figure includes the fill reserve and
// the fixed margin of every node it counts, as the fit rule applies them.
func TestReserve_RequiredIncludesTheReserve(t *testing.T) {
	req := with(skewReq(Measured(40000)), func(r *Request) { r.MarginMiB, r.PerNodeReserveMiB = 300, 512 })
	req.MaxPool = 3
	err := CheckCapacity(req, []Candidate{ps("a", 10000), ps("b", 10000), ps("c", 10000)})
	np, ok := err.(*NoPlacement)
	if !ok || np.Reason != ExceedsCapacity {
		t.Fatalf("err %v", err)
	}
	if want := 40000 + 3*(512+300); np.RequiredMiB != want {
		t.Fatalf("required %d, want %d", np.RequiredMiB, want)
	}
}
