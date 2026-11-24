package availability

import (
	"context"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// Uptime is up (free, in use, loading) over counted time (up, lost, set
// aside); switched-off time is in neither, unknown time neither.
func TestUptimes_FormulaAndWindow(t *testing.T) {
	c := newClock(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)) // the history begins here
	store := openStore(t, t.TempDir(), c)
	now := time.Date(2026, 11, 30, 12, 30, 0, 0, time.UTC)
	c.set(now)
	from := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) // the hour that holds now-7d
	h := time.Hour.Milliseconds()
	store.AddNodeHours([]usage.NodeHour{
		{Node: "n1", Name: "zone-a", Hour: from.Add(-time.Hour), LostMs: h},                       // before the window: not counted
		{Node: "n1", Name: "zone-a", Hour: from, FreeMs: h / 2, InUseMs: h / 4, LoadingMs: h / 4}, // 3600 up
		{Node: "n1", Name: "zone-a", Hour: from.Add(time.Hour), LostMs: h / 4, SetAsideMs: h / 4, OffMs: h / 2},
		{Node: "n2", Name: "zone-b", Hour: from.Add(2 * time.Hour), OffMs: h}, // only off: no figure
	})
	flushStore(t, store)
	w, m, err := Uptimes(context.Background(), store, nil, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if !time.Time(w.From).Equal(from) || !w.Complete || !w.Recording || !time.Time(*w.CoveredFrom).Equal(from) {
		t.Fatalf("window %+v", w)
	}
	a := m["n1"]
	a.Finish()
	if a.UpS != 3600 || a.LostS != 900 || a.SetAsideS != 900 || a.OffS != 1800 || a.CountedS != 5400 || a.KnownS != 7200 ||
		a.Uptime == nil || *a.Uptime != .667 || a.Name != "zone-a" {
		t.Fatalf("n1 %+v", a.Totals)
	}
	b := m["n2"]
	b.Finish()
	if b.Uptime != nil || b.OffS != 3600 {
		t.Fatalf("n2 %+v", b.Totals)
	}
	// A group is weighted by time.
	var g Totals
	g.Add(m["n1"].Totals)
	g.Add(m["n2"].Totals)
	g.Finish()
	if *g.Uptime != .667 || g.OffS != 5400 {
		t.Fatalf("group %+v", g)
	}
}

// Before the window's days of history exist the figure is over what there
// is, and says from when.
func TestUptimes_ShortHistory(t *testing.T) {
	begins := time.Date(2026, 11, 23, 16, 36, 0, 0, time.UTC)
	c := newClock(begins)
	store := openStore(t, t.TempDir(), c)
	now := begins.Add(26 * time.Hour)
	c.set(now)
	w, _, err := Uptimes(context.Background(), store, nil, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Complete || !time.Time(*w.CoveredFrom).Equal(begins) || !time.Time(*w.HistoryBeginsAt).Equal(begins) {
		t.Fatalf("window %+v", w)
	}
	// An off store: nothing recorded, no figures.
	off := usage.Open(usage.Options{Logger: quiet})
	w, m, _ := Uptimes(context.Background(), off, nil, 7, now)
	if w.Recording || w.CoveredFrom != nil || len(m) != 0 {
		t.Fatalf("off %+v %v", w, m)
	}
}
