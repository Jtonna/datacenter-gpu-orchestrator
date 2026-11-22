package pool

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The agent learns its pause state from the registration and heartbeat
// answers. After an unpause the node stays paused for placement until an
// answer has carried the unpaused state, so the agent cannot refuse the
// launch as "paused". A pause takes effect at once.

const upID = "02:00:00:00:00:71"

// wantPlaceable checks the scheduler's view and the node facts.
func (r *rig) wantPlaceable(id string, want bool) {
	r.t.Helper()
	n, ok := r.node(id)
	if !ok {
		r.t.Fatalf("node %s not in the view", id)
	}
	f := r.fact(id)
	if n.Paused == want || f.Placeable != want {
		r.t.Fatalf("placeable %v wanted: view paused %v, facts %+v", want, n.Paused, f)
	}
	if !want && f.State == StateActive && f.Unplaced != scheduler.UnplacedPaused {
		r.t.Fatalf("unplaced %q, want paused", f.Unplaced)
	}
}

func (r *rig) heartbeat(id string) HeartbeatResult {
	r.t.Helper()
	res := r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	if res.Status != 200 {
		r.t.Fatalf("heartbeat %d %s", res.Status, res.Err)
	}
	return res
}

func TestUnpauseWaitsUntilTheAgentHasBeenTold(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq(upID))
	r.heartbeat(upID)
	r.wantPlaceable(upID, true)

	// A pause takes effect at once, before the agent hears of it.
	r.p.SetPaused(upID, true)
	r.wantPlaceable(upID, false)
	if !r.heartbeat(upID).Answer.Paused {
		t.Fatal("the answer does not carry the pause")
	}

	// The unpause does not, until an answer has carried it.
	r.p.SetPaused(upID, false)
	r.wantPlaceable(upID, false)
	f := r.fact(upID)
	if f.Paused || !f.UnpausePending || f.UnplacedMessage != UnpausePendingMessage {
		t.Fatalf("facts %+v", f)
	}
	if res := r.heartbeat(upID); res.Answer.Paused {
		t.Fatal("the answer still carries the pause")
	}
	r.wantPlaceable(upID, true)
	if f := r.fact(upID); f.UnpausePending || f.UnplacedMessage != "" {
		t.Fatalf("facts after delivery %+v", f)
	}
}

// Paused and unpaused again before any answer told the agent of the pause:
// the agent never believed it was paused, so nothing waits.
func TestUnpauseBeforeThePauseWasDelivered(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq(upID))
	r.heartbeat(upID)
	r.p.SetPaused(upID, true)
	r.wantPlaceable(upID, false)
	r.p.SetPaused(upID, false)
	r.wantPlaceable(upID, true)
}

// A node lost at the moment of the unpause becomes placeable only after it is
// back and its heartbeat answer has told it.
func TestUnpauseOfALostNode(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq(upID))
	r.heartbeat(upID)
	r.p.SetPaused(upID, true)
	r.heartbeat(upID) // told: paused
	r.clk.advance(11 * time.Second)
	r.p.CheckLost()
	r.p.SetPaused(upID, false)
	if n, _ := r.node(upID); n.Online || !n.Paused {
		t.Fatalf("lost node %+v", n)
	}
	if f := r.fact(upID); f.State != StateLost || !f.UnpausePending || f.Placeable {
		t.Fatalf("facts %+v", f)
	}
	if res := r.heartbeat(upID); res.Answer.Paused { // back, and told
		t.Fatal("answer carries the pause")
	}
	if n, _ := r.node(upID); !n.Online {
		t.Fatal("not back online")
	}
	r.wantPlaceable(upID, true)
}

// After a Potluck restart the pause flag comes from the store and the agent
// learns it from the registration answer: the node is placeable as soon as
// the registration told it it is unpaused, and paused after one that told it
// it is paused, until a later answer says otherwise.
func TestUnpauseAcrossARestart(t *testing.T) {
	r := newRig(t)
	r.store.SetNodeAdmin(upID, cfgstore.NodeAdmin{Paused: true})
	r.start()
	// Unpaused while the node is not registered yet (the agent may believe
	// anything): the registration answer tells it.
	if _, err := r.p.SetPaused(upID, false); err != nil {
		t.Fatal(err)
	}
	if ans := r.p.Register(regReq(upID)); ans.Paused {
		t.Fatal("registration answer carries a pause")
	}
	r.heartbeat(upID)
	r.wantPlaceable(upID, true)

	// Paused across a restart: the registration tells the agent it is
	// paused; an unpause then waits for the next answer.
	r.p.SetPaused(upID, true)
	r.start()
	if ans := r.p.Register(regReq(upID)); !ans.Paused {
		t.Fatal("registration answer does not carry the pause")
	}
	r.heartbeat(upID)
	r.p.SetPaused(upID, false)
	r.wantPlaceable(upID, false)
	r.heartbeat(upID)
	r.wantPlaceable(upID, true)
}

// A heartbeat answered 410 (unknown agent) tells the agent nothing; the
// registration that follows does.
func TestUnpauseIsDeliveredByTheRegistrationAnswer(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq(upID))
	r.p.SetPaused(upID, true)
	r.heartbeat(upID)
	r.p.SetPaused(upID, false)
	r.wantPlaceable(upID, false)
	r.p.Register(regReq(upID)) // the agent registers again
	r.heartbeat(upID)
	r.wantPlaceable(upID, true)
}
