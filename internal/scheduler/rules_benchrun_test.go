package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
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

func TestStatus_LastRefusalPerProfile(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("huge", 60000)
	if got := h.c.statusDoc().LastRefusals; len(got) != 0 {
		t.Fatalf("refusals before any: %+v", got)
	}
	w := &waiter{profile: "huge", reply: make(chan answer, 2), benchRun: "br-9"}
	h.step(evAcquire{w: w})
	h.failed(w, 503, ReasonDoesNotFit)
	got := h.c.statusDoc().LastRefusals
	if len(got) != 1 || got[0].Profile != "huge" || got[0].Reason != string(ReasonDoesNotFit) || got[0].Status != 503 || got[0].Message == "" || got[0].BenchRun != "br-9" || got[0].At.IsZero() {
		t.Fatalf("last refusals %+v", got)
	}
	// A repeat inside the history dedupe window still updates the status record.
	first := got[0].At
	h.advance(time.Second)
	h.failed(h.request("huge"), 503, ReasonDoesNotFit)
	got = h.c.statusDoc().LastRefusals
	if len(got) != 1 || got[0].BenchRun != "" || !got[0].At.After(first) {
		t.Fatalf("after repeat %+v", got)
	}
}
