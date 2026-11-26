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

// The two figures the model server logged on real hardware, from fixtures
// shaped like the real files: the trunk has 32 (64) blocks plus one MTP
// (nextn) block, which the server never makes recurrent
// (llama_hparams::n_layer() = block_count - nextn_predict_layers; qwen35
// load_arch_hparams marks i < n_layer() && (i+1) % interval != 0; the
// hybrid memory's filter_recr takes il < n_layer() only).
//
//	Qwen 3.5 4B:  "llama_memory_recurrent: size = 50.25 MiB (1 cells, 32 layers ...)"
//	Qwen 3.8 27B: 149.62 MiB (the log of 22 Nov)
func TestCheckpointCell_KnownFigures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spec      ggufgen.Spec
		layers    int
		wantMiB   string
		wantBytes int64
	}{
		{"qwen3.5-4b (33 blocks, 1 nextn)", ggufgen.Qwen35_4B(), 24, "50.25", 52690944},
		{"qwen3.8-27b (65 blocks, 1 nextn)", ggufgen.Qwen38_27B(), 48, "149.62", 156893184},
		{"a file listing the recurrent layers", ggufgen.RecurrentListed27B(), 48, "149.62", 156893184},
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

// The window layers' K+V per token, as the server sizes the window cache
// (llama_hparams is_swa, n_embd_k_gqa(il) with key_length_swa on a window
// layer, layers that share another's cache own none):
//   - gemma3, pattern from the server's default 6 (il % 6 < 5): 40 of 48
//     layers, 8 KV heads x (256 + 256) = 163,840 elements per token;
//   - a file listing the pattern, with its own window head size and shared
//     layers; the listed layers count, the shared ones do not;
//   - a windowed architecture whose rule the server keeps in code this
//     reader does not know: unknown (no figure).
func TestWindowKVElemsPerToken(t *testing.T) {
	g3 := metaOf(t, ggufgen.Spec{Arch: "gemma3", Blocks: 48, Embd: 3840, Heads: 16, HeadsKV: 8, KeyLength: 256, ValueLength: 256, SlidingWindow: 1024})
	if n, ok := g3.WindowKVElemsPerToken(); !ok || n != 40*8*512 {
		t.Errorf("gemma3: %d %v, want %d", n, ok, 40*8*512)
	}
	g4 := metaOf(t, ggufgen.Gemma4Like())
	// 12 layers, pattern 5 window + 1 full, the last 2 share another's
	// cache: window layers owning a cache are 0-4 and 6-9 = 9, each 4 KV
	// heads x (128 + 128)
	if n, ok := g4.WindowKVElemsPerToken(); !ok || n != 9*4*256 {
		t.Errorf("gemma4-like: %d %v, want %d", n, ok, 9*4*256)
	}
	other := metaOf(t, ggufgen.Spec{Arch: "someswa", Blocks: 8, Embd: 512, Heads: 8, HeadsKV: 2, SlidingWindow: 512})
	if _, ok := other.WindowKVElemsPerToken(); ok || other.Family() != gguf.FamilyWindowed {
		t.Errorf("unknown windowed rule: %v %v", ok, other.Family())
	}
}
