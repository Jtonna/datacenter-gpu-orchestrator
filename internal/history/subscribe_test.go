package history

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

func TestSubscribeSeesEveryRecordedEventInOrder(t *testing.T) {
	s := Open(Options{Dir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer s.Close()
	var got []Event
	cancel := s.Subscribe(func(e Event) { got = append(got, e) })
	s.Record(Event{Ts: 1, Type: "a", Severity: Info, Summary: "A."})
	s.Record(Event{Ts: 2, Type: "b", Severity: Warn, Summary: "B."})
	cancel()
	s.Record(Event{Ts: 3, Type: "c", Severity: Info, Summary: "C."})
	if len(got) != 2 || got[0].Type != "a" || got[1].Type != "b" || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Nodes == nil || got[0].Data == nil {
		t.Fatalf("subscriber got a record that differs from the stored shape: %+v", got[0])
	}
}

func TestSubscribeOnAnOffStoreIsHarmless(t *testing.T) {
	s := Open(Options{})
	cancel := s.Subscribe(func(Event) { t.Fatal("an off store delivered an event") })
	s.Record(Event{Type: "x"})
	cancel()
}

// A panicking subscriber does not stop Record (it may run on the scheduler
// loop): the other subscribers and the store still get the event, the
// subscriber stays subscribed, and the failure is reported, also into this
// store, without a deadlock on its lock.
func TestSubscriberPanicIsContained(t *testing.T) {
	s := Open(Options{Dir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer s.Close()
	supervise.ResetForTest()
	fatals := make(chan string, 4)
	restore := supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { fatals <- r },
		Record: func(f supervise.Failure) {
			s.Record(Event{Type: "internalFailure", Severity: Error, Summary: f.Task})
		}})
	defer restore()
	var mu sync.Mutex
	var bad, good []string
	s.Subscribe(func(e Event) {
		mu.Lock()
		bad = append(bad, e.Type)
		mu.Unlock()
		if e.Type == "boom" {
			panic("subscriber bug")
		}
	})
	s.Subscribe(func(e Event) { mu.Lock(); good = append(good, e.Type); mu.Unlock() })
	s.Record(Event{Type: "boom"})
	s.Record(Event{Type: "after"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r := s.Query(Query{Types: []string{"internalFailure"}, Limit: 10}); len(r.Events) == 1 {
			if r.Events[0].Summary != "history.subscriber" {
				t.Fatalf("reported %+v", r.Events[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failure was not recorded into the store")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(good) < 2 || good[0] != "boom" || good[1] != "after" || len(bad) < 2 || bad[1] != "after" {
		t.Fatalf("subscribers got bad=%v good=%v", bad, good)
	}
	if len(fatals) != 0 {
		t.Fatal("one failure escalated")
	}
}

// The writer fails on one event: that event is lost, and the writer goes on
// in a fresh file with the queue kept.
func TestWriterRestartsInAFreshFile(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	restore := supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backoff: func(int) time.Duration { return time.Millisecond },
		Fatal:   func(r string) { t.Errorf("escalated: %s", r) }})
	defer restore()
	dir := t.TempDir()
	clock := time.Unix(1_800_000_000, 0)
	s := Open(Options{Dir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return clock }})
	s.Record(Event{Type: "first"})
	s.Flush()
	h, fired := supervise.PanicAtForTest(supervise.PointName("history.writer", s), 1)
	restoreHook := supervise.SetHookForTest(h)
	s.Record(Event{Type: "lost"})
	s.Record(Event{Type: "after"})
	s.Flush()
	restoreHook()
	s.Close()
	if fired() != 1 {
		t.Fatalf("fired %d", fired())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	if len(files) != 2 {
		t.Fatalf("files %v, want the old one and a fresh one", files)
	}
	var all strings.Builder
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all.Write(b)
	}
	if got := all.String(); !strings.Contains(got, `"type":"first"`) || !strings.Contains(got, `"type":"after"`) || strings.Contains(got, `"type":"lost"`) {
		t.Fatalf("written:\n%s", got)
	}
}
