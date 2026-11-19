package scheduler

import (
	"testing"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

// A request that carries a bench run id (X-Bench-Run) tags the load it causes:
// the history records of that instance get data.bench_run. Nothing else about
// scheduling changes.

func TestBenchRun_TagsTheInstancesHistoryRecords(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	w := &waiter{profile: "m", reply: make(chan answer, 2), benchRun: "br-1"}
	h.step(evAcquire{w: w})
	h.wantState("m", "starting")
	h.advance(1)
	h.healthy("m")
	for _, typ := range []string{history.TLoadStarted, history.TLoadFinished} {
		evs := h.of(typ)
		if len(evs) != 1 || evs[0].Data["bench_run"] != "br-1" {
			t.Fatalf("%s events %+v, want bench_run br-1", typ, evs)
		}
	}
}

func TestBenchRun_UntaggedRequestsLeaveRecordsAlone(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.request("m")
	h.advance(1)
	h.healthy("m")
	for _, e := range h.histLog {
		if _, ok := e.Data["bench_run"]; ok {
			t.Fatalf("untagged load carries bench_run: %+v", e)
		}
	}
}

func TestBenchRun_RefusalIsTagged(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("huge", 60000)
	w := &waiter{profile: "huge", reply: make(chan answer, 2), benchRun: "br-2"}
	h.step(evAcquire{w: w})
	h.failed(w, 503, ReasonDoesNotFit)
	evs := h.of(history.TRequestRefused)
	if len(evs) != 1 || evs[0].Data["bench_run"] != "br-2" {
		t.Fatalf("refusal events %+v", evs)
	}
}
