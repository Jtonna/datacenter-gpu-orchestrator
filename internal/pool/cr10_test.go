package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

func testLogs(rev *int64) func(string) *agentproto.Logs {
	return func(id string) *agentproto.Logs {
		if *rev == 0 {
			return nil
		}
		return &agentproto.Logs{Revision: *rev, URL: "/api/pool/nodes/" + id + "/logs", Format: agentproto.LogsFormatJSONLine, DenyUnits: []string{}}
	}
}

// CR-10: the registration answer carries the logs object and the heartbeat
// answer its revision; with logging off (or no Logs function at all, as
// before CR-10) neither names it.
func TestLogsObjectInRegistrationAndRevisionInHeartbeat(t *testing.T) {
	r := newRig(t)
	const id = "02:00:00:00:00:70"
	// Without a Logs function the answers are those of release 0.1.0.
	ans := r.p.Register(regReq(id))
	hb := r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	if !ans.Accepted || ans.Logs != nil || hb.Status != http.StatusOK || hb.Answer.LogsRevision != 0 {
		t.Fatalf("without logging: %+v %+v", ans, hb)
	}
	if b, _ := json.Marshal(hb.Answer); string(b) != `{"release":"0.1.0","paused":false}` {
		t.Fatalf("heartbeat answer without logging: %s", b)
	}
	if b, _ := json.Marshal(ans); strings.Contains(string(b), "logs") {
		t.Fatalf("registration answer without logging names logs: %s", b)
	}

	rev := int64(7)
	r.p.o.Logs = testLogs(&rev)
	ans = r.p.Register(regReq(id))
	if ans.Logs == nil || ans.Logs.Revision != 7 || ans.Logs.URL != "/api/pool/nodes/"+id+"/logs" {
		t.Fatalf("registration answer: %+v", ans.Logs)
	}
	if hb = r.p.Heartbeat(id, hbBody("s1", "", true, "true")); hb.Answer.LogsRevision != 7 {
		t.Fatalf("heartbeat answer: %+v", hb.Answer)
	}
	// A changed setting reaches the agent with its next heartbeat.
	rev = 8
	if hb = r.p.Heartbeat(id, hbBody("s1", "", true, "true")); hb.Answer.LogsRevision != 8 {
		t.Fatalf("heartbeat answer after a change: %+v", hb.Answer)
	}
	// Logging switched off: no revision, so the agent stops shipping.
	rev = 0
	if hb = r.p.Heartbeat(id, hbBody("s1", "", true, "true")); hb.Answer.LogsRevision != 0 {
		t.Fatalf("heartbeat answer with logging off: %+v", hb.Answer)
	}
	if ans = r.p.Register(regReq(id)); !ans.Accepted || ans.Logs != nil {
		t.Fatalf("registration answer with logging off: %+v", ans)
	}
}

// CR-10: the agent's shipping figures are optional. A release 0.1.0
// heartbeat has none; an unreadable object never rejects the heartbeat.
func TestShippingFiguresFromTheHeartbeat(t *testing.T) {
	r := newRig(t)
	const id = "02:00:00:00:00:70"
	r.p.Register(regReq(id))
	if res := r.p.Heartbeat(id, hbBody("s1", "", true, "true")); res.Status != http.StatusOK {
		t.Fatalf("%+v", res)
	}
	if f := r.fact(id); f.Logs != nil {
		t.Fatalf("a 0.1.0 heartbeat gave logs facts: %+v", f.Logs)
	}
	if b, _ := json.Marshal(r.fact(id)); strings.Contains(string(b), `"logs"`) {
		t.Fatalf("status of a node without logging names logs: %s", b)
	}
	with := func(logs string) []byte {
		b := hbBody("s1", "", true, "true")
		return append(b[:len(b)-1:len(b)-1], []byte(`,"logs":`+logs+`}`)...)
	}
	if res := r.p.Heartbeat(id, with(`{"lines_sent":120,"lines_dropped":3,"last_error":"429 rate_limited","last_error_age_s":4.5}`)); res.Status != http.StatusOK {
		t.Fatalf("%+v", res)
	}
	f := r.fact(id)
	if f.Logs == nil || f.Logs.Agent == nil || f.Logs.Agent.LinesSent != 120 || f.Logs.Agent.LinesDropped != 3 ||
		f.Logs.Agent.LastError != "429 rate_limited" || f.Logs.Agent.LastErrorAgeS == nil || *f.Logs.Agent.LastErrorAgeS != 4.5 {
		t.Fatalf("facts %+v", f.Logs)
	}
	for _, bad := range []string{`"soon"`, `[1,2]`, `{"lines_sent":"many"}`, `null`} {
		if res := r.p.Heartbeat(id, with(bad)); res.Status != http.StatusOK {
			t.Fatalf("logs %s rejected the heartbeat: %+v", bad, res)
		}
		if f := r.fact(id); f.Logs != nil || !f.ProgramsReady {
			t.Fatalf("logs %s: facts %+v", bad, f)
		}
	}
}

// CR-10, design 4.4: the log routes read a snapshot and never take the
// pool's lock.
func TestLogSnapshotFollowsMembershipWithoutTheLock(t *testing.T) {
	r := newRig(t)
	const id = "02:00:00:00:00:70"
	if _, ok := r.p.LogSnapshot().Node(id); ok {
		t.Fatal("an unregistered node is in the snapshot")
	}
	r.p.Register(regReq(id))
	n, ok := r.p.LogSnapshot().Node(id)
	if !ok || n != (LogNode{StartID: "s1", Name: "n", Platform: "linux-amd64-vulkan", Release: "0.1.0"}) {
		t.Fatalf("snapshot node %+v %v", n, ok)
	}
	// The snapshot is read while the pool's lock is held by someone else.
	r.p.mu.Lock()
	done := make(chan LogNode, 1)
	go func() { n, _ := r.p.LogSnapshot().Node(id); done <- n }()
	select {
	case got := <-done:
		if got.StartID != "s1" {
			t.Errorf("under the lock: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Error("reading the snapshot waited for the pool's lock")
	}
	r.p.mu.Unlock()

	// An agent restart: the new start id is the one that counts.
	req := regReq(id)
	req.AgentStartID, req.Name = "s2", "renamed"
	r.p.Register(req)
	if n, _ := r.p.LogSnapshot().Node(id); n.StartID != "s2" || n.Name != "renamed" {
		t.Fatalf("after a new registration: %+v", n)
	}
	// A heartbeat publishes nothing new.
	before := r.p.LogSnapshot()
	r.p.Heartbeat(id, hbBody("s2", "", true, "true"))
	if r.p.LogSnapshot() != before {
		t.Fatal("a heartbeat published a new snapshot")
	}
	// A node that left, or was removed, would get 410 on a heartbeat: gone.
	r.p.Deregister(context.Background(), id)
	if _, ok := r.p.LogSnapshot().Node(id); ok {
		t.Fatal("a node that left is still in the snapshot")
	}
	r.p.Register(regReq(id))
	if _, ok := r.p.LogSnapshot().Node(id); !ok {
		t.Fatal("a node that registered again is not in the snapshot")
	}
	if err := r.p.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.p.LogSnapshot().Node(id); ok {
		t.Fatal("a removed node is still in the snapshot")
	}
}

// CR-10: the profile of a launch is looked up from its id, for the node that
// runs it, while it runs and for its last lines after it ended.
func TestLogSnapshotKnowsTheProfileOfALaunch(t *testing.T) {
	r := newRig(t)
	a1, a2 := r.agent(1), r.agent(2)
	r.waitNode(a1.ID)
	r.waitNode(a2.ID)
	s := spec(51, a1, a2)
	s.Profile = "gemma-4-12b"
	res := r.p.Launch(context.Background(), s, func(scheduler.WorkerReport) {})
	if res.Reason != "" {
		t.Fatalf("%+v", res)
	}
	lid := res.LaunchIDs[s.Workers[0].Node]
	snap := r.p.LogSnapshot()
	if p, ok := snap.Profile(lid, a1.ID); !ok || p != "gemma-4-12b" {
		t.Fatalf("profile of %s on its node: %q %v", lid, p, ok)
	}
	if _, ok := snap.Profile(lid, a2.ID); ok {
		t.Fatal("another node got the profile of a launch that is not its own")
	}
	if _, ok := snap.Profile("ln-0-0-0", a1.ID); ok {
		t.Fatal("an unknown launch has a profile")
	}
	r.p.Stop(51, nil)
	r.p.WaitStops(10 * time.Second)
	eventually(t, 5*time.Second, func() bool { return r.fact(a1.ID).PotluckLaunchID == nil }, "the launch to be dropped")
	if p, ok := r.p.LogSnapshot().Profile(lid, a1.ID); !ok || p != "gemma-4-12b" {
		t.Fatalf("profile of the ended launch: %q %v", p, ok)
	}
	// Only the last keepEndedLaunches ended launches are remembered.
	r.p.mu.Lock()
	for i := range keepEndedLaunches {
		l := &launch{id: "ln-old-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String(), node: "x", profile: "p"}
		r.p.launches[l.id] = l
		r.p.endLaunchLocked(l, true)
	}
	n := len(r.p.ended)
	r.p.publishLogSnapLocked()
	r.p.mu.Unlock()
	if n != keepEndedLaunches {
		t.Fatalf("%d ended launches kept", n)
	}
	if _, ok := r.p.LogSnapshot().Profile(lid, a1.ID); ok {
		t.Fatal("the oldest ended launch was not forgotten")
	}
}

// CR-10, agent batch: with logging on, both answers carry Potluck's own
// clock (server_time_unix_ms), from which the agent stamps its log lines.
// With logging off they do not, so they stay the answers of release 0.1.0
// (checked byte for byte in TestLogsObjectInRegistrationAndRevisionInHeartbeat).
func TestServerTimeOnlyWithLogging(t *testing.T) {
	r := newRig(t)
	const id = "02:00:00:00:00:71"
	if ans := r.p.Register(regReq(id)); ans.ServerTimeUnixMs != 0 {
		t.Fatalf("server time without logging: %+v", ans)
	}
	rev := int64(7)
	r.p.o.Logs = testLogs(&rev)
	// The rig's clock is the real one, so the answers may fall in a later
	// millisecond than the reading taken before them.
	before := r.p.o.Clock().UnixMilli()
	ans := r.p.Register(regReq(id))
	hb := r.p.Heartbeat(id, hbBody("s1", "", true, "true"))
	after := r.p.o.Clock().UnixMilli()
	for _, got := range []int64{ans.ServerTimeUnixMs, hb.Answer.ServerTimeUnixMs} {
		if got < before || got > after {
			t.Fatalf("server time: registration %d heartbeat %d, want within %d..%d", ans.ServerTimeUnixMs, hb.Answer.ServerTimeUnixMs, before, after)
		}
	}
	b, _ := json.Marshal(hb.Answer)
	if !strings.Contains(string(b), `"server_time_unix_ms":`) {
		t.Fatalf("heartbeat answer: %s", b)
	}
	rev = 0
	if hb = r.p.Heartbeat(id, hbBody("s1", "", true, "true")); hb.Answer.ServerTimeUnixMs != 0 {
		t.Fatalf("server time with logging switched off: %+v", hb.Answer)
	}
}
