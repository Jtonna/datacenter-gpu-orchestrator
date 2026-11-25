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
}

const (
	typU32 = 4
	typStr = 8
	typArr = 9
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

// Qwen 3.5 4B's shape: 32 blocks, qwen35 with no full_attention_interval
// key (the server's default 4: every fourth layer has attention, the other
// 24 are recurrent), ssm state 128, inner 4096, groups 16, conv 4. The
// server's log: "RS buffer size = 50.25 MiB (1 cells) R 2.25 MiB, S 48.00
// MiB", and each checkpoint 50.251 MiB.
func Qwen35_4B() Spec {
	return Spec{Arch: "qwen35", Blocks: 32, Embd: 2560, Heads: 16, HeadsKV: 4, KeyLength: 256, ValueLength: 256,
		SSMConv: 4, SSMInner: 4096, SSMState: 128, SSMGroups: 16}
}

// A 27B hybrid whose file lists the recurrent layers (head_count_kv 0 on
// each) for an architecture the server has no default interval for: 64
// blocks, 48 recurrent, ssm state 128, inner 6144, groups 16, conv 4. The
// server's log of 22 Nov: 149.62 MiB.
func Hybrid27B() Spec {
	kv := make([]uint32, 64)
	for i := range kv {
		if (i+1)%4 == 0 {
			kv[i] = 4
		}
	}
	return Spec{Arch: "qwen3x", Blocks: 64, Embd: 5120, Heads: 24, HeadsKVPerLayer: kv, KeyLength: 256, ValueLength: 256,
		SSMConv: 4, SSMInner: 6144, SSMState: 128, SSMGroups: 16}
}
