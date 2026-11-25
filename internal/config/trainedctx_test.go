package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// The model server caps every slot at the model's trained context
// (llama.cpp tools/server/server-context.cpp n_ctx_slot). A write that sets
// a profile's ctx above it is refused, unless the profile says
// ctx_above_trained: true; a stored profile that keeps its ctx and model
// still loads (Options.KeepCtx), as a missing file does.
func trained(m map[string]int) func(string) (int, bool) {
	return func(path string) (int, bool) {
		v, ok := m[filepath.Base(path)]
		return v, ok
	}
}

func TestProfileCtxAboveTrained_RefusedOnWrite(t *testing.T) {
	tc := trained(map[string]int{"tiny.gguf": 2048})
	n := func(v int) *int { return &v }
	d := NewDocument()
	d.Profiles = map[string]*ProfileSpec{"t": {Model: "tiny.gguf", Ctx: n(8192)}}
	_, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, TrainedCtx: tc})
	if err == nil || !strings.Contains(err.Error(), "ctx 8,192 is above this model's trained context of 2,048") || !strings.Contains(err.Error(), "ctx_above_trained") {
		t.Fatalf("ctx above the trained context: %v, want refused naming ctx_above_trained", err)
	}
	// At or under: accepted.
	d.Profiles["t"].Ctx = n(2048)
	if _, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, TrainedCtx: tc}); err != nil {
		t.Fatalf("ctx at the trained context: %v", err)
	}
	// The profile means it.
	yes := true
	d.Profiles["t"] = &ProfileSpec{Model: "tiny.gguf", Ctx: n(8192), CtxAboveTrained: &yes}
	c, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, TrainedCtx: tc})
	if err != nil {
		t.Fatalf("ctx_above_trained: %v", err)
	}
	if p := c.Profiles["t"]; !p.CtxAboveTrained || p.Ctx != 8192 {
		t.Fatalf("core profile: %+v", p)
	}
	// Unknown trained context (header not readable): not refused.
	d.Profiles["t"] = &ProfileSpec{Model: "other.gguf", Ctx: n(8192)}
	if _, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, TrainedCtx: tc}); err != nil {
		t.Fatalf("unknown trained context: %v", err)
	}
}

func TestProfileCtxAboveTrained_StoredProfileKeepsLoading(t *testing.T) {
	tc := trained(map[string]int{"tiny.gguf": 2048})
	n := func(v int) *int { return &v }
	d := NewDocument()
	d.Profiles = map[string]*ProfileSpec{"bench-tinyllama-1a-8192": {Model: "tiny.gguf", Ctx: n(8192)}}
	keep := map[string]KeptCtx{"bench-tinyllama-1a-8192": {Model: "tiny.gguf", Ctx: 8192}}
	if _, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, TrainedCtx: tc, KeepCtx: keep}); err != nil {
		t.Fatalf("a stored profile that keeps its ctx: %v, want accepted", err)
	}
	// Raising it further is a new ctx: refused.
	d.Profiles["bench-tinyllama-1a-8192"].Ctx = n(16384)
	if _, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, TrainedCtx: tc, KeepCtx: keep}); err == nil {
		t.Fatal("a changed ctx above the trained context was accepted")
	}
	// A new profile with the same ctx: refused.
	d.Profiles["bench-tinyllama-1a-8192"].Ctx = n(8192)
	d.Profiles["new"] = &ProfileSpec{Model: "tiny.gguf", Ctx: n(8192)}
	if _, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, TrainedCtx: tc, KeepCtx: keep}); err == nil || !strings.Contains(err.Error(), `profile "new": ctx`) {
		t.Fatalf("a new profile above the trained context: %v, want refused", err)
	}
}
