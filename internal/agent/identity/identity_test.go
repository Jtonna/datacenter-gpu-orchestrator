package identity

import (
	"errors"
	"net"
	"testing"
)

type fakeNet struct {
	src      string
	owners   map[string]string // ip -> iface
	def      string
	physical map[string]bool
	macs     map[string]string
}

func (f fakeNet) SourceFor(dst net.IP) (net.IP, error) {
	if f.src == "" {
		return nil, errors.New("unreachable")
	}
	return net.ParseIP(f.src), nil
}
func (f fakeNet) InterfaceOf(ip net.IP) (string, error) { return f.owners[ip.String()], nil }
func (f fakeNet) DefaultRouteInterface() string         { return f.def }
func (f fakeNet) IsPhysical(i string) bool              { return f.physical[i] }
func (f fakeNet) MAC(i string) (string, error)          { return f.macs[i], nil }

func TestResolve(t *testing.T) {
	macs := map[string]string{"enp64s0f2": "02:00:00:00:00:0A", "eno1": "02:00:00:00:00:0b", "br0": "02:00:00:00:00:0c"}
	phys := map[string]bool{"enp64s0f2": true, "eno1": true}
	potluck := net.ParseIP("192.0.2.1")

	// PS5: the route to Potluck leaves by the Ethernet interface.
	id, err := Resolve(fakeNet{src: "192.0.2.101", owners: map[string]string{"192.0.2.101": "enp64s0f2"}, physical: phys, macs: macs}, potluck)
	if err != nil || id.ID != "02:00:00:00:00:0a" || id.Addr != "192.0.2.101" || id.RouteIface != "enp64s0f2" {
		t.Fatalf("%+v %v", id, err)
	}
	// Host machine: Potluck's address is its own; the owning interface is used.
	id, _ = Resolve(fakeNet{src: "192.0.2.1", owners: map[string]string{"192.0.2.1": "eno1"}, physical: phys, macs: macs}, potluck)
	if id.ID != "02:00:00:00:00:0b" || id.Iface != "eno1" {
		t.Fatalf("%+v", id)
	}
	// Route through a bridge: fall back to the default route's interface.
	id, _ = Resolve(fakeNet{src: "192.0.2.2", owners: map[string]string{"192.0.2.2": "br0"}, def: "eno1", physical: phys, macs: macs}, potluck)
	if id.ID != "02:00:00:00:00:0b" || id.RouteIface != "br0" {
		t.Fatalf("%+v", id)
	}
	// No physical interface at all: no id.
	if _, err := Resolve(fakeNet{src: "192.0.2.2", owners: map[string]string{"192.0.2.2": "br0"}, def: "docker0", physical: phys, macs: macs}, potluck); err == nil {
		t.Fatal("want error")
	}
	if _, err := Resolve(fakeNet{}, potluck); err == nil {
		t.Fatal("unreachable must fail")
	}
}

func TestRealNetLoopback(t *testing.T) {
	src, err := RealNet{}.SourceFor(net.ParseIP("127.0.0.1"))
	if err != nil || !src.IsLoopback() {
		t.Fatal(src, err)
	}
}
