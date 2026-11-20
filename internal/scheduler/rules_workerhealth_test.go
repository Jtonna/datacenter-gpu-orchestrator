package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Worker health detection (design 8.4a): three channels per worker in use,
// a health state, and the effects of hung (log and quarantine only).

// idlePair has "m" ready and idle on workers a and b, never unloaded by its
// ttl, with workers answering.
func idlePair(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	h.release(h.ready("m"))
	h.wantState("m", "ready")
	return h
}

func (h *harness) nodeHealth(id core.NodeID) *WorkerHealthStatus {
	h.t.Helper()
	for _, n := range h.c.statusDoc().View.Nodes {
		if n.ID == id {
			return n.Health
		}
	}
	h.t.Fatalf("no node %s in the status", id)
	return nil
}

func (h *harness) wantHealth(id core.NodeID, state string) *WorkerHealthStatus {
	h.t.Helper()
	got := h.nodeHealth(id)
	if got == nil || got.State != state {
		h.t.Fatalf("node %s health = %+v, want state %s", id, got, state)
	}
	return got
}

func (h *harness) healthEvents() []*WorkerHealthEvent {
	var out []*WorkerHealthEvent
	for _, ev := range h.feedLog {
		if ev.Kind == FeedWorkerHealth {
			out = append(out, ev.Worker)
		}
	}
	return out
}

func TestWorkerHealth_ProbeNotRunWhileTheWorkerReceivesAnUpload(t *testing.T) {
	h, _ := stallHarness(t)
	sentAt(h, 5*mib, 0) // a has at least 1 MiB and is not done: receiving; b is waiting
	h.advance(25 * time.Second)
	if got := h.wprober.callsTo(addrA); got != 0 {
		t.Fatalf("worker a was probed %d times while receiving", got)
	}
	if got := h.wprober.callsTo(addrB); got < 2 {
		t.Fatalf("worker b (waiting) was probed %d times, want it probed every interval", got)
	}
	if ha := h.nodeHealth("a"); ha.Channels.Probe != ChanUnknown {
		t.Fatalf("a probe channel = %s, want unknown (not run)", ha.Channels.Probe)
	}
}

func TestWorkerHealth_ProbeNotRunWhileTheUploadIsNotMeasured(t *testing.T) {
	h, _ := stallHarness(t) // the fake child has no byte counter reading
	h.advance(60 * time.Second)
	if a, b := h.wprober.callsTo(addrA), h.wprober.callsTo(addrB); a+b != 0 {
		t.Fatalf("probed %d+%d times while the upload could not be ruled out", a, b)
	}
}

func TestWorkerHealth_UploadChannelSilentIsSuspectAndRecovers(t *testing.T) {
	h, _ := stallHarness(t)
	sentAt(h, 5*mib, 0)
	h.advance(29 * time.Second)
	h.wantHealth("a", HealthOK)
	h.advance(3 * time.Second)
	got := h.wantHealth("a", HealthSuspect)
	if got.Channels.Upload != ChanSilent || got.Channels.Heartbeat != ChanOK || got.Channels.Probe != ChanUnknown {
		t.Fatalf("channels %+v", got.Channels)
	}
	if _, ok := got.SilentForS["upload"]; !ok || len(got.SilentForS) != 1 {
		t.Fatalf("silent_for_s %v, want only upload", got.SilentForS)
	}
	h.wantHealth("b", HealthOK) // b receives nothing yet: upload is unknown for it
	sentAt(h, 6*mib, 0)         // bytes move again
	h.advance(2 * time.Second)
	got = h.wantHealth("a", HealthOK)
	if got.Channels.Upload != ChanOK || len(got.SilentForS) != 0 {
		t.Fatalf("after growth %+v", got)
	}
}

func TestWorkerHealth_NoProbeWithARequestInFlightByDefault(t *testing.T) {
	h := idlePair(t)
	n := h.wprober.callsTo(addrA)
	g := h.request("m")
	h.granted(g) // now a request is in flight
	h.advance(60 * time.Second)
	if got := h.wprober.callsTo(addrA); got != n {
		t.Fatalf("probed %d more times with a request in flight", got-n)
	}
	if ha := h.nodeHealth("a"); ha.Channels.Probe != ChanUnknown {
		t.Fatalf("probe channel %s, want unknown while a request runs", ha.Channels.Probe)
	}
}

func TestWorkerHealth_ProbeRunsWithARequestInFlightWhenSwitchedOn(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Defaults.WorkerProbeDuringInference = true
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	g := h.ready("m") // not released: a request is in flight
	if h.inst("m").inFlight != 1 {
		t.Fatalf("in flight = %d", h.inst("m").inFlight)
	}
	n := h.wprober.callsTo(addrA)
	h.advance(35 * time.Second)
	if got := h.wprober.callsTo(addrA); got-n < 3 {
		t.Fatalf("probed %d times in 35 s with the switch on, want every 10 s", got-n)
	}
	h.release(g)
}

func TestWorkerHealth_ProbeResumesWhenTheRequestEnds(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	g := h.ready("m")
	h.advance(60 * time.Second)
	if got := h.wprober.callsTo(addrA); got != 0 {
		t.Fatalf("probed %d times with a request in flight", got)
	}
	h.release(g)
	h.advance(time.Second)
	if got := h.wprober.callsTo(addrA); got == 0 {
		t.Fatal("no probe after the request ended")
	}
	h.wantHealth("a", HealthOK)
}

func TestWorkerHealth_NeverProbesAWorkerNoInstanceUses(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 6, 15000), omen(8000))
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	h.release(h.ready("m"))
	h.advance(60 * time.Second)
	if got := h.wprober.callsTo("10.0.0.6:50052"); got != 0 {
		t.Fatalf("unused worker c probed %d times", got)
	}
	if h.nodeHealth("c") != nil || h.nodeHealth("omen") != nil {
		t.Fatal("an unused or local node has a health object")
	}
	if h.wprober.callsTo(addrA) == 0 {
		t.Fatal("worker a not probed")
	}
	// Once the instance is gone, nothing is probed any more.
	h.step(evUnload{profile: "m", force: true})
	n := h.wprober.callsTo(addrA)
	h.advance(60 * time.Second)
	if got := h.wprober.callsTo(addrA); got != n {
		t.Fatalf("probed a %d more times after the unload", got-n)
	}
	if h.nodeHealth("a") != nil {
		t.Fatal("health still reported with no instance")
	}
}

func TestWorkerHealth_OneSilentChannelIsSuspect(t *testing.T) {
	h := idlePair(t)
	h.wprober.setDown(addrA, true)
	h.advance(35 * time.Second) // three failed probes, at 10, 20 and 30 s
	got := h.wantHealth("a", HealthSuspect)
	if got.Channels.Probe != ChanSilent || got.Channels.Heartbeat != ChanOK || got.Channels.Upload != ChanUnknown {
		t.Fatalf("channels %+v", got.Channels)
	}
	h.wantHealth("b", HealthOK)
	if h.logs.find("worker hung") != nil {
		t.Fatal("hung logged for one silent channel")
	}
	if h.c.quarantined("a") {
		t.Fatal("a suspect worker was quarantined")
	}
}

func TestWorkerHealth_TwoFailedProbesAreNotSilent(t *testing.T) {
	h := idlePair(t)
	h.wprober.setDown(addrA, true)
	h.advance(15 * time.Second) // probes at 10 and 20 are not both in yet; at most two failures
	if got := h.nodeHealth("a"); got.Channels.Probe == ChanSilent {
		t.Fatalf("probe silent after fewer than 3 failures: %+v", got)
	}
	h.wprober.setDown(addrA, false)
	h.advance(60 * time.Second)
	h.wantHealth("a", HealthOK)
}

// The incident: the machine stops reporting to the fleet manager and its port
// stops answering, while its container is still "running" and the model is
// loaded and idle.
func TestWorkerHealth_HeartbeatAndProbeSilentIsHung(t *testing.T) {
	h := idlePair(t)
	h.wprober.setDown(addrA, true) // probes fail at 10, 20, 30 s: silent from 30 s
	h.advance(10 * time.Second)
	h.view.modify("a", func(n *core.Node) { n.Online = false }) // heartbeat silent from 40 s
	h.viewChanged()
	h.advance(29 * time.Second)
	h.wantHealth("a", HealthSuspect)
	if h.logs.find("worker hung") != nil {
		t.Fatal("hung before the second channel was silent")
	}
	h.advance(2 * time.Second)

	recs := h.logs.find("worker hung")
	if len(recs) != 1 || recs[0].Level.String() != "ERROR" {
		t.Fatalf("worker hung records: %+v", recs)
	}
	r := recs[0].Attrs
	if r["node"] != core.NodeID("a") || r["addr"] != addrA || r["instances"] != "m" ||
		!strings.Contains(r["silent"].(string), "heartbeat=") || !strings.Contains(r["silent"].(string), "probe=") {
		t.Fatalf("attrs %v", r)
	}
	if !h.c.quarantined("a") || h.c.quarantined("b") {
		t.Fatal("node a must be quarantined and b not")
	}
	if len(h.logs.find("node quarantined", "node", "a")) != 1 {
		t.Fatalf("quarantine log %+v", h.logs.find("node quarantined"))
	}
	evs := h.healthEvents()
	found := false
	for _, e := range evs {
		if e.Node == "a" && e.Health != nil && e.Health.State == HealthHung {
			found = true
		}
	}
	if !found {
		t.Fatalf("no hung workerHealth event in %v", evs)
	}
	h.advance(5 * time.Minute)
	if n := len(h.logs.find("worker hung")); n != 1 {
		t.Fatalf("worker hung logged %d times", n)
	}
}

func TestWorkerHealth_StaleFleetViewDoesNotMakeTheHeartbeatSilent(t *testing.T) {
	h := idlePair(t)
	h.view.stale()
	h.advance(61 * time.Second) // the view is stale now
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(5 * time.Minute)
	got := h.nodeHealth("a")
	if got.Channels.Heartbeat != ChanUnknown || got.State != HealthOK {
		t.Fatalf("a = %+v, want heartbeat unknown and state ok", got)
	}
	if _, silent := got.SilentForS["heartbeat"]; silent {
		t.Fatal("heartbeat reported silent on a stale view")
	}
}

func TestWorkerHealth_ProbeOnlyIsHungAfterThreeTimesTheSilence(t *testing.T) {
	h := idlePair(t)
	h.view.stale() // the heartbeat is unknown from 60 s on
	h.wprober.setDown(addrA, true)
	h.advance(100 * time.Second) // probe silent since about 30 s: 70 s of silence
	h.wantHealth("a", HealthSuspect)
	if h.logs.find("worker hung") != nil {
		t.Fatal("hung too early")
	}
	h.advance(25 * time.Second) // 95 s of silence, above 3 x 30 s
	got := h.wantHealth("a", HealthHung)
	if got.Channels.Heartbeat != ChanUnknown || got.Channels.Probe != ChanSilent {
		t.Fatalf("channels %+v", got.Channels)
	}
	if len(h.logs.find("worker hung")) != 1 || !h.c.quarantined("a") {
		t.Fatal("not logged once and quarantined")
	}
	h.wantState("m", "ready") // hung does not stop anything
}

func TestWorkerHealth_ProbeAloneIsOnlySuspectWhileTheViewIsLive(t *testing.T) {
	h := idlePair(t)
	h.wprober.setDown(addrA, true)
	h.advance(10 * time.Minute)
	h.wantHealth("a", HealthSuspect)
	h.wantState("m", "ready")
}

func TestWorkerHealth_RecoveryNeedsEveryKnownChannelHealthy(t *testing.T) {
	h := idlePair(t)
	h.view.stale()
	h.wprober.setDown(addrA, true)
	h.advance(130 * time.Second)
	h.wantHealth("a", HealthHung)

	h.view.fresh() // the heartbeat is known and ok again, the probe is still silent
	h.advance(time.Second)
	h.wantHealth("a", HealthHung)
	if len(h.logs.find("worker recovered")) != 0 {
		t.Fatal("recovered while the probe is silent")
	}

	h.wprober.setDown(addrA, false)
	h.advance(10 * time.Second)
	got := h.wantHealth("a", HealthOK)
	if got.Channels.Probe != ChanOK || got.Channels.Heartbeat != ChanOK || len(got.SilentForS) != 0 {
		t.Fatalf("after recovery %+v", got)
	}
	recs := h.logs.find("worker recovered", "node", "a")
	if len(recs) != 1 || recs[0].Level.String() != "INFO" || recs[0].Attrs["hung_for"] == nil {
		t.Fatalf("recovered records %+v", recs)
	}
	last := h.healthEvents()
	if e := last[len(last)-1]; e.Node != "a" || e.Health == nil || e.Health.State != HealthOK {
		t.Fatalf("last event %+v", e)
	}
}

// The default: the connect probe never runs and its channel is always unknown,
// while the rest of the feature stays on.
func TestWorkerHealth_ProbeOffByDefaultOtherChannelsStayOn(t *testing.T) {
	if got := config.DefaultDefaults().WorkerProbeInterval; got != 0 {
		t.Fatalf("default worker_probe_interval = %v, want 0", got)
	}
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	h.release(h.ready("m"))
	h.wprober.setDown(addrA, true)
	h.advance(10 * time.Minute)
	if a, b := h.wprober.callsTo(addrA), h.wprober.callsTo(addrB); a+b != 0 {
		t.Fatalf("probed %d+%d times with the probe off", a, b)
	}
	got := h.wantHealth("a", HealthOK)
	if got.Channels.Probe != ChanUnknown || got.Channels.Heartbeat != ChanOK {
		t.Fatalf("channels %+v, want probe unknown and heartbeat ok", got.Channels)
	}
}

func TestWorkerHealth_ProbeOffUploadChannelStillWorks(t *testing.T) {
	h, _ := stallHarness(t)
	h.cfg.Defaults.WorkerProbeInterval = 0
	sentAt(h, 5*mib, 0)
	h.advance(32 * time.Second)
	got := h.wantHealth("a", HealthSuspect)
	if got.Channels.Upload != ChanSilent || got.Channels.Probe != ChanUnknown {
		t.Fatalf("channels %+v", got.Channels)
	}
	if a, b := h.wprober.callsTo(addrA), h.wprober.callsTo(addrB); a+b != 0 {
		t.Fatalf("probed %d+%d times with the probe off", a, b)
	}
}

// With the probe off, an idle loaded model on a worker whose node stops
// reporting: suspect from the heartbeat alone, hung after 3 x worker_silence.
// Node loss (30 s) kills the instance and drops the health object, so the rule
// is only reachable when 4 x worker_silence is below it: silence 5 s here.
func TestWorkerHealth_HeartbeatAloneIsSuspectThenHungWithProbeOff(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.cfg.Defaults.WorkerProbeInterval = 0
	h.cfg.Defaults.WorkerSilence = 5 * time.Second
	h.llama("m", 8000, func(p *core.Profile) {
		p.ForceNodes, p.TensorSplit, p.TTL = []string{"a", "b"}, []int{1, 1}, ptrDur(0)
	})
	h.release(h.ready("m"))
	h.view.modify("a", func(n *core.Node) { n.Online = false }) // heartbeat silent from 5 s
	h.viewChanged()
	h.advance(4 * time.Second)
	h.wantHealth("a", HealthOK)
	h.advance(2 * time.Second)
	got := h.wantHealth("a", HealthSuspect)
	if got.Channels.Heartbeat != ChanSilent || got.Channels.Probe != ChanUnknown {
		t.Fatalf("channels %+v", got.Channels)
	}
	h.advance(12 * time.Second) // 11 s of heartbeat silence, below 3 x 5 s
	h.wantHealth("a", HealthSuspect)
	if h.logs.find("worker hung") != nil || h.c.quarantined("a") {
		t.Fatal("hung before 3 x worker_silence")
	}
	h.advance(3 * time.Second) // ...and above it now
	got = h.wantHealth("a", HealthHung)
	if got.Channels.Heartbeat != ChanSilent || got.Channels.Probe != ChanUnknown {
		t.Fatalf("channels %+v", got.Channels)
	}
	if len(h.logs.find("worker hung")) != 1 || !h.c.quarantined("a") || h.c.quarantined("b") {
		t.Fatal("not logged once and quarantined (a only)")
	}
}

// The heartbeat-alone rule needs no channel known ok: with the probe enabled
// and answering, a silent heartbeat stays suspect.
func TestWorkerHealth_HeartbeatAloneStaysSuspectWhileTheProbeIsOk(t *testing.T) {
	h := idlePair(t)
	h.cfg.Defaults.WorkerSilence = 5 * time.Second
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(25 * time.Second) // 20 s silent, above 3 x 5 s, still before node loss
	got := h.wantHealth("a", HealthSuspect)
	if got.Channels.Probe != ChanOK {
		t.Fatalf("channels %+v", got.Channels)
	}
}

func TestWorkerHealth_ProbeResultIsAnEventNotAnInlineCall(t *testing.T) {
	// The core never connects itself: it emits an effect and takes the result
	// as an event. With the probes held, nothing is decided.
	h := idlePair(t)
	h.holdWorkerProbes = true
	h.wprober.setDown(addrA, true)
	h.advance(60 * time.Second)
	if len(h.wprobes) == 0 {
		t.Fatal("no effWorkerProbe emitted")
	}
	if len(h.wprobes) > 2 {
		t.Fatalf("%d probes emitted while the first of a worker is still pending", len(h.wprobes))
	}
	h.wantHealth("a", HealthOK)
}
