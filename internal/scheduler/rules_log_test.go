package scheduler

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// Scheduling is observable in the log: one structured line per event,
// emitted from the core's step function. These tests assert on messages and
// attribute keys through a capturing handler, never on formatting.

func (h *harness) mustLog(msg string, match ...any) logRec {
	h.t.Helper()
	got := h.logs.find(msg, match...)
	if len(got) == 0 {
		var all []string
		for _, r := range h.logs.all() {
			all = append(all, r.Msg)
		}
		h.t.Fatalf("no log %q matching %v; have: %s", msg, match, strings.Join(all, " | "))
	}
	return got[0]
}

func (h *harness) mustHaveAttrs(r logRec, keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		if _, ok := r.Attrs[k]; !ok {
			h.t.Fatalf("log %q lacks attr %q: %v", r.Msg, k, r.Attrs)
		}
	}
}

func (h *harness) logIndex(msg string) int {
	for i, r := range h.logs.all() {
		if r.Msg == msg {
			return i
		}
	}
	return -1
}

func TestLog_StartupSequence(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	w := &waiter{profile: "m", alias: true, reply: make(chan answer, 2)}
	h.step(evAcquire{w: w})
	h.healthy("m")
	h.granted(w)

	e := h.mustLog("queue entry created", "profile", "m", "alias", true, "waiters", 1)
	if e.Level != slog.LevelInfo {
		t.Errorf("level %v", e.Level)
	}
	pc := h.mustLog("placement chosen", "profile", "m", "need_mib", 8000, "measured", true)
	h.mustHaveAttrs(pc, "nodes", "budgets")
	h.mustLog("pre-flight ok", "node", "a", "free_mib", 15000)
	h.mustHaveAttrs(h.mustLog("pre-flight ok", "node", "a"), "duration", "addr")
	cs := h.mustLog("child started", "profile", "m", "port", 5801, "pid", 6801)
	if !strings.Contains(cs.Attrs["cmd"].(string), "--rpc 192.0.2.2:50052") {
		t.Errorf("cmd = %v", cs.Attrs["cmd"])
	}
	rd := h.mustLog("instance ready", "profile", "m")
	h.mustHaveAttrs(rd, "load_duration", "memory_mib")

	order := []string{"queue entry created", "placement chosen", "pre-flight ok", "child started", "instance ready"}
	last := -1
	for _, m := range order {
		i := h.logIndex(m)
		if i <= last {
			t.Fatalf("%q out of order (index %d after %d)", m, i, last)
		}
		last = i
	}
}

func TestLog_EvictionClaimStopAndStopped(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m1", 12000)
	h.llama("m2", 12000)
	g := h.ready("m1")
	h.release(g)
	h.request("m2")
	h.advance(61 * time.Second)

	cl := h.mustLog("claim made", "profile", "m2")
	h.mustHaveAttrs(cl, "nodes", "victims")
	if got := cl.Attrs["victims"]; !strings.Contains(strings.Join(toStrings(got), ","), "m1") {
		t.Errorf("victims = %v", got)
	}
	h.mustLog("instance stopping", "profile", "m1", "reason", "evicted for m2")
	st := h.mustLog("instance stopped", "profile", "m1", "requests_served", 1)
	h.mustHaveAttrs(st, "loaded_for", "reason")
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []core.NodeID:
		var out []string
		for _, n := range x {
			out = append(out, string(n))
		}
		return out
	}
	return nil
}

func TestLog_VictimDraining(t *testing.T) {
	h := twoBig(t)
	h.autoExit = false
	h.ready("big")
	h.request("other")
	h.advance(61 * time.Second)
	h.wantState("big", "draining")
	d := h.mustLog("victim draining", "profile", "big", "reason", "evicted for other", "in_flight", 1)
	h.mustHaveAttrs(d, "entry")
}

func TestLog_LoadFailureBackoffStartAndEnd(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.request("m")
	h.exit("m", 1)
	lf := h.mustLog("load failed", "profile", "m")
	if lf.Level != slog.LevelError {
		t.Errorf("level %v, want ERROR", lf.Level)
	}
	h.mustHaveAttrs(lf, "reason", "exit", "last_lines")
	if !strings.Contains(lf.Attrs["exit"].(string), "exit code 1") || !strings.Contains(lf.Attrs["last_lines"].(string), "out of memory") {
		t.Errorf("attrs %v", lf.Attrs)
	}
	bo := h.mustLog("profile backed off", "profile", "m")
	if bo.Level != slog.LevelWarn {
		t.Errorf("level %v", bo.Level)
	}
	h.mustHaveAttrs(bo, "for", "until")
	if len(h.logs.find("profile backoff ended")) != 0 {
		t.Fatal("backoff ended too early")
	}
	h.advance(61 * time.Second)
	h.mustLog("profile backoff ended", "profile", "m")
}

func TestLog_PreflightFailureQuarantineReleaseAndReturn(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("m", 8000)
	h.prober.fail["192.0.2.2:50052"] = probeFail("192.0.2.2:50052")
	h.request("m")
	pf := h.mustLog("pre-flight failed", "node", "a", "kind", "timeout")
	if pf.Level != slog.LevelWarn {
		t.Errorf("level %v", pf.Level)
	}
	h.mustHaveAttrs(pf, "err", "duration")
	q := h.mustLog("node quarantined", "node", "a")
	if q.Level != slog.LevelWarn {
		t.Errorf("level %v", q.Level)
	}
	h.mustHaveAttrs(q, "until", "for")
	h.mustLog("instance returned", "profile", "m")
	if len(h.logs.find("node released from quarantine")) != 0 {
		t.Fatal("released too early")
	}
	h.advance(31 * time.Second)
	r := h.mustLog("node released from quarantine", "node", "a")
	if r.Level != slog.LevelInfo {
		t.Errorf("level %v", r.Level)
	}
}

func TestLog_QueueTimeoutWarns(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.cfg.Defaults.ForcedNodeGrace = time.Hour // keep the fail-fast out of this test
	h.llama("f", 8000, func(p *core.Profile) { p.ForceNodes = []string{"ghost"} })
	h.request("f")
	h.advance(601 * time.Second)
	qt := h.mustLog("queue timeout", "profile", "f")
	if qt.Level != slog.LevelWarn {
		t.Errorf("level %v", qt.Level)
	}
	h.mustHaveAttrs(qt, "waited", "reason", "detail")
}

func TestLog_NodeLossIsError(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.autoExit = false
	h.ready("m")
	h.view.modify("a", func(n *core.Node) { n.Online = false })
	h.viewChanged()
	h.advance(31 * time.Second)
	nl := h.mustLog("node lost", "profile", "m", "node", "a")
	if nl.Level != slog.LevelError {
		t.Errorf("level %v", nl.Level)
	}
	h.mustLog("instance stopping", "profile", "m", "reason", "node loss")
}

func TestLog_StopReasonsTTLAdminAndShutdown(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("t", 8000)
	h.llama("u", 8000, func(p *core.Profile) { p.TTL = ptrDur(0) })
	h.release(h.ready("t"))
	h.advance(301 * time.Second)
	h.mustLog("instance stopping", "profile", "t", "reason", "ttl")

	h.release(h.ready("u"))
	if err := h.c.unload("u", false); err != nil {
		t.Fatal(err)
	}
	h.mustLog("instance stopping", "profile", "u", "reason", "admin")

	h.advance(10 * time.Second) // the nodes settle
	h.release(h.ready("t"))
	h.step(evShutdown{})
	b := h.mustLog("shutdown begun")
	h.mustHaveAttrs(b, "grace", "force")
	h.mustLog("instance stopping", "profile", "t", "reason", "shutdown")
	h.mustLog("shutdown complete")
}

// The hot path stays quiet: admitting and finishing requests on a ready
// instance logs nothing at INFO or above.
func TestLog_NoInfoPerRequestOnHotPath(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.release(h.ready("m"))
	n := len(h.logs.all())
	for i := 0; i < 5; i++ {
		w := h.request("m")
		h.release(h.granted(w))
	}
	for _, r := range h.logs.all()[n:] {
		if r.Level >= slog.LevelInfo {
			t.Fatalf("request path logged %v %q", r.Level, r.Msg)
		}
	}
}

// Defect 2 at the scheduler: a remote-only placement starts its child with
// CUDA_VISIBLE_DEVICES emptied, a placement with the local node does not.
func TestLaunchEnvHidesHostGPUOnlyForRemoteOnlyPlacements(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), omen(8000))
	h.autoStart = false
	h.llama("remote", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	h.llama("local", 6000, func(p *core.Profile) { p.ForceNodes = []string{"host-1"} })
	h.request("remote")
	h.request("local")
	if len(h.starts) != 2 {
		t.Fatalf("starts = %d", len(h.starts))
	}
	envOf := func(i int) []string { return h.starts[i].spec.Env }
	if got := strings.Join(envOf(0), ","); got != "CUDA_VISIBLE_DEVICES=" {
		t.Errorf("remote-only env = %q", got)
	}
	if got := envOf(1); len(got) != 0 {
		t.Errorf("local env = %q", got)
	}
}

// "devices" in the instance ready line counts the GPU/RPC devices the model
// was placed on, not the host (CPU) rows of the load log.
func TestLog_InstanceReadyCountsPlacedDevicesNotHostRows(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	h.request("m")
	h.child("m").load = instance.LoadStats{Devices: []instance.DeviceMem{
		{Name: "RPC0[192.0.2.2:50052]", ModelMiB: 7000, KVMiB: 500, ComputeMiB: 100},
		{Name: "CPU_Mapped", Host: true, ModelMiB: 300},
		{Name: "CPU", Host: true, ComputeMiB: 20},
	}}
	h.healthy("m")
	rd := h.mustLog("instance ready", "profile", "m")
	if got := rd.Attrs["devices"]; fmt.Sprint(got) != "1" {
		t.Fatalf("devices = %v, want 1 (one RPC device, two host rows)", got)
	}
}

// The raw probe output stays available at debug level; the warning leads
// with the real cause, not the expected CUDA noise.
func TestLog_PreflightFailureLeadsWithCauseAndKeepsRawAtDebug(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("m", 8000)
	raw := "0.00.021.162 E ggml_cuda_init: failed to initialize CUDA: no CUDA-capable device is detected\n" +
		"/src/ggml/src/ggml-rpc/ggml-rpc.cpp:653: Failed to connect to 192.0.2.2:50052\n"
	h.prober.fail["192.0.2.2:50052"] = &instance.ProbeError{Kind: instance.ProbeExit, Addr: "192.0.2.2:50052",
		Detail: "signal: aborted (core dumped)", Stderr: raw}
	h.request("m")
	w := h.mustLog("pre-flight failed", "node", "a")
	msg := fmt.Sprint(w.Attrs["err"])
	if !strings.Contains(msg, "Failed to connect to 192.0.2.2:50052") || strings.Contains(msg, "CUDA") {
		t.Fatalf("warning err = %q", msg)
	}
	d := h.mustLog("pre-flight raw output", "node", "a")
	if d.Level != slog.LevelDebug || !strings.Contains(fmt.Sprint(d.Attrs["stderr"]), "ggml_cuda_init") {
		t.Fatalf("debug record = %+v", d)
	}
}
