// Package ggufgen writes tiny, valid GGUF headers for tests, so no binary
// fixtures are committed. The files hold metadata only (no tensors).
package ggufgen

import (
	"bytes"
	"encoding/binary"
	"os"
)

// Spec describes the model the header declares.
type Spec struct {
	Arch          string // general.architecture, default "llama"
	Blocks        uint32 // <arch>.block_count, default 4
	Embd          uint32 // <arch>.embedding_length, default 256
	Heads         uint32 // <arch>.attention.head_count, default 8
	HeadsKV       uint32 // <arch>.attention.head_count_kv, default 2
	Context       uint32 // <arch>.context_length, default 4096
	SlidingWindow uint32 // <arch>.attention.sliding_window; 0 leaves the key out
	KeyLength     uint32 // <arch>.attention.key_length; 0 leaves the key out
	ValueLength   uint32 // <arch>.attention.value_length; 0 leaves the key out
	// FullAttentionInterval is <arch>.full_attention_interval (hybrid
	// models: every n-th layer has attention, the others are recurrent); 0
	// leaves the key out.
	FullAttentionInterval uint32
	// HeadsKVPerLayer, when set, writes <arch>.attention.head_count_kv as a
	// per-layer array instead of HeadsKV (0 marks a layer without
	// attention: a recurrent layer of a hybrid model).
	HeadsKVPerLayer []uint32
	// The recurrent state keys (<arch>.ssm.*); 0 leaves a key out.
	SSMConv, SSMInner, SSMState, SSMGroups uint32
	// NextN is <arch>.nextn_predict_layers (MTP blocks at the end of the
	// block list); 0 leaves the key out.
	NextN uint32
	// RecurrentLayers, when set, writes <arch>.attention.recurrent_layers
	// (a bool per block); SWAPattern <arch>.attention.sliding_window_pattern.
	RecurrentLayers, SWAPattern []bool
	// SharedKVLayers is <arch>.attention.shared_kv_layers; KeyLengthSWA and
	// ValueLengthSWA <arch>.attention.key_length_swa / value_length_swa.
	SharedKVLayers, KeyLengthSWA, ValueLengthSWA uint32
}

const (
	typBool = 7
	typU32  = 4
	typStr  = 8
	typArr  = 9
)

func u32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }
func str(s string) []byte { return append(u64(uint64(len(s))), s...) }

// Bytes returns the GGUF file contents for s.
func Bytes(s Spec) []byte {
	def := func(v *uint32, d uint32) {
		if *v == 0 {
			*v = d
		}
	}
	if s.Arch == "" {
		s.Arch = "llama"
	}
	def(&s.Blocks, 4)
	def(&s.Embd, 256)
	def(&s.Heads, 8)
	def(&s.HeadsKV, 2)
	def(&s.Context, 4096)
	var kvs [][]byte
	add := func(key string, typ uint32, payload []byte) {
		kvs = append(kvs, append(append(str(key), u32(typ)...), payload...))
	}
	add("general.architecture", typStr, str(s.Arch))
	add(s.Arch+".block_count", typU32, u32(s.Blocks))
	add(s.Arch+".embedding_length", typU32, u32(s.Embd))
	add(s.Arch+".attention.head_count", typU32, u32(s.Heads))
	if len(s.HeadsKVPerLayer) > 0 {
		arr := append(u32(typU32), u64(uint64(len(s.HeadsKVPerLayer)))...)
		for _, v := range s.HeadsKVPerLayer {
			arr = append(arr, u32(v)...)
		}
		add(s.Arch+".attention.head_count_kv", typArr, arr)
	} else {
		add(s.Arch+".attention.head_count_kv", typU32, u32(s.HeadsKV))
	}
	add(s.Arch+".context_length", typU32, u32(s.Context))
	for _, o := range []struct {
		key string
		v   uint32
	}{{".attention.sliding_window", s.SlidingWindow}, {".attention.key_length", s.KeyLength},
		{".attention.value_length", s.ValueLength}, {".full_attention_interval", s.FullAttentionInterval},
		{".ssm.conv_kernel", s.SSMConv}, {".ssm.inner_size", s.SSMInner}, {".ssm.state_size", s.SSMState}, {".ssm.group_count", s.SSMGroups}} {
		if o.v > 0 {
			add(s.Arch+o.key, typU32, u32(o.v))
		}
	}
	for _, o := range []struct {
		key string
		v   []bool
	}{{".attention.recurrent_layers", s.RecurrentLayers}, {".attention.sliding_window_pattern", s.SWAPattern}} {
		if len(o.v) == 0 {
			continue
		}
		arr := append(u32(typBool), u64(uint64(len(o.v)))...)
		for _, x := range o.v {
			if x {
				arr = append(arr, 1)
			} else {
				arr = append(arr, 0)
			}
		}
		add(s.Arch+o.key, typArr, arr)
	}
	for _, o := range []struct {
		key string
		v   uint32
	}{{".nextn_predict_layers", s.NextN}, {".attention.shared_kv_layers", s.SharedKVLayers},
		{".attention.key_length_swa", s.KeyLengthSWA}, {".attention.value_length_swa", s.ValueLengthSWA}} {
		if o.v > 0 {
			add(s.Arch+o.key, typU32, u32(o.v))
		}
	}
	var b bytes.Buffer
	b.WriteString("GGUF")
	b.Write(u32(3))
	b.Write(u64(0))
	b.Write(u64(uint64(len(kvs))))
	for _, kv := range kvs {
		b.Write(kv)
	}
	return b.Bytes()
}

// Write writes the file at path.
func Write(path string, s Spec) error { return os.WriteFile(path, Bytes(s), 0o644) }

// Qwen 3.5 4B's shape as the real file has it: 33 blocks, the last an MTP
// block (nextn_predict_layers 1), qwen35 with no full_attention_interval
// key (the server's default 4). The server makes blocks i < 32 with
// (i+1) % 4 != 0 recurrent: 24. ssm state 128, inner 4096, groups 16,
// conv 4. The server's log: "llama_memory_recurrent: size = 50.25 MiB (1
// cells, 32 layers, 1 seqs), R (f32): 2.25 MiB, S (f32): 48.00 MiB", and
// each checkpoint 50.251 MiB.
func Qwen35_4B() Spec {
	return Spec{Arch: "qwen35", Blocks: 33, NextN: 1, Embd: 2560, Heads: 16, HeadsKV: 4, KeyLength: 256, ValueLength: 256,
		SSMConv: 4, SSMInner: 4096, SSMState: 128, SSMGroups: 16}
}

// Qwen 3.8 27B's shape: 65 blocks, the last an MTP block, qwen35 rules:
// 48 recurrent, ssm state 128, inner 6144, groups 16, conv 4. The server's
// log of 22 Nov: 149.62 MiB.
func Qwen38_27B() Spec {
	return Spec{Arch: "qwen35", Blocks: 65, NextN: 1, Embd: 5120, Heads: 24, HeadsKV: 4, KeyLength: 256, ValueLength: 256,
		SSMConv: 4, SSMInner: 6144, SSMState: 128, SSMGroups: 16}
}

// RecurrentListed27B is the 27B's recurrent shape in a file that lists the
// recurrent layers (<arch>.attention.recurrent_layers) for an architecture
// with no default interval: 64 blocks, 48 recurrent.
func RecurrentListed27B() Spec {
	rec := make([]bool, 64)
	for i := range rec {
		rec[i] = (i+1)%4 != 0
	}
	return Spec{Arch: "qwen3x", Blocks: 64, Embd: 5120, Heads: 24, HeadsKV: 4, KeyLength: 256, ValueLength: 256, RecurrentLayers: rec,
		SSMConv: 4, SSMInner: 6144, SSMState: 128, SSMGroups: 16}
}

// Gemma4Like is a small windowed model in the gemma4 layout: 12 blocks, a
// listed window pattern (5 window layers, then 1 full), the last 2 blocks
// reading another layer's cache (shared_kv_layers 2), window layers with
// head size 128, full layers 256, 4 KV heads, window 512.
func Gemma4Like() Spec {
	pat := make([]bool, 12)
	for i := range pat {
		pat[i] = i%6 < 5
	}
	return Spec{Arch: "gemma4", Blocks: 12, Embd: 1024, Heads: 8, HeadsKV: 4, KeyLength: 256, ValueLength: 256,
		KeyLengthSWA: 128, ValueLengthSWA: 128, SlidingWindow: 512, SWAPattern: pat, SharedKVLayers: 2}
}

// Gemma4_12B is the header of the real Gemma 4 12B file (gemma-4-12b-it,
// Q4_K_M): every key the checkpoint size reads, with the file's values.
// 48 blocks; the pattern is 5 window layers then 1 full, repeated (40
// window layers); head_count_kv 8 on window layers and 1 on the others;
// window K and V head size 256 (512 on the full layers); window 1024;
// shared_kv_layers 0; trained context 262,144.
func Gemma4_12B() Spec {
	pat := make([]bool, 48)
	kv := make([]uint32, 48)
	for i := range pat {
		pat[i] = i%6 < 5
		kv[i] = 1
		if pat[i] {
			kv[i] = 8
		}
	}
	return Spec{Arch: "gemma4", Blocks: 48, Embd: 3840, Heads: 16, HeadsKVPerLayer: kv, Context: 262144,
		KeyLength: 512, ValueLength: 512, KeyLengthSWA: 256, ValueLengthSWA: 256,
		SlidingWindow: 1024, SWAPattern: pat}
}
