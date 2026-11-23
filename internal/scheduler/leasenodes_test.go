package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The grant names the instance, its load session and its machines with
// their roles, for the request log: the host node first, the rest helpers.
func TestLease_CarriesInstanceSessionAndNodeRoles(t *testing.T) {
	h := hostHarness(t)
	w := h.request("m")
	h.workersUp()
	h.healthy("m")
	g := h.granted(w)
	in := h.inst("m")
	if g.Instance != in.iid || g.Session == "" || g.Session != in.sessionID {
		t.Fatalf("grant %+v, instance %d session %q", g, in.iid, in.sessionID)
	}
	if len(g.Nodes) != 2 || g.Nodes[0].ID != "a" || g.Nodes[0].Role != RoleHost || g.Nodes[1].ID != "b" || g.Nodes[1].Role != RoleHelper {
		t.Fatalf("nodes %+v", g.Nodes)
	}
	if g.Nodes[0].Name == "" {
		t.Fatal("no name")
	}
	h.release(g)
	// A second request shares the same slice (built once per instance).
	g2 := h.granted(h.request("m"))
	if &g2.Nodes[0] != &g.Nodes[0] {
		t.Fatal("lease nodes rebuilt per request")
	}
	h.release(g2)
}

// Without a host node the scheduler's machine hosts: the local node, when it
// is placed, is the host and every other node a helper.
func TestLease_LocalNodeHostsWithoutAHostNode(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000), laptop("l", 7000))
	h.llama("m", 12000, func(p *core.Profile) { p.ForceNodes = []string{"host-1", "l"} })
	g := h.ready("m")
	if len(g.Nodes) != 2 {
		t.Fatalf("nodes %+v", g.Nodes)
	}
	for _, n := range g.Nodes {
		want := RoleHelper
		if n.ID == "host-1" {
			want = RoleHost
		}
		if n.Role != want {
			t.Fatalf("nodes %+v", g.Nodes)
		}
	}
}
