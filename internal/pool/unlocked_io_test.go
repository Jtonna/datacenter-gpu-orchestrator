package pool

import (
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/startups"
)

// blockingStore is a start-up store whose first Insert waits until release
// is closed.
type blockingStore struct {
	startups.MemStore
	entered, release chan struct{}
	once             sync.Once
}

func (s *blockingStore) Insert(r *startups.Record) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.MemStore.Insert(r)
}

// A slow start-up store must not hold up the other nodes: the record is
// written after the pool lock is released, so a heartbeat of another node is
// answered while the write waits.
func TestSlowStartupStoreDoesNotDelayOtherNodes(t *testing.T) {
	r := newRig(t)
	const otherID = "02:00:00:00:00:72"
	if ans := r.p.Register(regReq(otherID)); !ans.Accepted { // no start-up report: no store write
		t.Fatalf("%+v", ans)
	}
	bs := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	r.p.o.Startups = bs
	released := false
	release := func() {
		if !released {
			released = true
			close(bs.release)
		}
	}
	defer release()

	regDone := make(chan struct{})
	go func() {
		defer close(regDone)
		r.p.Register(startupReq(upID, "s1", "b1", 20, map[string]float64{"agent_start": 20}))
	}()
	select {
	case <-bs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the registration never wrote its start-up record")
	}

	hbDone := make(chan HeartbeatResult, 1)
	go func() { hbDone <- r.p.Heartbeat(otherID, hbBody("s1", "", true, "true")) }()
	select {
	case res := <-hbDone:
		if res.Status != 200 {
			t.Fatalf("heartbeat %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a heartbeat of another node waited for the start-up store")
	}

	release()
	select {
	case <-regDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the registration did not return after the store answered")
	}
	recs, _ := bs.List(upID, 0)
	if len(recs) != 1 || recs[0].ID == 0 {
		t.Fatalf("stored %+v", recs)
	}
}

// poolSink is a history sink that reads the pool while it records.
type poolSink struct {
	p     *Pool
	mu    sync.Mutex
	types []string
}

func (s *poolSink) Record(e history.Event) {
	_ = s.p.Nodes() // takes the pool lock
	s.mu.Lock()
	s.types = append(s.types, e.Summary)
	s.mu.Unlock()
}

// History is recorded after the pool lock is released, in the order the
// events happened, so a sink may read the pool.
func TestHistorySinkMayReadThePool(t *testing.T) {
	r := newRig(t)
	sink := &poolSink{p: r.p}
	r.p.SetHistory(sink)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.p.Register(regReq(upID))
		r.p.Register(regReq("02:00:00:00:00:72"))
		r.p.Record(upID, "operator note", nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recording history deadlocked on the pool lock")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.types) != 3 || sink.types[2] != "operator note" ||
		sink.types[0] == sink.types[1] {
		t.Fatalf("events %q", sink.types)
	}
}
