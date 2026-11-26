package cfgsvc

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// A write that sets a ctx above the model's trained context is refused
// unless the profile says ctx_above_trained; stored profiles above it (the
// 12 bench-tinyllama ones on live) still load after a restart and may be
// edited as long as they keep their model and ctx.
func TestCtxAboveTrained_StoredLoadsNewRefused(t *testing.T) {
	ctx := context.Background()
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	known := map[string]int{} // nothing known yet: the stored profiles are written as on live today
	tc := func(p string) (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		v, ok := known[filepath.Base(p)]
		return v, ok
	}
	opts := Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }, TrainedCtx: tc}}
	svc := New(ctx, opts)
	for _, id := range []string{"bench-tinyllama-1a-8192", "bench-tinyllama-1a-32768"} {
		n := 8192
		if strings.HasSuffix(id, "32768") {
			n = 32768
		}
		if _, err := svc.CreateProfile(ctx, ui("x"), id, llama("tiny.gguf", n)); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	known["tiny.gguf"] = 2048
	mu.Unlock()

	svc = New(ctx, opts) // a restart with the check on
	if e := svc.LoadError(); e != "" {
		t.Fatalf("stored profiles above the trained context: load error %s", e)
	}
	if p := svc.Config().Profiles["bench-tinyllama-1a-8192"]; p == nil || p.Ctx != 8192 {
		t.Fatalf("stored profile after the restart: %+v", p)
	}
	t.Run("an edit that keeps model and ctx passes", func(t *testing.T) {
		p := llama("tiny.gguf", 8192)
		p.Description = "edited"
		if _, err := svc.UpdateProfile(ctx, ui("x"), "bench-tinyllama-1a-8192", p); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a new profile above is refused", func(t *testing.T) {
		_, err := svc.CreateProfile(ctx, ui("x"), "bench-tinyllama-2-8192", llama("tiny.gguf", 8192))
		if err == nil || !strings.Contains(err.Error(), "above this model's trained context of 2,048") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("raising a stored ctx is refused", func(t *testing.T) {
		if _, err := svc.UpdateProfile(ctx, ui("x"), "bench-tinyllama-1a-8192", llama("tiny.gguf", 16384)); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("a profile that means it passes", func(t *testing.T) {
		p := llama("tiny.gguf", 8192)
		yes := true
		p.CtxAboveTrained = &yes
		if _, err := svc.CreateProfile(ctx, ui("x"), "means-it", p); err != nil {
			t.Fatal(err)
		}
	})
}
