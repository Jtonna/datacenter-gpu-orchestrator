package placer

import (
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// A per-device cost measured on one node (NodeDeviceMiB) replaces
// DeviceOverheadMiB on that node only, for that count only.
func TestNodeDeviceMiB_OnThatNodeAndCountOnly(t *testing.T) {
	need := EstimatedMargin(5000, 0)
	free := []Candidate{nd("a", 1, 5700), nd("b", 1, 5500)}
	req := base(need)
	// With 600 on every node a holds 5000 (5100) and b does not (4900).
	if pl, err := Place(req, free); err != nil || !reflect.DeepEqual(placedIDs(pl), []string{"a"}) {
		t.Fatalf("setting: %v %v", placedIDs(pl), err)
	}
	// b measured 200 on one device: b alone now holds it (5300) and is the
	// smaller budget that fits (the single-node order prefers it).
	req.NodeDeviceMiB = func(id core.NodeID, n int) int {
		if id == "b" && n == 1 {
			return 200
		}
		return 600
	}
	if pl, err := Place(req, free); err != nil || !reflect.DeepEqual(placedIDs(pl), []string{"b"}) {
		t.Fatalf("measured on b: %v %v", placedIDs(pl), err)
	}
	// The bound: only b's low figure makes the need fit at all.
	tight := base(EstimatedMargin(5250, 0))
	tight.MaxPool = 1
	if pl, err := Place(tight, free); err == nil {
		t.Fatalf("setting placed %v", placedIDs(pl))
	}
	tight.NodeDeviceMiB = req.NodeDeviceMiB
	if pl, err := Place(tight, free); err != nil || !reflect.DeepEqual(placedIDs(pl), []string{"b"}) {
		t.Fatalf("tight: %v %v", placedIDs(pl), err)
	}
	if err := CheckCapacity(tight, free); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	// A measured need takes no per-device cost at all.
	m := base(Measured(5600))
	m.NodeDeviceMiB = req.NodeDeviceMiB
	if pl, err := Place(m, free); err != nil || !reflect.DeepEqual(placedIDs(pl), []string{"a"}) {
		t.Fatalf("measured need: %v %v", placedIDs(pl), err)
	}
}

// A NodeDeviceMiB that gives every node the setting places exactly as none.
func TestNodeDeviceMiB_TheSettingEverywhereChangesNothing(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 3000; i++ {
		var free []Candidate
		for j := 0; j < 1+r.IntN(6); j++ {
			free = append(free, nd(string(rune('a'+j)), 1+r.IntN(2), 1000+r.IntN(9000)))
			free[j].Shared = r.IntN(2) == 0
		}
		req := Request{Need: EstimatedMargin(1000+r.IntN(20000), r.IntN(100)), MaxPool: 1 + r.IntN(5), MinShareMiB: r.IntN(1500),
			DeviceOverheadMiB: r.IntN(1000), SkewPermille: r.IntN(300), PerNodeReserveMiB: r.IntN(400)}
		with := req
		with.NodeDeviceMiB = func(core.NodeID, int) int { return req.DeviceOverheadMiB }
		p1, e1 := Place(req, free)
		p2, e2 := Place(with, free)
		if !reflect.DeepEqual(p1, p2) || (e1 == nil) != (e2 == nil) {
			t.Fatalf("case %d: %v %v | %v %v", i, p1, e1, p2, e2)
		}
		if c1, c2 := CheckCapacity(req, free), CheckCapacity(with, free); !reflect.DeepEqual(c1, c2) {
			t.Fatalf("capacity %d: %v | %v", i, c1, c2)
		}
	}
}
