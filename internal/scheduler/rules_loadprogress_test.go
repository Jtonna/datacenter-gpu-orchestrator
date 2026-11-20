package scheduler

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// Load progress: what /api/status and /running show for an instance that is
// still loading. A PS5 tier link of 11 MB/s (ps5() in the harness) and a
// 1100 MB model on one worker make an estimated 100 s load.

const mb = 1_000_000

func loadingHarness(t *testing.T, fileBytes int64) *harness {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: fileBytes}
	h.request("m")
	h.wantState("m", "starting")
	return h
}

func (h *harness) loadStatus(profile string) InstanceStatus {
	h.t.Helper()
	for _, in := range h.c.statusDoc().Instances {
		if in.Profile == profile {
			return in
		}
	}
	h.t.Fatalf("no status for %s", profile)
	return InstanceStatus{}
}

func near(t *testing.T, what string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is absent, want %v", what, want)
	}
	if math.Abs(*got-want) > tol {
		t.Fatalf("%s = %v, want %v +- %v", what, *got, want, tol)
	}
}

func TestLoadProgress_EstimatedFromSlowestLink(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	h.advance(50 * time.Second)
	s := h.loadStatus("m")
	near(t, "load_elapsed_s", s.LoadElapsedS, 50, 0.5)
	if s.LoadBytesTotal == nil || *s.LoadBytesTotal != 1100*mb {
		t.Fatalf("load_bytes_total = %v", s.LoadBytesTotal)
	}
	if s.LoadBytesSent != nil {
		t.Fatalf("load_bytes_sent = %d for an estimate", *s.LoadBytesSent)
	}
	near(t, "load_progress", s.LoadProgress, 0.5, 0.01)
	near(t, "load_eta_s", s.LoadEtaS, 50, 1)
	if s.LoadProgressSource == nil || *s.LoadProgressSource != "estimated" {
		t.Fatalf("source = %v", s.LoadProgressSource)
	}
}

func TestLoadProgress_EstimateCappedBelowOne(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	h.advance(310 * time.Second) // three times the expected load time, inside the load timeout
	s := h.loadStatus("m")
	near(t, "load_progress", s.LoadProgress, 0.99, 1e-9)
	near(t, "load_eta_s", s.LoadEtaS, 0, 0.5)
}

func TestLoadProgress_RateIsSharedBetweenLoadsInProgress(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	for _, n := range []string{"m", "n"} {
		h.llama(n, 8000)
		h.needs[n] = NeedInfo{Need: placer.Measured(8000), FileBytes: 1100 * mb}
		h.request(n)
	}
	h.advance(50 * time.Second)
	for _, n := range []string{"m", "n"} {
		near(t, n+" load_progress", h.loadStatus(n).LoadProgress, 0.25, 0.01) // 5.5 MB/s each
	}
}

func TestLoadProgress_BeforeTheChildRunsNothingIsDone(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.autoProbe = false
	h.llama("m", 8000)
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: 1100 * mb}
	h.request("m")
	h.advance(5 * time.Second)
	s := h.loadStatus("m")
	near(t, "load_elapsed_s", s.LoadElapsedS, 5, 0.5)
	near(t, "load_progress", s.LoadProgress, 0, 1e-9)
}

func TestLoadProgress_MeasuredWhenTheChildCanBeCounted(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	ch := h.child("m")
	ch.setSent(0, 0, nil) // not connected yet
	h.advance(10 * time.Second)
	ch.setSent(300*mb, 1, nil)
	h.advance(2 * time.Second)
	s := h.loadStatus("m")
	if s.LoadProgressSource == nil || *s.LoadProgressSource != "measured" {
		t.Fatalf("source = %v", s.LoadProgressSource)
	}
	if s.LoadBytesSent == nil || *s.LoadBytesSent != 300*mb {
		t.Fatalf("load_bytes_sent = %v", s.LoadBytesSent)
	}
	near(t, "load_progress", s.LoadProgress, 300.0/1100.0, 1e-3)
	if s.LoadEtaS == nil || *s.LoadEtaS <= 0 {
		t.Fatalf("load_eta_s = %v", s.LoadEtaS)
	}
}

func TestLoadProgress_MeasuredEtaFollowsTheMeasuredRate(t *testing.T) {
	h := loadingHarness(t, 1000*mb)
	ch := h.child("m")
	ch.setSent(0, 0, nil)
	// 50 MB/s measured: 100 MB more every 2 s.
	for i := 1; i <= 8; i++ {
		ch.setSent(int64(i)*100*mb, 1, nil)
		h.advance(2 * time.Second)
	}
	s := h.loadStatus("m") // 800 MB of 1000 MB sent, 200 MB left at 50 MB/s
	near(t, "load_progress", s.LoadProgress, 0.8, 0.02)
	near(t, "load_eta_s", s.LoadEtaS, 4, 1.5)
}

func TestLoadProgress_MeasuredNeverReachesOneBeforeReady(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	h.child("m").setSent(1100*mb, 1, nil) // everything acked, model not ready yet
	h.advance(3 * time.Second)
	s := h.loadStatus("m")
	near(t, "load_progress", s.LoadProgress, 0.99, 1e-9)
	if s.LoadBytesSent == nil || *s.LoadBytesSent != 1100*mb {
		t.Fatalf("load_bytes_sent = %v", s.LoadBytesSent)
	}
}

func TestLoadProgress_MeasuredNeverGoesBackwards(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	ch := h.child("m")
	ch.setSent(500*mb, 1, nil)
	h.advance(2 * time.Second)
	ch.setSent(200*mb, 1, nil) // a socket closed: the sum dropped
	h.advance(2 * time.Second)
	if s := h.loadStatus("m"); s.LoadBytesSent == nil || *s.LoadBytesSent != 500*mb {
		t.Fatalf("load_bytes_sent = %v, want 500 MB", s.LoadBytesSent)
	}
}

func TestLoadProgress_ReadErrorFallsBackToTheEstimate(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	ch := h.child("m")
	ch.setSent(300*mb, 1, nil)
	h.advance(2 * time.Second)
	ch.setSent(0, 0, errors.New("netlink: busy"))
	h.advance(10 * time.Second)
	s := h.loadStatus("m")
	if s.LoadProgressSource == nil || *s.LoadProgressSource != "estimated" || s.LoadBytesSent != nil {
		t.Fatalf("source %v sent %v after a read error", s.LoadProgressSource, s.LoadBytesSent)
	}
	near(t, "load_progress", s.LoadProgress, 12.0/100.0, 0.02)
	// The scheduler keeps trying: a good read is measured again.
	ch.setSent(400*mb, 1, nil)
	h.advance(2 * time.Second)
	if s := h.loadStatus("m"); s.LoadProgressSource == nil || *s.LoadProgressSource != "measured" {
		t.Fatalf("source %v after recovery", s.LoadProgressSource)
	}
}

func TestLoadProgress_NoSocketsYetIsStillAnEstimate(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	h.child("m").setSent(0, 0, nil) // the child has not connected yet
	h.advance(5 * time.Second)
	s := h.loadStatus("m")
	if s.LoadProgressSource == nil || *s.LoadProgressSource != "estimated" || s.LoadBytesSent != nil {
		t.Fatalf("source %v sent %v", s.LoadProgressSource, s.LoadBytesSent)
	}
}

func TestLoadProgress_UnsupportedPlatformIsAskedOnlyOnce(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	h.advance(30 * time.Second)
	if n := h.child("m").sentCalls(); n != 1 {
		t.Fatalf("an unsupported counter was asked %d times", n)
	}
}

func TestLoadProgress_SampledAtMostOncePerSecond(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	ch := h.child("m")
	ch.setSent(1*mb, 1, nil)
	h.advance(20 * time.Second)
	if n := ch.sentCalls(); n < 15 || n > 21 {
		t.Fatalf("%d samples in 20 s, want about one per second", n)
	}
	// An event storm does not make more samples.
	before := ch.sentCalls()
	for i := 0; i < 50; i++ {
		h.step(evTick{})
	}
	if n := ch.sentCalls(); n != before {
		t.Fatalf("%d extra samples from ticks within the same second", n-before)
	}
}

func TestLoadProgress_SamplingNeverRunsInsideTheStep(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	ch := h.child("m")
	ch.setSent(1*mb, 1, nil)
	h.autoSample = false // the runner would do this off the event loop
	before := ch.sentCalls()
	h.advance(10 * time.Second)
	if n := ch.sentCalls(); n != before {
		t.Fatalf("the core called SentBytes itself %d times", n-before)
	}
	// With no answer ever arriving the status still has an estimate.
	near(t, "load_progress", h.loadStatus("m").LoadProgress, 0.10, 0.02)
}

func TestLoadProgress_SampleAfterTheLoadIsIgnored(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	in := h.inst("m")
	h.healthy("m")
	h.step(evLoadSample{iid: in.iid, bytes: 5, sockets: 1}) // late result
	s := h.loadStatus("m")
	if s.LoadElapsedS != nil || s.LoadProgress != nil || s.LoadBytesTotal != nil {
		t.Fatalf("a ready instance shows load fields: %+v", s)
	}
}

func TestLoadProgress_LocalOnlyPlacementHasNoUpload(t *testing.T) {
	h := newHarness(t).nodes(omen(7000))
	h.llama("m", 4000)
	h.request("m")
	h.wantState("m", "starting")
	h.advance(12 * time.Second)
	s := h.loadStatus("m")
	near(t, "load_elapsed_s", s.LoadElapsedS, 12, 0.5)
	if s.LoadBytesTotal != nil || s.LoadBytesSent != nil || s.LoadProgress != nil || s.LoadEtaS != nil || s.LoadProgressSource != nil {
		t.Fatalf("local-only load shows upload fields: %+v", s)
	}
	if got := h.c.runningList(); len(got) != 1 || got[0].LoadProgress != nil {
		t.Fatalf("running %+v", got)
	}
}

func TestLoadProgress_BytesTotalIsTheRemoteShareOfTheFile(t *testing.T) {
	h := newHarness(t).nodes(omen(7000), ps5("a", 2, 15000))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes = []string{"host-1", "a"}
		p.TensorSplit = []int{1, 3}
	})
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: 4000 * mb}
	h.request("m")
	h.wantState("m", "starting")
	s := h.loadStatus("m")
	if s.LoadBytesTotal == nil || *s.LoadBytesTotal != 3000*mb {
		t.Fatalf("load_bytes_total = %v, want 3000 MB (3/4 of the file goes to the worker)", s.LoadBytesTotal)
	}
}

func TestLoadProgress_NoKnownLinkSpeedMeansNoEstimate(t *testing.T) {
	n := ps5("a", 2, 15000)
	n.LinkMBs = 0
	h := newHarness(t).nodes(n)
	h.llama("m", 8000)
	h.needs["m"] = NeedInfo{Need: placer.Measured(8000), FileBytes: 1100 * mb}
	h.request("m")
	h.advance(10 * time.Second)
	s := h.loadStatus("m")
	if s.LoadBytesTotal == nil || s.LoadElapsedS == nil {
		t.Fatalf("status %+v", s)
	}
	if s.LoadProgress != nil || s.LoadEtaS != nil || s.LoadProgressSource != nil {
		t.Fatalf("an estimate without a link speed: %+v", s)
	}
}

func TestLoadProgress_RunningListCarriesLoadProgress(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	h.advance(50 * time.Second)
	got := h.c.runningList()
	if len(got) != 1 || got[0].Profile != "m" || got[0].State != RunningStarting {
		t.Fatalf("running %+v", got)
	}
	near(t, "running load_progress", got[0].LoadProgress, 0.5, 0.01)
	h.healthy("m")
	if got := h.c.runningList(); got[0].LoadProgress != nil {
		t.Fatalf("a ready instance has load_progress %v", *got[0].LoadProgress)
	}
}

func TestLoadProgress_LoggedEvery30Seconds(t *testing.T) {
	h := loadingHarness(t, 1100*mb)
	h.advance(29 * time.Second)
	if n := len(h.logs.find("load progress")); n != 0 {
		t.Fatalf("%d lines before 30 s", n)
	}
	h.advance(2 * time.Second)
	r := h.mustLog("load progress", "profile", "m")
	h.mustHaveAttrs(r, "elapsed", "percent", "eta", "source", "bytes_total")
	if r.Level.String() != "INFO" {
		t.Fatalf("level %v", r.Level)
	}
	h.advance(60 * time.Second)
	if n := len(h.logs.find("load progress", "profile", "m")); n != 3 {
		t.Fatalf("%d lines after 91 s, want 3", n)
	}
	h.healthy("m")
	h.advance(120 * time.Second)
	if n := len(h.logs.find("load progress", "profile", "m")); n != 3 {
		t.Fatalf("logging went on after ready: %d lines", n)
	}
}

func TestLoadProgress_LoggedForLocalLoadsToo(t *testing.T) {
	h := newHarness(t).nodes(omen(7000))
	h.llama("m", 4000)
	h.request("m")
	h.advance(31 * time.Second)
	r := h.mustLog("load progress", "profile", "m")
	h.mustHaveAttrs(r, "elapsed")
	if _, ok := r.Attrs["percent"]; ok {
		t.Fatalf("local-only line has a percent: %v", r.Attrs)
	}
}
