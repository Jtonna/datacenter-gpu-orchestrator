package gguf_test

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

// The need estimate's KV cache counts only the trunk's attention layers, as
// the pinned server allocates it (llama-model.cpp create_memory: the hybrid
// filter_attn and the plain cache's filter both take il < n_layer(), and
// n_layer() = block_count - nextn_predict_layers): an MTP block at the end
// keeps no KV cache, whatever the interval says.
func TestKVElemsPerToken_TrunkOnly(t *testing.T) {
	for _, c := range []struct {
		name   string
		spec   ggufgen.Spec
		layers int // attention layers with a KV cache
		per    int // K+V elements per layer: 2 x heads_kv x head size
	}{
		// Qwen 3.5 4B: 33 blocks (32 + 1 MTP), every 4th has attention:
		// il 3, 7, ..., 31 = 8; il 32 (the MTP block) is not on the interval
		// either: no change for this file
		{"qwen3.5-4b", ggufgen.Qwen35_4B(), 8, 2 * 4 * 256},
		// Qwen 3.8 27B: 65 blocks (64 + 1 MTP): il 3, ..., 63 = 16; no change
		{"qwen3.8-27b", ggufgen.Qwen38_27B(), 16, 2 * 4 * 256},
		// a qwen35 file whose MTP block falls on the interval: 32 blocks
		// (31 + 1 MTP); il 31 is no longer counted: 7, not 8
		{"qwen35, MTP on the interval", func() ggufgen.Spec { s := ggufgen.Qwen35_4B(); s.Blocks = 32; return s }(), 7, 2 * 4 * 256},
		// a plain (all-attention) model with one MTP block: 10 blocks, 9 cached
		{"plain with MTP", ggufgen.Spec{Arch: "glm4moe", Blocks: 10, NextN: 1, Embd: 1024, Heads: 8, HeadsKV: 2, KeyLength: 128, ValueLength: 128}, 9, 2 * 2 * 128},
		// without MTP nothing changes: every block
		{"plain", ggufgen.Spec{Arch: "llama", Blocks: 10, Embd: 1024, Heads: 8, HeadsKV: 2, KeyLength: 128, ValueLength: 128}, 10, 2 * 2 * 128},
	} {
		m := metaOf(t, c.spec)
		got, err := m.KVElemsPerToken()
		if err != nil {
			t.Fatal(err)
		}
		if want := c.layers * c.per; got != want {
			t.Errorf("%s: %d elements per token (%d layers), want %d (%d layers)", c.name, got, got/c.per, want, c.layers)
		}
	}
}
