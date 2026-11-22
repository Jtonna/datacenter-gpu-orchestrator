package pool

import (
	"context"
	"strings"
	"testing"
)

// The agent's host uptime and boot id reach the node facts; an agent that
// sends neither (or values that cannot be right) has neither, and its
// heartbeat is still taken.
func TestFactsCarryHostUptimeAndBootID(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq(upID))
	body := strings.Replace(string(hbBody("s1", "", true, "true")), `"seq":1,`, `"seq":1,"host_uptime_s":812.5,"boot_id":"0123456789abcdef0123456789abcdef",`, 1)
	if res := r.p.Heartbeat(upID, []byte(body)); res.Status != 200 {
		t.Fatalf("heartbeat %d %s", res.Status, res.Err)
	}
	f := r.fact(upID)
	if f.HostUptimeS == nil || *f.HostUptimeS != 812.5 || f.BootID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("facts %+v", f)
	}
	// An older agent: neither field.
	r.heartbeat(upID)
	if f := r.fact(upID); f.HostUptimeS != nil || f.BootID != "" {
		t.Fatalf("older agent: %+v", f)
	}
	// Values of the wrong type or out of bounds are dropped, the heartbeat is kept.
	for _, bad := range []string{`"host_uptime_s":"long","boot_id":7,`, `"host_uptime_s":-1,"boot_id":"not a boot id",`} {
		body := strings.Replace(string(hbBody("s1", "", true, "true")), `"seq":1,`, `"seq":1,`+bad, 1)
		if res := r.p.Heartbeat(upID, []byte(body)); res.Status != 200 {
			t.Fatalf("%s: heartbeat %d %s", bad, res.Status, res.Err)
		}
		if f := r.fact(upID); f.HostUptimeS != nil || f.BootID != "" || !f.ProgramsReady {
			t.Fatalf("%s: facts %+v", bad, f)
		}
	}
}

// The local node of the feed: only while the view has one, with the
// machine's own uptime and boot id.
func TestLocalFacts(t *testing.T) {
	r := newRig(t)
	r.p.o.HostInfo = func() (float64, string, bool) { return 99.25, "fedcba9876543210fedcba9876543210", true }
	if _, ok := r.p.Local(); ok {
		t.Fatal("a local node without a local probe")
	}
	r.cfg.LocalProbeCmd = "probe"
	r.p.o.RunProbe = func(context.Context, string) ([]byte, error) { return []byte("24576, 1024\n"), nil }
	r.p.probeOnce(context.Background())
	l, ok := r.p.Local()
	if !ok || l.ID != "host-1" || l.Name != "host-1" || !l.Local || l.State != StateActive || l.Release != "0.1.0" ||
		l.HostUptimeS == nil || *l.HostUptimeS != 99.25 || l.BootID != "fedcba9876543210fedcba9876543210" {
		t.Fatalf("local %+v %v", l, ok)
	}
	r.p.o.HostInfo = func() (float64, string, bool) { return 0, "", false }
	if l, _ := r.p.Local(); l.HostUptimeS != nil || l.BootID != "" {
		t.Fatalf("unreadable host info: %+v", l)
	}
}
