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
	id, err := Resolve(fakeNet{src: "192.0.2.101", owners: map[string]string{"192.0.2.101": "enp64s0f2"}, physical: phys, macs: macs}, potluck, false)
	if err != nil || id.ID != "02:00:00:00:00:0a" || id.Addr != "192.0.2.101" || id.RouteIface != "enp64s0f2" {
		t.Fatalf("%+v %v", id, err)
	}
	// Host machine: Potluck's address is its own; the owning interface is used.
	id, _ = Resolve(fakeNet{src: "192.0.2.1", owners: map[string]string{"192.0.2.1": "eno1"}, physical: phys, macs: macs}, potluck, false)
	if id.ID != "02:00:00:00:00:0b" || id.Iface != "eno1" {
		t.Fatalf("%+v", id)
	}
	// Route through a bridge: fall back to the default route's interface.
	id, _ = Resolve(fakeNet{src: "192.0.2.2", owners: map[string]string{"192.0.2.2": "br0"}, def: "eno1", physical: phys, macs: macs}, potluck, false)
	if id.ID != "02:00:00:00:00:0b" || id.RouteIface != "br0" {
		t.Fatalf("%+v", id)
	}
	// No physical interface at all: no id.
	if _, err := Resolve(fakeNet{src: "192.0.2.2", owners: map[string]string{"192.0.2.2": "br0"}, def: "docker0", physical: phys, macs: macs}, potluck, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := Resolve(fakeNet{}, potluck, false); err == nil {
		t.Fatal("unreachable must fail")
	}
}

// A container on a compose bridge network (Docker install): eth0 is a veth
// and there is no physical interface. With virtualOK the id is the MAC of
// the interface on the route to Potluck; without it (bare) the rule and the
// error stay as they are.
func TestResolveContainer(t *testing.T) {
	potluck := net.ParseIP("172.30.0.2")
	ctr := fakeNet{src: "172.30.0.10", owners: map[string]string{"172.30.0.10": "eth0"}, def: "eth0",
		physical: map[string]bool{}, macs: map[string]string{"eth0": "02:00:00:00:00:1A"}}
	id, err := Resolve(ctr, potluck, true)
	if err != nil || id.ID != "02:00:00:00:00:1a" || id.Iface != "eth0" || id.RouteIface != "eth0" || id.Addr != "172.30.0.10" || !id.Virtual {
		t.Fatalf("docker, virtual only: %+v %v", id, err)
	}
	_, err = Resolve(ctr, potluck, false)
	want := `no physical network interface: route to Potluck leaves by "eth0", default route by "eth0"`
	if err == nil || err.Error() != want {
		t.Fatalf("bare: err %v, want %q", err, want)
	}
	// Docker on the host network: a physical interface is present and wins,
	// also over a virtual route interface.
	host := fakeNet{src: "192.0.2.2", owners: map[string]string{"192.0.2.2": "br0"}, def: "eno1",
		physical: map[string]bool{"eno1": true}, macs: map[string]string{"br0": "02:00:00:00:00:0c", "eno1": "02:00:00:00:00:0b"}}
	id, err = Resolve(host, net.ParseIP("192.0.2.1"), true)
	if err != nil || id.ID != "02:00:00:00:00:0b" || id.Iface != "eno1" || id.Virtual {
		t.Fatalf("docker, host network: %+v %v", id, err)
	}
	// An all-zero or empty MAC is still no id.
	for _, mac := range []string{"", "00:00:00:00:00:00"} {
		ctr.macs = map[string]string{"eth0": mac}
		if id, err := Resolve(ctr, potluck, true); err == nil {
			t.Fatalf("MAC %q: want an error, got %+v", mac, id)
		}
	}
	// No interface owns the source address: nothing to take the MAC from.
	ctr.owners, ctr.def = map[string]string{}, ""
	if _, err := Resolve(ctr, potluck, true); err == nil {
		t.Fatal("no route interface: want an error")
	}
}

func TestRealNetLoopback(t *testing.T) {
	src, err := RealNet{}.SourceFor(net.ParseIP("127.0.0.1"))
	if err != nil || !src.IsLoopback() {
		t.Fatal(src, err)
	}
}
