package pool

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/startups"
)

// fixedClock is Potluck's clock in these tests: it moves only when told.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fixedClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

var startT0 = time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)

// startupRig is the pool rig with a fixed clock and a start-up store that
// survives a Potluck restart.
func startupRig(t *testing.T) (*rig, *fixedClock, *startups.MemStore) {
	r := newRig(t)
	c := &fixedClock{t: startT0}
	ms := &startups.MemStore{}
	r.p.o.Clock, r.p.o.Startups = c.now, ms
	return r, c, ms
}

func f64p(v float64) *float64 { return &v }
func i64p(v int64) *int64     { return &v }

func startupReq(id, startID, boot string, agentStart float64, marks map[string]float64) agentproto.RegisterRequest {
	req := regReq(id)
	req.AgentStartID = startID
	req.ModelSource = agentproto.ModelSource{Kind: agentproto.ModelSourceShare, Target: "192.0.2.1:/models"}
	req.Startup = &agentproto.Startup{BootID: boot, AgentStartS: agentStart, UserspaceStartS: f64p(3), SoftReboots: i64p(0),
		Marks: marks, ExternalMarks: map[string]float64{}}
	return req
}

// hbWithStartup is a heartbeat that carries a start-up report.
func hbWithStartup(startID string, marks string) []byte {
	return []byte(strings.Replace(string(hbBody(startID, "", true, "true")), `"seq":1,`,
		`"seq":1,"startup":{"boot_id":"b1","agent_start_s":20,"marks":`+marks+`,"external_marks":{"network_ok":9.5}},`, 1))
}

func TestStartupRecordFromRegistrationAndHeartbeats(t *testing.T) {
	r, c, ms := startupRig(t)
	ans := r.p.Register(startupReq(upID, "s1", "b1", 20, map[string]float64{"agent_start": 20}))
	if !ans.Accepted || ans.StartupAck != 1 {
		t.Fatalf("%+v", ans)
	}
	l := r.fact(upID).LastStartup
	if l == nil || l.Kind != startups.KindFull || !l.Inferred || l.HeadlineS != nil || !l.Open ||
		l.Missing != "registered, programs_ready, share_mounted" || !l.At.Equal(startT0) {
		t.Fatalf("after registration %+v", l)
	}
	c.advance(20 * time.Second)
	res := r.p.Heartbeat(upID, hbWithStartup("s1", `{"agent_start":20,"registered":23.5,"programs_ready":41.25,"share_mounted":30}`))
	if res.Status != 200 || res.Answer.StartupAck != 5 {
		t.Fatalf("%+v", res)
	}
	l = r.fact(upID).LastStartup
	if l.HeadlineS == nil || *l.HeadlineS != 41.25 || l.Open || l.Missing != "" {
		t.Fatalf("after the marks %+v", l)
	}
	recs, _ := ms.List(upID, 0)
	if len(recs) != 1 || recs[0].ExternalMarks["network_ok"] != 9.5 || !recs[0].ReadyAt.Equal(startT0.Add(17750*time.Millisecond)) {
		t.Fatalf("stored %+v", recs)
	}
	// A heartbeat without the report (all acknowledged) changes nothing.
	if res := r.p.Heartbeat(upID, hbBody("s1", "", true, "true")); res.Status != 200 || res.Answer.StartupAck != 0 {
		t.Fatalf("%+v", res)
	}

	// Potluck restarts; the same agent process registers again: no new record.
	r.start()
	r.p.o.Clock, r.p.o.Startups = c.now, ms
	if ans := r.p.Register(startupReq(upID, "s1", "b1", 20, map[string]float64{"agent_start": 20, "registered": 23.5})); ans.StartupAck != 2 {
		t.Fatalf("%+v", ans)
	}
	if recs, _ := ms.List(upID, 0); len(recs) != 1 {
		t.Fatalf("%d records after a re-registration", len(recs))
	}
	if l := r.fact(upID).LastStartup; l == nil || *l.HeadlineS != 41.25 {
		t.Fatalf("%+v", l)
	}

	// The agent restarts on the same boot and userspace: an agent restart.
	c.advance(time.Hour)
	r.p.Register(startupReq(upID, "s2", "b1", 3620, map[string]float64{"agent_start": 3620}))
	l = r.fact(upID).LastStartup
	if l.Kind != startups.KindAgentRestart || l.Inferred || l.HeadlineS != nil || l.Open || l.Missing != "" {
		t.Fatalf("agent restart %+v", l)
	}
	// The machine starts again: a full start, known (not inferred).
	c.advance(time.Hour)
	r.p.Register(startupReq(upID, "s3", "b2", 25, map[string]float64{"agent_start": 25}))
	if l := r.fact(upID).LastStartup; l.Kind != startups.KindFull || l.Inferred {
		t.Fatalf("full start %+v", l)
	}
	if recs, _ := ms.List(upID, 0); len(recs) != 3 || recs[0].AgentStartID != "s3" || recs[1].Kind != startups.KindAgentRestart {
		t.Fatalf("%+v", recs)
	}
	// A record that never gets its marks is closed after its lifetime.
	c.advance(startups.Lifetime)
	r.p.CheckLost()
	if recs, _ := ms.List(upID, 1); recs[0].Open || recs[0].HeadlineS != nil || recs[0].Missing == "" {
		t.Fatalf("%+v", recs[0])
	}
}

// A reason other than a pause holds the figure back until it clears.
func TestStartupBlockedUntilTheAgentIsReached(t *testing.T) {
	r, c, ms := startupRig(t)
	r.failCallBack("connection refused")
	r.p.Register(startupReq(upID, "s1", "b1", 20, map[string]float64{"agent_start": 20}))
	r.p.SetPaused(upID, true) // a pause someone set does not hold it back
	c.advance(18 * time.Second)
	r.p.Heartbeat(upID, hbWithStartup("s1", `{"registered":23.5,"programs_ready":41.25,"share_mounted":30}`))
	if l := r.fact(upID).LastStartup; l.BlockedBy != "agent_unreachable" || l.HeadlineS != nil {
		t.Fatalf("%+v", l)
	}
	c.advance(82 * time.Second) // 100 s after the registration
	r.failCallBack("")
	eventually(t, 5*time.Second, func() bool { l := r.fact(upID).LastStartup; return l.HeadlineS != nil }, "the figure")
	recs, _ := ms.List(upID, 1)
	if *recs[0].HeadlineS != 123.5 || recs[0].WasBlockedBy != "agent_unreachable" || !recs[0].ReadyAt.Equal(startT0.Add(100*time.Second)) {
		t.Fatalf("%+v", recs[0])
	}
}

func TestExpectRestart(t *testing.T) {
	r, c, ms := startupRig(t)
	if err := r.p.ExpectRestart("02:00:00:00:00:99", ExpectSoft, "x"); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("unknown node: %v", err)
	}
	r.p.Register(startupReq(upID, "s1", "b1", 20, map[string]float64{"agent_start": 20}))
	var conflict *ConflictError
	if err := r.p.ExpectRestart(upID, ExpectSoft, "x"); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "not paused") {
		t.Fatalf("not paused: %v", err)
	}
	r.p.SetPaused(upID, true)
	r.p.Heartbeat(upID, hbBody("s1", "L-7", true, "true")) // a launch this run does not hold
	if err := r.p.ExpectRestart(upID, ExpectSoft, "x"); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "launch") {
		t.Fatalf("with a launch: %v", err)
	}
	r.p.Heartbeat(upID, hbBody("s1", "", true, "true"))
	if err := r.p.ExpectRestart(upID, ExpectSoft, "systemd update"); err != nil {
		t.Fatal(err)
	}
	if k, at, ok := r.p.ExpectedRestart(upID); !ok || k != ExpectSoft || !at.Equal(startT0) {
		t.Fatal("not pending")
	}
	// Away: INFO, not the ERROR of a lost agent.
	c.advance(LostAfter + time.Second)
	r.p.CheckLost()
	logs := r.logs.String()
	if !strings.Contains(logs, "agent away for an expected restart") || strings.Contains(logs, "agent lost") {
		t.Fatalf("logs: %s", logs)
	}
	r.hist.mu.Lock()
	var away history.Event
	for _, e := range r.hist.ev {
		if strings.Contains(e.Summary, "expected restart") {
			away = e
		}
	}
	r.hist.mu.Unlock()
	if away.Severity != history.Info {
		t.Fatalf("history %+v", away)
	}
	// Back 95 s after the call, a soft restart; ready 12 s later.
	c.advance(95*time.Second - LostAfter - time.Second)
	req := startupReq(upID, "s2", "b1", 5003, map[string]float64{"agent_start": 5003})
	req.Startup.UserspaceStartS, req.Startup.SoftReboots = f64p(4990.5), i64p(1)
	r.p.Register(req)
	if _, _, ok := r.p.ExpectedRestart(upID); ok {
		t.Fatal("still pending after the registration")
	}
	c.advance(12 * time.Second)
	r.p.Heartbeat(upID, []byte(strings.Replace(string(hbWithStartup("s2", `{"registered":5004,"programs_ready":5010,"share_mounted":5006}`)),
		`"agent_start_s":20`, `"agent_start_s":5003`, 1)))
	rec, _ := ms.Latest(upID)
	if !rec.Requested || rec.RequestedKind != ExpectSoft || rec.RequestComment != "systemd update" || *rec.AwayS != 95 ||
		rec.Kind != startups.KindSoft || *rec.HeadlineS != 19.5 || *rec.AwayToReadyS != 101 {
		t.Fatalf("%+v", rec)
	}
	if l := r.fact(upID).LastStartup; !l.Requested {
		t.Fatalf("%+v", l)
	}

	// Never back: expired, said once.
	if err := r.p.ExpectRestart(upID, ExpectFull, "kernel"); err != nil {
		t.Fatal(err)
	}
	c.advance(ExpectRestartFor)
	r.p.CheckLost()
	r.p.CheckLost()
	if n := strings.Count(r.logs.String(), "expected restart did not happen"); n != 1 {
		t.Fatalf("expiry logged %d times", n)
	}
	if _, _, ok := r.p.ExpectedRestart(upID); ok {
		t.Fatal("pending after it expired")
	}
	// Once expired, a lost agent is an error again.
	r.p.CheckLost()
	if !strings.Contains(r.logs.String(), "agent lost: no heartbeat") {
		t.Fatal("no error for a lost agent after the expiry")
	}
	// A left node cannot expect a restart.
	r.p.Deregister(t.Context(), upID)
	if err := r.p.ExpectRestart(upID, ExpectFull, "x"); !errors.As(err, &conflict) {
		t.Fatalf("left: %v", err)
	}
}

// An agent of 0.1.3 sends no report: no record, no acknowledgement, no error;
// a report that cannot be read never fails the heartbeat.
func TestStartupFromAnOlderAgent(t *testing.T) {
	r, _, ms := startupRig(t)
	if ans := r.p.Register(regReq(upID)); !ans.Accepted || ans.StartupAck != 0 {
		t.Fatalf("%+v", ans)
	}
	if res := r.heartbeat(upID); res.Answer.StartupAck != 0 {
		t.Fatalf("%+v", res)
	}
	if l := r.fact(upID).LastStartup; l != nil {
		t.Fatalf("%+v", l)
	}
	body := strings.Replace(string(hbBody("s1", "", true, "true")), `"seq":1,`, `"seq":1,"startup":"soon",`, 1)
	if res := r.p.Heartbeat(upID, []byte(body)); res.Status != 200 || res.Answer.StartupAck != 0 {
		t.Fatalf("%+v", res)
	}
	if all, _ := ms.All(); len(all) != 0 {
		t.Fatalf("%+v", all)
	}
	if strings.Contains(r.logs.String(), "level=ERROR") {
		t.Fatalf("logs: %s", r.logs.String())
	}
}
