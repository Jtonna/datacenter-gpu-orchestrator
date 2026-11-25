package estimate

import (
	"fmt"
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
	q := Checkpoint(meta(t, ggufgen.Qwen35_4B()), "f16", 8192)
	if q.Family != gguf.FamilyRecurrent || q.Estimate || q.Unknown || q.CellBytes != 52690944 || fmt.Sprintf("%.2f", q.CellMiB()) != "50.25" {
		t.Fatalf("4B %+v", q)
	}
	if got := q.TotalMiB(32); got != 1608 { // 32 x 50.25
		t.Fatalf("4B x 32 = %d", got)
	}
	if got := q.TotalMiB(0); got != 0 {
		t.Fatalf("x 0 = %d", got)
	}
	big := Checkpoint(meta(t, ggufgen.Qwen38_27B()), "f16", 8192)
	if fmt.Sprintf("%.2f", big.CellMiB()) != "149.62" || big.TotalMiB(32) != 4788 { // 32 x 149.625 = 4788
		t.Fatalf("27B %.4f x 32 = %d", big.CellMiB(), big.TotalMiB(32))
	}
	plain := Checkpoint(meta(t, ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8}), "f16", 8192)
	if plain.Family != gguf.FamilyPlain || plain.CellBytes != 0 || plain.TotalMiB(32) != 0 {
		t.Fatalf("plain %+v", plain)
	}
}

// A window model has no exact rule proven against a server log: its size
// is unknown (no number), whatever its header. The header arithmetic is
// kept apart (BoundBytes: min(window, ctx) x the window layers' K+V) only
// to be compared with the first load's log line; it is never shown.
//
//	gemma3 shape, window 1024: 1024 x 163,840 elements x 2 bytes (f16) = 320 MiB
//	an architecture whose window rule is not known here: no bound either
func TestCheckpoint_WindowedIsUnknown(t *testing.T) {
	g3 := ggufgen.Spec{Arch: "gemma3", Blocks: 48, Embd: 3840, Heads: 16, HeadsKV: 8, KeyLength: 256, ValueLength: 256, SlidingWindow: 1024}
	w := Checkpoint(meta(t, g3), "f16", 8192)
	if w.Family != gguf.FamilyWindowed || !w.Unknown || w.CellBytes != 0 || w.TotalMiB(32) != 0 || w.BoundBytes != 1024*163840*2 {
		t.Fatalf("windowed %+v", w)
	}
	if short := Checkpoint(meta(t, g3), "f16", 512); short.BoundBytes != 512*163840*2 {
		t.Fatalf("ctx 512: %d", short.BoundBytes)
	}
	u := Checkpoint(meta(t, ggufgen.Spec{Arch: "someswa", Blocks: 8, Embd: 512, Heads: 8, HeadsKV: 2, SlidingWindow: 512}), "f16", 8192)
	if !u.Unknown || u.CellBytes != 0 || u.BoundBytes != 0 {
		t.Fatalf("unknown rule %+v", u)
	}
}

// A recurrent architecture the code has no exact rule for (a made-up one
// with the same ssm keys as Qwen 3.5) is unknown: no multiplied guess.
func TestCheckpoint_RecurrentWithoutAnExactRuleIsUnknown(t *testing.T) {
	s := ggufgen.Qwen35_4B()
	s.Arch, s.FullAttentionInterval = "madeuphybrid", 4
	c := Checkpoint(meta(t, s), "f16", 8192)
	if c.Family != gguf.FamilyRecurrent || !c.Unknown || c.CellBytes != 0 || c.TotalMiB(32) != 0 {
		t.Fatalf("%+v", c)
	}
}
