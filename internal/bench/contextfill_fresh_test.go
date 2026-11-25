package bench

import (
	"strings"
	"testing"
	"time"
)

// Fresh samples (after step A): a node's figure is its agent's last report,
// so a mark's "after" must be a report taken after the turn ended, "lowest"
// counts only reports taken inside the turn, and a mark with no fresh report
// in time is stale, with no growth given as a number.

func nodeOf(t *testing.T, m FillMark, id string) FillNode {
	t.Helper()
	for _, n := range m.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("mark %d %%: no node %s in %+v", m.Pct, id, m.Nodes)
	return FillNode{}
}

func parseAt(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return v
}

// Reports every 150 ms and turns that end at once: each mark waits for the
// next report (late, within two intervals) and reads the growth from it.
func TestContextFill_WaitsForASampleNewerThanTheTurn(t *testing.T) {
	api := newFake().prof("small", fakeProf{chatFn: recallingFill})
	api.reportEvery = 150 * time.Millisecond
	api.mem = func(id string, loading bool, n int) *int64 {
		v := int64(9000)
		if id == "n1" {
			v -= int64(n) * 40
		}
		return &v
	}
	tm := fillMgr(t, api, func(o *ManagerOptions) { o.FillReportEvery = 150 * time.Millisecond })
	tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[50,90]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.Steps[0].Outcome != OutcomeReady {
		t.Fatalf("%s %s", r.Steps[0].Outcome, r.Steps[0].Reason)
	}
	f := fillOf(t, r, 0)
	waited := false
	for i, m := range f.Marks {
		n1 := nodeOf(t, m, "n1")
		end := parseAt(t, m.TurnEndedAt)
		if n1.Stale || n1.AfterSampledAt == "" || !parseAt(t, n1.AfterSampledAt).After(end) {
			t.Errorf("mark %d: after not newer than the turn's end %s: %+v", i, m.TurnEndedAt, n1)
		}
		if n1.BeforeSampledAt == "" || n1.GrowthMiB == nil || *n1.GrowthMiB != int64(40*(i+1)) || n1.KiBPerToken == nil {
			t.Errorf("mark %d growth: %+v", i, n1)
		}
		if n1.AfterWaitMS != nil && *n1.AfterWaitMS > 0 {
			waited = true
		}
		if n1.AfterWaitMS == nil || *n1.AfterWaitMS > 2*150+100 {
			t.Errorf("mark %d wait: %v", i, n1.AfterWaitMS)
		}
	}
	if !waited {
		t.Error("no mark waited for a report")
	}
}

// A node that stops reporting after the first turn starts: its marks are
// stale, with no growth or KiB per token, no growth row, and the step still
// ends ready; the other nodes read normally.
func TestContextFill_NoFreshSampleIsStaleNotZero(t *testing.T) {
	api := newFake().prof("small", fakeProf{chatFn: recallingFill})
	api.reportEvery = 50 * time.Millisecond
	api.mem = func(id string, loading bool, n int) *int64 { v := int64(9000); return &v }
	api.withhold = func(id string, chats int) bool { return id == "n1" && chats >= 1 }
	tm := fillMgr(t, api, func(o *ManagerOptions) { o.FillReportEvery = 50 * time.Millisecond })
	tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[50,90]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.Steps[0].Outcome != OutcomeReady {
		t.Fatalf("%s %s", r.Steps[0].Outcome, r.Steps[0].Reason)
	}
	f := fillOf(t, r, 0)
	for i, m := range f.Marks {
		n1 := nodeOf(t, m, "n1")
		if !n1.Stale || !strings.Contains(n1.StaleReason, "no fresh sample") || n1.GrowthMiB != nil || n1.KiBPerToken != nil {
			t.Errorf("mark %d n1: %+v", i, n1)
		}
		if n2 := nodeOf(t, m, "n2"); n2.Stale || n2.AfterSampledAt == "" {
			t.Errorf("mark %d n2: %+v", i, n2)
		}
	}
	ms, err := tm.store.RunMeasurements(r.ID, MeasureQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if (m.Metric == "fill_growth_mib" || m.Metric == "fill_kib_per_token") && m.Node == "ps5-a" {
			t.Errorf("a stale mark gave a row: %+v", m)
		}
	}
	if md := fillMDString(f); !strings.Contains(md, "stale") {
		t.Errorf("report does not say stale:\n%s", md)
	}
}

// Turns shorter than a report interval have no report inside them: lowest
// is not given and the count says zero. A turn longer than a few intervals
// counts its reports and gives the lowest of them.
func TestContextFill_LowestCountsOnlySamplesInsideTheTurn(t *testing.T) {
	for _, c := range []struct {
		name  string
		delay time.Duration
		every time.Duration
		some  bool
	}{{"short turn", 0, 300 * time.Millisecond, false}, {"long turn", 500 * time.Millisecond, 50 * time.Millisecond, true}} {
		t.Run(c.name, func(t *testing.T) {
			api := newFake().prof("small", fakeProf{chatFn: func(body string) fakeReply {
				r := recallingFill(body)
				r.delay = c.delay
				return r
			}})
			api.reportEvery = c.every
			api.mem = func(id string, loading bool, n int) *int64 { v := int64(9000) - int64(n)*10; return &v }
			tm := fillMgr(t, api, func(o *ManagerOptions) { o.FillReportEvery = c.every })
			tm.bench(`{"id":"fill","profile":"small","timeout_s":60,"phases":[{"type":"load"},{"type":"context_fill","marks_pct":[90]},{"type":"unload"}]}`)
			tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
			r := tm.finished(tm.run("s").ID)
			f := fillOf(t, r, 0)
			n1 := nodeOf(t, f.Marks[0], "n1")
			if c.some {
				if n1.SamplesDuringTurn < 3 || n1.LowestMiB == nil || *n1.LowestMiB != 8990 {
					t.Errorf("long turn: %+v", n1)
				}
			} else if n1.SamplesDuringTurn != 0 || n1.LowestMiB != nil {
				t.Errorf("short turn: %+v", n1)
			}
		})
	}
}

// The settle hold: after the load, the phase waits the hold (settle_s, else
// the scheduler's free_average_window) before the first turn, and its
// "before" is a report taken after the hold.
func TestContextFill_SettleHoldBeforeTheFirstTurn(t *testing.T) {
	api := newFake().prof("small", fakeProf{chatFn: recallingFill})
	api.reportEvery = 100 * time.Millisecond
	api.mem = func(id string, loading bool, n int) *int64 { v := int64(9000) - int64(n)*40; return &v }
	tm := fillMgr(t, api, func(o *ManagerOptions) {
		o.FillReportEvery = 100 * time.Millisecond
		o.FillSettle = func() time.Duration { return 700 * time.Millisecond } // the scheduler's window
	})
	tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[90]},{"type":"unload"}]}`)
	tm.bench(`{"id":"fill1","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[90],"settle_s":1},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["fill","fill1"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	for step, want := range []int64{700, 1000} {
		f := fillOf(t, r, step)
		if f.SettleHoldMS != want || f.SettleHeldMS < want {
			t.Errorf("step %d: hold %d held %d, want %d", step, f.SettleHoldMS, f.SettleHeldMS, want)
		}
		n1 := nodeOf(t, f.Marks[0], "n1")
		start := parseAt(t, f.Marks[0].TurnStartedAt)
		before := parseAt(t, n1.BeforeSampledAt)
		if before.After(start) || n1.GrowthMiB == nil || *n1.GrowthMiB != 40 {
			t.Errorf("step %d: before %s, turn %s: %+v", step, n1.BeforeSampledAt, f.Marks[0].TurnStartedAt, n1)
		}
	}
}

func fillMDString(f *FillResult) string {
	var b strings.Builder
	fillMD(&b, f)
	return b.String()
}
