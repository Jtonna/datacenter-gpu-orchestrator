package scheduler

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Zone facts (zonefacts.go): role and share per member, why a node is set
// aside, and when a claimed or settling node is free.

func sharesAddUp(t *testing.T, in InstanceStatus) {
	t.Helper()
	sum, hosts := 0.0, 0
	for _, m := range in.Nodes {
		sum += m.Share
		if m.Role == RoleHost {
			hosts++
		} else if m.Role != RoleHelper {
			t.Fatalf("member %s role %q", m.ID, m.Role)
		}
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("shares of %s add up to %v: %+v", in.Profile, sum, in.Nodes)
	}
	if hosts > 1 {
		t.Fatalf("%d hosts: %+v", hosts, in.Nodes)
	}
}

func instOf(t *testing.T, st Status, profile string) InstanceStatus {
	t.Helper()
	for _, in := range st.Instances {
		if in.Profile == profile {
			return in
		}
	}
	t.Fatalf("no instance %s in %+v", profile, st.Instances)
	return InstanceStatus{}
}

func TestZone_HostOnNodeRoleAndShareFromTheSplit(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 5, 15000))
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.c.agents = true
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL, p.Host, p.Parallel = []string{"a", "b", "c"}, []int{40, 35, 25}, ptrDur(0), core.HostNode, 4
	})
	w := h.request("m")
	h.launched(WorkerResult{LaunchIDs: map[core.NodeID]string{"b": "l-b", "c": "l-c"}})
	h.healthy("m")
	h.release(h.granted(w))
	in := instOf(t, h.c.statusDoc(), "m")
	if in.Host != HostOnNode || in.HostNode != "a" || in.Parallel != 4 {
		t.Fatalf("host %q %q parallel %d", in.Host, in.HostNode, in.Parallel)
	}
	want := map[core.NodeID]struct {
		role  string
		share float64
	}{"a": {RoleHost, .40}, "b": {RoleHelper, .35}, "c": {RoleHelper, .25}}
	for _, m := range in.Nodes {
		if w := want[m.ID]; m.Role != w.role || math.Abs(m.Share-w.share) > 1e-9 {
			t.Fatalf("member %s: %s %v, want %s %v", m.ID, m.Role, m.Share, w.role, w.share)
		}
	}
	sharesAddUp(t, in)
}

func TestZone_ServerHostWithItsOwnGPUIsTheHost(t *testing.T) {
	h := newHarness(t).nodes(hostNode(8000), ps5("a", 2, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.ForceNodes, p.TensorSplit = []string{"host-1", "a"}, []int{1, 3} })
	h.ready("m")
	in := instOf(t, h.c.statusDoc(), "m")
	if in.Host != HostServer || in.HostNode != "" || in.Parallel != 1 {
		t.Fatalf("host %q %q parallel %d", in.Host, in.HostNode, in.Parallel)
	}
	for _, m := range in.Nodes {
		switch {
		case m.ID == "host-1" && (m.Role != RoleHost || m.Share != .25):
			t.Fatalf("local %+v", m)
		case m.ID == "a" && (m.Role != RoleHelper || m.Share != .75):
			t.Fatalf("a %+v", m)
		}
	}
	sharesAddUp(t, in)
}

func TestZone_ServerHostWithRemoteWorkersOnlyHasHelpersByBudget(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 9000))
	h.llama("m", 16000)
	h.ready("m")
	in := instOf(t, h.c.statusDoc(), "m")
	if in.Host != HostServer || len(in.Nodes) != 2 {
		t.Fatalf("%+v", in)
	}
	for _, m := range in.Nodes {
		if m.Role != RoleHelper || m.Share <= 0 {
			t.Fatalf("member %+v", m)
		}
	}
	sharesAddUp(t, in)
	// The share is the budget split the child was given.
	a, b := in.Nodes[0], in.Nodes[1]
	if got, want := a.Share/b.Share, float64(a.BudgetMiB)/float64(b.BudgetMiB); math.Abs(got-want) > 1e-9 {
		t.Fatalf("share ratio %v, budget ratio %v", got, want)
	}
}

func TestZone_SingleNodeIsTheWholeModel(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.ready("m")
	in := instOf(t, h.c.statusDoc(), "m")
	if len(in.Nodes) != 1 || in.Nodes[0].Role != RoleHelper || in.Nodes[0].Share != 1 {
		t.Fatalf("%+v", in.Nodes)
	}
}

func TestZone_SetAsideAfterAFailedPreflightNamesTheLoad(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.prober.fail["192.0.2.2:50052"] = probeFail("192.0.2.2:50052")
	h.request("m")
	st := h.c.statusDoc()
	if len(st.Quarantines) != 1 {
		t.Fatalf("quarantines %+v", st.Quarantines)
	}
	q := st.Quarantines[0]
	if q.Node != "a" || q.Reason != SetAsidePreflight || q.Profile != "m" || !q.Since.Equal(h.clk.Now()) ||
		q.Until.Sub(q.Since) != 30*time.Second || q.Level != 1 || q.Message != probeFail("192.0.2.2:50052").Error() {
		t.Fatalf("quarantine %+v", q)
	}
	if n := st.View.Nodes[0]; n.QuarantineReason != SetAsidePreflight || !n.Quarantined.Equal(q.Until) {
		t.Fatalf("node %+v", n)
	}
	// Once it ends the node shows no reason.
	delete(h.prober.fail, "192.0.2.2:50052")
	h.advance(31 * time.Second)
	if n := h.c.statusDoc().View.Nodes[0]; n.QuarantineReason != "" {
		t.Fatalf("reason after the end: %+v", n)
	}
}

func TestZone_SetAsideWhenTheAgentIsUnreachable(t *testing.T) {
	h := hostHarness(t)
	h.request("m")
	h.launched(WorkerResult{Node: "b", Reason: ReasonAgentUnreachable, Message: "connect: refused"})
	st := h.c.statusDoc()
	if len(st.Quarantines) != 1 || st.Quarantines[0].Node != "b" || st.Quarantines[0].Reason != SetAsideAgentUnreachable ||
		st.Quarantines[0].Profile != "m" || st.Quarantines[0].Message != "connect: refused" {
		t.Fatalf("quarantines %+v", st.Quarantines)
	}
}

func TestZone_SetAsideReasonOfEveryCause(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{errWorkerHung, SetAsideWorkerHung},
		{errNodeLost, SetAsideNodeLost},
		{fmt.Errorf("wrapped: %w", errNodeLost), SetAsideNodeLost},
		{setAsideErr(SetAsidePreflight, "p", errBoom), SetAsidePreflight},
		{setAsideErr(SetAsideAgentUnreachable, "p", errors.New("x")), SetAsideAgentUnreachable},
		{errBoom, SetAsideOther},
	} {
		h := newHarness(t).nodes(ps5("a", 2, 15000))
		h.step(evTick{})
		h.c.quarantine("a", tc.cause)
		st := h.c.statusDoc()
		if len(st.Quarantines) != 1 || st.Quarantines[0].Reason != tc.want || st.Quarantines[0].Message != tc.cause.Error() {
			t.Fatalf("%v: %+v", tc.cause, st.Quarantines)
		}
	}
}

func nodeStatus(t *testing.T, st Status, id core.NodeID) NodeStatus {
	t.Helper()
	for _, n := range st.View.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no node %s", id)
	return NodeStatus{}
}

func TestZone_FreeInFromTheVictimsDrainStopAndSettle(t *testing.T) {
	h := twoBig(t)
	h.autoExit = false
	g := h.ready("big") // a request in flight: the victim drains
	h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	n := nodeStatus(t, h.c.statusDoc(), "a")
	if n.ClaimedBy != "other" || n.FreeBasis != FreeDraining || !n.FreeAt.IsZero() {
		t.Fatalf("draining: %+v", n)
	}
	h.release(g) // the drain ends; the victim stops
	h.wantState("big", "stopping")
	n = nodeStatus(t, h.c.statusDoc(), "a")
	if n.FreeBasis != FreeStopping || !n.FreeAt.Equal(h.clk.Now().Add(3*time.Second)) {
		t.Fatalf("stopping: %+v", n)
	}
	h.exit("big", 0) // stopped: the node settles
	n = nodeStatus(t, h.c.statusDoc(), "a")
	if n.FreeBasis != FreeSettling || !n.FreeAt.Equal(h.clk.Now().Add(3*time.Second)) || !n.FreeAt.Equal(n.SettleUntil) {
		t.Fatalf("settling: %+v", n)
	}
}

func TestZone_NoFreeTimeOnAnIdleOrHeldNode(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.step(evTick{})
	h.release(h.ready("m"))
	st := h.c.statusDoc()
	for _, id := range []core.NodeID{"a", "b"} {
		if n := nodeStatus(t, st, id); n.FreeBasis != "" || !n.FreeAt.IsZero() {
			t.Fatalf("%s: %+v", id, n)
		}
	}
}
