package gguf_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

func TestContextCache_TrainedCtx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := ggufgen.Write(p, ggufgen.Spec{Context: 2048}); err != nil {
		t.Fatal(err)
	}
	var c gguf.ContextCache
	if v, ok := c.TrainedCtx(p); !ok || v != 2048 {
		t.Fatalf("got %d %v", v, ok)
	}
	// A new version of the file is read again.
	if err := ggufgen.Write(p, ggufgen.Spec{Context: 131072, Blocks: 8}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(p, later, later)
	if v, ok := c.TrainedCtx(p); !ok || v != 131072 {
		t.Fatalf("after a change: %d %v", v, ok)
	}
	if _, ok := c.TrainedCtx(filepath.Join(t.TempDir(), "none.gguf")); ok {
		t.Fatal("a missing file has a trained context")
	}
}
