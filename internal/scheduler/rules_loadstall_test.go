package scheduler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// Load stall detection (design 7.6): a measured upload whose acknowledged
// bytes stop rising fails after load_stall_timeout.

const stallFile = 14_334_446_752

// stallHarness loads "m" onto two workers with a one-hour load timeout, so
// only the stall rule can end the load, and returns the waiting request.
func stallHarness(t *testing.T, mod ...func(*core.Profile)) (*harness, *waiter) {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.LoadTimeout = []string{"a", "b"}, []int{1, 1}, time.Hour
		for _, f := range mod {
			f(p)
		}
	})
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: stallFile}
	w := h.request("m")
	h.wantState("m", "starting")
	return h, w
}

func sentAt(h *harness, a, b int64) {
	h.child("m").setSentPeers(map[string]int64{addrA: a, addrB: b})
}

func TestLoadStall_FailsAfterTheTimeoutWithoutNewBytes(t *testing.T) {
	h, w := stallHarness(t)
	h.autoExit = false
	sentAt(h, 549, 0)
	h.advance(179 * time.Second)
	h.pending(w)
	h.advance(3 * time.Second)
	e := h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
	want := "load stalled: no bytes acknowledged by the workers for 3m0s (sent 549 of 14334446752)"
	if !strings.Contains(e.Message, want) {
		t.Fatalf("message %q, want it to contain %q", e.Message, want)
	}
	h.wantState("m", "stopping")
	if _, on := h.c.backoffActive("m"); !on {
		t.Fatal("no profile backoff")
	}
	if h.c.quarantined("a") || h.c.quarantined("b") {
		t.Fatal("a stalled load must not quarantine nodes")
	}
	got := h.recent()
	if len(got) != 1 || got[0].Outcome != LoadFailed || got[0].Reason != want {
		t.Fatalf("recent %+v", got)
	}
	var finished *LoadOutcome
	for _, ev := range h.feedLog {
		if ev.Kind == FeedLoadFinished {
			finished = ev.Outcome
		}
	}
	if finished == nil || finished.Outcome != LoadFailed || finished.Reason != want {
		t.Fatalf("loadFinished event %+v", finished)
	}
	recs := h.logs.find("load stalled", "profile", "m", "bytes_sent", 549)
	if len(recs) != 1 || recs[0].Level.String() != "ERROR" || !strings.Contains(recs[0].Attrs["workers"].(string), addrA+"=549") {
		t.Fatalf("log records %+v", recs)
	}
}

func TestLoadStall_QuietStartCountsFromTheFirstReading(t *testing.T) {
	h, w := stallHarness(t)
	h.child("m").setSent(0, 0, nil) // no connection yet: not a reading
	h.advance(30 * time.Second)
	sentAt(h, 0, 0) // connected, still nothing sent: the clock starts here
	h.advance(170 * time.Second)
	h.pending(w)
	h.advance(15 * time.Second)
	h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
}

func TestLoadStall_GrowthRestartsTheClock(t *testing.T) {
	h, w := stallHarness(t)
	sentAt(h, 100, 0)
	h.advance(170 * time.Second)
	sentAt(h, 101, 0)
	h.advance(170 * time.Second)
	h.pending(w)
	sentAt(h, 101, 5) // a worker's own growth also grows the total
	h.advance(170 * time.Second)
	h.pending(w)
	h.advance(15 * time.Second)
	h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
}

func TestLoadStall_NotAppliedToAnEstimate(t *testing.T) {
	h, w := stallHarness(t) // the fake child has no byte counter reading
	h.advance(10 * time.Minute)
	h.pending(w)
	h.wantState("m", "starting")
}

func TestLoadStall_NotAppliedBeforeAnyReading(t *testing.T) {
	h, w := stallHarness(t)
	h.child("m").setSent(0, 0, nil) // no socket yet: not a reading
	h.advance(10 * time.Minute)
	h.pending(w)
}

func TestLoadStall_NotAppliedOnceTheUploadIsComplete(t *testing.T) {
	h, w := stallHarness(t)
	sentAt(h, stallFile/2, stallFile/2) // 100%: warmup and context creation are quiet
	h.advance(20 * time.Minute)
	h.pending(w)
	h.wantState("m", "starting")
	sentAt(h, stallFile*99/200, stallFile*99/200) // 99%
	h.advance(20 * time.Minute)
	h.pending(w)
}

func TestLoadStall_NotAppliedToALocalOnlyLoad(t *testing.T) {
	h := newHarness(t).nodes(omen(8000))
	h.llama("m", 6000, func(p *core.Profile) { p.LoadTimeout = time.Hour })
	w := h.request("m")
	h.wantState("m", "starting")
	h.advance(10 * time.Minute)
	h.pending(w)
}

func TestLoadStall_ZeroDisablesAndProfileOverrides(t *testing.T) {
	h, w := stallHarness(t)
	h.cfg.Defaults.LoadStallTimeout = 0
	sentAt(h, 1, 0)
	h.advance(20 * time.Minute)
	h.pending(w)

	zero := time.Duration(0)
	h, w = stallHarness(t, func(p *core.Profile) { p.LoadStallTimeout = &zero })
	sentAt(h, 1, 0)
	h.advance(20 * time.Minute)
	h.pending(w)

	short := 30 * time.Second
	h, w = stallHarness(t, func(p *core.Profile) { p.LoadStallTimeout = &short })
	sentAt(h, 1, 0)
	h.advance(31 * time.Second)
	e := h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
	if !strings.Contains(e.Message, "for 30s (sent 1 of") {
		t.Fatalf("message %q", e.Message)
	}
}

func TestLoadStall_TimeRemainingIsDroppedAndStalledSecondsShown(t *testing.T) {
	h, _ := stallHarness(t)
	sentAt(h, 500*mb, 500*mb)
	h.advance(10 * time.Second)
	sentAt(h, 600*mb, 600*mb)
	h.advance(5 * time.Second) // the rise is seen within a second of this
	s := h.loadStatus("m")
	if s.LoadEtaS == nil || h.report("m").EtaS == nil || h.report("m").StalledS != nil {
		t.Fatalf("an eta is expected while bytes move: %+v", h.report("m"))
	}
	h.advance(7 * time.Second) // about 12 s without growth
	if s = h.loadStatus("m"); s.LoadEtaS == nil || h.report("m").StalledS != nil {
		t.Fatalf("at 12 s: eta %v stalled %v", s.LoadEtaS, h.report("m").StalledS)
	}
	h.advance(8 * time.Second) // about 20 s
	s = h.loadStatus("m")
	r := h.report("m")
	if s.LoadEtaS != nil || r.EtaS != nil {
		t.Fatalf("eta still shown: flat %v, load %v", s.LoadEtaS, r.EtaS)
	}
	near(t, "stalled_s", r.StalledS, 19.5, 1.5)
	if s.LoadProgress == nil || r.Progress == nil || r.BytesSent == nil || *r.BytesSent != 1200*mb {
		t.Fatalf("other fields changed: %+v", r)
	}
	b, _ := json.Marshal(r.StalledS)
	t.Logf("stalled_s = %s", b)
	// The 30 s progress line says stalled= instead of an eta.
	h.advance(15 * time.Second)
	var line *logRec
	for _, rec := range h.logs.find("load progress") {
		rec := rec
		line = &rec
	}
	if line == nil || line.Attrs["stalled"] == nil || line.Attrs["eta"] != nil {
		t.Fatalf("last load progress line %+v", line)
	}
	// Bytes moving again brings the eta back and drops stalled_s.
	sentAt(h, 700*mb, 700*mb)
	h.advance(2 * time.Second)
	if h.report("m").StalledS != nil || h.report("m").EtaS == nil {
		t.Fatalf("not recovered: %+v", h.report("m"))
	}
}

func TestLoadStall_EstimateKeepsItsEtaAndNeverShowsStalled(t *testing.T) {
	h, _ := stallHarness(t)
	h.advance(60 * time.Second)
	s := h.loadStatus("m")
	if s.LoadEtaS == nil || h.report("m").StalledS != nil {
		t.Fatalf("estimate: eta %v load %+v", s.LoadEtaS, h.report("m"))
	}
}
