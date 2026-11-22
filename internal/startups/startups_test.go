package startups

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func TestClassify(t *testing.T) {
	boot := func(id string, agent float64, us *float64, soft *int64) Facts {
		return Facts{BootID: id, AgentStartS: agent, UserspaceStartS: us, SoftReboots: soft}
	}
	for _, c := range []struct {
		name     string
		prev     *Facts
		cur      Facts
		kind     string
		inferred bool
	}{
		{"full: another boot id", &Facts{BootID: "a", AgentStartS: 20, UserspaceStartS: f64(3), SoftReboots: i64(0)},
			boot("b", 25, f64(3.5), i64(0)), KindFull, false},
		{"full: another boot id, no systemd figures (Docker)", &Facts{BootID: "a", AgentStartS: 40}, boot("b", 60, nil, nil), KindFull, false},
		{"soft: higher soft-restart count", &Facts{BootID: "a", AgentStartS: 20, UserspaceStartS: f64(3), SoftReboots: i64(0)},
			boot("a", 5000, f64(4980), i64(1)), KindSoft, false},
		{"soft: later userspace start, systemd older than 254", &Facts{BootID: "a", AgentStartS: 20, UserspaceStartS: f64(3)},
			boot("a", 5000, f64(4980), nil), KindSoft, false},
		{"agent restart: same boot, same userspace", &Facts{BootID: "a", AgentStartS: 20, UserspaceStartS: f64(3), SoftReboots: i64(0)},
			boot("a", 900, f64(3), i64(0)), KindAgentRestart, false},
		{"agent restart: same boot, systemd figures missing", &Facts{BootID: "a", AgentStartS: 20}, boot("a", 900, nil, nil), KindAgentRestart, true},
		{"agent restart: same boot, Docker container restarted", &Facts{BootID: "a", AgentStartS: 30}, boot("a", 7200, nil, nil), KindAgentRestart, true},
		{"first record: full", nil, boot("a", 25, f64(3), i64(0)), KindFull, true},
		{"first record: soft", nil, boot("a", 5000, f64(4980), i64(1)), KindSoft, true},
		{"first record: agent long after userspace", nil, boot("a", 9000, f64(3), i64(0)), KindAgentRestart, true},
		{"first record: userspace long after boot without a soft count", nil, boot("a", 5000, f64(4980), nil), KindAgentRestart, true},
		{"first record, no systemd figures: early agent is a full start", nil, boot("a", 40, nil, nil), KindFull, true},
		{"first record, no systemd figures: late agent", nil, boot("a", 4000, nil, nil), KindAgentRestart, true},
		{"previous record without a boot id", &Facts{AgentStartS: 20}, boot("a", 30, f64(3), i64(0)), KindFull, true},
	} {
		kind, inferred, note := Classify(c.prev, c.cur)
		if kind != c.kind || inferred != c.inferred || note == "" {
			t.Errorf("%s: got %s inferred=%v (%s), want %s inferred=%v", c.name, kind, inferred, note, c.kind, c.inferred)
		}
	}
}

var t0 = time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)

func report(marks map[string]float64) *agentproto.Startup {
	return &agentproto.Startup{BootID: "b", AgentStartS: 20, UserspaceStartS: f64(3), SoftReboots: i64(0), Marks: marks, ExternalMarks: map[string]float64{}}
}

// The figure is the latest of registered, programs_ready and share_mounted,
// whatever order they arrive in; with one missing there is none.
func TestHeadlineInEveryOrder(t *testing.T) {
	marks := map[string]float64{"registered": 23.5, "programs_ready": 41.25, "share_mounted": 30}
	orders := [][]string{
		{"registered", "programs_ready", "share_mounted"}, {"registered", "share_mounted", "programs_ready"},
		{"programs_ready", "registered", "share_mounted"}, {"programs_ready", "share_mounted", "registered"},
		{"share_mounted", "registered", "programs_ready"}, {"share_mounted", "programs_ready", "registered"},
	}
	for _, order := range orders {
		r := New("n1", "console-1", "0.1.3", "bare", "s1", true, t0, report(map[string]float64{"agent_start": 20}), nil)
		if r.Kind != KindFull {
			t.Fatalf("kind %s", r.Kind)
		}
		for i, m := range order {
			r.Merge(report(map[string]float64{m: marks[m]}))
			r.Evaluate(t0.Add(time.Duration(i+1)*2*time.Second), "")
			if i < 2 && (r.HeadlineS != nil || r.Missing == "" || !r.Open) {
				t.Fatalf("%v after %d marks: %+v", order, i+1, r)
			}
		}
		if r.HeadlineS == nil || *r.HeadlineS != 41.25 || r.Open || r.Missing != "" {
			t.Fatalf("%v: %+v", order, r)
		}
		if !r.ReadyAt.Equal(t0.Add(17750 * time.Millisecond)) { // registered 23.5 at t0, ready 41.25
			t.Fatalf("%v: ready at %v", order, r.ReadyAt)
		}
	}
	// One missing: no figure, and the record says which.
	r := New("n1", "console-1", "0.1.3", "bare", "s1", true, t0, report(map[string]float64{"registered": 23, "programs_ready": 41}), nil)
	r.Evaluate(t0.Add(time.Minute), "")
	if r.HeadlineS != nil || r.Missing != "share_mounted" || r.Condition() != "missing share_mounted" {
		t.Fatalf("%+v", r)
	}
	// It never comes: the record closes without a figure, still saying so.
	if r.Expire(t0.Add(Lifetime - time.Second)) {
		t.Fatal("expired early")
	}
	if !r.Expire(t0.Add(Lifetime)) || r.Open || r.HeadlineS != nil || r.Missing != "share_mounted" {
		t.Fatalf("%+v", r)
	}
	if r.Evaluate(t0.Add(Lifetime+time.Minute), "") {
		t.Fatal("a closed record changed")
	}
	// A push node does not need the share.
	r = New("n1", "x", "0.1.3", "bare", "s2", false, t0, report(map[string]float64{"registered": 23, "programs_ready": 41}), nil)
	r.Evaluate(t0, "")
	if r.HeadlineS == nil || *r.HeadlineS != 41 {
		t.Fatalf("%+v", r)
	}
}

// A soft restart counts from the userspace start.
func TestHeadlineOfASoftRestart(t *testing.T) {
	prev := New("n1", "x", "0.1.3", "bare", "s0", true, t0, report(map[string]float64{}), nil)
	rep := &agentproto.Startup{BootID: "b", AgentStartS: 5003, UserspaceStartS: f64(4990.5), SoftReboots: i64(1),
		Marks: map[string]float64{"registered": 5004, "programs_ready": 5010, "share_mounted": 5006}, ExternalMarks: map[string]float64{}}
	r := New("n1", "x", "0.1.3", "bare", "s1", true, t0, rep, prev)
	r.Evaluate(t0, "")
	if r.Kind != KindSoft || r.Inferred || r.HeadlineS == nil || *r.HeadlineS != 19.5 {
		t.Fatalf("%+v", r)
	}
	// An agent restart gets no figure and is closed at once.
	rep2 := &agentproto.Startup{BootID: "b", AgentStartS: 6000, UserspaceStartS: f64(4990.5), SoftReboots: i64(1),
		Marks: map[string]float64{"registered": 6001, "programs_ready": 6002, "share_mounted": 6001}, ExternalMarks: map[string]float64{}}
	a := New("n1", "x", "0.1.3", "bare", "s2", true, t0, rep2, r)
	if a.Kind != KindAgentRestart || a.Open || a.Evaluate(t0, "") || a.HeadlineS != nil || a.Condition() != "" {
		t.Fatalf("%+v", a)
	}
}

// A reason other than a pause holds the figure back; when it clears, the
// figure is the moment it cleared, mapped onto the node's axis.
func TestBlockedByAndItsClearing(t *testing.T) {
	r := New("n1", "x", "0.1.3", "bare", "s1", true, t0,
		report(map[string]float64{"registered": 23.5, "programs_ready": 41.25, "share_mounted": 30}), nil)
	if !r.Evaluate(t0.Add(18*time.Second), "no_tier_match") || r.BlockedBy != "no_tier_match" || r.HeadlineS != nil ||
		r.Condition() != "blocked_by no_tier_match" {
		t.Fatalf("%+v", r)
	}
	// Still blocked: nothing changes.
	if r.Evaluate(t0.Add(30*time.Second), "no_tier_match") {
		t.Fatal("changed while still blocked")
	}
	// A pause someone set does not hold it back; the reason cleared 100 s
	// after the registration: 23.5 + 100.
	if !r.Evaluate(t0.Add(100*time.Second), "paused") {
		t.Fatal("no change when the reason cleared")
	}
	if r.HeadlineS == nil || *r.HeadlineS != 123.5 || r.WasBlockedBy != "no_tier_match" || r.BlockedBy != "" || r.Open ||
		!r.ReadyAt.Equal(t0.Add(100*time.Second)) {
		t.Fatalf("%+v", r)
	}
	// Paused from the start: a figure all the same.
	p := New("n1", "x", "0.1.3", "bare", "s2", true, t0,
		report(map[string]float64{"registered": 23.5, "programs_ready": 41.25, "share_mounted": 30}), nil)
	p.Evaluate(t0, "paused")
	if p.HeadlineS == nil || *p.HeadlineS != 41.25 {
		t.Fatalf("%+v", p)
	}
	// Blocked until the lifetime ends: no figure, the reason stays.
	b := New("n1", "x", "0.1.3", "bare", "s3", true, t0,
		report(map[string]float64{"registered": 23.5, "programs_ready": 41.25, "share_mounted": 30}), nil)
	b.Evaluate(t0, "bad_floor")
	b.Expire(t0.Add(Lifetime))
	if b.Open || b.HeadlineS != nil || b.BlockedBy != "bad_floor" {
		t.Fatalf("%+v", b)
	}
}

// A requested start gets its away times on Potluck's clock.
func TestRequestedStart(t *testing.T) {
	call := t0.Add(-90 * time.Second)
	r := New("n1", "x", "0.1.3", "bare", "s1", true, t0, &agentproto.Startup{BootID: "c", AgentStartS: 20,
		Marks: map[string]float64{"registered": 23}, ExternalMarks: map[string]float64{}}, &Record{BootID: "b"})
	r.MarkRequested("full", "kernel update", call, t0)
	r.Merge(report(map[string]float64{"programs_ready": 40, "share_mounted": 25}))
	r.Evaluate(t0.Add(20*time.Second), "")
	if !r.Requested || *r.AwayS != 90 || r.AwayToReadyS == nil || *r.AwayToReadyS != 107 || r.RequestedKind != "full" {
		t.Fatalf("%+v", r)
	}
}

func TestMergeKeepsFirstValuesAndBounds(t *testing.T) {
	r := New("n1", "x", "0.1.3", "bare", "s1", true, t0, report(map[string]float64{"registered": 23}), nil)
	ext := map[string]float64{"bad name": 1, "ok": 2}
	for i := 0; i < 40; i++ {
		ext[fmt.Sprintf("m%02d", i)] = float64(i)
	}
	if !r.Merge(&agentproto.Startup{Marks: map[string]float64{"registered": 99, "programs_ready": -1}, ExternalMarks: ext}) {
		t.Fatal("no change")
	}
	if r.Marks["registered"] != 23 || len(r.ExternalMarks) != agentproto.StartupMaxExternalMarks {
		t.Fatalf("%v %d", r.Marks, len(r.ExternalMarks))
	}
	if _, ok := r.Marks["programs_ready"]; ok {
		t.Fatal("a negative mark was taken")
	}
	if _, ok := r.ExternalMarks["bad name"]; ok {
		t.Fatal("a bad name was taken")
	}
	if r.Merge(&agentproto.Startup{Marks: map[string]float64{"registered": 23}}) {
		t.Fatal("the same marks again are a change")
	}
	order := r.MarksInOrder()
	for i := 1; i < len(order); i++ {
		if order[i].S < order[i-1].S {
			t.Fatalf("marks out of order: %v", order)
		}
	}
}

// The aggregates never put two kinds into one figure.
func TestAggregatesNeverMixKinds(t *testing.T) {
	var recs []Record
	add := func(node, kind string, h float64, minute int, requested bool) {
		r := Record{ID: int64(len(recs) + 1), NodeID: node, NodeName: "name-" + node, Kind: kind,
			RegisteredAt: t0.Add(time.Duration(minute) * time.Minute), Requested: requested}
		if h >= 0 {
			r.HeadlineS = f64(h)
		}
		recs = append(recs, r)
	}
	add("n1", KindFull, 40, 1, false)
	add("n1", KindFull, 60, 2, false)
	add("n1", KindFull, 50, 3, true)
	add("n1", KindFull, -1, 4, false) // no figure
	add("n1", KindSoft, 10, 5, true)
	add("n1", KindSoft, 12, 6, false)
	add("n1", KindAgentRestart, -1, 7, false)
	add("n2", KindFull, 90, 1, false)
	ag := Aggregates(recs)
	if len(ag) != 4 {
		t.Fatalf("%d groups", len(ag))
	}
	full, soft, ar, n2 := ag[0], ag[1], ag[2], ag[3]
	if full.Kind != KindFull || full.Count != 4 || full.WithFigure != 3 || *full.LastS != 50 || *full.MedianS != 50 || *full.WorstS != 60 ||
		full.Requested != 1 || full.Last.HeadlineS != nil {
		t.Fatalf("full %+v", full)
	}
	if soft.Kind != KindSoft || soft.Count != 2 || *soft.LastS != 12 || *soft.MedianS != 11 || *soft.WorstS != 12 {
		t.Fatalf("soft %+v", soft)
	}
	if ar.Kind != KindAgentRestart || ar.Count != 1 || ar.LastS != nil || ar.MedianS != nil {
		t.Fatalf("agent restart %+v", ar)
	}
	if n2.NodeID != "n2" || *n2.WorstS != 90 {
		t.Fatalf("n2 %+v", n2)
	}
}

// The store: a record survives the round trip, it is found by its agent
// start, and a node keeps its newest 500.
func TestStoreRoundTripAndCap(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil { // again: nothing to migrate
		t.Fatal(err)
	}
	for name, s := range map[string]interface {
		Insert(*Record) error
		Update(*Record) error
		List(string, int) ([]Record, error)
		All() ([]Record, error)
		Latest(string) (*Record, error)
		ByStart(string, string) (*Record, error)
	}{"sqlite": st, "memory": &MemStore{}} {
		r := New("n1", "console-1", "0.1.3", "bare", "s1", true, t0, &agentproto.Startup{BootID: "b", AgentStartS: 20.5,
			UserspaceStartS: f64(3.25), SoftReboots: i64(0), Marks: map[string]float64{"registered": 23.5},
			ExternalMarks: map[string]float64{"network_ok": 9.5}}, nil)
		if err := s.Insert(r); err != nil || r.ID == 0 {
			t.Fatalf("%s: insert %v", name, err)
		}
		r.Merge(report(map[string]float64{"programs_ready": 41, "share_mounted": 30}))
		r.Evaluate(t0.Add(time.Minute), "")
		if err := s.Update(r); err != nil {
			t.Fatalf("%s: update %v", name, err)
		}
		got, err := s.ByStart("n1", "s1")
		if err != nil || got == nil || got.ID != r.ID || *got.HeadlineS != 41 || got.ExternalMarks["network_ok"] != 9.5 ||
			*got.UserspaceStartS != 3.25 || !got.RegisteredAt.Equal(t0) || got.Kind != KindFull || !got.Inferred {
			t.Fatalf("%s: round trip %+v %v", name, got, err)
		}
		if x, _ := s.ByStart("n1", "other"); x != nil {
			t.Fatalf("%s: found an unknown start", name)
		}
		for i := 0; i < KeepPerNode+5; i++ {
			x := New("n2", "c2", "0.1.3", "bare", fmt.Sprintf("a%d", i), true, t0.Add(time.Duration(i)*time.Second), report(map[string]float64{}), nil)
			if err := s.Insert(x); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		l, _ := s.List("n2", 0)
		if len(l) != KeepPerNode || l[0].AgentStartID != fmt.Sprintf("a%d", KeepPerNode+4) || l[len(l)-1].AgentStartID != "a5" {
			t.Fatalf("%s: kept %d, newest %s, oldest %s", name, len(l), l[0].AgentStartID, l[len(l)-1].AgentStartID)
		}
		if l, _ := s.List("n1", 0); len(l) != 1 {
			t.Fatalf("%s: the cap of n2 touched n1", name)
		}
		if last, _ := s.Latest("n2"); last == nil || last.AgentStartID != fmt.Sprintf("a%d", KeepPerNode+4) {
			t.Fatalf("%s: latest %+v", name, last)
		}
		if all, _ := s.All(); len(all) != KeepPerNode+1 {
			t.Fatalf("%s: all %d", name, len(all))
		}
		if err := s.Insert(New("n1", "c", "0.1.3", "bare", "s1", true, t0, report(nil), nil)); err == nil {
			t.Fatalf("%s: a second record of the same agent start", name)
		}
	}
	if !strings.Contains(migrations[0], "CREATE TABLE startups") {
		t.Fatal("migration")
	}
}

// boot_origin (an external mark) is the base of the figure for every kind of
// start; without it a full start counts from the kernel start and a soft
// restart from the userspace start, and the record says which (Origin).
func TestHeadlineOrigin(t *testing.T) {
	prev := New("n1", "x", "0.1.3", "bare", "s0", false, t0, report(map[string]float64{}), nil)
	soft := func(us *float64, ext map[string]float64) *Record {
		rep := &agentproto.Startup{BootID: "b", AgentStartS: 20, UserspaceStartS: us, SoftReboots: i64(1),
			Marks: map[string]float64{"registered": 22, "programs_ready": 25.8}, ExternalMarks: ext}
		r := New("n1", "x", "0.1.3", "bare", "s1", false, t0, rep, prev)
		r.Evaluate(t0, "")
		return r
	}
	full := func(ext map[string]float64) *Record {
		rep := &agentproto.Startup{BootID: "c", AgentStartS: 20, UserspaceStartS: f64(3), SoftReboots: i64(0),
			Marks: map[string]float64{"registered": 22, "programs_ready": 25.8}, ExternalMarks: ext}
		r := New("n1", "x", "0.1.3", "bare", "s1", false, t0, rep, prev)
		r.Evaluate(t0, "")
		return r
	}
	for _, c := range []struct {
		name     string
		r        *Record
		kind     string
		headline float64
		origin   string
	}{
		{"soft restart with boot_origin: the shutdown is included", soft(f64(15.8), map[string]float64{"boot_origin": 10}), KindSoft, 15.8, OriginBootOrigin},
		{"soft restart with boot_origin_partial", soft(f64(15.8), map[string]float64{"boot_origin_partial": 15.8}), KindSoft, 10, OriginUserspace},
		{"soft restart with neither", soft(f64(15.8), map[string]float64{}), KindSoft, 10, OriginUserspace},
		{"soft restart with boot_origin, userspace start unknown", soft(nil, map[string]float64{"boot_origin": 10}), KindSoft, 15.8, OriginBootOrigin},
		{"soft restart, boot_origin after the userspace start is not taken", soft(f64(15.8), map[string]float64{"boot_origin": 16}), KindSoft, 10, OriginUserspace},
		{"full start with boot_origin 0", full(map[string]float64{"boot_origin": 0}), KindFull, 25.8, OriginBootOrigin},
		{"full start without", full(map[string]float64{}), KindFull, 25.8, OriginKernel},
		{"full start with boot_origin_partial", full(map[string]float64{"boot_origin_partial": 3}), KindFull, 25.8, OriginKernel},
		{"boot_origin after ready is not taken", full(map[string]float64{"boot_origin": 30}), KindFull, 25.8, OriginKernel},
	} {
		if c.r.Kind != c.kind || c.r.HeadlineS == nil || *c.r.HeadlineS != c.headline || c.r.Origin != c.origin {
			t.Errorf("%s: kind %s headline %v origin %q; want %s %v %q (%s)", c.name, c.r.Kind, c.r.HeadlineS, c.r.Origin,
				c.kind, c.headline, c.origin, c.r.Condition())
		}
		if l := LastOf(c.r); l.Origin != c.origin {
			t.Errorf("%s: last %+v", c.name, l)
		}
	}
	// A soft restart without boot_origin still waits for the userspace start.
	if r := soft(nil, map[string]float64{"boot_origin_partial": 15.8}); r.HeadlineS != nil || r.Missing != "userspace_start" || r.Origin != "" {
		t.Errorf("no userspace start: %+v", r)
	}
	// An agent restart has no figure and no origin.
	a := New("n1", "x", "0.1.3", "bare", "s9", false, t0, report(map[string]float64{"registered": 22, "programs_ready": 25}), prev)
	a.ExternalMarks["boot_origin"] = 0
	if a.Evaluate(t0, ""); a.Kind != KindAgentRestart || a.Origin != "" {
		t.Errorf("agent restart %+v", a)
	}
}

// A record stored before the origin existed reads unchanged: no origin, the
// same figure.
func TestOldRecordReadsUnchanged(t *testing.T) {
	old := `{"id":7,"node_id":"n1","node_name":"x","release":"0.1.4","agent_start_id":"s1","kind":"soft","inferred":false,` +
		`"registered_at":"2026-11-01T10:00:00Z","boot_id":"b","agent_start_s":5003,"userspace_start_s":4990.5,"soft_reboots":1,` +
		`"marks":{"registered":5004,"programs_ready":5010},"external_marks":{"boot_origin":4980},"need_share":false,` +
		`"headline_s":19.5,"ready_s":5010,"open":false,"requested":false}`
	var r Record
	if err := json.Unmarshal([]byte(old), &r); err != nil {
		t.Fatal(err)
	}
	if r.Origin != "" || *r.HeadlineS != 19.5 || r.Evaluate(t0, "") {
		t.Fatalf("%+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), `"origin"`) {
		t.Fatalf("an old record gained an origin: %s", b)
	}
}

// A group whose figures count from different origins says so.
func TestAggregatesOrigin(t *testing.T) {
	recs := []Record{
		{ID: 1, NodeID: "n1", Kind: KindSoft, RegisteredAt: t0, HeadlineS: f64(10), Origin: OriginUserspace},
		{ID: 2, NodeID: "n1", Kind: KindSoft, RegisteredAt: t0.Add(time.Minute), HeadlineS: f64(16), Origin: OriginBootOrigin},
		{ID: 3, NodeID: "n2", Kind: KindFull, RegisteredAt: t0, HeadlineS: f64(30)}, // stored before the origin
		{ID: 4, NodeID: "n2", Kind: KindFull, RegisteredAt: t0.Add(time.Minute), HeadlineS: f64(31), Origin: OriginBootOrigin},
		{ID: 5, NodeID: "n2", Kind: KindFull, RegisteredAt: t0.Add(2 * time.Minute), HeadlineS: f64(32), Origin: OriginKernel},
		{ID: 6, NodeID: "n3", Kind: KindSoft, RegisteredAt: t0, HeadlineS: f64(10)}, // stored before the origin
		{ID: 7, NodeID: "n3", Kind: KindSoft, RegisteredAt: t0.Add(time.Minute), HeadlineS: f64(11), Origin: OriginUserspace},
	}
	ag := Aggregates(recs)
	// Every full start counts from kernel start; an old soft record counted from userspace.
	if len(ag) != 3 || ag[0].Origin != OriginBootOrigin || !ag[0].MixedOrigins || ag[1].Origin != OriginKernel || ag[1].MixedOrigins ||
		ag[2].Origin != OriginUserspace || ag[2].MixedOrigins {
		t.Fatalf("%+v", ag)
	}
}

// With boot_origin and boot_ready (the console's boot screen finished), the
// figure ends at the later of boot_ready and Potluck's own readiness point,
// so it equals the console's own figure whenever the boot screen finishes
// last. Potluck's own point stays as placeable_s; headline_end says which
// ended the figure.
func TestHeadlineEndsAtBootReady(t *testing.T) {
	prev := New("n1", "x", "0.1.6", "bare", "s0", false, t0, report(map[string]float64{}), nil)
	soft := func(ext map[string]float64) *Record {
		rep := &agentproto.Startup{BootID: "b", AgentStartS: 222000, UserspaceStartS: f64(222001.5), SoftReboots: i64(1),
			Marks: map[string]float64{"registered": 222003, "programs_ready": 222007.002}, ExternalMarks: ext}
		r := New("n1", "x", "0.1.6", "bare", "s1", false, t0, rep, prev)
		r.Evaluate(t0, "")
		return r
	}
	// The console's canary: exactly these three marks.
	r := soft(map[string]float64{"boot_origin": 221998.764, "boot_ready": 222008.15})
	if r.HeadlineS == nil || *r.HeadlineS != 9.386 || r.PlaceableS == nil || *r.PlaceableS != 8.238 ||
		r.HeadlineEnd != HeadlineEndBootReady || r.Origin != OriginBootOrigin || *r.ReadyS != 222007.002 {
		t.Fatalf("canary %+v", r)
	}
	if l := LastOf(r); *l.HeadlineS != 9.386 || *l.PlaceableS != 8.238 || l.HeadlineEnd != HeadlineEndBootReady {
		t.Fatalf("last %+v", l)
	}
	// boot_ready before Potluck's own point: the own point.
	r = soft(map[string]float64{"boot_origin": 221998.764, "boot_ready": 222005})
	if *r.HeadlineS != 8.238 || *r.PlaceableS != 8.238 || r.HeadlineEnd != HeadlineEndPlaceable {
		t.Fatalf("earlier boot_ready %+v", r)
	}
	// No boot_ready, or no boot_origin: as before, no new fields.
	for _, ext := range []map[string]float64{{"boot_origin": 221998.764}, {"boot_ready": 222008.15}} {
		r = soft(ext)
		if r.PlaceableS != nil || r.HeadlineEnd != "" {
			t.Fatalf("%v: %+v", ext, r)
		}
		want := 8.238
		if _, ok := ext["boot_origin"]; !ok {
			want = 5.502 // from the userspace start
		}
		if *r.HeadlineS != want {
			t.Fatalf("%v: headline %v", ext, *r.HeadlineS)
		}
	}
	// A boot_ready earlier than boot_origin is not taken.
	r = soft(map[string]float64{"boot_origin": 221998.764, "boot_ready": 221990})
	if *r.HeadlineS != 8.238 || r.HeadlineEnd != "" {
		t.Fatalf("boot_ready before boot_origin %+v", r)
	}
}

// boot_ready is written late: a figure taken from Potluck's own marks is
// updated when boot_ready arrives in a later report, the stored record
// follows, and so do the group figures.
func TestLateBootReadyUpdatesTheFigure(t *testing.T) {
	st := &MemStore{}
	prev := New("n1", "x", "0.1.6", "bare", "s0", false, t0, report(map[string]float64{}), nil)
	rep := &agentproto.Startup{BootID: "b", AgentStartS: 222000, UserspaceStartS: f64(222001.5), SoftReboots: i64(1),
		Marks: map[string]float64{"registered": 222003, "programs_ready": 222007.002}, ExternalMarks: map[string]float64{"boot_origin": 221998.764}}
	r := New("n1", "x", "0.1.6", "bare", "s1", false, t0, rep, prev)
	if err := st.Insert(r); err != nil {
		t.Fatal(err)
	}
	r.Evaluate(t0, "")
	if *r.HeadlineS != 8.238 || r.Open || r.HeadlineEnd != "" {
		t.Fatalf("first figure %+v", r)
	}
	st.Update(r)
	later := &agentproto.Startup{BootID: "b", AgentStartS: 222000, Marks: map[string]float64{},
		ExternalMarks: map[string]float64{"boot_origin": 221998.764, "boot_ready": 222008.15}}
	if !r.Merge(later) {
		t.Fatal("the late boot_ready changed nothing")
	}
	st.Update(r)
	got, _ := st.ByStart("n1", "s1")
	if *got.HeadlineS != 9.386 || *got.PlaceableS != 8.238 || got.HeadlineEnd != HeadlineEndBootReady {
		t.Fatalf("stored %+v", got)
	}
	all, _ := st.All()
	for _, g := range Aggregates(all) {
		if g.Kind == KindSoft && (*g.LastS != 9.386 || *g.MedianS != 9.386 || *g.WorstS != 9.386) {
			t.Fatalf("group %+v", g)
		}
	}
	// The same report again changes nothing.
	if r.Merge(later) {
		t.Fatal("changed again")
	}
}

// A record stored before placeable_s and headline_end reads unchanged, also
// one that has boot_ready among its marks: a merge without a new mark does
// not refigure it.
func TestOldRecordWithBootReadyReadsUnchanged(t *testing.T) {
	old := `{"id":7,"node_id":"n1","node_name":"x","release":"0.1.5","agent_start_id":"s1","kind":"soft","inferred":false,` +
		`"registered_at":"2026-11-01T10:00:00Z","boot_id":"b","agent_start_s":5003,"userspace_start_s":4990.5,"soft_reboots":1,` +
		`"marks":{"registered":5004,"programs_ready":5010},"external_marks":{"boot_origin":4980,"boot_ready":5020},"need_share":false,` +
		`"headline_s":30,"origin":"boot_origin","ready_s":5010,"open":false,"requested":false}`
	var r Record
	if err := json.Unmarshal([]byte(old), &r); err != nil {
		t.Fatal(err)
	}
	if *r.HeadlineS != 30 || r.PlaceableS != nil || r.HeadlineEnd != "" || r.Evaluate(t0, "") {
		t.Fatalf("%+v", r)
	}
	if r.Merge(&agentproto.Startup{BootID: "b", Marks: map[string]float64{}, ExternalMarks: map[string]float64{"boot_ready": 5020}}) {
		t.Fatal("an unchanged mark changed an old record")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "placeable_s") || strings.Contains(string(b), "headline_end") {
		t.Fatalf("%s", b)
	}
}
