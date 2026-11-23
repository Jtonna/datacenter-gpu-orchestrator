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

// Retention removes everything of a finished run together (rows and
// transcripts), never a pinned run, and logs every removal. 0 keeps for ever.
func TestRetentionRemovesWholeRunsAndLogsThem(t *testing.T) {
	var keep atomic.Int64 // 0: keep for ever
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api, func(o *ManagerOptions) {
		o.Retention = func() time.Duration { return time.Duration(keep.Load()) }
	})
	tm.bench(`{"id":"ask","profile":"gemma","phases":[{"type":"load"},{"type":"requests","requests":[{"name":"hi","prompt":"hi","max_tokens":4}]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","cooldown_s":0,"only_when_idle":false,"benchmarks":["ask"]}`)
	old := tm.finished(tm.run("s").ID)
	pinned := tm.finished(tm.run("s").ID)
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
		t.Fatal("no transcript to remove:", err)
	}
	time.Sleep(5 * time.Millisecond)

	tm.m.sweepRetention() // keep for ever
	if _, err := tm.store.GetRun(old.ID); err != nil {
		t.Fatalf("0 removed a run: %v", err)
	}

	keep.Store(int64(time.Millisecond))
	tm.m.sweepRetention()
	if _, err := tm.store.GetRun(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired run still there: %v", err)
	}
	if _, err := tm.store.GetRun(pinned.ID); err != nil {
		t.Fatalf("pinned run removed: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("files left: %v", err)
	}
	if ms, _ := tm.store.QueryMeasurements(MeasureFilter{Run: old.ID}); len(ms) != 0 {
		t.Fatalf("measurements left: %d", len(ms))
	}
	runs, _ := tm.store.ListRuns(RunFilter{})
	if len(runs) != 1 || runs[0].ID != pinned.ID {
		t.Fatalf("list %+v", runs)
	}
	log, err := tm.store.Removals(0)
	if err != nil || len(log) != 1 {
		t.Fatalf("%v %+v", err, log)
	}
	rm := log[0]
	if rm.RunID != old.ID || rm.Actor != "system" || rm.Steps != 1 || rm.Measurements == 0 || rm.FileBytes == 0 || rm.State != RunCompleted || rm.Name != "s" ||
		!strings.Contains(rm.Reason, "bench_retention_days") {
		t.Fatalf("removal %+v", rm)
	}
	if _, err := tm.store.RemoveRun(pinned.ID, "x", "y", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("pinned: %v", err)
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

	// Unpinned, it goes at the next check.
	tm.store.SetPinned(pinned.ID, false)
	tm.m.sweepRetention()
	if runs, _ := tm.store.ListRuns(RunFilter{}); len(runs) != 0 {
		t.Fatalf("%+v", runs)
	}
}
