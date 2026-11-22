package scheduler

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

func TestRunning_DrainingShownAsReady_CmdRendered(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big")
	h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	got := h.c.runningList()
	if len(got) != 1 || got[0].Profile != "big" || got[0].State != RunningReady || !got[0].ReadySince.Equal(t0) {
		t.Fatalf("running %+v", got)
	}
	if !strings.HasPrefix(got[0].Cmd, "llama-server-rpc --host 127.0.0.1 --port 5801 --model /models/big.gguf") ||
		!strings.Contains(got[0].Cmd, "--rpc 192.0.2.2:50052 --device RPC0") || !strings.Contains(got[0].Cmd, "--ctx-size 4096") {
		t.Fatalf("cmd %q", got[0].Cmd)
	}
	_ = g
}

func TestRunning_StartingAndStopping(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000)
	h.llama("n", 8000)
	h.autoExit = false
	h.release(h.ready("m"))
	h.step(evUnload{profile: "m"})
	h.autoProbe = false
	h.request("n")
	got := h.c.runningList()
	if len(got) != 2 || got[0].State != RunningStopping || got[1].State != RunningStarting {
		t.Fatalf("running %+v", got)
	}
	if !strings.Contains(got[1].Cmd, "--model /models/n.gguf") {
		t.Fatalf("provisional cmd %q", got[1].Cmd)
	}
}

func TestProfiles_EveryProfileWithState(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.llama("idle", 8000)
	h.ready("m")
	got := h.c.profileStates()
	want := []ProfileState{{Name: "idle", State: RunningStopped}, {Name: "m", State: RunningReady, ReadySince: t0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles %+v", got)
	}
}

func TestUpstream_ReadyOrDrainingOnly(t *testing.T) {
	h := twoBig(t)
	h.autoProbe = false
	h.request("big")
	if _, ok := h.c.upstream("big"); ok {
		t.Fatalf("upstream while starting")
	}
	h.completeProbes()
	h.healthy("big")
	if a, ok := h.c.upstream("big"); !ok || a != "127.0.0.1:5801" {
		t.Fatalf("upstream %q %v", a, ok)
	}
	h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	if _, ok := h.c.upstream("big"); !ok {
		t.Fatalf("no upstream while draining")
	}
}

func TestStatus_LoadLogMatchedToNodesByEndpoint(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 9000), ps5("b", 4, 9000), hostNode(8000))
	h.llama("m", 20000)
	h.request("m")
	ch := h.child("m")
	ch.load = instance.LoadStats{Devices: []instance.DeviceMem{
		{Name: "RPC0[192.0.2.4:50052]", ModelMiB: 4000, KVMiB: 500, ComputeMiB: 300},
		{Name: "RPC0[192.0.2.2:50052]", ModelMiB: 3000, KVMiB: 400, ComputeMiB: 200},
		{Name: "CUDA0", ModelMiB: 2000, KVMiB: 100, ComputeMiB: 100},
		{Name: "CPU", Host: true, ModelMiB: 300},
	}}
	st := h.c.statusDoc()
	if len(st.Instances) != 1 {
		t.Fatalf("instances %+v", st.Instances)
	}
	in := st.Instances[0]
	byID := map[core.NodeID]PlacedNodeStatus{}
	for _, n := range in.Nodes {
		byID[n.ID] = n
	}
	if m := byID["b"].Memory; len(m) != 1 || m[0].TotalMiB != 4800 {
		t.Fatalf("b memory %+v", m)
	}
	if m := byID["a"].Memory; len(m) != 1 || m[0].TotalMiB != 3600 {
		t.Fatalf("a memory %+v", m)
	}
	if m := byID["host-1"].Memory; len(m) != 1 || m[0].Name != "CUDA0" {
		t.Fatalf("host-1 memory %+v", m)
	}
	if len(in.HostMemory) != 1 || in.DeviceTotalMiB != 4800+3600+2200 {
		t.Fatalf("host %+v total %v", in.HostMemory, in.DeviceTotalMiB)
	}
}

func TestStatus_QueueClaimsQuarantinesBackoffsView(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("big", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("other", 12000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("bad", 8000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
	g := h.ready("big")
	h.request("other")
	h.advance(61 * time.Second)
	h.request("bad")
	h.exit("bad", 1)
	h.c.quarantine("b", errBoom)
	st := h.c.statusDoc()
	if len(st.Queue) != 1 || st.Queue[0].Profile != "other" || !st.Queue[0].Claimed ||
		!reflect.DeepEqual(st.Queue[0].Claim, []core.NodeID{"a"}) || !reflect.DeepEqual(st.Queue[0].Victims, []string{"big"}) {
		t.Fatalf("queue %+v", st.Queue)
	}
	if len(st.Backoffs) != 1 || st.Backoffs[0].Profile != "bad" {
		t.Fatalf("backoffs %+v", st.Backoffs)
	}
	if len(st.Quarantines) != 1 || st.Quarantines[0].Node != "b" {
		t.Fatalf("quarantines %+v", st.Quarantines)
	}
	if len(st.View.Nodes) != 2 || st.View.Stale || st.View.Nodes[0].Holder != "big" || st.View.Nodes[0].ClaimedBy != "other" ||
		st.View.Nodes[0].PlanBudgetMiB != 15000 {
		t.Fatalf("view %+v", st.View)
	}
	if st.Instances[0].State != "draining" || st.Instances[0].VictimOf != "other" {
		t.Fatalf("instance %+v", st.Instances[0])
	}
	_ = g
}

func TestPlan_DryRunChangesNothing(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 9000))
	h.llama("big", 12000)
	h.llama("small", 7000)
	h.llama("huge", 20000)
	h.llama("never", 90000)
	g := h.ready("big") // on a
	before := h.c.statusDoc()

	if r := h.c.dryPlan("big"); r.Action != PlanServe {
		t.Fatalf("big: %+v", r)
	}
	if r := h.c.dryPlan("small"); r.Action != PlanStart || len(r.Placement) != 1 || r.Placement[0].ID != "b" {
		t.Fatalf("small: %+v", r)
	}
	if r := h.c.dryPlan("huge"); r.Action != PlanQueue || r.Reason == "" {
		t.Fatalf("huge (big busy, within hold): %+v", r)
	}
	if after := h.c.statusDoc(); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run while big is busy changed state:\nbefore %+v\nafter  %+v", before, after)
	}
	h.release(g)
	h.advance(30 * time.Second)
	before = h.c.statusDoc()
	if r := h.c.dryPlan("huge"); r.Action != PlanEvict || len(r.Victims) != 1 || r.Victims[0].Profile != "big" || r.Victims[0].Busy {
		t.Fatalf("huge (big idle): %+v", r)
	}
	if r := h.c.dryPlan("never"); r.Action != PlanReject || r.CapacityMiB != 15000+15000 {
		t.Fatalf("never: %+v", r)
	}
	after := h.c.statusDoc()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run changed state:\nbefore %+v\nafter  %+v", before, after)
	}
}

// An instance still in pre-flight has no port yet: /running must not show a
// fake "--port 0". A llama command is shown without the port flag (the hub
// still finds --rpc and --ctx-size); a kind: command line is not shown.
func TestRunning_PreflightCmdHasNoFakePort(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), hostNode(8000))
	h.llama("m", 8000)
	h.command("whisper", 2000)
	h.autoProbe, h.autoStart = false, false
	h.request("m")
	h.request("whisper")
	got := h.c.runningList()
	if len(got) != 2 {
		t.Fatalf("running %+v", got)
	}
	for _, r := range got {
		if strings.Contains(r.Cmd, "--port") || strings.Contains(r.Cmd, " 0 ") {
			t.Errorf("%s: cmd shows a fake port: %q", r.Profile, r.Cmd)
		}
	}
	if m := got[0]; m.Profile != "m" || !strings.Contains(m.Cmd, "--rpc 192.0.2.2:50052") || !strings.Contains(m.Cmd, "--ctx-size 4096") {
		t.Errorf("llama pre-flight cmd %q", m.Cmd)
	}
	if got[1].Cmd != "" {
		t.Errorf("whisper pre-flight cmd %q, want empty", got[1].Cmd)
	}
}
