package scheduler

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

// scopedTrainedHarness: one console "a" with free MiB, forced_fit_check
// off, the real need provider over a model trained at 32,768, and
// production "w" at that ctx with no written need.
func scopedTrainedHarness(t *testing.T, free int, mod func(*core.Profile)) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := ggufgen.Write(filepath.Join(dir, "w.gguf"), ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8, Context: 32768}); err != nil {
		t.Fatal(err)
	}
	n := ps5("a", 2, free)
	n.TotalMiB = max(n.TotalMiB, free+1000)
	h := newHarness(t).nodes(n)
	h.cfg.Defaults.ForcedFitCheck = config.ForcedFitOff
	h.c.cfg.ModelsDir = dir
	h.c.needs = &ConfigNeeds{Cfg: h.c.cfg}
	h.llama("w", 0, func(p *core.Profile) {
		p.Model, p.Ctx = "w.gguf", 32768
		if mod != nil {
			mod(p)
		}
	})
	h.viewChanged()
	return h
}

// A SCOPED load whose ctx override is above the trained context is planned,
// fit-checked on the zones it names and loaded at the trained context, with
// the note, like a configured profile; its status carries both the scope
// fields and the context fields; asking for it again is the same load.
func TestRealCtx_ScopedCtxAboveTrainedIsPlannedAndLoadedAtTheTrainedContext(t *testing.T) {
	const note = "ctx 131,072 is above this model's trained context of 32,768; the server runs it at 32,768"
	// 15,000 MiB is too little for 4 x 32,768 on "a" (TestScoped_FitCheckedOnNamedZonesWithForcedFitOff)
	// and enough for 32,768: the fit check must use the trained figure.
	h := scopedTrainedHarness(t, 15000, nil)
	r := h.c.scopedPlan(mustScoped(t, h, scoped4x))
	if r.Action != PlanStart || r.Ctx != 32768 || r.CtxNote != note {
		t.Fatalf("plan: action %s ctx %d note %q reason %q", r.Action, r.Ctx, r.CtxNote, r.Reason)
	}
	w := h.scopedLoad(scoped4x)
	if a, ok := h.peek(w); ok && a.err != nil {
		t.Fatalf("refused: %v", a.err)
	}
	h.wantState("w@x4", "starting")
	in := h.c.insts["w@x4"]
	if in.prof.Ctx != 32768 || in.prof.CtxAsked != 4*32768 {
		t.Fatalf("instance profile ctx %d asked %d", in.prof.Ctx, in.prof.CtxAsked)
	}
	s := h.c.instStatus(in)
	if s.Production != "w" || s.Scope != "x4" || s.Ctx != 32768 || s.CtxNote != note {
		t.Fatalf("status: production %q scope %q ctx %d note %q", s.Production, s.Scope, s.Ctx, s.CtxNote)
	}
	if !strings.Contains(s.Cmd, "32768") || strings.Contains(s.Cmd, "131072") {
		t.Fatalf("launched with %q", s.Cmd)
	}
	again := h.scopedLoad(scoped4x) // the same settings: not "loaded with other settings"
	if a, ok := h.peek(again); ok && a.err != nil {
		t.Fatalf("the same scoped load again: %v", a.err)
	}
}

// With ctx_above_trained on the production profile the scoped copy runs at
// its own ctx, and the override names that ctx, not the production one.
func TestRealCtx_ScopedCtxAboveTrainedWithTheOverride(t *testing.T) {
	h := scopedTrainedHarness(t, 60000, func(p *core.Profile) { p.CtxAboveTrained = true })
	w := h.scopedLoad(scoped4x)
	if a, ok := h.peek(w); ok && a.err != nil {
		t.Fatalf("refused: %v", a.err)
	}
	in := h.c.insts["w@x4"]
	if in == nil || in.prof.Ctx != 4*32768 || in.prof.CtxAsked != 0 {
		t.Fatalf("instance %+v", in)
	}
	i := slices.Index(in.prof.ExtraArgs, "--override-kv")
	if i < 0 || i+1 >= len(in.prof.ExtraArgs) || !strings.HasSuffix(in.prof.ExtraArgs[i+1], ".context_length=int:131072") {
		t.Fatalf("extra args %v", in.prof.ExtraArgs)
	}
}
