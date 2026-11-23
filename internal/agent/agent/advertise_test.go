package agent

import (
	"bytes"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncLog) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// --advertise-addr (Docker behind NAT): the registration names the
// advertised address; the agent still binds its own; the status shows it.
func TestAdvertiseAddrIsOnlyTheRegistrationAddr(t *testing.T) {
	e := newTenvWith(t, func(o *Options, _ *Deps) { o.AdvertiseAddr = "192.0.2.77" })
	if err := e.a.ResolveIdentity(); err != nil {
		t.Fatal(err)
	}
	if req := e.a.RegisterRequest(); req.Addr != "192.0.2.77" {
		t.Fatalf("registration addr %q", req.Addr)
	}
	if a := e.a.Addr(); a != "127.0.0.1" {
		t.Fatalf("the agent binds its own address, got %q", a)
	}
	e.start()
	e.waitReady()
	if st := localStatus(t, e); st.AdvertiseAddr != "192.0.2.77" {
		t.Fatalf("%+v", st)
	}
}

func TestNoAdvertiseAddrKeepsTheOwnAddr(t *testing.T) {
	e := newTenv(t)
	if err := e.a.ResolveIdentity(); err != nil {
		t.Fatal(err)
	}
	if req := e.a.RegisterRequest(); req.Addr != "127.0.0.1" {
		t.Fatalf("registration addr %q", req.Addr)
	}
	e.start()
	e.waitReady()
	if st := localStatus(t, e); st.AdvertiseAddr != "" {
		t.Fatalf("%+v", st)
	}
}

// --allow-from that covers every address is warned about.
func TestAllowFromEverythingWarns(t *testing.T) {
	for _, c := range []struct {
		pfx  string
		warn bool
	}{{"0.0.0.0/0", true}, {"::/0", true}, {"172.30.0.0/16", false}} {
		var lb syncLog
		newTenvWith(t, func(o *Options, d *Deps) {
			o.AllowFrom = []netip.Prefix{netip.MustParsePrefix(c.pfx)}
			d.Logger = slog.New(slog.NewTextHandler(&lb, nil))
		})
		got := strings.Contains(lb.String(), "level=WARN") && strings.Contains(lb.String(), "allow-from")
		if got != c.warn {
			t.Fatalf("%s: warned %v:\n%s", c.pfx, got, lb.String())
		}
	}
}
