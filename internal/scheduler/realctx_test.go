package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// trainedNeeds is the harness's need function plus the trained contexts of
// its model files (as ConfigNeeds reads them from the GGUF headers).
type trainedNeeds struct {
	NeedFunc
	trained map[string]int
}

func (t trainedNeeds) TrainedCtx(model string) (int, string, bool) {
	v, ok := t.trained[model]
	return v, "llama", ok
}

// A profile whose ctx is above its model's trained context is planned and
// launched at the trained context (the model server caps it there anyway),
// and the plan says so. ctx_above_trained keeps the ctx and adds the
// override that lifts the server's cap.
func TestRealCtx_PlannedAndLaunchedAtTheTrainedContext(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000))
	h.c.needs = trainedNeeds{NeedFunc: NeedFunc(func(p *core.Profile) (NeedInfo, error) {
		return NeedInfo{Need: placer.Measured(500), FileBytes: 1 << 29}, nil
	}), trained: map[string]int{"tiny.gguf": 2048}}
	h.llama("tiny-8192", 500, func(p *core.Profile) { p.Model, p.Ctx = "tiny.gguf", 8192 })
	h.llama("tiny-2048", 500, func(p *core.Profile) { p.Model, p.Ctx = "tiny.gguf", 2048 })
	h.llama("tiny-means-it", 500, func(p *core.Profile) { p.Model, p.Ctx, p.CtxAboveTrained = "tiny.gguf", 8192, true })
	h.c.applyConfig(h.cfg)

	p := h.c.cfg.Profiles["tiny-8192"]
	if p.Ctx != 2048 || p.CtxAsked != 8192 {
		t.Fatalf("planned profile: ctx %d asked %d", p.Ctx, p.CtxAsked)
	}
	if h.cfg.Profiles["tiny-8192"].Ctx != 8192 {
		t.Fatal("the configuration itself was changed")
	}
	r := h.c.dryPlanOf("tiny-8192", p)
	if r.Ctx != 2048 || r.CtxNote != "ctx 8,192 is above this model's trained context of 2,048; the server runs it at 2,048" {
		t.Fatalf("plan: ctx %d note %q", r.Ctx, r.CtxNote)
	}
	if r := h.c.dryPlanOf("tiny-2048", h.c.cfg.Profiles["tiny-2048"]); r.Ctx != 2048 || r.CtxNote != "" {
		t.Fatalf("plan at the trained context: %d %q", r.Ctx, r.CtxNote)
	}
	m := h.c.cfg.Profiles["tiny-means-it"]
	if m.Ctx != 8192 || m.CtxAsked != 0 || strings.Join(m.ExtraArgs, " ") != "--override-kv llama.context_length=int:8192" {
		t.Fatalf("ctx_above_trained: %+v", m)
	}

	h.load("tiny-8192")
	h.drain()
	var argv string
	for _, ch := range h.children {
		argv = strings.Join(ch.argv, " ")
	}
	if !strings.Contains(argv, "--ctx-size 2048 ") {
		t.Fatalf("launched with: %s", argv)
	}
	// A configuration write that changes nothing keeps the running instance current.
	h.c.applyConfig(h.cfg)
	for _, in := range h.c.insts {
		if h.c.staleProfile(in) {
			t.Fatal("the running instance became stale after a write that changed nothing")
		}
	}
}
