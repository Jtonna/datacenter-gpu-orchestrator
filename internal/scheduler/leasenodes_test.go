package scheduler

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
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

// The three shapes of an instance's machines, host first, the same in the
// lease (the request log's row) and in the status: (1) hosted on the
// scheduler's machine with console helpers: in the lease the scheduler's
// machine is the host with no share (it holds no layers), then the helpers
// with theirs; the status lists only the placed helpers;
// (2) a console that hosts alone: the whole model; (3) a console host
// with helpers: the host first, every share from the split.
func TestLease_HostFirstWithSharesInTheThreeShapes(t *testing.T) {
	type want struct {
		id    core.NodeID
		role  string
		share float64
	}
	// The status lists placed machines only: an unplaced server host is in
	// the lease, not in the status.
	check := func(name string, got []LeaseNode, st []PlacedNodeStatus, w []want) {
		t.Helper()
		skip := 0
		if len(w) > 0 && w[0].share == 0 && w[0].role == RoleHost {
			skip = 1
		}
		if len(got) != len(w) || len(st) != len(w)-skip {
			t.Fatalf("%s: lease %+v status %+v", name, got, st)
		}
		for i := range w {
			g := got[i]
			placed := i >= skip
			var s PlacedNodeStatus
			if placed {
				s = st[i-skip]
			}
			if core.NodeID(g.ID) != w[i].id || g.Role != w[i].role || g.Share != w[i].share || g.Name == "" {
				t.Fatalf("%s: lease node %d %+v, want %+v", name, i, g, w[i])
			}
			if !placed {
				continue
			}
			if s.ID != w[i].id || s.Role != w[i].role || s.Share != w[i].share {
				t.Fatalf("%s: status node %d %+v, want %+v", name, i, s, w[i])
			}
		}
	}

	// (1) The scheduler's machine hosts; two consoles hold the layers.
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 16000, func(p *core.Profile) { p.ForceNodes, p.TensorSplit = []string{"a", "b"}, []int{60, 40} })
	g := h.ready("m")
	check("server host", g.Nodes, instOf(t, h.c.statusDoc(), "m").Nodes,
		[]want{{"host-1", RoleHost, 0}, {"a", RoleHelper, .6}, {"b", RoleHelper, .4}})

	// (2) A console hosts alone.
	h = hostHarnessWith(t, []string{"a"}, nil)
	w := h.request("m")
	h.workersUpAll()
	h.healthy("m")
	g = h.granted(w)
	check("console alone", g.Nodes, instOf(t, h.c.statusDoc(), "m").Nodes, []want{{"a", RoleHost, 1}})
	h.release(g)

	// (3) A console hosts, two more help.
	h = hostHarnessWith(t, []string{"a", "b", "c"}, []int{50, 30, 20})
	w = h.request("m")
	h.workersUpAll()
	h.healthy("m")
	g = h.granted(w)
	check("console host and helpers", g.Nodes, instOf(t, h.c.statusDoc(), "m").Nodes,
		[]want{{"a", RoleHost, .5}, {"b", RoleHelper, .3}, {"c", RoleHelper, .2}})
	h.release(g)
}

// hostHarnessWith: agents membership, consoles a, b, c, a profile hosted on
// the first of nodes and split by split (nil: even).
func hostHarnessWith(t *testing.T, nodes []string, split []int) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 5, 15000))
	h.cfg.Defaults.Membership = config.MembershipAgents
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL, p.Host = nodes, split, ptrDur(0), core.HostNode
	})
	return h
}

// workersUpAll reports every worker launch of the last load started.
func (h *harness) workersUpAll() {
	h.t.Helper()
	l := h.lastLaunch()
	ids := map[core.NodeID]string{}
	for _, w := range l.spec.Workers {
		ids[w.Node] = "l-" + string(w.Node)
	}
	h.step(evWorkersLaunched{iid: l.spec.IID, res: WorkerResult{LaunchIDs: ids}})
}
