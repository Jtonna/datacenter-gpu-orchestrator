package pool

import "testing"

// A node carries its machine's system memory from the registration
// (mem_total_mib), for the reload-from-cache figure; 0 when not sent.
func TestNodeCarriesSystemMemory(t *testing.T) {
	r := newRig(t)
	req := regReq("02:00:00:00:00:70")
	req.MemTotalMiB = 15000
	r.p.Register(req)
	r.p.Heartbeat("02:00:00:00:00:70", hbBody("s1", "", true, "true"))
	if n, ok := r.node("02:00:00:00:00:70"); !ok || n.SystemMiB != 15000 {
		t.Fatalf("node %+v, want system memory 15000 MiB", n)
	}
	r.p.Register(regReq("02:00:00:00:00:71"))
	r.p.Heartbeat("02:00:00:00:00:71", hbBody("s1", "", true, "true"))
	if n, ok := r.node("02:00:00:00:00:71"); !ok || n.SystemMiB != 0 {
		t.Fatalf("node %+v, want system memory unknown", n)
	}
}
