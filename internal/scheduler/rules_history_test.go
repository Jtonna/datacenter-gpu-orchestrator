package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
)

// The event history (design 9.1): the core appends a history.Event at every
// point where it logs a safety or lifecycle fact.

// of returns the recorded events of a type.
func (h *harness) of(typ string) []history.Event {
	var out []history.Event
	for _, e := range h.histLog {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// one returns the single event of a type that matches every key=value of data.
func (h *harness) one(typ string, data ...any) history.Event {
	h.t.Helper()
	var got []history.Event
	for _, e := range h.of(typ) {
		ok := true
		for i := 0; i+1 < len(data); i += 2 {
			if fmt.Sprint(e.Data[data[i].(string)]) != fmt.Sprint(data[i+1]) {
				ok = false
			}
		}
		if ok {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		var types []string
		for _, e := range h.histLog {
			types = append(types, e.Type)
		}
		h.t.Fatalf("%d %s events matching %v, want 1; have %v", len(got), typ, data, types)
	}
	return got[0]
}

func (h *harness) none(typ string) {
	h.t.Helper()
	if n := len(h.of(typ)); n != 0 {
		h.t.Fatalf("%d %s events, want none: %+v", n, typ, h.of(typ))
	}
}

// examples collects one record per type for the report (-v shows them).
var examples = map[string]history.Event{}

func (h *harness) show(e history.Event) history.Event {
	h.t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		h.t.Fatalf("event does not marshal: %v", err)
	}
	if e.Ts == 0 || e.Type == "" || e.Summary == "" || e.Nodes == nil || e.Data == nil || e.Severity == "" {
		h.t.Fatalf("incomplete event %s", b)
	}
	if !strings.HasSuffix(e.Summary, ".") {
		h.t.Fatalf("summary %q is not one plain sentence", e.Summary)
	}
	examples[e.Type] = e
	if tt, ok := h.t.(interface{ Logf(string, ...any) }); ok {
		tt.Logf("%s", b)
	}
	return e
}

func TestHistory_LoadStartedAndFinishedReady(t *testing.T) {
	h := twoWorkers(t, 2000*mb)
	st := h.one(history.TLoadStarted)
	h.show(st)
	if st.Profile != "m" || st.ModelFile != "m.gguf" || len(st.Nodes) != 2 || st.Nodes[0] != "a" || st.SessionID == "" ||
		st.Severity != history.Info || st.Ts != h.clk.Now().UnixMilli() || st.Data["need_mib"] != 8000 {
		t.Fatalf("%+v", st)
	}
	h.child("m").setSentPeers(map[string]int64{addrA: 1000 * mb, addrB: 1000 * mb})
	setLoad(h, "m", rpcDev(addrA, 3900, 100, 50), rpcDev(addrB, 3800, 100, 50))
	h.advance(20 * time.Second)
	h.healthy("m")
	fin := h.show(h.one(history.TLoadFinished, "outcome", "ready"))
	if fin.SessionID != st.SessionID || fin.Severity != history.Info {
		t.Fatalf("%+v", fin)
	}
	d := fin.Data
	if d["load_s"].(float64) < 20 || d["bytes"].(int64) != 2000*mb || d["bytes_measured"] != true {
		t.Fatalf("data %+v", d)
	}
	if v := d["avg_mb_s"].(float64); v < 90 || v > 101 { // 2000 MB over about 20 s
		t.Fatalf("avg_mb_s %v", v)
	}
	nodes := d["nodes_detail"].([]map[string]any)
	if len(nodes) != 2 || nodes[0]["node"] != "a" || nodes[0]["bytes"].(int64) != 1000*mb || nodes[0]["memory_mib"].(float64) != 4050 ||
		nodes[1]["memory_mib"].(float64) != 3950 {
		t.Fatalf("nodes_detail %+v", nodes)
	}
	h.show(h.one(history.TLearnedNeed))
	// Memory samples around the load: one per shared-memory node at the start and at the end.
	var starts, ends int
	for _, e := range h.of(history.TNodeMemory) {
		switch e.Data["reason"] {
		case "load_start":
			starts++
		case "load_end":
			ends++
		}
	}
	if starts != 2 || ends != 2 {
		t.Fatalf("nodeMemory samples: %d at load start, %d at load end, want 2 and 2", starts, ends)
	}
	var mem history.Event
	for _, e := range h.of(history.TNodeMemory) {
		if e.Data["reason"] == "load_end" {
			mem = e
		}
	}
	h.show(mem)
	if mem.Data["total_mib"] != 16000 || mem.Data["used_mib"] != 1000 || mem.Data["budget_mib"] != 14000 || len(mem.Nodes) != 1 {
		t.Fatalf("%+v", mem.Data)
	}
}

func TestHistory_PeriodicNodeMemorySamplesEveryMinuteWhileInUse(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.ForceNodes, p.TensorSplit, p.TTL = []string{"a"}, []int{1}, ptrDur(0) })
	h.release(h.ready("m"))
	n0 := len(h.of(history.TNodeMemory)) // the load start and end
	h.advance(5*time.Minute + 5*time.Second)
	var periodic []history.Event
	for _, e := range h.of(history.TNodeMemory) {
		if e.Data["reason"] == "periodic" {
			periodic = append(periodic, e)
		}
	}
	if len(periodic) != 5 {
		t.Fatalf("%d periodic samples in 5 minutes, want 5 (after %d before)", len(periodic), n0)
	}
	for i := 1; i < len(periodic); i++ {
		if d := periodic[i].Ts - periodic[i-1].Ts; d < 59_000 || d > 61_000 {
			t.Fatalf("samples %d ms apart", d)
		}
		if periodic[i].Nodes[0] != "a" {
			t.Fatalf("sampled %v: only the node that backs the instance", periodic[i].Nodes)
		}
	}
	h.show(periodic[0])
	// Nothing backs an instance any more: sampling stops.
	h.step(evUnload{profile: "m", force: true})
	h.advance(time.Minute)
	cnt := len(h.of(history.TNodeMemory))
	h.advance(10 * time.Minute)
	if len(h.of(history.TNodeMemory)) != cnt {
		t.Fatal("sampling goes on for a node no instance uses")
	}
}

func TestHistory_LoadStalledAndFailed(t *testing.T) {
	h, _ := stallHarness(t)
	h.autoExit = false
	sentAt(h, 549, 0)
	h.advance(182 * time.Second)
	st := h.show(h.one(history.TLoadStalled))
	if st.Severity != history.Error || st.Data["bytes_sent"].(int64) != 549 || st.Data["limit_s"].(float64) != 180 {
		t.Fatalf("%+v", st)
	}
	fin := h.show(h.one(history.TLoadFinished, "outcome", "failed"))
	if fin.Severity != history.Error || !strings.Contains(fin.Data["reason"].(string), "load stalled") || fin.SessionID != st.SessionID {
		t.Fatalf("%+v", fin)
	}
	bo := h.show(h.one(history.TProfileBackedOff))
	if bo.Profile != "m" || bo.Data["for_s"].(float64) != 60 {
		t.Fatalf("%+v", bo)
	}
}

func TestHistory_LowMemoryCancel(t *testing.T) {
	h, _ := watchHarness(t)
	h.autoExit = false
	freeOf(h, "b", 700)
	e := h.show(h.one(history.TLoadLowMemory))
	if e.Severity != history.Error || len(e.Nodes) != 1 || e.Nodes[0] != "b" || e.Data["available_mib"] != 700 || e.Data["floor_mib"] != 768 {
		t.Fatalf("%+v", e)
	}
	fin := h.one(history.TLoadFinished, "outcome", "failed")
	if fin.SessionID != e.SessionID {
		t.Fatal("not the same session")
	}
}

func TestHistory_NodeLostWorkerHungRecoveryAndReturn(t *testing.T) {
	h := lostHarness(t, recoveryCfg())
	g := h.ready("m") // a request in flight
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(31 * time.Second)
	lost := h.show(h.one(history.TNodeLost))
	if lost.Severity != history.Error || lost.Nodes[0] != "a" || lost.Profile != "m" || lost.Data["in_flight"] != 1 {
		t.Fatalf("%+v", lost)
	}
	hung := h.show(h.one(history.TWorkerHealth, "state", "hung"))
	if hung.Severity != history.Error || hung.Nodes[0] != "a" || hung.Data["node_lost"] != true {
		t.Fatalf("%+v", hung)
	}
	h.show(h.one(history.TNodeQuarantined))
	un := h.show(h.one(history.TInstanceUnloaded, "reason", "node loss"))
	if un.Severity != history.Warn {
		t.Fatalf("%+v", un)
	}
	req := h.show(h.one(history.TRecoveryRequest))
	if req.Data["action"] != "restart_rpc_server" || req.Nodes[0] != "a" {
		t.Fatalf("%+v", req)
	}
	resp := h.show(h.one(history.TRecoveryResponse))
	if resp.Data["http_status"] != 200 || resp.Data["outcome"] != RecoveryStored || resp.Severity != history.Info {
		t.Fatalf("%+v", resp)
	}
	// The node reports what it did.
	h.view.modify("a", func(n *core.Node) {
		n.Recovery = core.RecoveryOutcome{Present: true, Action: "restart_rpc_server", At: h.clk.Now().UnixMilli(), Outcome: "ok",
			Nonce: req.Data["nonce"].(string), DurationMs: 4200}
	})
	h.viewChanged()
	out := h.show(h.one(history.TRecoveryOutcome))
	if out.Data["ours"] != true || out.Data["outcome"] != "ok" {
		t.Fatalf("%+v", out)
	}
	h.view.modify("a", func(n *core.Node) { n.Online = true })
	h.viewChanged()
	ok := h.one(history.TWorkerHealth, "state", "ok")
	if ok.Severity != history.Info || ok.Data["previous"] != "hung" || ok.Data["hung_for_s"].(float64) < 30 {
		t.Fatalf("%+v", ok)
	}
	_ = g
}

func TestHistory_RecoveryRefusedStopsAndIsRecorded(t *testing.T) {
	h := hungHarness(t, "a", recoveryCfg(threeSteps))
	h.rposter.script = []recResp{{status: 409, body: "busy"}}
	h.goLive()
	r := h.show(h.one(history.TRecoveryResponse, "http_status", 409))
	if r.Severity != history.Warn || r.Data["outcome"] != RecoveryRefused || r.Data["body"] != "busy" {
		t.Fatalf("%+v", r)
	}
	s := h.show(h.one(history.TRecoveryStopped))
	if s.Severity != history.Warn || s.Data["http_status"] != 409 {
		t.Fatalf("%+v", s)
	}
}

func TestHistory_RequestRefused(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("huge", 99999)
	h.failed(h.request("huge"), http.StatusServiceUnavailable, ReasonDoesNotFit)
	e := h.show(h.one(history.TRequestRefused, "reason", string(ReasonDoesNotFit)))
	if e.Severity != history.Warn || e.Profile != "huge" || e.Data["status"] != 503 || e.Data["need_mib"] == nil || e.Data["capacity_mib"] == nil {
		t.Fatalf("%+v", e)
	}
	// Not recorded: an unknown model, a client that left, a shutdown.
	n := len(h.histLog)
	w := h.request("huge")
	_ = w
	h.cancel(h.request("huge"))
	if got := h.histLog[n:]; len(got) != 0 {
		// the same refusal again within 30 s is suppressed
		t.Fatalf("repeat recorded: %+v", got)
	}
}

func TestHistory_RepeatedRefusalsAreCountedNotRepeated(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("huge", 99999)
	for i := 0; i < 5; i++ {
		h.failed(h.request("huge"), http.StatusServiceUnavailable, ReasonDoesNotFit)
		h.advance(time.Second)
	}
	if n := len(h.of(history.TRequestRefused)); n != 1 {
		t.Fatalf("%d records for 5 identical refusals within 30 s, want 1", n)
	}
	h.advance(time.Minute)
	h.failed(h.request("huge"), http.StatusServiceUnavailable, ReasonDoesNotFit)
	got := h.of(history.TRequestRefused)
	if len(got) != 2 || got[1].Data["suppressed_since_last"] != 4 {
		t.Fatalf("%+v", got)
	}
}

func TestHistory_RefusalKinds(t *testing.T) {
	// queue timeout
	h := twoBig(t)
	h.ready("big") // busy: cannot be evicted before drain_after... the entry times out
	h.cfg.Defaults.DrainAfter = 24 * time.Hour
	w := h.request("other")
	h.advance(11 * time.Minute)
	h.failed(w, http.StatusServiceUnavailable, ReasonClusterBusy)
	h.show(h.one(history.TRequestRefused, "reason", string(ReasonClusterBusy)))
	// backoff
	h = newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = true
	w = h.request("m")
	h.exit("m", 1)
	h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
	h.failed(h.request("m"), http.StatusServiceUnavailable, ReasonBackoff)
	h.show(h.one(history.TRequestRefused, "reason", string(ReasonBackoff)))
	h.none(history.TChildExited) // it exited while loading: that is a load failure, not a ready child exit
	// forced node missing
	h = newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"ghost"} })
	h.request("f")
	h.advance(40 * time.Second)
	h.show(h.one(history.TRequestRefused, "reason", string(ReasonForcedUnavailable)))
}

func TestHistory_EvictionAndUnloadReasons(t *testing.T) {
	h := twoBig(t)
	h.release(h.ready("big"))
	h.advance(60 * time.Second)
	h.request("other")
	ev := h.show(h.one(history.TEviction))
	if ev.Profile != "big" || ev.Data["victim"] != "big" || ev.Data["for_profile"] != "other" || ev.Data["waited_s"] == nil {
		t.Fatalf("%+v", ev)
	}
	un := h.show(h.one(history.TInstanceUnloaded, "reason", "eviction"))
	if un.Profile != "big" || un.Data["loaded_for_s"].(float64) < 60 || un.Data["requests_served"] != 1 || un.Severity != history.Info {
		t.Fatalf("%+v", un)
	}
}

func TestHistory_UnloadReasonsTTLApiAndShutdown(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 6, 15000))
	h.llama("t", 3000, func(p *core.Profile) { p.TTL = ptrDur(2 * time.Minute) })
	h.llama("a", 3000, func(p *core.Profile) { p.TTL = ptrDur(0) })
	h.llama("s", 3000, func(p *core.Profile) { p.TTL = ptrDur(0) })
	h.release(h.ready("t"))
	h.release(h.ready("a"))
	h.release(h.ready("s"))
	h.advance(3 * time.Minute)
	ttl := h.show(h.one(history.TInstanceUnloaded, "reason", "ttl"))
	if ttl.Profile != "t" {
		t.Fatalf("%+v", ttl)
	}
	h.step(evUnload{profile: "a"})
	h.show(h.one(history.TInstanceUnloaded, "reason", "api"))
	h.step(evShutdown{})
	h.advance(time.Second)
	h.show(h.one(history.TInstanceUnloaded, "reason", "shutdown"))
}

func TestHistory_ChildExitedWhileReady(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.TTL = ptrDur(0) })
	h.release(h.ready("m"))
	h.exit("m", 137)
	e := h.show(h.one(history.TChildExited))
	if e.Severity != history.Error || e.Data["state"] != "ready" {
		t.Fatalf("%+v", e)
	}
	un := h.one(history.TInstanceUnloaded, "reason", "child exited")
	if un.Severity != history.Warn {
		t.Fatalf("%+v", un)
	}
}

func TestHistory_PreflightFailureAndQuarantine(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.prober.fail[addrA] = probeFail(addrA)
	h.request("m")
	e := h.show(h.one(history.TPreflightFailed))
	if e.Severity != history.Warn || e.Nodes[0] != "a" || e.Data["kind"] == nil {
		t.Fatalf("%+v", e)
	}
	q := h.show(h.one(history.TNodeQuarantined))
	if q.Data["for_s"].(float64) != 30 || q.Data["level"] != 1 {
		t.Fatalf("%+v", q)
	}
	h.advance(31 * time.Second)
	h.show(h.one(history.TQuarantineEnded))
}

func TestHistory_WorkerSuspect(t *testing.T) {
	h := idlePair(t)
	h.cfg.Defaults.WorkerSilence = 5 * time.Second
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(7 * time.Second)
	e := h.show(h.one(history.TWorkerHealth, "state", "suspect"))
	if e.Severity != history.Warn || e.Nodes[0] != "a" {
		t.Fatalf("%+v", e)
	}
}

func TestHistory_FleetViewAndFeed(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.TTL = ptrDur(0) })
	h.release(h.ready("m"))
	h.none(history.TFleetViewStale)
	h.view.stale()
	h.advance(65 * time.Second)
	st := h.show(h.one(history.TFleetViewStale))
	if st.Severity != history.Warn || st.Data["stale_after_s"].(float64) != 60 {
		t.Fatalf("%+v", st)
	}
	h.view.fresh()
	h.advance(time.Second)
	rec := h.show(h.one(history.TFleetViewRecovered))
	if rec.Data["stale_for_s"].(float64) < 4 {
		t.Fatalf("%+v", rec)
	}
	h.view.mu.Lock()
	h.view.connected = false
	h.view.mu.Unlock()
	h.advance(10 * time.Second)
	dn := h.show(h.one(history.TFleetFeedDown))
	if dn.Severity != history.Warn {
		t.Fatalf("%+v", dn)
	}
	h.view.mu.Lock()
	h.view.connected = true
	h.view.mu.Unlock()
	h.advance(10 * time.Second)
	up := h.show(h.one(history.TFleetFeedRecovered))
	if up.Data["down_for_s"].(float64) < 9 {
		t.Fatalf("%+v", up)
	}
}

func TestHistory_NeedsMiBTooLow(t *testing.T) {
	h := newHarness(t).nodes(psNode("a", 2, 12400))
	h.llama("m", 10000)
	h.request("m")
	setLoad(h, "m", rpcDev(addrA, 10500, 400, 300))
	h.healthy("m")
	e := h.show(h.one(history.TNeedsMiBTooLow))
	if e.Severity != history.Warn || e.Data["configured_mib"] != 10000 || e.Data["measured_mib"] != 11200 {
		t.Fatalf("%+v", e)
	}
}

// The history hand-off is a plain append: with no sink nothing is retained
// beyond what the step took, and an event is never produced twice.
func TestHistory_TakeClearsTheOutbox(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.request("m")
	if len(h.histLog) == 0 || len(h.c.hist) != 0 {
		t.Fatalf("log %d, outbox %d", len(h.histLog), len(h.c.hist))
	}
	seen := map[string]int{}
	for _, e := range h.histLog {
		seen[fmt.Sprintf("%s@%d@%s", e.Type, e.Ts, e.Summary)]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Fatalf("event %s recorded %d times", k, n)
		}
	}
}

// Every type the design lists has an example, except those main() writes.
func TestHistory_EveryTypeHasAnExample(t *testing.T) {
	want := []string{history.TLoadStarted, history.TLoadFinished, history.TRequestRefused, history.TLoadStalled,
		history.TLoadLowMemory, history.TNodeLost, history.TEviction, history.TInstanceUnloaded, history.TChildExited,
		history.TWorkerHealth, history.TNodeQuarantined, history.TQuarantineEnded, history.TProfileBackedOff,
		history.TRecoveryRequest, history.TRecoveryResponse, history.TRecoveryOutcome, history.TRecoveryStopped,
		history.TFleetViewStale, history.TFleetViewRecovered, history.TFleetFeedDown, history.TFleetFeedRecovered,
		history.TPreflightFailed, history.TLearnedNeed, history.TNeedsMiBTooLow, history.TNodeMemory}
	var missing []string
	for _, w := range want {
		if _, ok := examples[w]; !ok {
			missing = append(missing, w)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("no example for %v (run the whole file: examples are collected by the tests above)", missing)
	}
}

var _ = config.DefaultRecovery
var _ = instance.StopKill
var _ = placer.Measured
