package bench

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Retention is off at 0 (the default): even a run finished 200 days ago
// stays whole. Set, it trims a finished, unpinned run: the requests and the
// transcripts go, the run, its steps and its measurements stay (also through
// a later re-mapping), a pinned run is never touched, and every trim is
// logged.
func TestRetentionTrimsAndKeepsMeasurements(t *testing.T) {
	var keep atomic.Int64 // 0: off
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api, func(o *ManagerOptions) {
		o.Retention = func() time.Duration { return time.Duration(keep.Load()) }
	})
	tm.bench(`{"id":"ask","profile":"gemma","phases":[{"type":"load"},{"type":"requests","requests":[{"name":"hi","prompt":"hi","max_tokens":4}]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","cooldown_s":0,"only_when_idle":false,"benchmarks":["ask"]}`)
	old := tm.finished(tm.run("s").ID)
	pinned := tm.finished(tm.run("s").ID)
	recent := tm.finished(tm.run("s").ID)
	// old and pinned finished 200 days ago, recent two days ago.
	ago := func(id string, d time.Duration) {
		at := time.Now().Add(-d).UnixMilli()
		if _, err := tm.store.db.Exec(`UPDATE runs SET queued_at=?, finished_at=? WHERE id=?`, at, at, id); err != nil {
			t.Fatal(err)
		}
	}
	ago(old.ID, 200*24*time.Hour)
	ago(pinned.ID, 200*24*time.Hour)
	ago(recent.ID, 2*24*time.Hour)
	if _, err := tm.store.SetPinned(pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	if r, _ := tm.store.GetRun(pinned.ID); !r.Pinned {
		t.Fatal("not pinned")
	}
	if _, err := tm.store.SetPinned("nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pin unknown: %v", err)
	}
	dir, _ := tm.store.RunDir(old.ID)
	if _, err := os.Stat(filepath.Join(dir, "step-000.json")); err != nil {
		t.Fatal("no transcript:", err)
	}
	before, _ := tm.store.QueryMeasurements(MeasureFilter{Run: old.ID})
	if len(before) == 0 {
		t.Fatal("no measurements")
	}
	requests := func(id string) int {
		var n int
		tm.store.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE step_id IN (SELECT id FROM run_steps WHERE run_id=?)`, id).Scan(&n)
		return n
	}

	// Off: nothing changes, however old.
	tm.m.sweepRetention()
	if r, err := tm.store.GetRun(old.ID); err != nil || r.TrimmedAt != 0 || requests(old.ID) == 0 {
		t.Fatalf("off trimmed a run: %v %+v", err, r)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("off removed files: %v", err)
	}
	if log, _ := tm.store.Removals(0); len(log) != 0 {
		t.Fatalf("off logged %+v", log)
	}

	// The preview lists what 183 days would trim and changes nothing.
	exp, err := tm.store.ExpiredRuns(time.Now().Add(-183*24*time.Hour).UnixMilli(), 0)
	if err != nil || len(exp) != 1 || exp[0].ID != old.ID {
		t.Fatalf("preview %v %+v", err, exp)
	}

	keep.Store(int64(183 * 24 * time.Hour))
	tm.m.sweepRetention()
	r, err := tm.store.GetRun(old.ID)
	if err != nil || r.TrimmedAt == 0 || len(r.Steps) != 1 {
		t.Fatalf("trimmed run: %v %+v", err, r)
	}
	if requests(old.ID) != 0 {
		t.Fatal("requests left")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("files left: %v", err)
	}
	if ms, _ := tm.store.QueryMeasurements(MeasureFilter{Run: old.ID}); len(ms) != len(before) {
		t.Fatalf("measurements %d, had %d", len(ms), len(before))
	}
	for _, id := range []string{pinned.ID, recent.ID} {
		if r, _ := tm.store.GetRun(id); r.TrimmedAt != 0 || requests(id) == 0 {
			t.Fatalf("%s trimmed", id)
		}
	}
	if runs, _ := tm.store.ListRuns(RunFilter{}); len(runs) != 3 {
		t.Fatalf("list %d", len(runs))
	}
	log, err := tm.store.Removals(0)
	if err != nil || len(log) != 1 {
		t.Fatalf("%v %+v", err, log)
	}
	rm := log[0]
	if rm.RunID != old.ID || rm.Actor != "system" || rm.Steps != 1 || rm.Measurements != len(before) || rm.FileBytes == 0 || rm.State != RunCompleted || rm.Name != "s" ||
		!strings.Contains(rm.Reason, "bench_retention_days") || !strings.Contains(rm.Reason, "measurements kept") {
		t.Fatalf("removal %+v", rm)
	}
	if _, err := tm.store.RemoveRun(pinned.ID, "x", "y", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("pinned: %v", err)
	}
	if _, err := tm.store.RemoveRun(old.ID, "x", "y", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("twice: %v", err)
	}
	found := false
	tm.evMu.Lock()
	for _, e := range tm.hist {
		if e.Type == "benchRunRemoved" {
			found = true
		}
	}
	tm.evMu.Unlock()
	if !found {
		t.Error("no history event")
	}
	// A second sweep finds nothing new.
	tm.m.sweepRetention()
	if log, _ := tm.store.Removals(0); len(log) != 1 {
		t.Fatalf("second sweep %+v", log)
	}

	// A later re-mapping (a newer MeasureVersion) keeps the trimmed run's record.
	if _, err := tm.store.db.Exec(`DELETE FROM bench_meta`); err != nil {
		t.Fatal(err)
	}
	ddir := filepath.Dir(tm.store.Path())
	tm.m.Stop(t.Context())
	tm.store.Close()
	s, err := OpenStore(ddir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ms, _ := s.QueryMeasurements(MeasureFilter{Run: old.ID}); len(ms) != len(before) {
		t.Fatalf("after re-mapping %d, had %d", len(ms), len(before))
	}
}
