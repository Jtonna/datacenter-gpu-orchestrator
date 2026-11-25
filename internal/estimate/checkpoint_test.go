package estimate

import (
	"path/filepath"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

func meta(t *testing.T, s ggufgen.Spec) *gguf.Meta {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := ggufgen.Write(p, s); err != nil {
		t.Fatal(err)
	}
	m, err := gguf.ReadMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCheckpoint_PerFamily(t *testing.T) {
	q := Checkpoint(meta(t, ggufgen.Qwen35_4B()), "f16")
	if q.Family != gguf.FamilyRecurrent || q.Estimate || q.CellBytes != 52690944 || q.CellMiB() != 50.25 {
		t.Fatalf("4B %+v", q)
	}
	if got := q.TotalMiB(32); got != 1608 { // 32 x 50.25
		t.Fatalf("4B x 32 = %d", got)
	}
	if got := q.TotalMiB(0); got != 0 {
		t.Fatalf("x 0 = %d", got)
	}
	big := Checkpoint(meta(t, ggufgen.Hybrid27B()), "f16")
	if big.TotalMiB(32) != 4788 { // 32 x 149.625 = 4788
		t.Fatalf("27B x 32 = %d", big.TotalMiB(32))
	}
	plain := Checkpoint(meta(t, ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8}), "f16")
	if plain.Family != gguf.FamilyPlain || plain.CellBytes != 0 || plain.TotalMiB(32) != 0 {
		t.Fatalf("plain %+v", plain)
	}
	// windowed: (1024 + 512) cells, padded to 256 = 1536, x (K+V of every
	// KV layer: 48 x 2 x 8 x 256 f16) = 1536 x 393216 bytes = 576 MiB, an
	// upper bound (the window layers only, really), marked an estimate
	w := Checkpoint(meta(t, ggufgen.Spec{Arch: "gemma3", Blocks: 48, Embd: 3840, Heads: 16, HeadsKV: 8, KeyLength: 256, ValueLength: 256, SlidingWindow: 1024}), "f16")
	if w.Family != gguf.FamilyWindowed || !w.Estimate || w.CellBytes != 1536*393216 {
		t.Fatalf("windowed %+v", w)
	}
	if q8 := Checkpoint(meta(t, ggufgen.Spec{Arch: "gemma3", Blocks: 48, Embd: 3840, Heads: 16, HeadsKV: 8, KeyLength: 256, ValueLength: 256, SlidingWindow: 1024}), "q8_0"); q8.CellBytes >= w.CellBytes {
		t.Fatalf("q8_0 window %d not below f16 %d", q8.CellBytes, w.CellBytes)
	}
}
