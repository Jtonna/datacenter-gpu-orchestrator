package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// CR-9: the agent reports its watchdog floor at registration and in every
// heartbeat.
func TestWatchdogFloorReported(t *testing.T) {
	e := newTenvWith(t, func(o *Options, d *Deps) { o.WatchdogFloorMiB = 1500 })
	e.start()
	e.waitReady()
	if got := e.pl.lastRegister().WatchdogFloorMiB; got != 1500 {
		t.Fatalf("registration watchdog_floor_mib = %d, want 1500", got)
	}
	if got := e.pl.lastBeat().WatchdogFloorMiB; got != 1500 {
		t.Fatalf("heartbeat watchdog_floor_mib = %d, want 1500", got)
	}
}

// CR-9: POST /launch with an expect_mib that does not fit the node's device
// (here one that shares system memory: MemAvailable 12000, floor 1000) is
// answered 409 would_not_fit and starts nothing.
func TestWouldNotFitRefusal(t *testing.T) {
	e := newTenv(t)
	e.start()
	e.waitReady()
	req := workerReq("ln-1")
	req.Worker.ExpectMiB = 11500
	w := e.do("POST", "/launch", req, fromPotluck)
	var ref agentproto.LaunchRefusal
	json.Unmarshal(w.Body.Bytes(), &ref)
	if w.Code != 409 || ref.Reason != agentproto.ReasonWouldNotFit {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var st agentproto.LaunchState
	json.Unmarshal(e.do("GET", "/launch", nil, fromPotluck).Body.Bytes(), &st)
	if st.State != agentproto.StateNone {
		t.Fatalf("state %s after the refusal", st.State)
	}
	// The same launch without expect_mib (a release 0.1.0 scheduler) starts.
	if w := e.do("POST", "/launch", workerReq("ln-2"), fromPotluck); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// CR-9: a stop while the leave marker exists deregisters first, so the node
// shows left and not lost; without the marker a stop stays silent.
func TestLeaveOnPurpose(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "disabled")
	deregs := func(e *tenv) []string {
		e.pl.mu.Lock()
		defer e.pl.mu.Unlock()
		return append([]string(nil), e.pl.deregs...)
	}
	t.Run("no marker", func(t *testing.T) {
		e := newTenvWith(t, func(o *Options, d *Deps) { o.LeaveMarker = marker })
		e.start()
		e.waitReady()
		e.stop()
		if got := deregs(e); len(got) != 0 {
			t.Fatalf("deregistered without the marker: %v", got)
		}
	})
	t.Run("marker", func(t *testing.T) {
		e := newTenvWith(t, func(o *Options, d *Deps) { o.LeaveMarker = marker })
		e.start()
		e.waitReady()
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		e.stop()
		got := deregs(e)
		if len(got) != 1 || got[0] != "POST /api/pool/nodes/02:00:00:00:00:ab/deregister" {
			t.Fatalf("deregister calls: %v", got)
		}
	})
}
