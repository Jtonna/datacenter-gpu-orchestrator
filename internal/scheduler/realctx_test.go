package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
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

// After the load the scheduler reads the server's own context per slot. A
// figure other than the planned one is a warn event, shown on the instance
// ("runs at 2,048, profile says 8,192"), and requests carry the real figure
// for the request log. A read that fails is not a load failure: the
// instance says "context not confirmed".
func TestRealCtx_ServedContextIsReadAndCompared(t *testing.T) {
	h := newHarness(t).nodes(hostNode(7000), ps5("ps5-a", 1, 12000), ps5("ps5-b", 2, 12000))
	h.llama("big", 500, func(p *core.Profile) { p.Ctx = 8192 })
	h.llama("same", 500, func(p *core.Profile) { p.Ctx = 4000 })
	h.llama("unread", 500)

	g := h.ready("big")
	if g.Ctx != 8192 {
		t.Fatalf("before the read the grant carries the planned ctx: %d", g.Ctx)
	}
	in := h.inst("big")
	h.step(evServedCtx{iid: in.iid, ctx: 2048})
	if r := h.c.dryPlanOf("big", h.c.cfg.Profiles["big"]); r.CtxNote != "runs at 2,048, profile says 8,192" {
		t.Fatalf("plan note %q", r.CtxNote)
	}
	st := h.c.instStatus(in)
	if st.Ctx != 8192 || st.CtxServed == nil || *st.CtxServed != 2048 || st.CtxNote != "runs at 2,048, profile says 8,192" {
		t.Fatalf("instance status: ctx %d served %v note %q", st.Ctx, st.CtxServed, st.CtxNote)
	}
	var ev *history.Event
	for i := range h.histLog {
		if h.histLog[i].Type == history.TContextMismatch {
			ev = &h.histLog[i]
		}
	}
	if ev == nil || ev.Severity != history.Warn || ev.Data["served_ctx"] != 2048 || !strings.Contains(ev.Summary, "runs at 2,048, profile says 8,192") {
		t.Fatalf("event: %+v", ev)
	}
	h.release(g)
	g2 := h.granted(h.request("big"))
	if g2.Ctx != 2048 {
		t.Fatalf("a request after the read carries %d, want the server's 2,048", g2.Ctx)
	}
	h.release(g2)

	// The server pads the slot to 256 tokens: 4,096 for a planned 4,000 is the same context.
	h.ready("same")
	si := h.inst("same")
	n := len(h.histLog)
	h.step(evServedCtx{iid: si.iid, ctx: 4096})
	if st := h.c.instStatus(si); st.CtxNote != "" || len(h.histLog) != n {
		t.Fatalf("padding taken for a mismatch: %q, events %d -> %d", st.CtxNote, n, len(h.histLog))
	}

	h.ready("unread")
	ui := h.inst("unread")
	h.step(evServedCtx{iid: ui.iid, err: errors.New("/props answered 404")})
	if st := h.c.instStatus(ui); st.CtxServed != nil || st.CtxNote != "context not confirmed: /props answered 404" || st.State != "ready" {
		t.Fatalf("unread: %+v", st)
	}
}

// The runner reads /props once the load is healthy, at the instance's own
// address with its key, and the next requests carry the figure.
func TestRealCtx_RunnerReadsPropsAfterTheLoad(t *testing.T) {
	clk := newFakeClock()
	view := newFakeView(clk)
	view.set(ps5("a", 2, 15000))
	cfg := testConfig()
	cfg.Profiles["big"] = llamaP("big", 4000, func(p *core.Profile) { p.Ctx = 8192 })
	var mu sync.Mutex
	var addrs []string
	s, err := New(cfg, Options{Clock: clk, View: view, Prober: newFakeProber(view), WorkerProber: newFakeWorkerProber(),
		Launcher: &fakeLauncher{nextPort: 5800, autoHealthy: true, autoExit: true},
		Needs:    NeedFunc(func(p *core.Profile) (NeedInfo, error) { return NeedInfo{Need: placer.Measured(p.NeedsMiB)}, nil }),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadSlotCtx: func(_ context.Context, addr, key string) (int, error) {
			mu.Lock()
			addrs = append(addrs, addr)
			mu.Unlock()
			return 2048, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	go s.Run(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	l, err := s.Acquire(ctxT(t), "big")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := s.Status(ctxT(t))
		if len(st.Instances) == 1 && st.Instances[0].CtxServed != nil {
			if *st.Instances[0].CtxServed != 2048 || st.Instances[0].CtxNote != "runs at 2,048, profile says 8,192" {
				t.Fatalf("instance: %+v", st.Instances[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the served context was never read")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	if len(addrs) != 1 || addrs[0] != l.Addr {
		t.Fatalf("read at %v, the instance is at %s", addrs, l.Addr)
	}
	mu.Unlock()
	l2, err := s.Acquire(ctxT(t), "big")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()
	if l2.Ctx != 2048 {
		t.Fatalf("lease ctx %d", l2.Ctx)
	}
}
