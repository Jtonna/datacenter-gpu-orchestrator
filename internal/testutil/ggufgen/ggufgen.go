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
}

const (
	typU32 = 4
	typStr = 8
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
	add(s.Arch+".attention.head_count_kv", typU32, u32(s.HeadsKV))
	add(s.Arch+".context_length", typU32, u32(s.Context))
	if s.SlidingWindow > 0 {
		add(s.Arch+".attention.sliding_window", typU32, u32(s.SlidingWindow))
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
