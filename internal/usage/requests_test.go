package usage

import (
	"math/rand"
	"sync"
	"testing"
	"time"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *clock) add(d time.Duration) { c.set(c.now().Add(d)) }

type fakeLive struct {
	first  time.Time
	chunks int64
}

func (f *fakeLive) FirstByte() time.Time { return f.first }
func (f *fakeLive) Chunks() int64        { return f.chunks }

func begin(profile string) Begin {
	return Begin{Kind: KindV1, Path: "/v1/chat/completions", Stream: true, Profile: profile, Requested: profile,
		Caller: "anonymous", CallerKind: "default", SourceAddr: "192.0.2.50"}
}

func done(in, reused, out int) End {
	return End{Status: StatusDone, HTTPStatus: 200, TokensIn: in, Reused: reused, TokensOut: out,
		PrefillTPS: 400, DecodeTPS: 14, PrefillMS: float64(in-reused) / 400 * 1000, DecodeMS: float64(out) / 14 * 1000}
}

func recorderT(t *testing.T, dir string, c *clock) (*Recorder, *Store) {
	t.Helper()
	st := openT(t, Options{Dir: dir, Now: c.now})
	return NewRecorder(st), st
}

func TestRecorder_LifecycleLiveThenStored(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 17, 16, 8, 30, 0, time.UTC)}
	r, _ := recorderT(t, t.TempDir(), c)
	q := r.Queued(begin("gemma:32k"))
	run := r.Queued(begin("gemma:32k"))
	c.add(5 * time.Second)
	live := &fakeLive{}
	run.Started(Placement{SchedID: 7, Instance: 3, Session: "s-1", HostNode: "ps5-01",
		Nodes: []Node{{ID: "ps5-01", Name: "ps5-01", Role: "host"}, {ID: "ps5-02", Name: "ps5-02", Role: "helper"}}}, live)
	c.add(2 * time.Second)
	live.first = c.now()
	c.add(10 * time.Second)
	live.chunks = 141

	rows := r.Live()
	if len(rows) != 2 || rows[0].Status != StatusQueued || rows[1].Status != StatusRunning {
		t.Fatalf("live %+v", rows)
	}
	if rows[0].WaitMS != 17000 || rows[0].TokensOut != nil {
		t.Fatalf("queued row %+v", rows[0])
	}
	rr := rows[1]
	if rr.DurationMS != 12000 || rr.TokensOut == nil || *rr.TokensOut != 141 || !rr.Live || rr.DecodeTPS == nil || *rr.DecodeTPS != 14.1 ||
		rr.FirstTokenAt != live.first.UnixMilli() || len(rr.Nodes) != 2 || rr.HostNode != "ps5-01" {
		t.Fatalf("running row %+v", rr)
	}
	run.Ended(done(2460, 2000, 318))
	run.Ended(End{Status: StatusFailed}) // only the first end counts
	q.Ended(End{Status: StatusCancelled, Reason: ReasonClientLeftQueued, HTTPStatus: 499})
	if len(r.Live()) != 0 {
		t.Fatal("ended requests still live")
	}
	if err := r.Flush(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(run.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || *got.TokensIn != 2460 || *got.TokensReused != 2000 || *got.TokensOut != 318 || *got.PrefillTPS != 400 ||
		got.Live || got.SchedID != 7 || got.Session != "s-1" || got.Nodes[1].Role != "helper" || got.Caller != "anonymous" ||
		got.SourceAddr != "192.0.2.50" || got.WaitMS != 5000 || got.DurationMS != 12000 {
		t.Fatalf("stored %+v", got)
	}
	gq, _ := r.Get(q.ID())
	if gq.Status != StatusCancelled || gq.Reason != ReasonClientLeftQueued || gq.StartedAt != 0 || gq.WaitMS != 17000 {
		t.Fatalf("stored queued %+v", gq)
	}
	if _, err := r.Get(12345); err != ErrNotFound {
		t.Fatal(err)
	}
}

// Rows survive a restart; rows a crash left queued or running are closed as
// failed with reason scheduler_restart and counted in the roll-ups.
func TestRecorder_RestartKeepsRowsAndClosesStaleOnes(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}
	st := Open(Options{Dir: dir, Now: c.now})
	r := NewRecorder(st)
	r.Queued(begin("a")).Ended(done(10, 0, 5))
	r.Queued(begin("a")) // stays queued
	s := r.Queued(begin("a"))
	c.add(time.Second)
	s.Started(Placement{}, nil) // stays running
	if err := st.Flush(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	st.Close() // a crash: no end for two of them

	r2, _ := recorderT(t, dir, c)
	if err := r2.Flush(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	p, err := r2.List(Filter{})
	if err != nil || len(p.Requests) != 3 {
		t.Fatalf("%v %+v", err, p)
	}
	restarted := 0
	for _, row := range p.Requests {
		if row.Status == StatusFailed && row.Reason == ReasonSchedulerRestart {
			restarted++
		}
	}
	if restarted != 2 {
		t.Fatalf("rows %+v", p.Requests)
	}
	sum, err := r2.Summarize(c.now().Add(-time.Hour).UnixMilli(), c.now().Add(time.Hour).UnixMilli(), StepHour, "")
	if err != nil || sum.Totals.Requests != 3 || sum.Totals.Failed != 2 || sum.Totals.Done != 1 {
		t.Fatalf("%v %+v", err, sum.Totals)
	}
	// New ids stay above the stored ones even if the clock went back.
	c.add(-time.Hour)
	if id := r2.Queued(begin("a")).ID(); id <= p.Requests[0].ID {
		t.Fatalf("id %d not above %d", id, p.Requests[0].ID)
	}
}

// The roll-ups equal the sums of the rows, by UTC hour and day, with
// requests at the boundaries and one that spans an hour (it counts in the
// hour it arrived).
func TestRollups_EqualTheSumOfRowsAcrossBoundaries(t *testing.T) {
	start := time.Date(2026, 11, 16, 22, 30, 0, 0, time.UTC)
	c := &clock{t: start}
	r, st := recorderT(t, t.TempDir(), c)
	rng := rand.New(rand.NewSource(1))
	statuses := []string{StatusDone, StatusDone, StatusDone, StatusFailed, StatusRefused, StatusCancelled}
	type tr struct {
		t   *Tracker
		end End
		at  time.Duration
	}
	// 300 requests over about 5 hours, crossing midnight; durations up to 20 min.
	var all []tr
	for i := 0; i < 300; i++ {
		c.set(start.Add(time.Duration(i) * time.Minute))
		e := done(rng.Intn(3000)+1, rng.Intn(500), rng.Intn(800)+1)
		e.Status = statuses[rng.Intn(len(statuses))]
		all = append(all, tr{r.Queued(begin([]string{"a", "b"}[i%2])), e, time.Duration(rng.Intn(1200)) * time.Second})
	}
	// Exact boundary arrivals and a request that spans 23:59:50 to 00:00:40.
	for _, at := range []time.Time{time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 16, 23, 59, 50, 0, time.UTC)} {
		c.set(at)
		all = append(all, tr{r.Queued(begin("a")), done(100, 0, 50), 50 * time.Second})
	}
	for _, x := range all {
		x.t.Started(Placement{}, nil)
	}
	c.set(start.Add(6 * time.Hour))
	for _, x := range all {
		x.t.Ended(x.end)
	}
	if err := st.Flush(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []struct{ table, expr string }{
		{"request_hours", "queued_at - queued_at % 3600000"},
		{"request_days", "queued_at - queued_at % 86400000"},
	} {
		rows, err := st.DB().Query(`SELECT b, p, n, d, ti, tr, tout FROM (SELECT ` + unit.expr + ` AS b, profile AS p, COUNT(*) AS n,
			SUM(status='done') AS d, SUM(COALESCE(tokens_in,0)) AS ti, SUM(COALESCE(tokens_reused,0)) AS tr, SUM(COALESCE(tokens_out,0)) AS tout
			FROM client_requests GROUP BY 1, 2) EXCEPT SELECT bucket, profile, requests, done, tokens_in, tokens_reused, tokens_out FROM ` + unit.table)
		if err != nil {
			t.Fatal(err)
		}
		diff := 0
		for rows.Next() {
			diff++
		}
		rows.Close()
		var n int
		st.DB().QueryRow(`SELECT COUNT(*) FROM ` + unit.table).Scan(&n)
		if diff != 0 || n == 0 {
			t.Fatalf("%s: %d buckets differ from the rows (%d buckets)", unit.table, diff, n)
		}
	}
	// The spanning request is in the 23:00 hour.
	var n int
	st.DB().QueryRow(`SELECT requests FROM request_hours WHERE bucket = ? AND profile = 'a'`, time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC).UnixMilli()).Scan(&n)
	var rowsIn int
	st.DB().QueryRow(`SELECT COUNT(*) FROM client_requests WHERE profile='a' AND queued_at >= ? AND queued_at < ?`,
		time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC).UnixMilli(), time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC).UnixMilli()).Scan(&rowsIn)
	if n != rowsIn || n == 0 {
		t.Fatalf("23:00 bucket %d, rows %d", n, rowsIn)
	}

	// The period query agrees with the rows, by hour and by day.
	from, to := start.Truncate(time.Hour).UnixMilli(), start.Add(6*time.Hour).UnixMilli()
	sum, err := r.Summarize(from, to, StepHour, "")
	if err != nil {
		t.Fatal(err)
	}
	var want, wantIn int64
	st.DB().QueryRow(`SELECT COUNT(*), SUM(COALESCE(tokens_in,0)) FROM client_requests`).Scan(&want, &wantIn)
	var series int64
	for _, b := range sum.Series {
		series += b.Requests
	}
	if sum.Totals.Requests != want || sum.Totals.TokensIn != wantIn || series != want || len(sum.Series) != 7 || sum.Source != "hourly" {
		t.Fatalf("summary %+v (want %d requests, %d in)", sum.Totals, want, wantIn)
	}
	if sum.Totals.PrefillTPS == nil || *sum.Totals.PrefillTPS < 399 || *sum.Totals.PrefillTPS > 401 {
		t.Fatalf("prefill %v", sum.Totals.PrefillTPS)
	}
	// Days from a local midnight (UTC-8): buckets start at 08:00 UTC.
	local := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC).UnixMilli()
	days, err := r.Summarize(local, local+2*86400000, StepDay, "a")
	if err != nil || len(days.Series) != 2 || days.Series[0].Start != local {
		t.Fatalf("%v %+v", err, days)
	}
	var wantA int64
	st.DB().QueryRow(`SELECT COUNT(*) FROM client_requests WHERE profile='a'`).Scan(&wantA)
	if days.Totals.Requests != wantA || days.OldestTS != start.UnixMilli() {
		t.Fatalf("days %+v oldest %d want %d", days.Totals, days.OldestTS, wantA)
	}
}

func TestList_FiltersAndPages(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}
	r, st := recorderT(t, t.TempDir(), c)
	for i := 0; i < 25; i++ {
		c.add(time.Minute)
		b := begin([]string{"a", "b"}[i%2])
		if i%5 == 0 {
			b.BenchRun = "run-1"
		}
		tk := r.Queued(b)
		tk.Started(Placement{Nodes: []Node{{ID: "n" + string(rune('0'+i%3)), Name: "zone-" + string(rune('0'+i%3)), Role: "host"}}}, nil)
		e := done(10, 0, 10)
		if i%4 == 0 {
			e = End{Status: StatusFailed, Reason: "cut", TokensIn: -1, Reused: -1, TokensOut: -1, PrefillTPS: -1, DecodeTPS: -1}
		}
		tk.Ended(e)
	}
	r.Queued(begin("a"))                           // live, queued
	r.Queued(begin("b")).Started(Placement{}, nil) // live, running
	st.Flush(ctxT(t))
	p, _ := r.List(Filter{Limit: 10})
	if len(p.Requests) != 12 || p.Requests[0].Status != StatusQueued || p.Requests[1].Status != StatusRunning || p.Next == 0 {
		t.Fatalf("first page %d rows, next %d", len(p.Requests), p.Next)
	}
	seen := len(p.Requests) - 2
	for p.Next != 0 {
		p, _ = r.List(Filter{Limit: 10, Before: p.Next})
		seen += len(p.Requests)
	}
	if seen != 25 {
		t.Fatalf("paged %d rows", seen)
	}
	yes, no := true, false
	for _, c := range []struct {
		f    Filter
		want int
	}{
		{Filter{Status: []string{StatusFailed}}, 7},
		{Filter{Status: []string{StatusQueued, StatusRunning}}, 2},
		{Filter{Profile: "a", Status: []string{StatusDone, StatusFailed}}, 13},
		{Filter{Node: "zone-1"}, 8},
		{Filter{Node: "n2"}, 8},
		{Filter{Bench: &yes}, 5},
		{Filter{Bench: &no, Status: []string{StatusDone, StatusFailed}}, 20},
		{Filter{From: time.Date(2026, 11, 17, 10, 10, 0, 0, time.UTC).UnixMilli(), To: time.Date(2026, 11, 17, 10, 20, 0, 0, time.UTC).UnixMilli(),
			Status: []string{StatusDone, StatusFailed}}, 10},
	} {
		p, err := r.List(c.f)
		if err != nil || len(p.Requests) != c.want {
			t.Errorf("%+v: %d rows, want %d (%v)", c.f, len(p.Requests), c.want, err)
		}
	}
}

// Request rows go after 90 days, hourly roll-ups after 13 months; daily
// roll-ups stay, so "All time" keeps its totals.
func TestRetention_TrimsRowsAndHoursKeepsDays(t *testing.T) {
	c := &clock{t: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)}
	r, st := recorderT(t, t.TempDir(), c)
	for _, at := range []time.Time{time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)} {
		c.set(at)
		for i := 0; i < 3; i++ {
			r.Queued(begin("a")).Ended(done(10, 0, 10))
		}
	}
	st.Flush(ctxT(t))
	c.set(time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC))
	if n := st.Trim(ctxT(t)); n != 7 { // 6 rows older than 90 days, 1 hour older than 13 months
		t.Fatalf("trimmed %d", n)
	}
	if count(t, st, "client_requests") != 3 || count(t, st, "request_hours") != 2 || count(t, st, "request_days") != 3 {
		t.Fatal("wrong rows kept")
	}
	sum, err := r.Summarize(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), c.now().UnixMilli(), StepDay, "")
	if err != nil || sum.Source != "daily" || sum.Totals.Requests != 9 || sum.OldestTS != time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("%v %+v oldest %d", err, sum.Totals, sum.OldestTS)
	}
}

func TestRecorder_OffStoreStillListsLive(t *testing.T) {
	r := NewRecorder(openT(t, Options{}))
	r.Queued(begin("a"))
	if p, err := r.List(Filter{}); err != nil || len(p.Requests) != 1 {
		t.Fatal(err, p)
	}
	if s, err := r.Summarize(0, 3600000, StepHour, ""); err != nil || s.On || s.Live.Queued != 1 {
		t.Fatal(err, s)
	}
	var nilRec *Recorder
	nilRec.Queued(begin("a")).Started(Placement{}, nil)
	nilRec.Queued(begin("a")).Ended(End{})
}
