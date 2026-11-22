// Package identity computes the node id (agent contract 4.1): the lowercase
// MAC of one physical network interface, chosen from the route to Potluck.
// A Docker install with no physical interface (a container on a bridge
// network) takes the MAC of the interface on the route instead. It is
// computed at every start and never stored.
package identity

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// NetOS is what identity needs from the OS.
type NetOS interface {
	// SourceFor returns the local address the kernel picks to reach dst.
	SourceFor(dst net.IP) (net.IP, error)
	// InterfaceOf returns the interface that owns ip, "" if none.
	InterfaceOf(ip net.IP) (string, error)
	DefaultRouteInterface() string
	IsPhysical(iface string) bool
	MAC(iface string) (string, error)
}

// Identity is the result.
type Identity struct {
	ID         string // lowercase MAC
	Iface      string // the interface the id comes from
	RouteIface string // the interface that carries the route to Potluck (stats primary)
	Addr       string // the node's address that reaches Potluck
	// Virtual is true when the id comes from a virtual interface (only with
	// virtualOK): it is only stable while that interface's MAC is fixed.
	Virtual bool
}

// Resolve applies the rules of 4.1. virtualOK (a Docker install) allows the
// last step: when neither the route interface nor the default route's is
// physical, the id is the MAC of the route interface.
func Resolve(n NetOS, potluckIP net.IP, virtualOK bool) (Identity, error) {
	var id Identity
	src, err := n.SourceFor(potluckIP)
	if err != nil {
		return id, fmt.Errorf("no route to Potluck %s: %w", potluckIP, err)
	}
	id.Addr = src.String()
	// The Potluck address belongs to this machine: the interface that owns it.
	iface, err := n.InterfaceOf(potluckIP)
	if err != nil {
		return id, err
	}
	if iface == "" {
		iface, err = n.InterfaceOf(src)
		if err != nil {
			return id, err
		}
	}
	id.RouteIface = iface
	if iface == "" || !n.IsPhysical(iface) {
		def := n.DefaultRouteInterface()
		switch {
		case def != "" && n.IsPhysical(def):
			iface = def
		case virtualOK && iface != "":
			id.Virtual = true
		default:
			return id, fmt.Errorf("no physical network interface: route to Potluck leaves by %q, default route by %q", iface, def)
		}
	}
	mac, err := n.MAC(iface)
	if err != nil {
		return id, err
	}
	if mac == "" || zeroMAC(mac) {
		return id, fmt.Errorf("interface %s has no MAC address", iface)
	}
	id.ID, id.Iface = strings.ToLower(mac), iface
	return id, nil
}

// zeroMAC reports whether mac is all zeros (loopback, some tunnels).
func zeroMAC(mac string) bool {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return false
	}
	for _, b := range hw {
		if b != 0 {
			return false
		}
	}
	return true
}

// RealNet implements NetOS with the standard library plus two file reads
// (default route and the physical check) supplied by the caller.
type RealNet struct {
	DefaultRoute func() string
	Physical     func(string) bool
}

// SourceFor uses a connected UDP socket: no packet is sent, the kernel only
// picks the route and source address.
func (RealNet) SourceFor(dst net.IP) (net.IP, error) {
	c, err := net.Dial("udp", net.JoinHostPort(dst.String(), "3122"))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, errors.New("no local address")
	}
	return ua.IP, nil
}

// InterfaceOf implements NetOS.
func (RealNet) InterfaceOf(ip net.IP) (string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, it := range ifs {
		addrs, err := it.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(ip) {
				return it.Name, nil
			}
		}
	}
	return "", nil
}

// DefaultRouteInterface implements NetOS.
func (r RealNet) DefaultRouteInterface() string {
	if r.DefaultRoute == nil {
		return ""
	}
	return r.DefaultRoute()
}

// IsPhysical implements NetOS.
func (r RealNet) IsPhysical(iface string) bool {
	if r.Physical == nil {
		return false
	}
	return r.Physical(iface)
}

// MAC implements NetOS.
func (RealNet) MAC(iface string) (string, error) {
	it, err := net.InterfaceByName(iface)
	if err != nil {
		return "", err
	}
	return it.HardwareAddr.String(), nil
}
