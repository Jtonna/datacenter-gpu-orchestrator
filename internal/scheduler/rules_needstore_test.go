package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// Learned needs survive a restart (needs.go): a measurement is written to
// the scheduler database by the shape of its load and restored before any
// request is planned; every profile of that shape uses it.

func openNeedStore(t *testing.T) *needstore.Store {
	t.Helper()
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := needstore.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// flushNeeds writes what the core queued for the store, as the runner's
// writer does after each step.
func flushNeeds(h *harness, st NeedStore) {
	h.t.Helper()
	applyNeedOps(st, h.c.takeNeedOps(), h.c.log)
}

func storedNeeds(t *testing.T, st *needstore.Store) map[string]needstore.Record {
	t.Helper()
	recs, skipped, err := st.All()
	if err != nil || len(skipped) != 0 {
		t.Fatalf("all: %v skipped %+v", err, skipped)
	}
	out := map[string]needstore.Record{}
	for _, r := range recs {
		out[r.Profile] = r
	}
	return out
}

// tooBigEstimate is one PS5 with a budget of 12000 MiB and a profile "f"
// forced onto it whose estimate (14000, 14700 with the margin) does not fit;
// what a load measures (11000) does.
func tooBigEstimate(t *testing.T, mode string) *harness {
	h := newHarness(t).nodes(psNode("a", 2, 12000))
	h.cfg.Defaults.ForcedFitCheck = mode
	h.cfg.Configured = true
	h.llama("f", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"a"}, time.Hour })
	h.needs["f"] = NeedInfo{Need: placer.Estimated(14000), FileBytes: 1 << 30}
	h.c.buildRelease = "0.1.3"
	return h
}

// measureF loads "f" with the fit check off and lets it measure 11000 MiB.
func measureF(t *testing.T, st NeedStore) *harness {
	h := tooBigEstimate(t, config.ForcedFitOff)
	h.c.restoreNeeds(st)
	flushNeeds(h, st)
	h.request("f")
	setLoad(h, "f", rpcDev(addrA, 10200, 500, 300))
	h.healthy("f")
	flushNeeds(h, st)
	return h
}

// restart is a new core on the same store, as at a scheduler start.
func restart(t *testing.T, st NeedStore, mod ...func(*harness)) *harness {
	h := tooBigEstimate(t, config.ForcedFitReject)
	for _, m := range mod {
		m(h)
	}
	h.advance(time.Hour) // the restart is later than the measurement
	h.c.restoreNeeds(st)
	h.viewChanged()
	return h
}

func TestStoredNeed_RestartRestoresTheMeasurementAndTheForcedFitUsesIt(t *testing.T) {
	st := openNeedStore(t)
	measureF(t, st)
	got := storedNeeds(t, st)["f"]
	if got.TotalMiB != 11000 || got.Model != "f.gguf" || got.Ctx != 4096 || got.Cause != needstore.CauseLoad ||
		got.Release != "0.1.3" || !got.MeasuredAt.Equal(t0) || len(got.Placement) != 1 || got.Placement[0] != "a" ||
		got.ModelBytes != 1<<30 || len(got.Nodes) != 1 || got.Nodes[0].MiB != 11000 {
		t.Fatalf("stored %+v", got)
	}

	// Without the store the estimate is refused before anything starts.
	h0 := tooBigEstimate(t, config.ForcedFitReject)
	h0.viewChanged()
	if r := h0.c.dryPlan("f"); r.Action != PlanReject {
		t.Fatalf("no store: plan %+v", r)
	}

	h := restart(t, st)
	r := h.c.dryPlan("f")
	if r.Action != PlanStart || r.FitNeedMiB != 11000 || !r.Measured {
		t.Fatalf("restored: plan %+v", r)
	}
	if want := "measured on 2026-11-17, restored at start; 14000 MiB before"; r.NeedBasis != want {
		t.Fatalf("basis %q, want %q", r.NeedBasis, want)
	}
	h.mustLog("learned memory needs restored", "restored", 1)
	ln := h.c.statusDoc().LearnedNeeds["f"]
	if !ln.Restored || ln.MeasuredMiB != 11000 || ln.NeedMiB != 11000 || ln.Source != string(NeedRestored) ||
		!ln.MeasuredAt.Equal(t0) || ln.Age != time.Hour || ln.MeasuredRelease != "0.1.3" || ln.EstimatedMiB != 14000 {
		t.Fatalf("status %+v", ln)
	}
	// And a real request starts.
	w := h.request("f")
	h.wantState("f", "starting")
	h.pending(w)
}

func TestStoredNeed_ConfiguredNeedsMiBStaysAsToday(t *testing.T) {
	st := openNeedStore(t)
	measureF(t, st) // 11000 stored
	// needs_mib 12000 is higher: it is used, as before a restart.
	h := restart(t, st, func(h *harness) { h.cfg.Profiles["f"].NeedsMiB = 12000; delete(h.needs, "f") })
	if r := h.c.dryPlan("f"); r.NeedBasis != "measured, needs_mib" || r.FitNeedMiB != 12000 {
		t.Fatalf("higher needs_mib: plan %+v", r)
	}
	// needs_mib 10000 is lower: the restored measurement wins, as a measurement of this run would.
	h = restart(t, st, func(h *harness) { h.cfg.Profiles["f"].NeedsMiB = 10000; delete(h.needs, "f") })
	if r := h.c.dryPlan("f"); r.NeedBasis != "measured on 2026-11-17, restored at start; 10000 MiB before" || r.FitNeedMiB != 11000 {
		t.Fatalf("lower needs_mib: plan %+v", r)
	}
}

func TestStoredNeed_RaiseAfterAMemoryKillIsStored(t *testing.T) {
	st := openNeedStore(t)
	h := agentsHarness(t)
	h.c.buildRelease = "0.1.3"
	h.c.restoreNeeds(st)
	w := h.request("m")
	h.launched(readyIDs())
	in := h.inst("m")
	ch := h.children[in.iid]
	ch.exit(instance.ExitStatus{Code: -1, Signal: "aborted"})
	delete(h.children, in.iid)
	h.step(evExited{iid: in.iid, status: <-ch.exitCh, workers: []WorkerReport{
		{IID: in.iid, Node: "a", Exit: &agentproto.ExitInfo{Reason: agentproto.ExitWatchdog}}}})
	h.failed(w, 502, ReasonLoadFailed)
	flushNeeds(h, st)
	got := storedNeeds(t, st)["m"]
	if got.TotalMiB != 8000+800 || got.Cause != string(agentproto.ExitWatchdog) || len(got.Placement) != 2 {
		t.Fatalf("stored %+v", got)
	}
}

func TestStoredNeed_AChangedSettingDoesNotMatch(t *testing.T) {
	for name, mod := range map[string]func(p *core.Profile){
		"ctx":        func(p *core.Profile) { p.Ctx = 8192 },
		"model":      func(p *core.Profile) { p.Model = "other.gguf" },
		"parallel":   func(p *core.Profile) { p.Parallel = 2 },
		"kv_quant":   func(p *core.Profile) { p.KVQuant = "q8_0" },
		"flash_attn": func(p *core.Profile) { p.FlashAttn = true },
		"extra_args": func(p *core.Profile) { p.ExtraArgs = []string{"--no-mmap"} },
	} {
		t.Run(name+" at start", func(t *testing.T) {
			st := openNeedStore(t)
			measureF(t, st)
			h := restart(t, st, func(h *harness) { mod(h.cfg.Profiles["f"]) })
			if r := h.c.dryPlan("f"); r.Action != PlanReject {
				t.Fatalf("plan %+v", r)
			}
			if _, ok := h.c.statusDoc().LearnedNeeds["f"]; ok {
				t.Fatal("still in status")
			}
			flushNeeds(h, st)
			if _, ok := storedNeeds(t, st)["f"]; !ok {
				t.Fatal("the row of the old shape was removed")
			}
		})
		t.Run(name+" while running", func(t *testing.T) {
			st := openNeedStore(t)
			h := measureF(t, st)
			nc := cloneCfg(h.c.cfg)
			mod(nc.Profiles["f"])
			h.step(evConfig{cfg: nc})
			if h.c.learnedFor(h.c.cfg.Profiles["f"]) != nil {
				t.Fatal("the changed profile still matches")
			}
			// changed back, it matches its shape again
			back := cloneCfg(h.c.cfg)
			back.Profiles["f"] = tooBigEstimate(t, config.ForcedFitOff).cfg.Profiles["f"]
			h.step(evConfig{cfg: back})
			if l := h.c.learnedFor(h.c.cfg.Profiles["f"]); l == nil || l.TotalMiB != 11000 {
				t.Fatalf("changed back: %+v", l)
			}
			flushNeeds(h, st)
			if _, ok := storedNeeds(t, st)["f"]; !ok {
				t.Fatal("the row was removed")
			}
		})
	}
	// A setting that does not change the need keeps it, at start and while running.
	st := openNeedStore(t)
	h := measureF(t, st)
	nc := cloneCfg(h.c.cfg)
	nc.Profiles["f"].TTL = ptrDur(time.Minute)
	h.step(evConfig{cfg: nc})
	flushNeeds(h, st)
	if h.c.learnedFor(h.c.cfg.Profiles["f"]) == nil {
		t.Fatal("a ttl change dropped the learned need")
	}
	h = restart(t, st, func(h *harness) { h.cfg.Profiles["f"].TTL = ptrDur(time.Minute) })
	if r := h.c.dryPlan("f"); r.Action != PlanStart {
		t.Fatalf("ttl change: plan %+v", r)
	}
}

func TestStoredNeed_AReplacedModelFileDropsIt(t *testing.T) {
	st := openNeedStore(t)
	measureF(t, st)
	h := restart(t, st, func(h *harness) { h.needs["f"] = NeedInfo{Need: placer.Estimated(14000), FileBytes: 2 << 30} })
	if r := h.c.dryPlan("f"); r.Action != PlanReject {
		t.Fatalf("plan %+v", r)
	}
	flushNeeds(h, st)
	if _, ok := storedNeeds(t, st)["f"]; ok {
		t.Fatal("the row was not removed")
	}
}

// sameShapeAsF adds profile "g": another name, the same shape as "f" (a
// different ttl and description do not change the need).
func sameShapeAsF(h *harness) {
	g := *h.cfg.Profiles["f"]
	g.Name, g.TTL = "g", ptrDur(time.Minute)
	h.cfg.Profiles["g"] = &g
	h.needs["g"] = h.needs["f"]
}

// Two profiles of one shape share one measurement: what "f" measured is
// used by "g", after a restart and in the same run.
func TestStoredNeed_TwoProfilesOfOneShapeShareIt(t *testing.T) {
	st := openNeedStore(t)
	measureF(t, st)
	h := restart(t, st, sameShapeAsF)
	for _, name := range []string{"f", "g"} {
		if r := h.c.dryPlan(name); r.Action != PlanStart || r.FitNeedMiB != 11000 || !r.Measured {
			t.Fatalf("%s: plan %+v", name, r)
		}
		if ln := h.c.statusDoc().LearnedNeeds[name]; ln.MeasuredMiB != 11000 {
			t.Fatalf("%s: status %+v", name, ln)
		}
	}
	if recs, _, _ := st.All(); len(recs) != 1 || recs[0].Profile != "f" {
		t.Fatalf("rows %+v, want one, measured by f", recs)
	}
	h.mustLog("learned memory needs restored", "restored", 1, "profiles_using_them", 2)

	// a profile with another ctx does not share it
	h2 := restart(t, st, func(h *harness) {
		sameShapeAsF(h)
		h.cfg.Profiles["g"].Ctx = 8192
	})
	if r := h2.c.dryPlan("g"); r.Action != PlanReject {
		t.Fatalf("other ctx: plan %+v", r)
	}
}

// Deleting a profile keeps its measurement: another profile of the shape
// keeps using it, at once and after a restart, and a profile created later
// with that shape finds it.
func TestStoredNeed_DeletingTheProfileKeepsTheRow(t *testing.T) {
	st := openNeedStore(t)
	h := measureF(t, st)
	nc := cloneCfg(h.c.cfg)
	g := *nc.Profiles["f"]
	g.Name = "g"
	nc.Profiles["g"] = &g
	h.needs["g"] = h.needs["f"]
	h.step(evConfig{cfg: nc})
	h.step(evUnload{profile: "f", force: true})
	h.advance(10 * time.Second)
	nc = cloneCfg(h.c.cfg)
	delete(nc.Profiles, "f")
	h.step(evConfig{cfg: nc})
	flushNeeds(h, st)
	if l := h.c.learnedFor(h.c.cfg.Profiles["g"]); l == nil || l.TotalMiB != 11000 {
		t.Fatalf("g lost the measurement: %+v", l)
	}
	if _, ok := storedNeeds(t, st)["f"]; !ok {
		t.Fatal("the row was removed with its profile")
	}

	// At start, with f gone and g there, g uses it.
	h2 := restart(t, st, func(h *harness) { sameShapeAsF(h); delete(h.cfg.Profiles, "f") })
	if r := h2.c.dryPlan("g"); r.Action != PlanStart || r.FitNeedMiB != 11000 {
		t.Fatalf("g after a restart: plan %+v", r)
	}
	// With no profile of that shape at all, the row is kept for a later one.
	h3 := restart(t, st, func(h *harness) { delete(h.cfg.Profiles, "f") })
	flushNeeds(h3, st)
	if _, ok := storedNeeds(t, st)["f"]; !ok {
		t.Fatal("a row without a profile was removed at start")
	}
}

// badStore returns rows that cannot be used, or fails outright.
type badStore struct {
	recs    []needstore.Record
	skipped []needstore.Skipped
	err     error
}

func (b *badStore) All() ([]needstore.Record, []needstore.Skipped, error) {
	return b.recs, b.skipped, b.err
}
func (b *badStore) Put(needstore.Record) error { return nil }
func (b *badStore) Delete(string) error        { return nil }

func TestStoredNeed_BadRowsWarnOnceAndDoNotStopTheStart(t *testing.T) {
	st := openNeedStore(t)
	measureF(t, st)
	recs, _, _ := st.All()
	bs := &badStore{recs: recs, skipped: []needstore.Skipped{{Profile: "x", Reason: "unreadable"}, {Profile: "y", Reason: "record version 0, this build reads 1"}}}
	h := restart(t, bs)
	if n := len(h.logs.find("some stored learned needs were ignored")); n != 1 {
		t.Fatalf("warned %d times", n)
	}
	if r := h.c.dryPlan("f"); r.Action != PlanStart {
		t.Fatalf("the good row was not used: plan %+v", r)
	}

	h = restart(t, &badStore{err: errors.New("disk I/O error")})
	if n := len(h.logs.find("stored learned needs cannot be read; starting from the estimates")); n != 1 {
		t.Fatalf("warned %d times", n)
	}
	if r := h.c.dryPlan("f"); r.Action != PlanReject {
		t.Fatalf("plan %+v", r)
	}
}

func TestStoredNeed_NoStoreBehavesAsBefore(t *testing.T) {
	h := tooBigEstimate(t, config.ForcedFitOff)
	h.c.restoreNeeds(nil)
	h.request("f")
	setLoad(h, "f", rpcDev(addrA, 10200, 500, 300))
	h.healthy("f")
	if ops := h.c.takeNeedOps(); len(ops) != 0 {
		t.Fatalf("queued %d writes with no store", len(ops))
	}
	if r := h.c.dryPlan("f"); r.NeedBasis != "measured this run; 14000 MiB before" {
		t.Fatalf("plan %+v", r)
	}
}

func TestStoredNeed_ForgetDropsTheNeedAndTheRow(t *testing.T) {
	st := openNeedStore(t)
	measureF(t, st)
	h := restart(t, st)
	had, err := h.c.forgetNeed("f")
	if err != nil || !had {
		t.Fatalf("forget: %v %v", had, err)
	}
	h.mustLog("learned memory need forgotten", "profile", "f", "measured_mib", 11000)
	flushNeeds(h, st)
	if _, ok := storedNeeds(t, st)["f"]; ok {
		t.Fatal("the row was not removed")
	}
	if r := h.c.dryPlan("f"); r.Action != PlanReject {
		t.Fatalf("plan %+v", r)
	}
	if had, err := h.c.forgetNeed("f"); err != nil || had {
		t.Fatalf("second forget: %v %v", had, err)
	}
	if _, err := h.c.forgetNeed("nope"); err == nil {
		t.Fatal("an unknown profile was accepted")
	}

	// forget-need by a profile that shares the shape, and by the name of a
	// deleted profile that measured a kept figure
	measureF(t, st)
	h = restart(t, st, sameShapeAsF)
	if had, err := h.c.forgetNeed("g"); err != nil || !had {
		t.Fatalf("forget by g: %v %v", had, err)
	}
	if h.c.learnedFor(h.c.cfg.Profiles["f"]) != nil {
		t.Fatal("f still has the figure g forgot")
	}
	measureF(t, st)
	h = restart(t, st, func(h *harness) { delete(h.cfg.Profiles, "f") })
	if had, err := h.c.forgetNeed("f"); err != nil || !had {
		t.Fatalf("forget by a deleted profile: %v %v", had, err)
	}
	flushNeeds(h, st)
	if recs, _, _ := st.All(); len(recs) != 0 {
		t.Fatalf("rows left %+v", recs)
	}
}

// The runner: New restores before Run, writes go through the writer
// goroutine, and everything queued is written before Run returns.
func TestStoredNeed_SchedulerRestoresAtNewAndWritesBeforeRunReturns(t *testing.T) {
	st := openNeedStore(t)
	measureF(t, st)
	clk := newFakeClock()
	view := newFakeView(clk)
	view.set(psNode("a", 2, 12000))
	cfg := testConfig()
	cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	cfg.Profiles["f"] = llamaP("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	cfg.Profiles["g"] = llamaP("g", 4000)
	s, err := New(cfg, Options{Clock: clk, View: view, Prober: newFakeProber(view), Launcher: newFakeLauncher(),
		WorkerProber: newFakeWorkerProber(), NeedStore: st, Release: "0.1.3",
		Needs: NeedFunc(func(p *core.Profile) (NeedInfo, error) {
			if p.Name == "f" {
				return NeedInfo{Need: placer.Estimated(14000), FileBytes: 1 << 30}, nil
			}
			return NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1 << 30}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	ctx := ctxT(t)
	stat, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ln := stat.LearnedNeeds["f"]; !ln.Restored || ln.MeasuredMiB != 11000 {
		t.Fatalf("status %+v", stat.LearnedNeeds)
	}
	if r, _ := s.Plan(ctx, "f"); !strings.HasPrefix(r.NeedBasis, "measured on ") {
		t.Fatalf("plan %+v", r)
	}
	had, err := s.ForgetNeed(ctx, "f")
	if err != nil || !had {
		t.Fatalf("forget %v %v", had, err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if _, ok := storedNeeds(t, st)["f"]; ok {
		t.Fatal("the forget was not written before Run returned")
	}
}
