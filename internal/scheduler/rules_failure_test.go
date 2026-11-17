package scheduler

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
)

// Design 8.4 (failures) and 8.5 (shutdown).

func TestQuarantine_DoublesPerRepeatAndResetsOnSuccess(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.prober.fail["10.0.0.2:50052"] = probeFail("10.0.0.2:50052")
	w := h.request("m")
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	for i, d := range want {
		ns := h.c.nodes["a"]
		if got := ns.quarUntil.Sub(h.clk.Now()); got != d {
			t.Fatalf("quarantine %d = %v, want %v", i, got, d)
		}
		h.advance(d) // expires; the entry is placed and probed again
	}
	delete(h.prober.fail, "10.0.0.2:50052")
	h.advance(240 * time.Second)
	h.wantState("m", "starting")
	if ns := h.c.nodes["a"]; ns.quarLevel != 0 || h.c.quarantined("a") {
		t.Fatalf("quarantine not reset: %+v", ns)
	}
	_ = w
}

func TestQuarantine_CapsAtTenMinutes(t *testing.T) {
	h := newHarness(t)
	h.step(evTick{})
	for i := 0; i < 8; i++ {
		h.c.quarantine("a", errBoom)
	}
	if got := h.c.nodes["a"].quarUntil.Sub(h.clk.Now()); got != 10*time.Minute {
		t.Fatalf("quarantine %v", got)
	}
}

func TestQuarantinedNodeNeverPlaced(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.step(evTick{})
	h.c.quarantine("a", errBoom)
	w := h.request("m")
	h.wantState("m", "absent")
	h.advance(30 * time.Second)
	h.wantState("m", "starting")
	_ = w
}

func TestFailure_ChildExitsDuringStarting_502TailAndBackoff(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	w1 := h.request("m")
	w2 := h.request("m")
	h.exit("m", 1)
	for _, w := range []*waiter{w1, w2} {
		e := h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
		if !slices.Contains(e.Tail, "error: out of memory") {
			t.Fatalf("tail %v", e.Tail)
		}
	}
	h.wantState("m", "absent")
	if b, on := h.c.backoffActive("m"); !on || b.until.Sub(h.clk.Now()) != 60*time.Second {
		t.Fatalf("backoff %+v %v", b, on)
	}
	if h.c.quarantined("a") {
		t.Fatalf("node quarantined for a child failure")
	}
	if h.c.nodes["a"].settle != setNone && h.c.nodes["a"].settle != setWaiting {
		t.Fatalf("a: %v", h.c.nodes["a"].settle)
	}
}

func TestFailure_LoadTimeout_502BackoffAndStop(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000, func(p *core.Profile) { p.LoadTimeout = 5 * time.Minute })
	h.autoExit = false
	w := h.request("m")
	h.advance(5*time.Minute - time.Second)
	h.pending(w)
	h.advance(time.Second)
	h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
	h.wantState("m", "stopping")
	if _, on := h.c.backoffActive("m"); !on {
		t.Fatalf("no backoff")
	}
}

func TestFailure_StartError_502AndBackoff(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.launcher.failNext = errors.New("exec: not found")
	w := h.request("m")
	e := h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
	if !strings.Contains(e.Message, "not found") {
		t.Fatalf("message %q", e.Message)
	}
	h.wantState("m", "absent")
	if h.c.nodes["a"].settle != setNone {
		t.Fatalf("a settles although no child ran")
	}
}

func TestFailure_NeedProviderError_IsLoadFailureNotCrash(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 0)
	h.needErr["m"] = errors.New("model file: open /models/m.gguf: no such file")
	w := h.request("m")
	h.failed(w, http.StatusBadGateway, ReasonLoadFailed)
	w2 := h.request("m")
	h.failed(w2, http.StatusServiceUnavailable, ReasonBackoff)
}

func TestBackoff_DoublesAndResetsOnReady(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	for i, d := range []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second} {
		h.request("m")
		h.exit("m", 1)
		b, _ := h.c.backoffActive("m")
		if got := b.until.Sub(h.clk.Now()); got != d {
			t.Fatalf("backoff %d = %v, want %v", i, got, d)
		}
		h.advance(d)
	}
	h.ready("m")
	if h.c.backoffs["m"] != nil {
		t.Fatalf("backoff not reset at ready")
	}
}

func TestBackoff_AnswersEntry503(t *testing.T) {
	// A load of m fails while an entry for m exists: the entry is answered.
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	w1 := h.request("m")
	// a returned instance creates the entry; simulate one by a node loss requeue
	in := h.inst("m")
	in.waiters = nil
	h.c.entries["m"] = &entry{prof: in.prof, seq: h.c.next(), created: h.clk.Now(), waiters: []*waiter{w1}}
	h.exit("m", 1)
	h.failed(w1, http.StatusServiceUnavailable, ReasonBackoff)
	h.noEntry("m")
}

func TestFailure_ChildExitsWhileReady_Absent(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	g := h.ready("m")
	h.exit("m", 139)
	h.wantState("m", "absent")
	h.release(g) // the failed request ends later; nothing breaks
	if _, on := h.c.backoffActive("m"); on {
		t.Fatalf("backoff after a ready instance died")
	}
}

func TestFailure_NodeLoss_After30sKillsInstance(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	g := h.ready("m")
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(29 * time.Second)
	h.wantState("m", "ready")
	h.advance(time.Second)
	h.wantState("m", "stopping")
	if modes := h.child("m").stopModes(); len(modes) != 1 || modes[0] != instance.StopKill {
		t.Fatalf("stop modes %v, want SIGKILL", modes)
	}
	_ = g
}

func TestFailure_NodeLoss_AbsentFromView(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	h.ready("m")
	h.view.remove("a")
	h.viewChanged()
	h.advance(30 * time.Second)
	h.wantState("m", "stopping")
}

func TestFailure_NodeLoss_CounterResetsOnReconnect(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.ready("m")
	h.view.remove("a")
	h.viewChanged()
	h.advance(20 * time.Second)
	h.view.reconnect()
	h.viewChanged()
	h.advance(20 * time.Second)
	h.wantState("m", "ready")
	h.view.set(ps5("a", 2, 7000))
	h.viewChanged()
	h.advance(time.Minute)
	h.wantState("m", "ready")
}

func TestFailure_NodeLoss_NotCountedWhileStale(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.ready("m")
	h.view.stale()
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.advance(5 * time.Minute)
	h.wantState("m", "ready") // loaded instances keep serving
}

func TestFailure_NodeLossDuringStarting_WaitersRequeued(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("big", 12000)
	h.llama("m", 12000)
	h.ready("big")
	h.cfg.Defaults.DrainAfter = time.Hour
	h.autoExit = false
	w := h.request("m")
	h.wantState("m", "starting")
	mine := h.nodesOf("m")[0]
	h.view.modify(mine, func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(30 * time.Second)
	h.wantState("m", "stopping")
	h.pending(w)
	if e := h.entry("m"); len(e.waiters) != 1 {
		t.Fatalf("waiters not requeued")
	}
	if _, on := h.c.backoffActive("m"); on {
		t.Fatalf("backed off for a node loss")
	}
}

func TestShutdown_AnswersQueued_StopsStarting_WaitsInFlight_ThenStops(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("busy", 12000)
	h.llama("loading", 12000)
	h.llama("queued", 12000)
	h.autoExit = false
	g := h.ready("busy")
	wl := h.request("loading")
	wq := h.request("queued")
	h.step(evShutdown{})
	h.failed(wq, http.StatusServiceUnavailable, ReasonShuttingDown)
	h.failed(wl, http.StatusServiceUnavailable, ReasonShuttingDown)
	h.wantState("loading", "stopping")
	h.wantState("busy", "ready")
	wn := h.request("busy")
	h.failed(wn, http.StatusServiceUnavailable, ReasonShuttingDown)
	h.advance(time.Minute)
	h.wantState("busy", "ready")
	h.release(g)
	h.wantState("busy", "stopping")
	for _, p := range []string{"busy", "loading"} {
		h.exitChild(h.inst(p).iid, instanceExit(0))
	}
	h.step(evTick{})
	if !h.c.finished {
		t.Fatalf("not finished")
	}
}

func TestShutdown_GraceExpiresStopsBusy(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	h.ready("m")
	h.step(evShutdown{})
	h.advance(299 * time.Second)
	h.wantState("m", "ready")
	h.advance(time.Second)
	h.wantState("m", "stopping")
}

func TestShutdown_DuringLoad(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("pf", 8000)
	h.llama("launch", 8000)
	h.autoProbe = false
	wp := h.request("pf") // in pre-flight
	h.autoProbe = true
	h.autoStart = false
	wl := h.request("launch") // Start pending
	h.step(evShutdown{})
	h.failed(wp, http.StatusServiceUnavailable, ReasonShuttingDown)
	h.failed(wl, http.StatusServiceUnavailable, ReasonShuttingDown)
	h.wantState("pf", "absent") // no child: absent at once
	h.wantState("launch", "stopping")
	h.completeProbes() // late results are ignored
	h.completeStarts() // the late child is stopped
	h.step(evTick{})
	if !h.c.finished {
		t.Fatalf("not finished: %v", h.c.insts)
	}
}

func TestShutdown_ForceKills(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	h.ready("m")
	h.step(evShutdown{force: true})
	if modes := h.child("m").stopModes(); len(modes) == 0 || modes[len(modes)-1] != instance.StopKill {
		t.Fatalf("modes %v", modes)
	}
}

func TestUnload_IdleStops_BusyRefused_ForceKills(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	g := h.ready("m")
	if err := h.c.unload("m", false); !errors.Is(err, ErrBusy) {
		t.Fatalf("unload busy: %v", err)
	}
	h.step(evUnload{profile: "m", force: true})
	h.wantState("m", "stopping")
	if modes := h.child("m").stopModes(); modes[0] != instance.StopKill {
		t.Fatalf("modes %v", modes)
	}
	_ = g
	if err := h.c.unload("nope", false); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("unload unknown: %v", err)
	}
}

func TestUnloadAllIdle_StopsOnlyIdle(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("idle", 8000)
	h.llama("busy", 8000)
	h.release(h.ready("idle"))
	h.ready("busy")
	reply := make(chan []string, 1)
	h.step(evUnloadIdle{reply: reply})
	if got := <-reply; !slices.Equal(got, []string{"idle"}) {
		t.Fatalf("stopped %v", got)
	}
	h.wantState("busy", "ready")
}

// Found by the random tests (seed 79): a claim dropped at shutdown returned
// a draining victim to ready, which admitted (and then re-answered) the
// victim profile's own queued waiters.
func TestShutdown_QueuedWaiterOfDrainingVictimGets503Once(t *testing.T) {
	h := twoBig(t)
	g := h.ready("big")
	h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	wb := h.request("big") // queued behind its own draining instance
	h.step(evShutdown{})
	h.failed(wb, http.StatusServiceUnavailable, ReasonShuttingDown)
	h.wantState("big", "ready") // claim dropped; it finishes its generation
	h.release(g)
	h.wantState("big", "absent")
}
