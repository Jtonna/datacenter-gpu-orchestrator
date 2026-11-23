package scheduler

import (
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// A panic inside a read-only query (the plan, the status, the needs) is
// recovered on the core goroutine: the caller gets a 500, the panic is
// counted, the core answers the next query and nothing changed. (0.1.7
// readiness: a divide by zero in a plan took the whole scheduler down.)
func TestReadQueryPanicIsRecovered(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("m", 8000), llamaP("boom", 4000))
	var boom atomic.Bool
	panicky := NeedFunc(func(p *core.Profile) (NeedInfo, error) {
		if boom.Load() && p.Name == "boom" {
			panic("integer divide by zero (test)")
		}
		return NeedInfo{Need: placer.Measured(p.NeedsMiB)}, nil
	})
	if err := f.s.query(ctxT(t), func(c *schedCore) { c.needs = panicky }); err != nil { // on the core goroutine
		t.Fatal(err)
	}
	if err := f.s.Load(ctxT(t), "m"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, ok := f.s.Upstream("m"); return ok })
	before, err := f.s.Status(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := f.s.ReadPanics(); n != 0 {
		t.Fatalf("read panics %d before any", n)
	}

	boom.Store(true)
	_, err = f.s.Plan(ctxT(t), "boom")
	boom.Store(false)
	var se *Error
	if !errors.As(err, &se) || se.Status != http.StatusInternalServerError || se.Reason != ReasonInternal {
		t.Fatalf("plan of a panicking profile: %v", err)
	}
	if n := f.s.ReadPanics(); n != 1 {
		t.Fatalf("read panics %d, want 1", n)
	}
	// supervise owns the recover: task scheduler.read, reported and counted
	// as every internal failure, never toward escalation.
	var st supervise.TaskStats
	for _, x := range supervise.Stats() {
		if x.Name == "scheduler.read" {
			st = x
		}
	}
	if st.Policy != "read" || st.Failures != 1 || st.FailuresLastMin != 0 {
		t.Fatalf("supervise stats %+v", st)
	}

	// the core goes on, and nothing changed
	if r, err := f.s.Plan(ctxT(t), "m"); err != nil || r.Action != PlanServe {
		t.Fatalf("next plan %+v %v", r, err)
	}
	after, err := f.s.Status(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Instances, after.Instances) || before.ConfigRevision != after.ConfigRevision ||
		!reflect.DeepEqual(before.LearnedNeeds, after.LearnedNeeds) {
		t.Fatalf("state changed:\n%+v\n%+v", before.Instances, after.Instances)
	}
	if _, ok := f.s.Upstream("m"); !ok {
		t.Fatal("m no longer served")
	}
}

// A panic in a state-changing event is not recovered: it still stops the
// scheduler, as before (isolation comes with 0.1.8).
func TestStateChangingPanicIsNotRecovered(t *testing.T) {
	h := newHarness(t)
	defer func() {
		if recover() == nil {
			t.Fatal("a panic in a state-changing query was swallowed")
		}
	}()
	h.step(evQuery{fn: func(*schedCore) { panic("in a state-changing query") }})
}
