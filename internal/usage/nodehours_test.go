package usage

import (
	"context"
	"testing"
	"time"
)

func TestNodeHours_AddsUpAcrossWritesAndRestarts(t *testing.T) {
	dir := t.TempDir()
	h0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	clock := h0.Add(90 * time.Minute)
	now := func() time.Time { return clock }
	s := Open(Options{Dir: dir, Now: now})
	if !s.AddNodeHours([]NodeHour{{Node: "02:00:00:00:00:01", Name: "zone-a", Hour: h0.Add(5 * time.Minute), FreeMs: 1000, InUseMs: 2000, BusyMs: 500, ReachableMs: 3000}}) {
		t.Fatal("not queued")
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// A restart in the same hour adds to the row.
	s = Open(Options{Dir: dir, Now: now})
	defer s.Close()
	s.AddNodeHours([]NodeHour{
		{Node: "02:00:00:00:00:01", Hour: h0, LostMs: 4000, SetAsideMs: 1000},
		{Node: "02:00:00:00:00:01", Name: "zone-a2", Hour: h0.Add(time.Hour), OffMs: 7000, ReachableMs: 7000},
	})
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.NodeHours(context.Background(), h0, h0.Add(2*time.Hour))
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %+v", err, rows)
	}
	r := rows[0]
	if !r.Hour.Equal(h0) || r.Name != "zone-a" || r.FreeMs != 1000 || r.InUseMs != 2000 || r.LostMs != 4000 || r.SetAsideMs != 1000 ||
		r.BusyMs != 500 || r.ReachableMs != 3000 || r.KnownMs() != 8000 {
		t.Fatalf("first hour %+v", r)
	}
	if r := rows[1]; r.Name != "zone-a2" || r.OffMs != 7000 || r.KnownMs() != 7000 {
		t.Fatalf("second hour %+v", r)
	}
	// The range is [from, to).
	if rows, _ := s.NodeHours(context.Background(), h0.Add(time.Hour), h0.Add(2*time.Hour)); len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
}

func TestNodeHours_TrimmedWithTheHourlyRetention(t *testing.T) {
	h0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	clock := h0
	s := Open(Options{Dir: t.TempDir(), Now: func() time.Time { return clock }, Retention: Retention{KeepHourly: 48 * time.Hour, TrimEvery: time.Hour}})
	defer s.Close()
	s.AddNodeHours([]NodeHour{{Node: "n", Hour: h0.Add(-72 * time.Hour), FreeMs: 1}, {Node: "n", Hour: h0.Add(-24 * time.Hour), FreeMs: 1}})
	s.Flush(context.Background())
	if n := s.Trim(context.Background()); n != 1 {
		t.Fatalf("trimmed %d", n)
	}
	rows, _ := s.NodeHours(context.Background(), h0.Add(-100*time.Hour), h0)
	if len(rows) != 1 || !rows[0].Hour.Equal(h0.Add(-24*time.Hour)) {
		t.Fatalf("%+v", rows)
	}
}

func TestNodeHours_OffStoreNeitherWritesNorFails(t *testing.T) {
	s := Open(Options{})
	if s.AddNodeHours([]NodeHour{{Node: "n", FreeMs: 1}}) {
		t.Fatal("queued on an off store")
	}
	if rows, err := s.NodeHours(context.Background(), time.Time{}, time.Now()); rows != nil || err != nil {
		t.Fatal(rows, err)
	}
}
