package api

import (
	"context"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
)

// The benchmark runner's EnsureProfile keeps a stored stage, display_name
// and description when it replaces a generated profile whose settings
// differ, and writes stage test on a new one (slice B2).
func TestEnsureProfileKeepsTheMarks(t *testing.T) {
	e, svc, _ := cfgEnv(t)
	doc := svc.Document()
	p, err := cfgsvc.ParseProfile([]byte(`{"model":"a.gguf","ctx":4096,"generated":true,"stage":"production","display_name":"Alpha fast","description":"kept by people"}`))
	if err != nil {
		t.Fatal(err)
	}
	doc.Profiles["bench-a-auto-4k"] = p
	if _, err := svc.Import(context.Background(), cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: "t", Comment: "marks"}}, doc, true); err != nil {
		t.Fatal(err)
	}
	h := &bench.HTTPAPI{Base: e.front.URL}
	// other settings: replaced, the marks and the description kept
	if err := h.EnsureProfile(context.Background(), "bench-a-auto-4k", map[string]any{"model": "a.gguf", "ctx": 8192, "generated": true, "stage": "test"}); err != nil {
		t.Fatal(err)
	}
	q := svc.Config().Profiles["bench-a-auto-4k"]
	if q.Ctx != 8192 || q.Stage != "production" || q.DisplayName != "Alpha fast" || q.Description != "kept by people" {
		t.Fatalf("after the replace %+v", q)
	}
	// a new one is written with the spec's stage
	if err := h.EnsureProfile(context.Background(), "bench-a-auto-8k", bench.ProfileSpecOf(benchInputs("a.gguf", 8192), nil)); err != nil {
		t.Fatal(err)
	}
	if q := svc.Config().Profiles["bench-a-auto-8k"]; q == nil || q.Stage != "test" {
		t.Fatalf("new %+v", q)
	}
}

func benchInputs(model string, ctx int) benchspec.Inputs {
	return benchspec.Inputs{Model: model, Ctx: ctx}
}
