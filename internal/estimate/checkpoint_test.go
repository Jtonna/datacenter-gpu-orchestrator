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

// Gemma 4 12B (the real file's header keys): one checkpoint is at most the
// window's 1,024 cells x the 40 window layers' K+V, proven against the
// server's log ("created context checkpoint ... size = 320.013 MiB" at
// every context from 2,048 to 131,072):
//
//	f16:  40 x 8 x (256 + 256) x 2 bytes x 1,024 = 335,544,320 bytes = 320.00 MiB
//	q8_0: the same elements at 17/16 bytes (34 per 32) = 178,257,920 bytes = 170.00 MiB
//
// Below the window, the context per slot bounds it (ctx 512: 160.00 MiB).
func TestCheckpoint_Gemma4_12BIsTheProvenBound(t *testing.T) {
	m := meta(t, ggufgen.Gemma4_12B())
	c := Checkpoint(m, "f16", 8192)
	if c.Family != gguf.FamilyWindowed || c.Unknown || !c.Estimate || c.CellBytes != 335544320 || fmt.Sprintf("%.2f", c.CellMiB()) != "320.00" {
		t.Fatalf("f16 %+v (%.3f MiB)", c, c.CellMiB())
	}
	if c.TotalMiB(32) != 10240 {
		t.Fatalf("32 x: %d", c.TotalMiB(32))
	}
	q := Checkpoint(m, "q8_0", 8192)
	if q.CellBytes != 178257920 || fmt.Sprintf("%.2f", q.CellMiB()) != "170.00" {
		t.Fatalf("q8_0 %+v (%.3f MiB)", q, q.CellMiB())
	}
	if s := Checkpoint(m, "f16", 512); s.CellBytes != 335544320/2 {
		t.Fatalf("ctx 512: %d", s.CellBytes)
	}
	// within 1% of the logged 320.013 MiB
	if d := 100 * (320.013 - c.CellMiB()) / c.CellMiB(); d < 0 || d > 1 {
		t.Fatalf("%.4f%% from the log", d)
	}
	// the same file without the pattern: no number
	s := ggufgen.Gemma4_12B()
	s.SWAPattern = nil
	if u := Checkpoint(meta(t, s), "f16", 8192); !u.Unknown || u.CellBytes != 0 {
		t.Fatalf("no pattern: %+v", u)
	}
}

// A window architecture other than gemma4 has no rule proven against a server log: its size
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

// A profile capped at its trained context (the real context) changes no
// checkpoint figure: a recurrent cell does not depend on the context, and a
// window model is unknown at any context.
func TestCheckpoint_DoesNotDependOnCtx(t *testing.T) {
	q := meta(t, ggufgen.Qwen35_4B())
	if a, b := Checkpoint(q, "f16", 2048), Checkpoint(q, "f16", 262144); a.CellBytes != b.CellBytes || a.CellBytes == 0 || a.Unknown {
		t.Fatalf("recurrent: %+v / %+v", a, b)
	}
	g3 := meta(t, ggufgen.Spec{Arch: "gemma3", Blocks: 48, Embd: 3840, Heads: 16, HeadsKV: 8, KeyLength: 256, ValueLength: 256, SlidingWindow: 1024})
	if a, b := Checkpoint(g3, "f16", 2048), Checkpoint(g3, "f16", 131072); !a.Unknown || !b.Unknown || a.CellBytes != 0 || b.CellBytes != 0 {
		t.Fatalf("windowed: %+v / %+v", a, b)
	}
}
