package scheduler

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// scopedLoad is LoadScoped on the harness: the waiter of the load.
func (h *harness) scopedLoad(r ScopedLoad) *waiter {
	h.t.Helper()
	p, _, ok := h.c.cfg.Resolve(r.Model)
	if !ok {
		h.t.Fatalf("no profile %s", r.Model)
	}
	sp, err := scopedProfile(h.c.cfg, p, r)
	if err != nil {
		h.t.Fatalf("scoped profile: %v", err)
	}
	w := &waiter{profile: sp.Name, synthetic: true, reply: make(chan answer, 1), scoped: sp}
	h.step(evAcquire{w: w})
	return w
}

func ptrInt(v int) *int       { return &v }
func ptrStr(v string) *string { return &v }
func ptrBool(v bool) *bool    { return &v }

// realisticScoped is a fleet and profiles shaped like live: automatic and
// forced profiles, a host-overhead-free estimate, one that never fits.
func realisticScoped(t *testing.T) *harness {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000), ps5("c", 5, 15000), ps5("d", 6, 15000), laptop("l", 7000))
	h.llama("m", 8000)
	h.llama("big", 0, func(p *core.Profile) { p.Ctx = 32768 })
	h.needs["big"] = NeedInfo{Need: placer.Estimated(26000), FileBytes: 1 << 34, DeviceMiB: 300}
	h.llama("onb", 6000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
	h.llama("pair", 20000, func(p *core.Profile) { p.ForceNodes = []string{"c", "d"} })
	h.llama("never", 90000)
	h.llama("tiered", 3000, func(p *core.Profile) { p.AllowTiers = []int{2} })
	h.viewChanged()
	return h
}

// plansOf is every configured profile's plan, and its plan on each node
// alone (PlanOn), as the flip list compares them.
func plansOf(h *harness) map[string]PlanReport {
	out := map[string]PlanReport{}
	for name, p := range h.c.cfg.Profiles {
		out[name] = h.c.dryPlan(name)
		for _, n := range []string{"a", "b", "c", "d", "l"} {
			if e := h.c.pinConflict(p); e != nil {
				continue
			}
			out[name+" on "+n] = h.c.dryPlanOf(name, pinCopy(p, []string{n}))
		}
	}
	return out
}

// Proof 1, the flip list: with no scoped profile held, every profile's plan
// (automatic, and on each chosen node) is the same, field by field, at each
// point of the same run, whether or not scoped loads came and went before
// (a refused one, a planned one, one loaded and unloaded).
func TestScoped_FlipListNoScopedProfileHeld(t *testing.T) {
	run := func(t *testing.T, scoped bool) []map[string]PlanReport {
		h := realisticScoped(t)
		var out []map[string]PlanReport
		if scoped {
			// refused at once: a zone not in the fleet view
			sp, _ := scopedProfile(h.c.cfg, h.c.cfg.Profiles["m"], ScopedLoad{Model: "m", Scope: "r0", Nodes: []string{"zz"}})
			w := &waiter{profile: sp.Name, synthetic: true, reply: make(chan answer, 1), scoped: sp}
			h.step(evAcquire{w: w})
			h.failed(w, http.StatusConflict, ReasonPinUnusable)
			// planned only
			sp2, _ := scopedProfile(h.c.cfg, h.c.cfg.Profiles["m"], ScopedLoad{Model: "m", Scope: "r1", Ctx: ptrInt(8192)})
			_ = h.c.scopedPlan(sp2)
			if len(h.c.scoped) != 0 {
				t.Fatalf("held after a refusal and a plan: %v", h.c.scoped)
			}
		}
		out = append(out, plansOf(h))
		g := h.ready("m")
		out = append(out, plansOf(h))
		h.release(g)
		h.step(evUnload{profile: "m"})
		h.drain()
		h.step(evTick{})
		out = append(out, plansOf(h))
		h.request("onb")
		h.healthy("onb")
		out = append(out, plansOf(h))
		if len(h.c.scoped) != 0 {
			t.Fatalf("scoped held: %v", h.c.scoped)
		}
		return out
	}
	without, with := run(t, false), run(t, true)
	if len(without) != len(with) {
		t.Fatalf("points %d and %d", len(without), len(with))
	}
	rows := 0
	for i := range without {
		for name, a := range without[i] {
			b, ok := with[i][name]
			if !ok || !reflect.DeepEqual(a, b) {
				t.Fatalf("point %d, %s:\nwithout %+v\nwith    %+v", i, name, a, b)
			}
			rows++
		}
		if len(with[i]) != len(without[i]) {
			t.Fatalf("point %d: %d and %d rows", i, len(without[i]), len(with[i]))
		}
	}
	if rows < 4*6 {
		t.Fatalf("only %d rows compared", rows)
	}
}

// Proofs 3 and 5: a scoped load at the production shape on consoles
// production is not using, while a production request is being served: both
// answer, neither is admitted to the other's instance, and production plans
// as with none loaded (a profile on other nodes: the same plan; the one
// loaded: admitted to its own instance).
func TestScoped_BesideProductionNeitherAdmittedToTheOther(t *testing.T) {
	checkInvariantsEachStep = true
	defer func() { checkInvariantsEachStep = false }()
	h := realisticScoped(t)
	h.autoExit = false
	gProd := h.ready("m") // in flight: not released
	prodNodes := h.nodesOf("m")
	var free []string
	for _, n := range []string{"a", "b", "c", "d"} {
		if !slices.Contains(prodNodes, core.NodeID(n)) && n != "b" {
			free = append(free, n)
		}
	}
	zone := free[0]
	beforeM, beforeOnb := h.c.dryPlan("m"), h.c.dryPlan("onb")

	w := h.scopedLoad(ScopedLoad{Model: "m", Scope: "run1", Nodes: []string{zone}})
	if a, ok := h.peek(w); ok && a.err != nil {
		t.Fatalf("scoped load refused: %v", a.err)
	}
	h.healthy("m@run1")
	h.wantNodes("m@run1", core.NodeID(zone))
	gScoped := h.granted(h.request("m@run1"))
	if gScoped.Profile != "m@run1" || gProd.Profile != "m" || gScoped.Instance == gProd.Instance {
		t.Fatalf("grants prod %+v scoped %+v", gProd, gScoped)
	}
	// production goes on being served by its own instance only
	for i := 0; i < 3; i++ {
		g := h.granted(h.request("m"))
		if g.Instance != gProd.Instance {
			t.Fatalf("a production request was admitted to instance %d, production's is %d", g.Instance, gProd.Instance)
		}
		h.release(g)
		g2 := h.granted(h.request("m@run1"))
		if g2.Instance != gScoped.Instance {
			t.Fatalf("a scoped request went to instance %d", g2.Instance)
		}
		h.release(g2)
	}
	if a, b := h.c.dryPlan("m"), beforeM; !reflect.DeepEqual(a, b) {
		t.Fatalf("production's plan moved:\nbefore %+v\nafter  %+v", b, a)
	}
	if a, b := h.c.dryPlan("onb"), beforeOnb; !reflect.DeepEqual(a, b) {
		t.Fatalf("a production profile on other nodes plans differently:\nbefore %+v\nafter  %+v", b, a)
	}
	if !reflect.DeepEqual(h.c.cfg.Profiles["m"], h.cfg.Profiles["m"]) || h.c.cfg.Profiles["m@run1"] != nil {
		t.Fatal("the configuration changed")
	}
	st := h.c.instStatus(h.inst("m@run1"))
	if st.StaleProfile {
		t.Fatalf("scoped instance reported stale: %+v", st)
	}
	h.release(gProd)
	h.release(gScoped)
}

// The run's settings apply to the scoped copy only; the name is
// "<id>@<scope>"; the same settings again attach, other settings are 409.
func TestScoped_SettingsNameAndConflict(t *testing.T) {
	h := realisticScoped(t)
	h.c.cfg.Profiles["m"].Aliases = []string{"m-old"}
	r := ScopedLoad{Model: "m-old", Scope: "bench-7", Nodes: []string{"c", "d"}, Ctx: ptrInt(16384), Parallel: ptrInt(2),
		KVQuant: ptrStr("q8_0"), FlashAttn: ptrBool(true), Thinking: ptrBool(false)}
	w := h.scopedLoad(r)
	if a, ok := h.peek(w); ok && a.err != nil {
		t.Fatal(a.err)
	}
	sp := h.c.scoped["m@bench-7"]
	if sp == nil {
		t.Fatalf("held %v", h.c.scoped)
	}
	want := *h.c.cfg.Profiles["m"]
	want.Name, want.Aliases = "m@bench-7", nil
	want.Ctx, want.Parallel, want.KVQuant, want.FlashAttn = 16384, 2, "q8_0", true
	want.ForceNodes, want.ExtraArgs = []string{"c", "d"}, []string{"--reasoning-budget", "0"}
	want.NeedsMiB = 0 // another shape: the written need is not inherited
	if !reflect.DeepEqual(*sp, want) {
		t.Fatalf("scoped\n%+v\nwant\n%+v", *sp, want)
	}
	p := h.c.cfg.Profiles["m"]
	if p.Ctx != 4096 || p.Parallel != 1 || len(p.ForceNodes) != 0 || len(p.ExtraArgs) != 0 {
		t.Fatalf("production changed: %+v", p)
	}
	// same settings: attaches to the same entry, no error
	w2 := h.scopedLoad(r)
	if a, ok := h.peek(w2); ok && a.err != nil {
		t.Fatal(a.err)
	}
	// other settings under the same name: 409
	r.Ctx = ptrInt(8192)
	h.failed(h.scopedLoad(r), http.StatusConflict, ReasonScopedConflict)
	if rep := h.c.scopedPlan(mustScoped(t, h, r)); rep.Action != PlanReject {
		t.Fatalf("plan with other settings %+v", rep)
	}
	// thinking on removes the variant
	sp3 := mustScoped(t, h, ScopedLoad{Model: "m", Scope: "x", Thinking: ptrBool(true)})
	if len(sp3.ExtraArgs) != 0 {
		t.Fatalf("thinking on %v", sp3.ExtraArgs)
	}
}

func mustScoped(t *testing.T, h *harness, r ScopedLoad) *core.Profile {
	t.Helper()
	p, _, _ := h.c.cfg.Resolve(r.Model)
	sp, err := scopedProfile(h.c.cfg, p, r)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// Refusals of the body: 400 with what is wrong.
func TestScoped_BadBodies(t *testing.T) {
	h := realisticScoped(t)
	h.command("cmd", 500)
	h.llama("m@taken", 500)
	h.c.cfg.Profiles["onb"].Aliases = []string{"m@alias"}
	cases := []struct {
		r    ScopedLoad
		want string
	}{
		{ScopedLoad{Model: "m"}, "scope must be"},
		{ScopedLoad{Model: "m", Scope: "has space"}, "scope must be"},
		{ScopedLoad{Model: "m", Scope: strings.Repeat("x", 65)}, "scope must be"},
		{ScopedLoad{Model: "m", Scope: "a@b"}, "scope must be"},
		{ScopedLoad{Model: "cmd", Scope: "s"}, "command profile"},
		{ScopedLoad{Model: "m", Scope: "taken"}, "configured profile id or alias"},
		{ScopedLoad{Model: "m", Scope: "alias"}, "configured profile id or alias"},
		{ScopedLoad{Model: "m", Scope: "s", Ctx: ptrInt(0)}, "ctx must be positive"},
		{ScopedLoad{Model: "m", Scope: "s", Parallel: ptrInt(-1)}, "parallel must be positive"},
		{ScopedLoad{Model: "m", Scope: "s", KVQuant: ptrStr("q8_0")}, "requires flash_attn"},
		{ScopedLoad{Model: "m", Scope: "s", KVQuant: ptrStr("q5")}, "unknown value"},
		{ScopedLoad{Model: "m", Scope: "s", Host: ptrStr("moon")}, "unknown value"},
		{ScopedLoad{Model: "m", Scope: "s", Host: ptrStr("node")}, "requires zones"},
		{ScopedLoad{Model: "m", Scope: "s", Host: ptrStr("node"), Nodes: []string{"a"}}, "membership: agents"},
	}
	for _, c := range cases {
		p, _, _ := h.c.cfg.Resolve(c.r.Model)
		_, err := scopedProfile(h.c.cfg, p, c.r)
		if err == nil || err.Status != http.StatusBadRequest || !strings.Contains(err.Message, c.want) {
			t.Errorf("%+v: got %v, want 400 %q", c.r, err, c.want)
		}
	}
}

// Lifetime: held while its entry or instance is; after the unload the name
// is forgotten and answers 404, and the need cached for it is dropped. A
// refused scoped load holds nothing; neither does a plan.
func TestScoped_LifetimeEndsWithItsInstance(t *testing.T) {
	checkInvariantsEachStep = true
	defer func() { checkInvariantsEachStep = false }()
	h := realisticScoped(t)
	h.scopedLoad(ScopedLoad{Model: "m", Scope: "r", Nodes: []string{"c"}})
	h.healthy("m@r")
	if _, err := h.c.need(h.c.scoped["m@r"]); err != nil || h.c.needs_["m@r"].Need.MiB == 0 {
		t.Fatalf("need %v %+v", err, h.c.needs_["m@r"])
	}
	h.step(evUnload{profile: "m@r"})
	h.drain()
	h.step(evTick{})
	if h.c.insts["m@r"] != nil || len(h.c.scoped) != 0 {
		t.Fatalf("still held: %v", h.c.scoped)
	}
	if _, ok := h.c.needs_["m@r"]; ok {
		t.Fatal("the need of a forgotten scoped name is kept")
	}
	h.failed(h.request("m@r"), http.StatusNotFound, ReasonUnknownModel)
	// the same name with other settings is a new scoped profile now
	h.scopedLoad(ScopedLoad{Model: "m", Scope: "r", Nodes: []string{"d"}, Ctx: ptrInt(8192)})
	h.healthy("m@r")
	h.wantNodes("m@r", "d")
	if h.inst("m@r").prof.Ctx != 8192 {
		t.Fatal("old settings")
	}
}

// History names the production id and the scope; the request carries the
// bench run as before.
func TestScoped_HistoryNamesProductionAndScope(t *testing.T) {
	h := realisticScoped(t)
	w := &waiter{profile: "m@run9", synthetic: true, reply: make(chan answer, 1), benchRun: "run9",
		scoped: mustScoped(t, h, ScopedLoad{Model: "m", Scope: "run9", Nodes: []string{"c"}})}
	h.step(evAcquire{w: w})
	h.healthy("m@run9")
	h.step(evUnload{profile: "m@run9"})
	h.drain()
	h.step(evTick{})
	n := 0
	for _, e := range h.histLog {
		if e.Data["scope"] == nil {
			continue
		}
		n++
		if e.Profile != "m" || e.Data["scope"] != "run9" || e.ModelFile != "m.gguf" {
			t.Fatalf("event %s: profile %q scope %v model %q", e.Type, e.Profile, e.Data["scope"], e.ModelFile)
		}
	}
	var types []string
	for _, e := range h.histLog {
		if e.Profile == "m@run9" {
			t.Fatalf("an event names the scoped name: %+v", e)
		}
		if e.Data["scope"] != nil {
			types = append(types, e.Type)
		}
	}
	if n == 0 || !slices.Contains(types, history.TInstanceUnloaded) {
		t.Fatalf("scoped events %v", types)
	}
}

// A configuration change keeps a scoped entry while its production profile
// stays (the run's copy, not the new definition), and fails it when the
// production profile is deleted.
func TestScoped_ConfigChange(t *testing.T) {
	checkInvariantsEachStep = true
	defer func() { checkInvariantsEachStep = false }()
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 14000)
	h.llama("m", 8000)
	h.llama("k", 8000)
	h.autoExit = false
	h.ready("big")
	h.scopedLoad(ScopedLoad{Model: "m", Scope: "q", Nodes: []string{"a"}}) // waits for big
	wk := &waiter{profile: "k@q", synthetic: true, reply: make(chan answer, 1), scoped: mustScoped(t, h, ScopedLoad{Model: "k", Scope: "q"})}
	h.step(evAcquire{w: wk})
	if h.entry("m@q") == nil || h.entry("k@q") == nil {
		t.Fatal("not queued")
	}
	nc := cloneCfg(h.cfg)
	nc.Profiles["m"].Ctx = 8192
	delete(nc.Profiles, "k")
	nc.Revision = 2
	h.step(evConfig{cfg: nc})
	h.cfg = nc
	if e := h.entry("m@q"); e.prof != h.c.scoped["m@q"] || e.prof.Ctx != 4096 {
		t.Fatalf("entry %+v", e.prof)
	}
	h.failed(wk, http.StatusNotFound, ReasonUnknownModel)
	h.step(evTick{})
	if h.c.scoped["k@q"] != nil {
		t.Fatal("scoped profile of a deleted production profile kept")
	}
}

// The run time is unchanged by a scoped load's wait (a sanity check that
// the queue treats it as any entry): it times out as any other.
func TestScoped_QueuedTimesOutAsAnyEntry(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000))
	h.llama("big", 14000)
	h.llama("m", 8000)
	h.autoExit = false
	g := h.ready("big")
	w := h.scopedLoad(ScopedLoad{Model: "m", Scope: "t", Nodes: []string{"a"}})
	h.pending(w)
	_ = g
	h.advance(h.c.cfg.Defaults.QueueTimeout + time.Second)
	h.step(evTick{})
	if h.entry("m@t") != nil && h.c.scoped["m@t"] == nil {
		t.Fatal("entry without its scoped profile")
	}
}

// The shared model path rule (internal/modelpath) applies to a scoped copy
// as to any profile: a production profile whose stored model path breaks it
// (dotted or absolute) gives no scoped load.
func TestScoped_ModelPathRule(t *testing.T) {
	h := realisticScoped(t)
	for _, bad := range []string{"../x.gguf", "/abs/x.gguf", ".hidden/x.gguf", "a/../../x.gguf"} {
		h.llama("bad", 1000, func(p *core.Profile) { p.Model = bad })
		_, err := scopedProfile(h.c.cfg, h.c.cfg.Profiles["bad"], ScopedLoad{Model: "bad", Scope: "s"})
		if err == nil || err.Status != http.StatusBadRequest || !strings.Contains(err.Message, "model") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := scopedProfile(h.c.cfg, h.c.cfg.Profiles["m"], ScopedLoad{Model: "m", Scope: "s", Ctx: ptrInt(1024)}); err != nil {
		t.Fatal(err)
	}
}

// A written need (needs_mib) belongs to the production shape. A scoped copy
// whose shape differs (ctx, parallel, kv_quant, flash_attn, extra args,
// host) does not inherit it: it plans from the estimate or a measurement of
// its own shape, and the plan says so. At the production shape the written
// need applies as before.
func TestScoped_WrittenNeedOnlyAtTheProductionShape(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	h.llama("tiny", 700, func(p *core.Profile) { p.Ctx = 2048 })
	h.viewChanged()
	same := mustScoped(t, h, ScopedLoad{Model: "tiny", Scope: "s0", Nodes: []string{"b"}})
	if same.NeedsMiB != 700 {
		t.Fatalf("production shape: needs_mib %d, want the written 700", same.NeedsMiB)
	}
	r := h.c.scopedPlan(same)
	if r.NeedMiB != 700 || strings.Contains(r.NeedBasis, "written need not used") {
		t.Fatalf("production shape plan %+v", r)
	}
	for _, c := range []ScopedLoad{
		{Ctx: ptrInt(8192), KVQuant: ptrStr("q8_0"), FlashAttn: ptrBool(true), Thinking: ptrBool(false)}, // the copy's case
		{Ctx: ptrInt(8192)},
		{Parallel: ptrInt(2)},
		{KVQuant: ptrStr("q8_0"), FlashAttn: ptrBool(true)},
		{FlashAttn: ptrBool(true)},
		{Thinking: ptrBool(false)},
	} {
		c.Model, c.Scope, c.Nodes = "tiny", "s1", []string{"b"}
		sp := mustScoped(t, h, c)
		if sp.NeedsMiB != 0 {
			t.Fatalf("%+v: inherited the written need %d", c, sp.NeedsMiB)
		}
		h.needs["tiny@s1"] = NeedInfo{Need: placer.Estimated(2600), FileBytes: 1 << 30} // what the estimate gives for its shape
		h.c.forgetScopedNeed("tiny@s1")
		r := h.c.scopedPlan(sp)
		if r.NeedMiB != 2600 || !strings.Contains(r.NeedBasis, "written need not used: the scoped shape differs from tiny") {
			t.Fatalf("%+v: plan need %d basis %q", c, r.NeedMiB, r.NeedBasis)
		}
	}
	if h.c.cfg.Profiles["tiny"].NeedsMiB != 700 {
		t.Fatal("production changed")
	}
}

// A measured need of the production shape is used by a scoped copy at that
// shape only; a copy with a raised ctx does not plan with it.
func TestScoped_MeasuredNeedOnlyAtTheProductionShape(t *testing.T) {
	h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
	p := h.llama("mm", 0, func(p *core.Profile) { p.Ctx = 2048 })
	h.needs["mm"] = NeedInfo{Need: placer.Estimated(1500), FileBytes: 1 << 30}
	h.viewChanged()
	h.c.putLearned(&learnedNeed{TotalMiB: 3100, key: needKeyOf(p), profile: "mm", devices: 1, at: h.c.now})
	same := mustScoped(t, h, ScopedLoad{Model: "mm", Scope: "m0", Nodes: []string{"b"}})
	h.needs["mm@m0"] = h.needs["mm"]
	if r := h.c.scopedPlan(same); r.NeedMiB != 3100 || !r.Measured {
		t.Fatalf("production shape: %+v", r)
	}
	big := mustScoped(t, h, ScopedLoad{Model: "mm", Scope: "m1", Nodes: []string{"b"}, Ctx: ptrInt(8192)})
	h.needs["mm@m1"] = NeedInfo{Need: placer.Estimated(2600), FileBytes: 1 << 30}
	r := h.c.scopedPlan(big)
	if r.NeedMiB == 3100 || (r.Measured && !strings.Contains(r.NeedBasis, "derived")) {
		t.Fatalf("raised ctx planned with production's measurement: %+v", r)
	}
}
