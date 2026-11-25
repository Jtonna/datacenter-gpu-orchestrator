package gguf_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

func metaOf(t *testing.T, s ggufgen.Spec) *gguf.Meta {
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

const mibF = 1 << 20

func TestCheckpointCell_KnownFigures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spec      ggufgen.Spec
		layers    int
		wantMiB   string
		wantBytes int64
	}{
		{"qwen3.5-4b", ggufgen.Qwen35_4B(), 24, "50.25", 52690944},
		{"27b-listed", ggufgen.Hybrid27B(), 48, "149.62", 156893184},
	} {
		m := metaOf(t, tc.spec)
		if n := m.RecurrentLayers(); n != tc.layers {
			t.Errorf("%s: recurrent layers %d, want %d", tc.name, n, tc.layers)
		}
		b := m.RecurrentCellBytes()
		if b != tc.wantBytes || fmt.Sprintf("%.2f", float64(b)/mibF) != tc.wantMiB {
			t.Errorf("%s: cell %d bytes (%.3f MiB), want %d (%s MiB)", tc.name, b, float64(b)/mibF, tc.wantBytes, tc.wantMiB)
		}
		if m.Family() != gguf.FamilyRecurrent {
			t.Errorf("%s: family %v", tc.name, m.Family())
		}
	}
}

// A plain attention model keeps no checkpoint; a windowed one is windowed.
func TestCheckpointCell_PlainAndWindowed(t *testing.T) {
	plain := metaOf(t, ggufgen.Spec{Blocks: 32, Embd: 4096, Heads: 32, HeadsKV: 8})
	if plain.Family() != gguf.FamilyPlain || plain.RecurrentLayers() != 0 || plain.RecurrentCellBytes() != 0 {
		t.Fatalf("plain: %v %d %d", plain.Family(), plain.RecurrentLayers(), plain.RecurrentCellBytes())
	}
	swa := metaOf(t, ggufgen.Spec{Arch: "gemma3", Blocks: 48, Embd: 3840, Heads: 16, HeadsKV: 8, KeyLength: 256, ValueLength: 256, SlidingWindow: 1024})
	if swa.Family() != gguf.FamilyWindowed || swa.RecurrentCellBytes() != 0 {
		t.Fatalf("windowed: %v %d", swa.Family(), swa.RecurrentCellBytes())
	}
}
