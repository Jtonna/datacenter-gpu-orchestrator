package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

type availAnswer struct {
	From            string                    `json:"from"`
	To              string                    `json:"to"`
	Step            string                    `json:"step"`
	HistoryBeginsAt *string                   `json:"history_begins_at"`
	Recording       bool                      `json:"recording"`
	Nodes           []availability.NodeSeries `json:"nodes"`
}

func getAvail(t *testing.T, e *env, query string) (int, availAnswer, string) {
	t.Helper()
	resp := e.get("/api/nodes/availability" + query)
	b := body(t, resp)
	var a availAnswer
	if resp.StatusCode == 200 {
		if err := json.Unmarshal([]byte(b), &a); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
	}
	return resp.StatusCode, a, b
}

// newUsageEnv is an env whose usage store reads the env's clock.
func newUsageEnv(t *testing.T, dir string, mod ...func(*usage.Options)) (*env, *usage.Store) {
	t.Helper()
	var ep atomic.Pointer[env]
	start := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC) // newEnv's clock
	var store *usage.Store
	e := newEnv(t, chatOK, func(o *Options) {
		uo := usage.Options{Dir: dir, Logger: o.Logger, Now: func() time.Time {
			if e := ep.Load(); e != nil {
				e.nowMu.Lock()
				defer e.nowMu.Unlock()
				return e.clock
			}
			return start
		}}
		for _, m := range mod {
			m(&uo)
		}
		store = usage.Open(uo)
		o.Usage = store
	})
	ep.Store(e)
	t.Cleanup(func() { store.Close() }) // after the API's Close (cleanups run last first)
	waitFor(t, func() bool { return e.api.avail.Reads() >= 1 })
	return e, store
}

// tick advances the env's clock by 10 s n times, sampling each time.
func tick(e *env, n int) {
	for range n {
		e.advance(10 * time.Second)
		e.api.avail.Tick(context.Background())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestAvailability_RouteAnswersEachMachineHourByHour(t *testing.T) {
	e, store := newUsageEnv(t, t.TempDir())
	tick(e, 6) // a free minute
	if r := e.post("/api/load", `{"model":"alpha"}`); r.StatusCode/100 != 2 {
		t.Fatalf("load: %d %s", r.StatusCode, body(t, r))
	}
	waitFor(t, func() bool {
		st, _ := e.sched.Status(context.Background())
		return len(st.Instances) == 1 && st.Instances[0].State == "ready"
	})
	tick(e, 12) // two minutes in use (the first interval still counts the state before)
	e.api.avail.Flush()
	if err := store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	code, a, raw := getAvail(t, e, "")
	if code != 200 || a.Step != "hour" || !a.Recording || a.HistoryBeginsAt == nil || len(a.Nodes) != 1 {
		t.Fatalf("%d %s", code, raw)
	}
	n := a.Nodes[0]
	if n.ID != "host-1" || n.Name != "host-1" || len(n.Cells) != 24 || n.Uptime == nil || *n.Uptime != 1 {
		t.Fatalf("%+v", n)
	}
	last := n.Cells[23]
	if !last.Partial || last.Seconds.Free+last.Seconds.InUse != 180 || last.Seconds.InUse < 110 || last.Seconds.InUse > 120 ||
		last.State != availability.InUse || last.BusyLevel != 1 {
		t.Fatalf("now %+v", last)
	}
	for _, c := range n.Cells[:23] {
		if c.State != availability.Unknown || c.Seconds.Unknown != 3600 {
			t.Fatalf("before the history: %+v", c)
		}
	}
	// One machine by name, a day roll-up, and the refusals.
	if code, a, raw := getAvail(t, e, "?node=host-1&step=day&from=2d"); code != 200 || a.Step != "day" || len(a.Nodes) != 1 || len(a.Nodes[0].Cells) != 3 {
		t.Fatalf("%d %s", code, raw)
	}
	for q, want := range map[string]int{"?node=nope": 404, "?group=nope": 404, "?step=week": 400, "?from=yesterday": 400,
		"?from=1795000000000&to=1794000000000": 400, "?from=900d": 400} {
		if code, _, raw := getAvail(t, e, q); code != want {
			t.Errorf("%s: %d %s", q, code, raw)
		}
	}
	// One zone group: only its zones.
	if code, a, raw := getAvail(t, e, "?group=rack-a"); code != 404 || len(a.Nodes) != 0 {
		t.Fatalf("group before it exists: %d %s", code, raw)
	}
	e.api.groups.Create("rack-a", nil)
	if code, a, raw := getAvail(t, e, "?group=rack-a"); code != 200 || len(a.Nodes) != 0 {
		t.Fatalf("empty group: %d %s", code, raw)
	}
	e.api.groups.AddMembers("rack-a", []string{"host-1"})
	if code, a, raw := getAvail(t, e, "?group=RACK-A"); code != 200 || len(a.Nodes) != 1 || a.Nodes[0].ID != "host-1" {
		t.Fatalf("group: %d %s", code, raw)
	}

	// Uptime over 7 days: the history is three minutes old, all of it up.
	var up UptimeDoc
	resp := e.get("/api/nodes/uptime")
	raw = body(t, resp)
	if err := json.Unmarshal([]byte(raw), &up); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if up.Window.Days != 7 || up.Window.Complete || !up.Window.Recording || up.Window.CoveredFrom == nil ||
		len(up.Zones) != 1 || up.Zones[0].Group != "rack-a" || up.Zones[0].Uptime == nil || *up.Zones[0].Uptime != 1 || up.Zones[0].UpS < 170 ||
		len(up.Groups) != 1 || up.Groups[0].Name != "rack-a" || *up.Groups[0].Uptime != 1 || up.Ungrouped.Uptime != nil || *up.Fleet.Uptime != 1 {
		t.Fatalf("uptime %s", raw)
	}
	if !strings.Contains(body(t, e.get("/api/groups")), `"uptime_7d":{"free_s"`) {
		t.Fatal("no uptime_7d on the groups")
	}
	for _, q := range []string{"?days=0", "?days=91", "?days=x"} {
		if r := e.get("/api/nodes/uptime" + q); r.StatusCode != 400 {
			t.Errorf("%s: %d", q, r.StatusCode)
		}
	}
}

// The sampler reads the status as any reader does (one read-only query per
// tick, measured by the core loop's longest event, Scheduler.LongestStep)
// and only queues its writes: with the usage writer stuck it samples on.
func TestAvailability_SamplerCostsTheCoreLoopNothing(t *testing.T) {
	release := make(chan struct{})
	e, store := newUsageEnv(t, t.TempDir(), func(o *usage.Options) {
		o.BeforeBatch = func() { <-release }
	})
	defer close(release)
	t0 := time.Now()
	tick(e, 2000) // 5.5 hours of samples, a write every minute
	took := time.Since(t0)
	longest, ev := e.sched.LongestStep()
	// A status read takes microseconds; the bound only leaves room for a
	// pause of the process (a write on the core loop would hang here: the
	// writer is stuck).
	t.Logf("longest core event: %v (%s)", longest, ev)
	if longest > 250*time.Millisecond {
		t.Fatalf("the core loop's longest event was %v (%s) while sampling", longest, ev)
	}
	if took > 10*time.Second {
		t.Fatalf("2000 samples took %v with the writer stuck", took)
	}
	if got := e.api.avail.Reads(); got < 2000 || got > 2002 {
		t.Fatalf("%d status reads for 2000 samples", got)
	}
	if q := store.Stats().Queued; q > 340 {
		t.Fatalf("%d writes queued for 333 minutes", q)
	}
	// The scheduler answers as before.
	if r := e.get("/health"); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
}

func TestAvailability_StoreOffOrDamagedKeepsTheSchedulerHealthy(t *testing.T) {
	// Off: no sampler starts (no core query at all), the route answers an
	// empty series that says it is not recording.
	e := newEnv(t, chatOK, func(o *Options) { o.Usage = usage.Open(usage.Options{Logger: o.Logger}) })
	if e.api.avail != nil {
		t.Fatal("the sampler started on an off store")
	}
	code, a, raw := getAvail(t, e, "")
	if code != 200 || a.Recording || a.HistoryBeginsAt != nil || len(a.Nodes) != 0 {
		t.Fatalf("%d %s", code, raw)
	}
	for _, p := range []string{"/health", "/api/status", "/api/nodes"} {
		if r := e.get(p); r.StatusCode != 200 && !(p == "/api/nodes" && r.StatusCode == 503) {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}

	// No store at all: an empty series.
	e2 := newEnv(t, chatOK)
	if code, a, raw := getAvail(t, e2, ""); code != 200 || a.Recording || len(a.Nodes) != 0 || a.HistoryBeginsAt != nil {
		t.Fatalf("%d %s", code, raw)
	}

	// Damaged: moved aside, the history begins now, an empty series.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, usage.FileName), []byte("damaged, not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	e3, _ := newUsageEnv(t, dir)
	code, a, raw = getAvail(t, e3, "")
	if code != 200 || !a.Recording || a.HistoryBeginsAt == nil || *a.HistoryBeginsAt != isoTime(e3.clock) {
		t.Fatalf("%d %s", code, raw)
	}
	if len(a.Nodes) != 1 || a.Nodes[0].KnownS != 0 {
		t.Fatalf("%s", raw)
	}
	if r := e3.get("/health"); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
}
