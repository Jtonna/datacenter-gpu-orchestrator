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

// No request is listed twice in one page, whatever it is doing while the
// page is built: a request that ends while the live rows are copied (here
// still in the live set when it is already final), a finished one both in
// memory and stored, and one stored while still kept in memory.
func TestList_NoRowTwiceAcrossLiveMemoryAndStore(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)}
	r, st := recorderT(t, t.TempDir(), c)
	var all []*Tracker
	for i := 0; i < 10; i++ {
		tr := r.Queued(begin("p"))
		tr.Started(Placement{}, nil)
		c.add(time.Second)
		if i < 8 {
			tr.Ended(done(10, 0, 2))
		}
		all = append(all, tr)
	}
	if err := r.Flush(t.Context()); err != nil { // the 8 finished: stored and still in memory
		t.Fatal(err)
	}
	// The race window: a tracker still in the live set whose row is final.
	all[7].mu.Lock()
	all[7].row.Status = StatusDone
	all[7].mu.Unlock()
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.live[all[7].row.ID] = all[7]
	}()
	for _, f := range []Filter{{}, {Limit: 3}, {Limit: 9}, {Status: []string{StatusDone, StatusRunning}}} {
		ids := map[int64]int{}
		var walk []Request
		for page := (Page{}); ; {
			p, err := r.List(Filter{Status: f.Status, Limit: f.Limit, Before: page.Next})
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range p.Requests {
				ids[row.ID]++
				walk = append(walk, row)
			}
			if p.Next == 0 {
				break
			}
			page = p
		}
		for id, n := range ids {
			if n != 1 {
				t.Fatalf("filter %+v: request %d listed %d times in %+v", f, id, n, walk)
			}
		}
		if len(ids) != 10 {
			t.Fatalf("filter %+v: %d requests, want 10", f, len(ids))
		}
	}
	_ = st
}
