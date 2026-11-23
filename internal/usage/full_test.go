package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pagesOf is the page count of a fresh usage.db in dir.
func pagesOf(t *testing.T, dir string) int {
	t.Helper()
	s := Open(Options{Dir: dir})
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`PRAGMA page_count`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A full disk: the batches fail ("database or disk is full"), are counted
// in failed with last_error, one WARN in a minute however many fail, the
// callers never wait, and the store stays on and keeps trying: once pages
// are free again (here the retention trim frees them) writes succeed.
func TestFullDisk_FailsCountedQuietlyAndRecovers(t *testing.T) {
	dir := t.TempDir()
	pages := pagesOf(t, dir)
	c := &clock{t: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}
	log, buf := newLog()
	st := openT(t, Options{Dir: dir, Now: c.now, Logger: log, MaxPages: pages + 4, BatchSize: 16})
	r := NewRecorder(st)
	slowest := time.Duration(0)
	for i := 0; i < 3000; i++ {
		t0 := time.Now()
		tr := r.Queued(begin(fmt.Sprintf("profile-%d", i%7)))
		tr.Started(Placement{Nodes: []Node{{ID: "02:00:00:00:00:01", Name: "zone-1", Role: "host"}}}, nil)
		tr.Ended(done(100, 20, 30))
		slowest = max(slowest, time.Since(t0))
		if i%500 == 0 {
			if err := r.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := r.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := st.Stats()
	if !s.On || s.Failed == 0 || !strings.Contains(s.LastError, "full") || s.Written == 0 || s.SizeBytes == 0 {
		t.Fatalf("%+v", s)
	}
	if n := strings.Count(buf.String(), "a write failed"); n != 1 {
		t.Fatalf("%d WARN lines for %d failed operations:\n%s", n, s.Failed, buf.String())
	}
	if slowest > 100*time.Millisecond {
		t.Fatalf("a request-path call took %v on a full disk", slowest)
	}
	t.Logf("full disk: %d operations failed, %d written; last error %q", s.Failed, s.Written, s.LastError)

	// The trim frees pages; the store, still on, writes again.
	c.add(200 * 24 * time.Hour)
	if st.Trim(t.Context()) == 0 {
		t.Fatal("nothing trimmed")
	}
	failed, written := st.Stats().Failed, st.Stats().Written
	for i := 0; i < 5; i++ {
		r.Queued(begin("after")).Ended(done(1, 0, 1))
	}
	if err := r.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := st.Stats(); s.Failed != failed || s.Written < written+10 {
		t.Fatalf("no recovery: failed %d -> %d, written %d -> %d", failed, s.Failed, written, s.Written)
	}
}

// Bytes on disk per request row and per node-hour row, indexes and
// roll-ups included: 10,000 of each, checkpointed. The figures are logged
// (go test -run TestSize -v) and held to a loose bound.
func TestSize_BytesPerRequestAndPerNodeHour(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 20,000 rows")
	}
	size := func(st *Store) int64 {
		t.Helper()
		if _, err := st.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(st.opt.Dir, FileName))
		if err != nil {
			t.Fatal(err)
		}
		if wal, err := os.Stat(filepath.Join(st.opt.Dir, FileName+"-wal")); err == nil && wal.Size() != 0 {
			t.Fatalf("WAL %d bytes after a truncating checkpoint", wal.Size())
		}
		return fi.Size()
	}
	const n = 10000

	// Requests: a day of them, 7 profiles, two machines each, a third from
	// a benchmark run, as the recorder writes them (queued, started, ended).
	c := &clock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	st := openT(t, Options{Dir: t.TempDir(), Now: c.now})
	r := NewRecorder(st)
	if err := r.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	base := size(st)
	for i := 0; i < n; i++ {
		b := begin(fmt.Sprintf("bench-qwen3-coder-30b-%d-32768", i%7))
		b.ModelFile = "Qwen3-Coder-30B-A3B-Instruct-Q4_K_M.gguf"
		if i%3 == 0 {
			b.BenchRun = "run-20261117T000000-0a1b2c"
		}
		tr := r.Queued(b)
		c.add(8 * time.Second)
		tr.Started(Placement{SchedID: uint64(i), Instance: 7, Session: "ld-1795392000000-7", HostNode: "02:00:00:00:00:01",
			Nodes: []Node{{ID: "02:00:00:00:00:01", Name: "zone-01", Role: "host"}, {ID: "02:00:00:00:00:02", Name: "zone-02", Role: "helper"}}}, nil)
		tr.Ended(done(2460, 2000, 318))
		if i%1000 == 0 {
			r.Flush(t.Context())
		}
	}
	if err := r.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	reqBytes := size(st) - base
	if s := st.Stats(); s.Failed != 0 || s.Dropped != 0 {
		t.Fatalf("%+v", s)
	}

	// Node-hours: 14 machines for 715 hours.
	c2 := &clock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	st2 := openT(t, Options{Dir: t.TempDir(), Now: c2.now})
	st2.Flush(t.Context())
	base2 := size(st2)
	var rows []NodeHour
	for i := 0; i < n; i++ {
		rows = append(rows, NodeHour{Node: fmt.Sprintf("02:00:00:00:00:%02x", i%14), Name: fmt.Sprintf("zone-%02d", i%14),
			Hour: c2.now().Add(time.Duration(i/14) * time.Hour), FreeMs: 1800000, InUseMs: 1200000, LoadingMs: 300000,
			LostMs: 0, SetAsideMs: 0, OffMs: 300000, BusyMs: 600000, ReachableMs: 3300000})
		if len(rows) == 200 {
			st2.AddNodeHours(rows)
			rows = nil
		}
	}
	st2.AddNodeHours(rows)
	if err := st2.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	st2.DB().QueryRow(`SELECT COUNT(*) FROM node_hours`).Scan(&count)
	if count != n {
		t.Fatalf("%d node-hours stored", count)
	}
	nhBytes := size(st2) - base2

	t.Logf("empty file %d bytes; %d requests: %d bytes, %.0f per row (row, indexes and roll-ups); %d node-hours: %d bytes, %.0f per row",
		base, n, reqBytes, float64(reqBytes)/n, n, nhBytes, float64(nhBytes)/n)
	if reqBytes/n > 2048 || nhBytes/n > 1024 {
		t.Fatalf("larger than expected: %d and %d bytes per row", reqBytes/n, nhBytes/n)
	}
}
