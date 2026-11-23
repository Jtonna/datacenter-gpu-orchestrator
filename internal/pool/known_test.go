package pool

import (
	"context"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// scanOf reads a list of events back, as history.Store.Scan does.
func scanOf(evs []history.Event) func(func(history.Event)) {
	return func(fn func(history.Event)) {
		for _, e := range evs {
			fn(e)
		}
	}
}

func member(ts int64, id, name, state string, extra map[string]any) history.Event {
	d := map[string]any{"id": id, "state": state}
	for k, v := range extra {
		d[k] = v
	}
	return history.Event{Ts: ts, Type: history.TNodeMembership, Nodes: []string{name}, Data: d}
}

func knownByID(ks []KnownNode) map[string]KnownNode {
	m := map[string]KnownNode{}
	for _, k := range ks {
		m[k.ID] = k
	}
	return m
}

// The known machines are the members when the previous run ended: a
// machine registered (active) when that run stopped, or carried into it
// from the run before and never seen in it. A machine lost, left or removed
// before the previous run stopped is dropped, as the pool drops it at a
// restart today; the run doing the reading (after the last start) is not
// looked at.
func TestKnownFrom_Fold(t *testing.T) {
	evs := []history.Event{
		{Ts: 1000, Type: history.TSchedulerStarted}, // run 1
		member(1100, "id-a", "node-a", StateActive, nil),
		member(1200, "id-b", "node-b", StateActive, nil),
		member(1300, "id-c", "node-c", StateActive, nil),
		member(1400, "id-c", "node-c", StateLeft, nil),
		member(1500, "id-d", "node-d", StateActive, nil),
		member(30000, "id-d", "node-d", StateLost, map[string]any{"last_seen": float64(20000)}), // lost before the stop: dropped
		member(31000, "id-f", "node-f", StateActive, nil),
		member(31500, "id-g", "node-g", StateActive, nil),
		{Ts: 50000, Type: history.TSchedulerStopping},
		{Ts: 60000, Type: history.TSchedulerStarted}, // run 2: a, b, f and g carried
		member(61000, "id-b", "node-b", StateActive, nil),
		member(62000, "id-f", "node-f", StateActive, nil),
		member(65000, "id-f", "node-f", StateLost, nil), // the ghost: registered, then lost in run 2
		{Ts: 66000, Type: history.TNodeAdmin, Nodes: []string{"id-g"}, Data: map[string]any{"id": "id-g", "action": "remove"}},
		member(66500, "id-a", "node-a", StateLost, map[string]any{"known": true, "last_seen": int64(50000)}), // run 2 listed a as not back
		{Ts: 70000, Type: history.TLoadStarted},                                                              // run 2 crashed after this
		{Ts: 90000, Type: history.TSchedulerStarted},                                                         // run 3: this one
		member(91000, "id-h", "node-h", StateActive, nil),
		member(92000, "id-b", "node-b", StateActive, nil),
	}
	got := knownByID(KnownFrom(scanOf(evs)))
	want := map[string]int64{"id-a": 50000, "id-b": 70000}
	if len(got) != len(want) {
		t.Fatalf("known = %+v, want a and b only", got)
	}
	for id, ms := range want {
		if k, ok := got[id]; !ok || k.LastSeen.UnixMilli() != ms {
			t.Errorf("%s: %+v, want last seen %d", id, k, ms)
		}
	}
	if got["id-a"].Name != "node-a" {
		t.Errorf("name %q", got["id-a"].Name)
	}
	// A carried machine stays known across further restarts until it
	// registers (and is then dropped only if it is lost again) or is removed.
	evs = append(evs, history.Event{Ts: 100000, Type: history.TSchedulerStopping}, history.Event{Ts: 200000, Type: history.TSchedulerStarted})
	got = knownByID(KnownFrom(scanOf(evs)))
	if got["id-a"].LastSeen.UnixMilli() != 50000 || got["id-b"].LastSeen.UnixMilli() != 100000 || got["id-h"].LastSeen.UnixMilli() != 100000 || len(got) != 3 {
		t.Errorf("after a fourth start: %+v", got)
	}
	// No start marker at all: nothing is known.
	if ks := KnownFrom(scanOf(evs[1:8])); len(ks) != 0 {
		t.Errorf("without a start: %+v", ks)
	}
}

func knownFact(p *Pool, id string) (NodeFacts, int) {
	var f NodeFacts
	n := 0
	for _, x := range p.Facts() {
		if x.ID == id {
			f = x
			n++
		}
	}
	return f, n
}

// A known machine that has not registered is listed as lost, with its last
// seen, once the grace after start is over; never before, and never as a
// node the scheduler may place on.
func TestKnown_ListedLostAfterGrace(t *testing.T) {
	r := newRig(t)
	seen := r.clk.now().Add(-70 * time.Minute)
	r.p.SetKnown([]KnownNode{{ID: "02:00:00:00:00:09", Name: "node-9", LastSeen: seen}})
	if _, n := knownFact(r.p, "02:00:00:00:00:09"); n != 0 {
		t.Fatal("listed during the grace")
	}
	r.clk.advance(KnownGrace - time.Second)
	if _, n := knownFact(r.p, "02:00:00:00:00:09"); n != 0 {
		t.Fatal("listed during the grace")
	}
	r.clk.advance(time.Second)
	f, n := knownFact(r.p, "02:00:00:00:00:09")
	if n != 1 || f.State != StateLost || f.Name != "node-9" || f.Placeable || !f.AbsentSinceStart {
		t.Fatalf("after the grace: %d %+v", n, f)
	}
	if f.LastSeenAt == nil || !f.LastSeenAt.Equal(seen) || f.LastSeenAgoS == nil || *f.LastSeenAgoS < 70*60 {
		t.Fatalf("last seen %v ago %v", f.LastSeenAt, f.LastSeenAgoS)
	}
	if f.Capabilities == nil {
		t.Error("capabilities must be a list")
	}
	if len(r.p.Nodes()) != 0 {
		t.Fatalf("placement sees the known machine: %+v", r.p.Nodes())
	}
}

// When the machine registers it is one entry, active, as any other.
func TestKnown_RegisterReplacesEntry(t *testing.T) {
	r := newRig(t)
	r.p.SetKnown([]KnownNode{{ID: "02:00:00:00:00:01", Name: "node-1", LastSeen: r.clk.now().Add(-time.Hour)}})
	r.clk.advance(KnownGrace)
	r.agent(1)
	eventually(t, 5*time.Second, func() bool {
		f, n := knownFact(r.p, "02:00:00:00:00:01")
		return n == 1 && f.State == StateActive && !f.AbsentSinceStart && f.LastSeenAt == nil
	}, "one active entry")
}

// A machine the owner removed stays removed, under its known name, and a
// known machine that is off can be removed and paused like any other.
func TestKnown_RemovedStaysRemoved(t *testing.T) {
	r := newRig(t)
	r.store.m["02:00:00:00:00:07"] = cfgstore.NodeAdmin{Removed: true}
	r.start() // the removal is read from the store
	r.p.SetKnown([]KnownNode{{ID: "02:00:00:00:00:07", Name: "node-7", LastSeen: r.clk.now().Add(-time.Hour)},
		{ID: "02:00:00:00:00:08", Name: "node-8", LastSeen: r.clk.now().Add(-time.Hour)}})
	r.clk.advance(KnownGrace)
	f, n := knownFact(r.p, "02:00:00:00:00:07")
	if n != 1 || f.State != StateRemoved || f.Name != "node-7" {
		t.Fatalf("removed: %d %+v", n, f)
	}
	if _, err := r.p.SetPaused("02:00:00:00:00:08", true); err != nil {
		t.Fatal(err)
	}
	if f, _ := knownFact(r.p, "02:00:00:00:00:08"); f.State != StateLost || !f.Paused {
		t.Fatalf("paused known: %+v", f)
	}
	if err := r.p.Remove(context.Background(), "02:00:00:00:00:08"); err != nil {
		t.Fatal(err)
	}
	if f, n := knownFact(r.p, "02:00:00:00:00:08"); n != 1 || f.State != StateRemoved || f.Name != "node-8" {
		t.Fatalf("after removal: %d %+v", n, f)
	}
	r.mu.Lock()
	forced := len(r.forced)
	r.mu.Unlock()
	if forced != 0 {
		t.Error("nothing to unload on a machine that never connected")
	}
}

// exclude_nodes switches a known machine off as it does a connected one.
func TestKnown_Excluded(t *testing.T) {
	r := newRig(t)
	r.cfg.ExcludeNodes = []string{"node-9"}
	r.p.SetKnown([]KnownNode{{ID: "02:00:00:00:00:09", Name: "node-9", LastSeen: r.clk.now().Add(-time.Hour)}})
	r.clk.advance(KnownGrace)
	if f, _ := knownFact(r.p, "02:00:00:00:00:09"); !f.Excluded || f.ExcludedBy != "node-9" || f.Unplaced != UnplacedExcluded {
		t.Fatalf("%+v", f)
	}
}

// Once listed, the machine is written to the history once per start, with
// its last seen, so the next start knows it even when older events are gone.
func TestKnown_AnnouncedOnce(t *testing.T) {
	r := newRig(t)
	seen := r.clk.now().Add(-time.Hour)
	r.p.SetKnown([]KnownNode{{ID: "02:00:00:00:00:09", Name: "node-9", LastSeen: seen}})
	count := func() (n int, last history.Event) {
		r.hist.mu.Lock()
		defer r.hist.mu.Unlock()
		for _, e := range r.hist.ev {
			if e.Type == history.TNodeMembership && e.Data["known"] == true {
				n++
				last = e
			}
		}
		return
	}
	r.p.CheckLost()
	if n, _ := count(); n != 0 {
		t.Fatal("announced during the grace")
	}
	r.clk.advance(KnownGrace)
	r.p.CheckLost()
	r.p.CheckLost()
	n, e := count()
	if n != 1 || e.Severity != history.Warn || e.Data["state"] != StateLost || e.Data["id"] != "02:00:00:00:00:09" ||
		e.Data["last_seen"] != seen.UnixMilli() || len(e.Nodes) != 1 || e.Nodes[0] != "node-9" {
		t.Fatalf("%d %+v", n, e)
	}
	// The event does not drop the machine at the next start.
	evs := []history.Event{{Ts: 1, Type: history.TSchedulerStarted}, member(2, "02:00:00:00:00:09", "node-9", StateActive, nil),
		{Ts: 3, Type: history.TSchedulerStarted}, e, {Ts: e.Ts + 1, Type: history.TSchedulerStarted}}
	if k := knownByID(KnownFrom(scanOf(evs)))["02:00:00:00:00:09"]; k.ID == "" {
		t.Fatal("dropped by its own event")
	}
}

// Across a Potluck restart: the machine connected in the first run and is
// off when the second starts; the second lists it from the history.
func TestKnown_AcrossRestart(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	eventually(t, 5*time.Second, func() bool { f, _ := knownFact(r.p, a.ID); return f.State == StateActive }, "registered")
	a.Close()
	r.hist.mu.Lock()
	r.hist.ev = append([]history.Event{{Ts: 1, Type: history.TSchedulerStarted}}, r.hist.ev...)
	r.hist.ev = append(r.hist.ev, history.Event{Ts: r.clk.now().UnixMilli(), Type: history.TSchedulerStopping})
	r.hist.mu.Unlock()
	stopped := r.clk.now()
	r.clk.advance(time.Minute)
	r.start()
	r.hist.mu.Lock()
	r.hist.ev = append(r.hist.ev, history.Event{Ts: r.clk.now().UnixMilli(), Type: history.TSchedulerStarted})
	evs := append([]history.Event(nil), r.hist.ev...)
	r.hist.mu.Unlock()
	r.p.SetKnown(KnownFrom(scanOf(evs)))
	r.clk.advance(KnownGrace)
	f, n := knownFact(r.p, a.ID)
	if n != 1 || f.State != StateLost || f.Name != "node-1" || f.LastSeenAt == nil || f.LastSeenAt.UnixMilli() != stopped.UnixMilli() {
		t.Fatalf("%d %+v", n, f)
	}
}

// The scheduler's history store is read in the background when the pool
// gets it.
func TestKnown_LoadedFromHistoryStore(t *testing.T) {
	dir := t.TempDir()
	st := history.Open(history.Options{Dir: dir})
	st.Record(history.Event{Ts: 1000, Type: history.TSchedulerStarted, Severity: history.Info})
	st.Record(member(2000, "02:00:00:00:00:05", "node-5", StateActive, nil))
	st.Record(history.Event{Ts: 3000, Type: history.TSchedulerStopping, Severity: history.Info})
	st.Close()
	st = history.Open(history.Options{Dir: dir})
	defer st.Close()
	st.Record(history.Event{Ts: 9000, Type: history.TSchedulerStarted, Severity: history.Info})
	r := newRig(t)
	r.p.SetHistory(st)
	r.clk.advance(KnownGrace)
	eventually(t, 5*time.Second, func() bool {
		f, n := knownFact(r.p, "02:00:00:00:00:05")
		return n == 1 && f.State == StateLost && f.LastSeenAt != nil && f.LastSeenAt.UnixMilli() == 3000
	}, "the known machine from the store")
}

// A machine lost before the previous run stopped is not listed after the
// restart: the pool drops it today, and it stays dropped.
func TestKnown_LostBeforeStopIsNotListed(t *testing.T) {
	r := newRig(t)
	a := r.agent(1)
	eventually(t, 5*time.Second, func() bool { f, _ := knownFact(r.p, a.ID); return f.State == StateActive }, "registered")
	a.Close()
	r.clk.advance(LeaseTTL + time.Second)
	r.p.CheckLost()
	if f, _ := knownFact(r.p, a.ID); f.State != StateLost {
		t.Fatalf("not lost: %+v", f)
	}
	r.hist.mu.Lock()
	r.hist.ev = append([]history.Event{{Ts: 1, Type: history.TSchedulerStarted}}, r.hist.ev...)
	r.hist.ev = append(r.hist.ev, history.Event{Ts: r.clk.now().UnixMilli(), Type: history.TSchedulerStopping})
	r.hist.mu.Unlock()
	r.start()
	r.hist.mu.Lock()
	r.hist.ev = append(r.hist.ev, history.Event{Ts: r.clk.now().UnixMilli(), Type: history.TSchedulerStarted})
	evs := append([]history.Event(nil), r.hist.ev...)
	r.hist.mu.Unlock()
	r.p.SetKnown(KnownFrom(scanOf(evs)))
	r.clk.advance(KnownGrace)
	if _, n := knownFact(r.p, a.ID); n != 0 {
		t.Fatal("a machine lost before the stop came back as a ghost")
	}
}
