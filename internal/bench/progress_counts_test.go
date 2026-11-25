package bench

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// B12: the progress document names the requested placement of the entry in
// flight and counts the outcomes so far.
func TestProgressCountsAndPlacement(t *testing.T) {
	api := newFake().prof("bad", fakeProf{fail: true}).prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"bad","profile":"bad","requests":[{"prompt":"hi","max_tokens":4}]}`)
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["bad","gemma"],"cooldown_s":0,"only_when_idle":false}`)
	run := tm.run("s")
	sawPlacement := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !sawPlacement {
		if d := tm.m.State(); d.Run != nil {
			if c, ok := d.Run["current"].(map[string]any); ok && c["placement"] == PlacementFixed {
				sawPlacement = true
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawPlacement {
		t.Error("current.placement never said fixed for a fixed-profile benchmark")
	}
	tm.finished(run.ID)
	time.Sleep(50 * time.Millisecond)
	tm.evMu.Lock()
	var last map[string]any
	for _, e := range tm.events {
		if e["kind"] == "progress" {
			last = e["run"].(map[string]any)
		}
	}
	tm.evMu.Unlock()
	if last == nil {
		t.Fatal("no progress")
	}
	c, ok := last["counts"].(RunCounts)
	if !ok {
		t.Fatalf("counts: %#v", last["counts"])
	}
	if c != (RunCounts{Steps: 2, Done: 2, Completed: 1, Passed: 1, Failed: 1}) {
		t.Fatalf("final counts %+v", c)
	}
}

func TestProgressPlacementOfAnEntry(t *testing.T) {
	use := func(p *benchspec.Placement) *itemPlan {
		return &itemPlan{members: []*benchspec.Benchmark{{ID: "r.1", Recipe: "r", Applied: &benchspec.Inputs{Placement: p}}}}
	}
	for _, c := range []struct {
		it   *itemPlan
		want string
	}{
		{use(&benchspec.Placement{Mode: benchspec.PlaceAll}), "all"},
		{use(&benchspec.Placement{Mode: benchspec.PlaceAny, N: 2}), "any:2"},
		{use(nil), "auto"},
		{&itemPlan{members: []*benchspec.Benchmark{{ID: "x", Profile: "p"}}}, PlacementFixed},
		{&itemPlan{}, ""},
	} {
		if got := c.it.placement(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.it.members, got, c.want)
		}
	}
}
