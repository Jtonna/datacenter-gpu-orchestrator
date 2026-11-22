package agent

import (
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// containerNet is a container on a compose bridge network: eth0 is a veth
// with a fixed MAC and there is no physical interface.
type containerNet struct{}

func (containerNet) SourceFor(net.IP) (net.IP, error)   { return net.ParseIP("172.30.0.10"), nil }
func (containerNet) InterfaceOf(net.IP) (string, error) { return "eth0", nil }
func (containerNet) DefaultRouteInterface() string      { return "eth0" }
func (containerNet) IsPhysical(string) bool             { return false }
func (containerNet) MAC(string) (string, error)         { return "02:00:00:00:00:10", nil }

// A Docker install in a container on a bridge network takes the MAC of the
// interface on the route to Potluck and says once, at INFO, that the id comes
// from a virtual interface; the address it reports and the stats primary are
// the container's own.
func TestIdentityDockerContainer(t *testing.T) {
	var buf syncBuf
	e := newTenvWith(t, func(o *Options, d *Deps) {
		o.Install = agentproto.InstallDocker
		d.Net = containerNet{}
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	})
	for i := 0; i < 3; i++ { // the start loop resolves the id on every round
		if err := e.a.ResolveIdentity(); err != nil {
			t.Fatal(err)
		}
	}
	if e.a.ID() != "02:00:00:00:00:10" || e.a.Addr() != "172.30.0.10" {
		t.Fatalf("id %q addr %q", e.a.ID(), e.a.Addr())
	}
	if e.a.ident.RouteIface != "eth0" {
		t.Fatalf("stats primary %q, want eth0", e.a.ident.RouteIface)
	}
	if n := buf.count("level=INFO"); n != 1 || buf.count("virtual interface") != 1 || buf.count("mac_address") != 1 {
		t.Fatalf("want one INFO line about the virtual interface, got %d:\n%s", n, buf.b.String())
	}
	if st := e.a.Status(); st.LastError != nil {
		t.Fatalf("last_error %q", *st.LastError)
	}
}

// A bare install keeps the rule: no physical interface, no id.
func TestIdentityBareNeedsPhysical(t *testing.T) {
	e := newTenvWith(t, func(o *Options, d *Deps) {
		o.Install = agentproto.InstallBare
		d.Net = containerNet{}
	})
	err := e.a.ResolveIdentity()
	if err == nil || !strings.Contains(err.Error(), `no physical network interface: route to Potluck leaves by "eth0", default route by "eth0"`) {
		t.Fatalf("err %v", err)
	}
	if e.a.ID() != "" {
		t.Fatalf("id %q", e.a.ID())
	}
}
