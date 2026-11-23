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

// The known machines come from the membership events of earlier runs: a
// machine connected when a run ended was last seen when that run ended
// (its stopping event, or its last event after a crash); a lost one when
// its heartbeats stopped; one that left (uninstalled) is not known.
func TestKnownFrom_Fold(t *testing.T) {
	evs := []history.Event{
		{Ts: 1000, Type: history.TSchedulerStarted},
		member(1100, "id-a", "node-a", StateActive, nil),
		member(1200, "id-b", "node-b", StateActive, nil),
		member(1300, "id-c", "node-c", StateActive, nil),
		member(1400, "id-c", "node-c", StateLeft, nil),
		member(1500, "id-d", "node-d", StateActive, nil),
		member(30000, "id-d", "node-d", StateLost, map[string]any{"last_seen": float64(20000)}), // decoded JSON
		member(31000, "id-e", "node-e", StateActive, nil),
		member(45000, "id-e", "node-e", StateLost, nil), // before last_seen was stored: Ts - LostAfter
		{Ts: 50000, Type: history.TSchedulerStopping},
		{Ts: 60000, Type: history.TSchedulerStarted},
		member(61000, "id-b", "node-b", StateActive, nil), // b is back in the second run
		{Ts: 70000, Type: history.TLoadStarted},           // the second run crashed after this
		{Ts: 90000, Type: history.TSchedulerStarted},      // this run
	}
	got := knownByID(KnownFrom(scanOf(evs)))
	if len(got) != 4 {
		t.Fatalf("known = %+v, want a, b, d, e", got)
	}
	want := map[string]int64{"id-a": 50000, "id-b": 70000, "id-d": 20000, "id-e": 45000 - LostAfter.Milliseconds()}
	for id, ms := range want {
		k, ok := got[id]
		if !ok || k.LastSeen.UnixMilli() != ms {
			t.Errorf("%s: %+v, want last seen %d", id, k, ms)
		}
	}
	if got["id-a"].Name != "node-a" {
		t.Errorf("name %q", got["id-a"].Name)
	}
	if _, ok := got["id-c"]; ok {
		t.Error("a machine that left (deregistered by its uninstall) is not known")
	}
	// The event of an earlier start that listed the machine as not back
	// carries its last seen forward, so a machine off across several
	// restarts keeps its first last seen.
	evs = append(evs, member(160000, "id-a", "node-a", StateLost, map[string]any{"known": true, "last_seen": int64(50000)}),
		history.Event{Ts: 200000, Type: history.TSchedulerStarted})
	if k := knownByID(KnownFrom(scanOf(evs)))["id-a"]; k.LastSeen.UnixMilli() != 50000 {
		t.Errorf("after a second restart: %+v", k)
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
	if k := knownByID(KnownFrom(scanOf([]history.Event{e})))["02:00:00:00:00:09"]; !k.LastSeen.Equal(time.UnixMilli(seen.UnixMilli())) {
		t.Fatalf("read back: %+v", k)
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
