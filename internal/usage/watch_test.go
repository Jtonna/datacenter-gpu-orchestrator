package usage

import (
	"sync"
	"testing"
	"time"
)

type watcher struct {
	mu       sync.Mutex
	on       bool
	asked    int
	kinds    []string
	statuses []string
}

func (w *watcher) Watching() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked++
	return w.on
}

func (w *watcher) Changed(kind string, row Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.kinds = append(w.kinds, kind)
	w.statuses = append(w.statuses, row.Status)
}

// The watcher hears queued, started and ended with the row as it is then;
// with no one listening it is asked and nothing more.
func TestRecorder_WatcherHearsEachChangeOnlyWhileWatching(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)}
	r := NewRecorder(nil)
	r.now = c.now
	w := &watcher{}
	r.SetWatcher(w)
	tr := r.Queued(begin("p"))
	tr.Started(Placement{}, nil)
	tr.Ended(done(10, 0, 2))
	if w.asked != 3 || len(w.kinds) != 0 {
		t.Fatalf("asked %d, heard %v", w.asked, w.kinds)
	}
	w.on = true
	tr = r.Queued(begin("p"))
	tr.Started(Placement{Nodes: []Node{{ID: "n1", Role: "host"}}}, nil)
	tr.Ended(done(10, 0, 2))
	tr.Ended(done(10, 0, 2)) // only the first end counts
	if got := w.kinds; len(got) != 3 || got[0] != ChangeQueued || got[1] != ChangeStarted || got[2] != ChangeEnded {
		t.Fatalf("%v", got)
	}
	if got := w.statuses; got[0] != StatusQueued || got[1] != StatusRunning || got[2] != StatusDone {
		t.Fatalf("%v", got)
	}
	r.SetWatcher(nil)
	r.Queued(begin("p"))
	if len(w.kinds) != 3 {
		t.Fatal("heard after the watcher was removed")
	}
}

// A request that just ended is listed (and found by id) before the writer
// stores it, and on an off store at all.
func TestRecorder_JustEndedRowsAreListedBeforeTheyAreStored(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)}
	r := NewRecorder(nil)
	r.now = c.now
	var ids []int64
	for i := 0; i < recentEnded+5; i++ {
		tr := r.Queued(begin("p"))
		tr.Started(Placement{}, nil)
		c.add(time.Second)
		tr.Ended(done(10, 0, 2))
		ids = append(ids, tr.ID())
	}
	page, err := r.List(Filter{Limit: 10})
	if err != nil || len(page.Requests) != 10 || page.Requests[0].ID != ids[len(ids)-1] || page.Next != page.Requests[9].ID {
		t.Fatalf("%v %+v", err, page)
	}
	next, _ := r.List(Filter{Limit: 500, Before: page.Next})
	if len(next.Requests) != recentEnded-10 || next.Next != 0 {
		t.Fatalf("%d rows, next %d", len(next.Requests), next.Next)
	}
	if row, err := r.Get(ids[len(ids)-1]); err != nil || row.Status != StatusDone || row.Nodes == nil {
		t.Fatalf("%v %+v", err, row)
	}
	if _, err := r.Get(ids[0]); err != ErrNotFound { // fell out of the recent ones, never stored
		t.Fatal(err)
	}

	// On a store whose writer is held, the row is listed once, then once
	// again after it is stored.
	gate := make(chan struct{})
	st := openT(t, Options{Dir: t.TempDir(), Now: c.now, BeforeBatch: func() { <-gate }})
	r2 := NewRecorder(st)
	tr := r2.Queued(begin("p"))
	tr.Ended(End{Status: StatusCancelled, Reason: ReasonClientLeftQueued})
	if p, _ := r2.List(Filter{}); len(p.Requests) != 1 || p.Requests[0].Status != StatusCancelled {
		t.Fatalf("%+v", p)
	}
	close(gate)
	if err := r2.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p, _ := r2.List(Filter{}); len(p.Requests) != 1 {
		t.Fatalf("%+v", p)
	}
}

type prefillLive struct {
	fakeLive
	total, reused, done int
	tps                 float64
	ok                  bool
}

func (p *prefillLive) Prefill() (int, int, int, float64, bool) {
	return p.total, p.reused, p.done, p.tps, p.ok
}

// While the prompt is read a live row says tokens in, reused and how many
// are done; once read, tokens in and the prefill rate.
func TestRecorder_LiveRowSaysThePromptWhenTheServerDoes(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)}
	r := NewRecorder(nil)
	r.now = c.now
	src := &prefillLive{}
	tr := r.Queued(begin("p"))
	tr.Started(Placement{}, src)
	row, _ := r.LiveByID(tr.ID())
	if row.TokensIn != nil || row.PrefillProcessed != nil || row.Live {
		t.Fatalf("%+v", row)
	}
	src.total, src.reused, src.done, src.tps, src.ok = 6000, 2000, 4048, 4096, true
	row, _ = r.LiveByID(tr.ID())
	if *row.TokensIn != 6000 || *row.TokensReused != 2000 || *row.PrefillProcessed != 4048 || *row.PrefillTPS != 4096 || !row.Live {
		t.Fatalf("%+v", row)
	}
	src.done = 6000
	row, _ = r.LiveByID(tr.ID())
	if row.PrefillProcessed != nil || *row.TokensIn != 6000 {
		t.Fatalf("%+v", row)
	}
}
