package pool

import (
	"context"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
)

// TS1 (tenth review): a worker stop counts as seen only when the exit was
// confirmed. A worker still stopping when the wait gives up leaves the stop
// unseen, so its readings never count as fresh and the scheduler waits to the
// end of the window; a stop confirmed by the agent is seen at once.
func TestAnUnconfirmedStopIsNotSeen(t *testing.T) {
	old := stopExitWait
	stopExitWait = 300 * time.Millisecond
	t.Cleanup(func() { stopExitWait = old })
	r := newRig(t)
	slow := r.agent(1, func(a *fakeagent.Agent) { a.IgnoreStop = true })
	quick := r.agent(2)
	r.waitNode(slow.ID)
	r.waitNode(quick.ID)
	for i, a := range []*fakeagent.Agent{slow, quick} {
		if res := r.p.Launch(context.Background(), spec(uint64(61+i), a), func(scheduler.WorkerReport) {}); res.Reason != "" {
			t.Fatalf("%+v", res)
		}
	}
	r.p.Stop(61, nil)
	r.p.Stop(62, nil)
	r.p.WaitStops(10 * time.Second)
	if !slow.Has("DELETE /launch/" + slow.LaunchState().LaunchID) {
		t.Fatalf("no stop was sent: %v", slow.Calls())
	}
	time.Sleep(300 * time.Millisecond) // several heartbeats of both
	if n, _ := r.node(slow.ID); !n.StopSeenAt.IsZero() || n.FiguresSinceStop != 0 {
		t.Fatalf("a stop the agent never confirmed was seen at %v, %d readings counted", n.StopSeenAt, n.FiguresSinceStop)
	}
	if n, _ := r.node(quick.ID); n.StopSeenAt.IsZero() || n.FiguresSinceStop < 2 {
		t.Fatalf("a confirmed stop: seen %v, readings %d", n.StopSeenAt, n.FiguresSinceStop)
	}
}
