package availability

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// ---- classification ----

func TestClassify_EveryStateFromMembershipAndStatus(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	facts := []pool.NodeFacts{
		{ID: "02:00:00:00:00:01", Name: "zone-free", State: pool.StateActive},
		{ID: "02:00:00:00:00:02", Name: "zone-paused", State: pool.StateActive, Paused: true},
		{ID: "02:00:00:00:00:03", Name: "zone-excluded", State: pool.StateActive, Excluded: true},
		{ID: "02:00:00:00:00:04", Name: "zone-lost", State: pool.StateLost},
		{ID: "02:00:00:00:00:05", Name: "zone-left", State: pool.StateLeft},
		{ID: "02:00:00:00:00:06", Name: "zone-removed", State: pool.StateRemoved},
		{ID: "02:00:00:00:00:07", Name: "zone-aside", State: pool.StateActive},
		{ID: "02:00:00:00:00:08", Name: "zone-loading", State: pool.StateActive},
		{ID: "02:00:00:00:00:09", Name: "zone-busy", State: pool.StateActive},
		{ID: "02:00:00:00:00:0a", Name: "zone-helper", State: pool.StateActive},
		{ID: "02:00:00:00:00:0b", Name: "zone-lost-paused", State: pool.StateLost, Paused: true},
		{ID: "02:00:00:00:00:0c", Name: "zone-stopping", State: pool.StateActive},
	}
	st := &scheduler.Status{
		Instances: []scheduler.InstanceStatus{
			{Profile: "big", State: "ready", InFlight: 3, Parallel: 4, Nodes: []scheduler.PlacedNodeStatus{{ID: "02:00:00:00:00:09"}, {ID: "02:00:00:00:00:0a"}}},
			{Profile: "new", State: "starting", Parallel: 1, Nodes: []scheduler.PlacedNodeStatus{{ID: "02:00:00:00:00:08"}}},
			{Profile: "old", State: "stopping", InFlight: 1, Parallel: 1, Nodes: []scheduler.PlacedNodeStatus{{ID: "02:00:00:00:00:0c"}}},
		},
		View: scheduler.ViewStatus{Nodes: []scheduler.NodeStatus{
			{ID: "02:00:00:00:00:07", Online: true, Quarantined: now.Add(time.Minute)},
			{ID: "host-1", Hostname: "scheduler-box", Local: true, Online: true},
			{ID: "02:00:00:00:00:0d", Hostname: "fleet-offline", Online: false},
			{ID: "02:00:00:00:00:0e", Hostname: "fleet-excluded", Online: true, Excluded: true},
		}},
	}
	want := map[string]struct {
		state     State
		busy      float64
		reachable bool
	}{
		"zone-free": {Free, 0, true}, "zone-paused": {Off, 0, true}, "zone-excluded": {Off, 0, true},
		"zone-lost": {Lost, 0, false}, "zone-left": {Lost, 0, false}, "zone-removed": {Off, 0, false},
		"zone-aside": {SetAside, 0, true}, "zone-loading": {Loading, 0, true}, "zone-busy": {InUse, .75, true},
		"zone-helper": {InUse, .75, true}, "zone-lost-paused": {Off, 0, false}, "zone-stopping": {InUse, 0, true},
		"scheduler-box": {Free, 0, true}, "fleet-offline": {Lost, 0, false}, "fleet-excluded": {Off, 0, true},
	}
	got := Classify(st, facts)
	if len(got) != len(want) {
		t.Fatalf("%d samples, want %d: %+v", len(got), len(want), got)
	}
	for _, s := range got {
		w, ok := want[s.Name]
		if s.Removed != (s.Name == "zone-removed") {
			t.Errorf("%s: removed %v", s.Name, s.Removed)
		}
		if !ok || s.State != w.state || s.Busy != w.busy || s.Reachable != w.reachable {
			t.Errorf("%s: %s busy %v reachable %v, want %+v", s.Name, s.State, s.Busy, s.Reachable, w)
		}
	}
}

func TestClassify_BusyIsInFlightOverParallelCappedAtOne(t *testing.T) {
	for _, tc := range []struct {
		inFlight, parallel int
		want               float64
	}{{0, 4, 0}, {1, 4, .25}, {2, 4, .5}, {4, 4, 1}, {9, 4, 1}, {1, 1, 1}} {
		st := &scheduler.Status{Instances: []scheduler.InstanceStatus{{Profile: "m", State: "draining", InFlight: tc.inFlight, Parallel: tc.parallel,
			Nodes: []scheduler.PlacedNodeStatus{{ID: "n"}}}}, View: scheduler.ViewStatus{Nodes: []scheduler.NodeStatus{{ID: "n", Online: true}}}}
		if s := Classify(st, nil); len(s) != 1 || s[0].State != InUse || s[0].Busy != tc.want {
			t.Fatalf("%d/%d: %+v", tc.inFlight, tc.parallel, s)
		}
	}
	for _, tc := range []struct {
		b     float64
		level int
	}{{0, 1}, {.24, 1}, {.25, 2}, {.5, 3}, {.74, 3}, {.75, 4}, {1, 4}} {
		if got := BusyLevel(tc.b); got != tc.level {
			t.Fatalf("busy %v: level %d, want %d", tc.b, got, tc.level)
		}
	}
}

// ---- sampler ----

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration)    { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func (c *clock) set(t time.Time)        { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func newClock(t time.Time) *clock       { return &clock{t: t} }
func ctxT(t *testing.T) context.Context { return context.Background() }

// script is a source whose answer the test sets.
type script struct {
	mu    sync.Mutex
	cur   []Sample
	err   error
	reads int
}

func (s *script) set(samples ...Sample) { s.mu.Lock(); s.cur = samples; s.mu.Unlock() }
func (s *script) fail(err error)        { s.mu.Lock(); s.err = err; s.mu.Unlock() }
func (s *script) src(context.Context) ([]Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return append([]Sample(nil), s.cur...), s.err
}

func openStore(t *testing.T, dir string, c *clock, o ...func(*usage.Options)) *usage.Store {
	t.Helper()
	opt := usage.Options{Dir: dir, Now: c.now, Logger: quiet}
	for _, f := range o {
		f(&opt)
	}
	s := usage.Open(opt)
	t.Cleanup(func() { s.Close() })
	return s
}

func flushStore(t *testing.T, s *usage.Store) {
	t.Helper()
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// run ticks every 10 s from the clock's time for d.
func run(smp *Sampler, c *clock, d time.Duration) {
	for end := c.now().Add(d); c.now().Before(end); {
		c.add(10 * time.Second)
		smp.Tick(context.Background())
	}
}

func readAll(t *testing.T, st *usage.Store, smp *Sampler, q Query, now time.Time) Series {
	t.Helper()
	s, err := Read(context.Background(), st, smp, q, now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sumSeconds(s Seconds) float64 { return s.Free + s.InUse + s.Loading + s.Down + s.Off + s.Unknown }

var (
	h10 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	a   = Sample{Node: "02:00:00:00:00:01", Name: "zone-a", State: Free, Reachable: true}
)

func with(s Sample, st State, busy float64) Sample { s.State, s.Busy = st, busy; return s }

func TestSampler_SecondsPerStateSumToTheSampledTime(t *testing.T) {
	c := newClock(h10)
	store := openStore(t, t.TempDir(), c)
	src := &script{}
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	src.set(a)
	smp.Tick(ctxT(t)) // 10:00:00, the first sample counts nothing yet
	run(smp, c, 10*time.Minute)
	src.set(with(a, InUse, .5))
	run(smp, c, 20*time.Minute)
	src.set(with(a, Loading, 0))
	run(smp, c, 5*time.Minute)
	src.set(with(with(a, Off, 0), Off, 0))
	run(smp, c, 5*time.Minute) // 10:40
	smp.Flush()
	flushStore(t, store)
	rows, err := store.NodeHours(ctxT(t), h10, h10.Add(time.Hour))
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %+v", err, rows)
	}
	r := rows[0]
	// Each interval carries the state seen at its start.
	// (the sample at 10:10:00 still saw free; the one at 10:40:00 has
	// counted nothing yet).
	if r.FreeMs != 610_000 || r.InUseMs != 1_200_000 || r.LoadingMs != 300_000 || r.OffMs != 290_000 || r.BusyMs != 600_000 ||
		r.ReachableMs != r.KnownMs() || r.KnownMs() != 40*60*1000 {
		t.Fatalf("%+v known %d", r, r.KnownMs())
	}
	// Read back: the states and unknown add up to the hour's span so far.
	now := c.now()
	s := readAll(t, store, smp, Query{From: h10, To: now}, now)
	cell := s.Nodes[0].Cells[0]
	if !cell.Partial || cell.SpanS != 40*60 || math.Abs(sumSeconds(cell.Seconds)-cell.SpanS) > 1e-6 || cell.Seconds.Unknown != 0 {
		t.Fatalf("cell %+v", cell)
	}
	if cell.Busy == nil || *cell.Busy != .5 || cell.BusyLevel != 3 || cell.State != InUse {
		t.Fatalf("busy %v level %d state %s", cell.Busy, cell.BusyLevel, cell.State)
	}
}

func TestSampler_WritesAtMostOncePerMinuteAndAtTheHoursEnd(t *testing.T) {
	c := newClock(h10.Add(58 * time.Minute))
	store := openStore(t, t.TempDir(), c)
	src := &script{}
	src.set(a, Sample{Node: "02:00:00:00:00:02", Name: "zone-b", State: Lost})
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	before := store.Stats().Written
	run(smp, c, 50*time.Second) // 58:50: no minute yet
	flushStore(t, store)
	if w := store.Stats().Written - before; w != 0 {
		t.Fatalf("%d operations written within the first minute", w)
	}
	run(smp, c, 10*time.Second) // 59:00: a minute
	flushStore(t, store)
	run(smp, c, 30*time.Second) // 59:30
	flushStore(t, store)
	run(smp, c, 30*time.Second) // 11:00:00: the hour ended
	flushStore(t, store)
	if w := store.Stats().Written - before; w != 2 { // one operation each (both machines)
		t.Fatalf("%d operations", w)
	}
	rows, _ := store.NodeHours(ctxT(t), h10, h10.Add(2*time.Hour))
	if len(rows) != 2 || rows[0].KnownMs() != 120_000 || rows[1].KnownMs() != 120_000 {
		t.Fatalf("%+v", rows)
	}
}

func TestSampler_HourBoundaryIsSplitInUTC(t *testing.T) {
	loc := time.FixedZone("UTC-8", -8*3600)
	c := newClock(h10.Add(59*time.Minute + 55*time.Second).In(loc))
	store := openStore(t, t.TempDir(), c)
	src := &script{}
	src.set(with(a, InUse, 1))
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	run(smp, c, 10*time.Second) // 10:59:55 → 11:00:05
	smp.Flush()
	flushStore(t, store)
	rows, _ := store.NodeHours(ctxT(t), h10, h10.Add(2*time.Hour))
	if len(rows) != 2 || !rows[0].Hour.Equal(h10) || rows[0].InUseMs != 5000 || !rows[1].Hour.Equal(h10.Add(time.Hour)) || rows[1].InUseMs != 5000 ||
		rows[0].BusyMs != 5000 {
		t.Fatalf("%+v", rows)
	}
}

func TestSampler_ARestartLeavesTheUnsampledPartUnknown(t *testing.T) {
	dir := t.TempDir()
	c := newClock(h10)
	src := &script{}
	src.set(a)
	store := usage.Open(usage.Options{Dir: dir, Now: c.now, Logger: quiet})
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	run(smp, c, 20*time.Minute) // 10:00 to 10:20 sampled
	smp.Flush()
	store.Close()

	c.set(h10.Add(40 * time.Minute)) // Potluck was down 20 minutes
	store = openStore(t, dir, c)
	smp = New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	run(smp, c, 10*time.Minute) // 10:40 to 10:50
	smp.Flush()
	flushStore(t, store)
	c.set(h10.Add(3 * time.Hour))
	s := readAll(t, store, nil, Query{From: h10.Add(-time.Hour), To: c.now()}, c.now())
	cells := s.Nodes[0].Cells
	if len(cells) != 4 {
		t.Fatalf("%d cells", len(cells))
	}
	// The hour before any sample: unknown, not free.
	if cells[0].State != Unknown || cells[0].Seconds.Unknown != 3600 || cells[0].Seconds.Free != 0 || cells[0].Uptime != nil {
		t.Fatalf("hour before %+v", cells[0])
	}
	if c1 := cells[1]; c1.Seconds.Free != 1800 || c1.Seconds.Unknown != 1800 || c1.State != Free || *c1.Uptime != 1 {
		t.Fatalf("restart hour %+v", c1)
	}
	if cells[2].State != Unknown || cells[3].State != Unknown {
		t.Fatalf("after: %+v %+v", cells[2], cells[3])
	}
	if s.Nodes[0].KnownS != 1800 || *s.Nodes[0].Uptime != 1 {
		t.Fatalf("%+v", s.Nodes[0])
	}
}

func TestSampler_AFailedReadCountsOnlyUpToTheMaxGap(t *testing.T) {
	c := newClock(h10)
	store := openStore(t, t.TempDir(), c)
	src := &script{}
	src.set(a)
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	src.fail(errors.New("status read timed out"))
	run(smp, c, 2*time.Minute)
	src.fail(nil)
	run(smp, c, time.Minute)
	smp.Flush()
	flushStore(t, store)
	rows, _ := store.NodeHours(ctxT(t), h10, h10.Add(time.Hour))
	// 30 s after the last good sample (the cap), then from the first good
	// sample again (10:02:10) to 10:03:00: 80 s; the rest unknown.
	if len(rows) != 1 || rows[0].FreeMs != 80_000 {
		t.Fatalf("%+v", rows)
	}
}

// The sampler reads one status per tick whatever the number of machines
// (the core loop answers one read-only query per 10 s), and a writer stuck
// on the disk never delays a tick: Submit only queues.
func TestSampler_OneReadPerTickAndABlockedWriterNeverDelaysIt(t *testing.T) {
	c := newClock(h10)
	release := make(chan struct{})
	var once sync.Once
	entered := make(chan struct{})
	store := openStore(t, t.TempDir(), c, func(o *usage.Options) {
		o.BeforeBatch = func() {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	defer close(release)
	var samples []Sample
	for i := range 200 {
		samples = append(samples, Sample{Node: string(rune('a'+i%26)) + time.Duration(i).String(), Name: "zone", State: InUse, Busy: .5, Reachable: true})
	}
	src := &script{}
	src.set(samples...)
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	c.add(time.Minute)
	smp.Tick(ctxT(t)) // a minute: the first write
	<-entered         // the writer is now stuck on its first batch
	t0 := time.Now()
	const ticks = 360 // an hour of ticks, with a write each minute and at the hour's end
	for range ticks {
		c.add(10 * time.Second)
		smp.Tick(ctxT(t))
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("%d ticks took %v with the writer blocked", ticks, d)
	}
	if got := smp.Reads(); got != ticks+2 {
		t.Fatalf("%d source reads for %d ticks", got, ticks+2)
	}
	if q := store.Stats().Queued; q > 70 {
		t.Fatalf("%d operations queued: more than one per minute", q)
	}
}

func TestSampler_StoreOffKeepsSamplingAndReadAnswersUnknown(t *testing.T) {
	c := newClock(h10)
	store := usage.Open(usage.Options{Logger: quiet}) // off
	src := &script{}
	src.set(a)
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	run(smp, c, 2*time.Minute)
	s := readAll(t, store, smp, Query{From: h10.Add(-23 * time.Hour), To: c.now()}, c.now())
	if s.Recording || !time.Time(s.HistoryBeginsAt).IsZero() || len(s.Nodes) != 1 || len(s.Nodes[0].Cells) != 24 {
		t.Fatalf("%+v", s)
	}
	for _, cell := range s.Nodes[0].Cells {
		if cell.State != Unknown {
			t.Fatalf("cell %+v", cell)
		}
	}
	// No store, no sampler: an empty series.
	s = readAll(t, nil, nil, Query{From: h10, To: c.now()}, c.now())
	if len(s.Nodes) != 0 || s.Recording {
		t.Fatalf("%+v", s)
	}
}

func TestRead_DamagedStoreAnswersAnEmptySeriesFromNow(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, usage.FileName), []byte("this is not a database, it is damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newClock(h10.Add(30 * time.Minute))
	store := openStore(t, dir, c)
	if !store.On() {
		t.Fatal("a damaged file is moved aside and replaced")
	}
	s := readAll(t, store, nil, Query{From: h10.Add(-24 * time.Hour), To: c.now()}, c.now())
	if len(s.Nodes) != 0 || !time.Time(s.HistoryBeginsAt).Equal(c.now()) {
		t.Fatalf("%+v", s)
	}
}

func TestRead_DayRollUpInUTCDays(t *testing.T) {
	c := newClock(time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC))
	store := openStore(t, t.TempDir(), c)
	day0 := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	store.AddNodeHours([]usage.NodeHour{
		{Node: "n1", Name: "zone-a", Hour: day0.Add(1 * time.Hour), FreeMs: 3_600_000, ReachableMs: 3_600_000},
		{Node: "n1", Name: "zone-a", Hour: day0.Add(23 * time.Hour), InUseMs: 3_600_000, BusyMs: 900_000, ReachableMs: 3_600_000},
		{Node: "n1", Name: "zone-a", Hour: day0.Add(24 * time.Hour), LostMs: 1_800_000, FreeMs: 1_800_000, ReachableMs: 1_800_000},
	})
	flushStore(t, store)
	s := readAll(t, store, nil, Query{From: day0.Add(5 * time.Hour), To: c.now(), Step: StepDay}, c.now())
	if s.Step != StepDay || !time.Time(s.From).Equal(day0) || !time.Time(s.To).Equal(day0.Add(48*time.Hour)) {
		t.Fatalf("range %+v", s)
	}
	cells := s.Nodes[0].Cells
	if len(cells) != 2 {
		t.Fatalf("%d cells", len(cells))
	}
	d0, d1 := cells[0], cells[1]
	if d0.SpanS != 86400 || d0.Seconds.Free != 3600 || d0.Seconds.InUse != 3600 || d0.Seconds.Unknown != 86400-7200 || *d0.Busy != .25 ||
		d0.BusyLevel != 2 || d0.State != InUse || *d0.Uptime != 1 {
		t.Fatalf("day 0 %+v", d0)
	}
	if !d1.Partial || d1.SpanS != 6*3600 || d1.Seconds.Down != 1800 || d1.DownLostS != 1800 || d1.State != Down || *d1.Uptime != .5 ||
		math.Abs(sumSeconds(d1.Seconds)-d1.SpanS) > 1e-6 {
		t.Fatalf("day 1 %+v", d1)
	}
	if u := *s.Nodes[0].Uptime; u != .833 { // 9000/10800, to 3 decimals
		t.Fatalf("uptime %v", u)
	}
	// The same hours step by step.
	h := readAll(t, store, nil, Query{From: day0, To: c.now()}, c.now())
	if n := len(h.Nodes[0].Cells); n != 30 {
		t.Fatalf("%d hour cells", n)
	}
}

func TestRead_NodeByIDOrNameAndTheUnsentMinute(t *testing.T) {
	c := newClock(h10)
	store := openStore(t, t.TempDir(), c)
	src := &script{}
	src.set(a, Sample{Node: "02:00:00:00:00:02", Name: "zone-b", State: Lost})
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	run(smp, c, 30*time.Second) // not flushed yet: the reader adds the pending time
	for _, n := range []string{"zone-b", "ZONE-B", "02:00:00:00:00:02"} {
		s := readAll(t, store, smp, Query{From: h10, To: c.now(), Node: n}, c.now())
		if len(s.Nodes) != 1 || s.Nodes[0].ID != "02:00:00:00:00:02" || s.Nodes[0].Cells[0].Seconds.Down != 30 || s.Nodes[0].Cells[0].DownLostS != 30 ||
			*s.Nodes[0].Uptime != 0 {
			t.Fatalf("%s: %+v", n, s.Nodes)
		}
	}
	if _, err := Read(ctxT(t), store, smp, Query{From: h10, To: c.now(), Node: "zone-z"}, c.now()); !errors.Is(err, ErrUnknownNode) {
		t.Fatal(err)
	}
}

func TestCellState_TheStripRule(t *testing.T) {
	for _, tc := range []struct {
		s    Seconds
		want State
	}{
		{Seconds{Unknown: 3600}, Unknown},
		{Seconds{Off: 1800, Free: 1800}, Off},
		{Seconds{Down: 300, Free: 3300}, Down},
		{Seconds{Down: 299, Free: 3301}, Free},
		{Seconds{Down: 100, Free: 50, Unknown: 3450}, Down},
		{Seconds{Loading: 1800, InUse: 1800}, Loading},
		{Seconds{InUse: 900, Free: 2700}, InUse},
		{Seconds{InUse: 899, Free: 2701}, Free},
	} {
		if got := CellState(tc.s); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.s, got, tc.want)
		}
	}
}

var _ = core.NodeID("")

// The supervised task samples on its ticker and writes what it counted
// when it stops.
func TestSampler_StartRunsAndWritesTheRestWhenItStops(t *testing.T) {
	store := openStore(t, t.TempDir(), newClock(time.Now()), func(o *usage.Options) { o.Now = time.Now })
	src := &script{}
	src.set(a)
	smp := New(store, src.src, Options{Every: 5 * time.Millisecond, FlushEvery: time.Hour, Logger: quiet})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	smp.Start(ctx, func() { close(done) })
	for smp.Reads() < 5 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	flushStore(t, store)
	rows, err := store.NodeHours(ctxT(t), time.Now().Add(-2*time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(rows) == 0 || rows[0].FreeMs <= 0 {
		t.Fatalf("%v %+v", err, rows)
	}
	if len(smp.Pending()) != 0 {
		t.Fatal("pending time left unwritten")
	}
}

// A removed machine is left out of a period it was only off in (the sampler
// counts it off once removed), and kept where it did anything else; asked
// for by name it is answered.
func TestRead_RemovedMachinesOnlyOffAreLeftOut(t *testing.T) {
	c := newClock(h10)
	store := openStore(t, t.TempDir(), c)
	gone := Sample{Node: "02:00:00:00:00:06", Name: "zone-removed", State: Off, Removed: true}
	src := &script{}
	src.set(a, gone)
	smp := New(store, src.src, Options{Now: c.now, Logger: quiet})
	smp.Tick(ctxT(t))
	run(smp, c, time.Hour)
	smp.Flush()
	flushStore(t, store)
	names := func(s Series) string {
		out := ""
		for _, n := range s.Nodes {
			out += n.Name + " "
		}
		return out
	}
	if s := readAll(t, store, smp, Query{From: h10, To: c.now()}, c.now()); names(s) != "zone-a " {
		t.Fatalf("listed %q", names(s))
	}
	if s := readAll(t, store, smp, Query{From: h10, To: c.now(), Node: "zone-removed"}, c.now()); len(s.Nodes) != 1 {
		t.Fatalf("by name: %+v", s.Nodes)
	}
	// Free in an earlier hour of the period: it did something, so it stays.
	store.AddNodeHours([]usage.NodeHour{{Node: gone.Node, Name: gone.Name, Hour: h10.Add(-time.Hour), FreeMs: 600_000, ReachableMs: 600_000}})
	flushStore(t, store)
	if s := readAll(t, store, smp, Query{From: h10.Add(-time.Hour), To: c.now()}, c.now()); names(s) != "zone-a zone-removed " {
		t.Fatalf("listed %q", names(s))
	}
	// A machine that is only off but not removed (paused) stays listed.
	src.set(a, Sample{Node: "02:00:00:00:00:02", Name: "zone-paused", State: Off, Reachable: true})
	run(smp, c, time.Minute)
	if s := readAll(t, store, smp, Query{From: h10, To: c.now()}, c.now()); !strings.Contains(names(s), "zone-paused") {
		t.Fatalf("listed %q", names(s))
	}
}

// Every figure of the series goes out with 3 decimals at most.
func TestRead_FiguresAreRoundedToThreeDecimals(t *testing.T) {
	c := newClock(h10.Add(20*time.Minute + 333*time.Millisecond))
	store := openStore(t, t.TempDir(), c)
	store.AddNodeHours([]usage.NodeHour{{Node: "n1", Name: "zone-a", Hour: h10, FreeMs: 700_001, LostMs: 500_000, ReachableMs: 700_001}})
	flushStore(t, store)
	s := readAll(t, store, nil, Query{From: h10, To: c.now()}, c.now())
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`\d+\.(\d+)`).FindAllStringSubmatch(string(b), -1) {
		if len(m[1]) > 3 {
			t.Fatalf("%s in %s", m[0], b)
		}
	}
	n := s.Nodes[0]
	if *n.Uptime != .583 || *n.Cells[0].Uptime != .583 || n.Cells[0].SpanS != 1200.333 || n.Cells[0].Seconds.Unknown != .332 {
		t.Fatalf("%s", b)
	}
}
