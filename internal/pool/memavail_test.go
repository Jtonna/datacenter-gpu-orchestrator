package pool

import "testing"

// A shared-memory node carries the MemAvailable its agent reported, before
// the floor is taken off (FreeMiB has it off), and its average: plans show
// both so nobody takes the floor off twice.
func TestNodeCarriesMemAvailable(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq(upID))
	r.heartbeat(upID) // mem_available_mib 9000, a shared Vulkan device
	n, ok := r.node(upID)
	if !ok || !n.SharedMemory || n.MemAvailableMiB != 9000 || n.FreeMiB != 9000-n.WatchdogFloorMiB {
		t.Fatalf("node %+v", n)
	}
	if !n.MemAvailableAvgKnown || n.MemAvailableAvgMiB != 9000 {
		t.Fatalf("average %d %v", n.MemAvailableAvgMiB, n.MemAvailableAvgKnown)
	}
}
