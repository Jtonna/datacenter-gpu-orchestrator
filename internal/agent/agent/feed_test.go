package agent

import (
	"testing"
	"time"
)

// The heartbeat carries the machine's own uptime (the agent's clock, which
// counts from kernel boot) and the kernel's boot id, read once per process.
func TestHeartbeatCarriesHostUptimeAndBootID(t *testing.T) {
	reads := 0
	e := newTenvWith(t, func(o *Options, d *Deps) {
		d.BootID = func() string { reads++; return "0123456789abcdef0123456789abcdef" }
	})
	hb := e.a.Heartbeat()
	if hb.HostUptimeS == nil || *hb.HostUptimeS != 3600 || hb.BootID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("first heartbeat: uptime %v boot id %q", hb.HostUptimeS, hb.BootID)
	}
	e.clock.Advance(1500 * time.Millisecond)
	hb = e.a.Heartbeat()
	if *hb.HostUptimeS != 3601.5 || reads != 1 {
		t.Fatalf("second heartbeat: uptime %v, boot id read %d times", *hb.HostUptimeS, reads)
	}
}

// A boot id that cannot be read is left out, never guessed.
func TestHeartbeatWithoutABootID(t *testing.T) {
	e := newTenvWith(t, func(o *Options, d *Deps) { d.BootID = func() string { return "" } })
	if hb := e.a.Heartbeat(); hb.BootID != "" || hb.HostUptimeS == nil {
		t.Fatalf("%+v", hb)
	}
}
