package scheduler

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// The structured load report: a `load` object on a loading instance and
// `recent_loads` with the outcome of finished attempts (design 7.6a).

const mib = 1 << 20

const (
	addrA = "192.0.2.2:50052"
	addrB = "192.0.2.4:50052"
)

// twoWorkers loads "m" onto workers a and b (an even tensor split unless one
// is given) and returns the harness with the child running.
func twoWorkers(t *testing.T, fileBytes int64, split ...int) *harness {
	t.Helper()
	if split == nil {
		split = []int{1, 1}
	}
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes = []string{"a", "b"}
		p.TensorSplit = split
	})
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: fileBytes}
	h.request("m")
	h.wantState("m", "starting")
	if got := h.inst("m").peers; len(got) != 2 {
		t.Fatalf("peers = %v", got)
	}
	return h
}

func (h *harness) report(profile string) *LoadReport {
	h.t.Helper()
	for _, r := range h.c.runningList() {
		if r.Profile == profile {
			return r.Load
		}
	}
	h.t.Fatalf("%s is not running", profile)
	return nil
}

func wantStates(t *testing.T, r *LoadReport, want ...string) {
	t.Helper()
	var got []string
	for _, w := range r.Workers {
		got = append(got, w.State)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("worker states %v, want %v", got, want)
	}
}

func TestLoadReport_SessionIDIsStableAndUnique(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	r1 := h.report("m")
	h.advance(20 * time.Second)
	r2 := h.report("m")
	if r1 == nil || r1.SessionID == "" || r1.SessionID != r2.SessionID {
		t.Fatalf("session ids %v / %v: want one stable value", r1, r2)
	}
	if r1.StartedAt != h.inst("m").sessionAt.UnixMilli() || r1.StartedAt == 0 {
		t.Fatalf("started_at = %d", r1.StartedAt)
	}
	near(t, "elapsed_s", &r2.ElapsedS, 20, 0.5)
	// A second attempt of the same profile has a different id.
	h.step(evUnload{profile: "m", force: true})
	h.advance(time.Hour)
	h.request("m")
	h.wantState("m", "starting")
	if r3 := h.report("m"); r3 == nil || r3.SessionID == r1.SessionID {
		t.Fatalf("second attempt reused %q", r1.SessionID)
	}
}

// A restart restarts the instance counter, so the start time must tell two
// attempts with the same instance id apart.
func TestLoadReport_SessionIDDiffersAcrossRestarts(t *testing.T) {
	a, b := sessionID(time.UnixMilli(1000), 1), sessionID(time.UnixMilli(2000), 1)
	if a == b || a != sessionID(time.UnixMilli(1000), 1) {
		t.Fatalf("session ids %q and %q", a, b)
	}
}

func TestLoadReport_NoReportBeforeTheChildRuns(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.autoProbe = false
	h.llama("m", 8000)
	h.request("m")
	if r := h.report("m"); r != nil {
		t.Fatalf("report during pre-flight: %+v", r)
	}
	h.completeProbes()
	h.healthy("m")
	if r := h.report("m"); r != nil {
		t.Fatalf("report on a ready instance: %+v", r)
	}
}

func TestLoadReport_WorkerTotalsFollowTheTensorSplit(t *testing.T) {
	h := twoWorkers(t, 4000*mb, 1, 3)
	r := h.report("m")
	if len(r.Workers) != 2 || r.Workers[0].BytesTotal != 1000*mb || r.Workers[1].BytesTotal != 3000*mb {
		t.Fatalf("workers %+v, want 1000 MB and 3000 MB", r.Workers)
	}
	if r.Workers[0].Addr != addrA || r.Workers[1].Addr != addrB {
		t.Fatalf("workers out of --rpc order: %+v", r.Workers)
	}
	if r.BytesTotal == nil || *r.BytesTotal != 4000*mb {
		t.Fatalf("bytes_total = %v", r.BytesTotal)
	}
}

func TestLoadReport_EvenSplitGivesEqualShares(t *testing.T) {
	h := twoWorkers(t, 3000*mb)
	r := h.report("m")
	if r.Workers[0].BytesTotal != 1500*mb || r.Workers[1].BytesTotal != 1500*mb {
		t.Fatalf("workers %+v", r.Workers)
	}
}

func TestLoadReport_WorkerTotalsAreCorrectedFromTheChildLog(t *testing.T) {
	h := twoWorkers(t, 14000*mb)
	ch := h.child("m")
	// The real child labels both workers RPC0[...]: the address tells them apart.
	ch.mu.Lock()
	ch.load = instance.LoadStats{Devices: []instance.DeviceMem{
		{Name: "CPU_Mapped", Host: true, ModelMiB: 360},
		{Name: "RPC0[" + addrA + "]", ModelMiB: 6686.84},
		{Name: "RPC0[" + addrB + "]", ModelMiB: 6615.53},
	}}
	ch.mu.Unlock()
	r := h.report("m")
	want0, want1 := int64(math.Round(6686.84*mibBytes)), int64(math.Round(6615.53*mibBytes))
	if r.Workers[0].BytesTotal != want0 || r.Workers[1].BytesTotal != want1 {
		t.Fatalf("workers %+v, want %d and %d (MiB x 1048576)", r.Workers, want0, want1)
	}
	if r.BytesTotal == nil || *r.BytesTotal != 14000*mb {
		t.Fatalf("overall bytes_total changed: %v", r.BytesTotal)
	}
}

func TestLoadReport_WorkerStatesFollowTheUpload(t *testing.T) {
	h := twoWorkers(t, 2000*mb) // 1000 MB each
	ch := h.child("m")
	step := func(sa, sb int64) *LoadReport {
		ch.setSentPeers(map[string]int64{addrA: sa, addrB: sb})
		h.advance(2 * time.Second)
		return h.report("m")
	}
	r := step(200_000, 0) // handshake only
	wantStates(t, r, "waiting", "waiting")
	if r.Source == nil || *r.Source != "measured" || r.Workers[0].BytesSent == nil || *r.Workers[0].BytesSent != 200_000 {
		t.Fatalf("report %+v", r)
	}
	r = step(400*mb, 0)
	wantStates(t, r, "receiving", "waiting")
	r = step(995*mb, 0) // within 1% of its total
	wantStates(t, r, "done", "waiting")
	r = step(1000*mb, 600_000) // b has only handshake bytes
	wantStates(t, r, "done", "waiting")
	r = step(900*mb, 5*mib)               // each state follows that worker's own bytes only
	wantStates(t, r, "done", "receiving") // a stays done: its bytes never go backwards
	r = step(1000*mb, 1000*mb)
	wantStates(t, r, "done", "done")
	if r.Workers[1].BytesSent == nil || *r.Workers[1].BytesSent != 1000*mb {
		t.Fatalf("workers %+v", r.Workers)
	}
}

func TestLoadReport_WorkerBytesNeverGoBackwards(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	ch := h.child("m")
	ch.setSentPeers(map[string]int64{addrA: 800 * mb, addrB: 0})
	h.advance(2 * time.Second)
	ch.setSentPeers(map[string]int64{addrA: 300 * mb, addrB: 10 * mb}) // a's socket closed
	h.advance(2 * time.Second)
	r := h.report("m")
	if *r.Workers[0].BytesSent != 800*mb || *r.Workers[1].BytesSent != 10*mb {
		t.Fatalf("workers sent %d and %d", *r.Workers[0].BytesSent, *r.Workers[1].BytesSent)
	}
}

func TestLoadReport_EstimateOmitsPerWorkerBytesAndGuessesNoState(t *testing.T) {
	h := twoWorkers(t, 2200*mb) // no byte counting on this fake child
	h.advance(30 * time.Second)
	r := h.report("m")
	if r.Source == nil || *r.Source != "estimated" || r.Progress == nil || *r.Progress <= 0 || r.BytesSent != nil {
		t.Fatalf("report %+v", r)
	}
	for _, w := range r.Workers {
		if w.BytesSent != nil || w.State != "waiting" || w.BytesTotal != 1100*mb {
			t.Fatalf("worker %+v", w)
		}
	}
}

func TestLoadReport_NoPerWorkerFiguresMeansNoGuessing(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	h.child("m").setSent(500*mb, 1, nil) // a sum without per-worker figures
	h.advance(2 * time.Second)
	r := h.report("m")
	if r.Source == nil || *r.Source != "measured" || r.BytesSent == nil {
		t.Fatalf("report %+v", r)
	}
	for _, w := range r.Workers {
		if w.BytesSent != nil || w.State != "waiting" {
			t.Fatalf("worker %+v", w)
		}
	}
}

func TestLoadReport_LocalOnlyLoadHasNoUploadFields(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000))
	h.llama("m", 4000)
	h.request("m")
	h.advance(12 * time.Second)
	r := h.report("m")
	if r == nil || r.SessionID == "" || r.BytesTotal != nil || r.BytesSent != nil || r.Progress != nil || r.EtaS != nil || r.Source != nil || len(r.Workers) != 0 {
		t.Fatalf("report %+v", r)
	}
	if r.Workers == nil {
		t.Fatal("workers is nil: it must be an empty list")
	}
	near(t, "elapsed_s", &r.ElapsedS, 12, 0.5)
}

func TestLoadReport_StatusCarriesTheSameReport(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	h.advance(5 * time.Second)
	st := h.loadStatus("m")
	if st.Load == nil || st.Load.SessionID != h.report("m").SessionID || len(st.Load.Workers) != 2 {
		t.Fatalf("status load %+v", st.Load)
	}
	h.healthy("m")
	if h.loadStatus("m").Load != nil || h.report("m") != nil {
		t.Fatal("a ready instance still has a load report")
	}
}

// ---- recent_loads ----

func (h *harness) recent() []LoadOutcome { return h.c.recentLoads() }

func TestRecentLoads_ReadyAttempt(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	id := h.report("m").SessionID
	h.advance(215 * time.Second)
	h.healthy("m")
	got := h.recent()
	if len(got) != 1 {
		t.Fatalf("recent %+v", got)
	}
	o := got[0]
	if o.SessionID != id || o.Model != "m" || o.Outcome != "ready" || o.Reason != "" ||
		strings.Join(o.Workers, ",") != addrA+","+addrB || o.FinishedAt != h.clk.Now().UnixMilli() {
		t.Fatalf("outcome %+v", o)
	}
	near(t, "load_s", &o.LoadS, 215, 0.5)
	if st := h.c.statusDoc(); len(st.RecentLoads) != 1 || st.RecentLoads[0].SessionID != id {
		t.Fatalf("status recent_loads %+v", st.RecentLoads)
	}
}

func TestRecentLoads_FailedWhenTheChildExitsDuringLoad(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	h.advance(10 * time.Second)
	h.exit("m", 1)
	got := h.recent()
	if len(got) != 1 || got[0].Outcome != "failed" || !strings.Contains(got[0].Reason, "exited") {
		t.Fatalf("recent %+v", got)
	}
	near(t, "load_s", &got[0].LoadS, 10, 0.5)
}

func TestRecentLoads_FailedOnLoadTimeout(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	h.advance(h.inst("m").loadTimeout + time.Second)
	got := h.recent()
	if len(got) != 1 || got[0].Outcome != "failed" || !strings.Contains(got[0].Reason, "timeout") {
		t.Fatalf("recent %+v", got)
	}
}

func TestRecentLoads_CancelledByUnload(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	h.step(evUnload{profile: "m", force: true})
	h.advance(time.Second)
	got := h.recent()
	if len(got) != 1 || got[0].Outcome != "cancelled" || got[0].Reason == "" {
		t.Fatalf("recent %+v", got)
	}
}

func TestRecentLoads_CancelledByShutdown(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	h.step(evShutdown{})
	h.advance(time.Second)
	got := h.recent()
	if len(got) != 1 || got[0].Outcome != "cancelled" || !strings.Contains(got[0].Reason, "shutting down") {
		t.Fatalf("recent %+v", got)
	}
}

func TestRecentLoads_RefusedLoadsDoNotAppearAndTheListIsNeverNil(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.needErr["m"] = errors.New("no such file")
	h.request("m")
	if got := h.recent(); len(got) != 0 {
		t.Fatalf("a refused load shows up: %+v", got)
	}
	if st := h.c.statusDoc(); st.RecentLoads == nil {
		t.Fatal("recent_loads must be an empty list, not nil")
	}
}

func TestRecentLoads_NewestFirstAndExpireAfterRetention(t *testing.T) {
	if RecentLoadRetention != 120*time.Second {
		t.Fatalf("retention = %v", RecentLoadRetention)
	}
	h := newHarness(t).nodes(hostNode(7000))
	h.llama("m", 4000)
	h.request("m")
	h.healthy("m")
	first := h.recent()[0].SessionID
	h.step(evUnload{profile: "m", force: true})
	h.advance(time.Minute) // node settled
	h.request("m")
	h.exit("m", 2)
	got := h.recent()
	if len(got) != 2 || got[0].Outcome != "failed" || got[1].SessionID != first || got[1].Outcome != "ready" {
		t.Fatalf("recent %+v", got)
	}
	h.advance(RecentLoadRetention - 30*time.Second) // the ready one is now over 120 s old, the failed one is not
	if got := h.recent(); len(got) != 1 || got[0].Outcome != "failed" {
		t.Fatalf("after the older one's retention: %+v", got)
	}
	h.advance(RecentLoadRetention)
	if got := h.recent(); len(got) != 0 {
		t.Fatalf("after retention: %+v", got)
	}
}

// ---- feed ----

func TestLoadFeed_StartedAndFinishedEvents(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	id := h.report("m").SessionID
	var started *LoadStart
	for _, ev := range h.feedLog {
		if ev.Kind == FeedLoadStarted {
			started = ev.LoadStarted
		}
	}
	if started == nil || started.SessionID != id || started.Model != "m" || started.StartedAt == 0 ||
		strings.Join(started.Workers, ",") != addrA+","+addrB {
		t.Fatalf("started event %+v", started)
	}
	h.healthy("m")
	var fin []*LoadOutcome
	for _, ev := range h.feedLog {
		if ev.Kind == FeedLoadFinished {
			fin = append(fin, ev.Outcome)
		}
	}
	if len(fin) != 1 || !reflect.DeepEqual(*fin[0], h.recent()[0]) {
		t.Fatalf("finished events %+v, recent %+v", fin, h.recent())
	}
}

// Real reading from hardware: the upload order was worker 1, 3, 2, so a worker
// is never judged by what a later one in the --rpc order has done.
func TestLoadReport_WorkerStateIgnoresOtherWorkers(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 6, 15000))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes = []string{"a", "b", "c"}
		p.TensorSplit = []int{1, 1, 1}
	})
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: 18375686226}
	h.request("m")
	h.wantState("m", "starting")
	peers := h.inst("m").peers
	if len(peers) != 3 {
		t.Fatalf("peers = %v", peers)
	}
	h.inst("m").workerEst = []int64{6443740692, 5935789507, 5996156027}
	h.child("m").setSentPeers(map[string]int64{peers[0]: 6443824024, peers[1]: 5142, peers[2]: 4268583552})
	h.advance(2 * time.Second)
	wantStates(t, h.report("m"), "done", "waiting", "receiving")
}
