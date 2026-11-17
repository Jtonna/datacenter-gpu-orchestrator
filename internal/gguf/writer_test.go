package gguf

import (
	"bytes"
	"encoding/binary"
)

// fx is a test-only GGUF writer.
type fx struct {
	version uint32
	kvs     [][]byte
	tensors uint64
	magic   string
}

func newFx() *fx { return &fx{version: 3, magic: "GGUF"} }

func le32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
func le64(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }
func gstr(s string) []byte { return append(le64(uint64(len(s))), s...) }

func (f *fx) add(key string, typ uint32, payload []byte) *fx {
	var b bytes.Buffer
	b.Write(gstr(key))
	b.Write(le32(typ))
	b.Write(payload)
	f.kvs = append(f.kvs, b.Bytes())
	return f
}
func (f *fx) u32(k string, v uint32) *fx { return f.add(k, tU32, le32(v)) }
func (f *fx) i32(k string, v int32) *fx  { return f.add(k, tI32, le32(uint32(v))) }
func (f *fx) str(k, v string) *fx        { return f.add(k, tStr, gstr(v)) }
func (f *fx) f32(k string) *fx           { return f.add(k, tF32, le32(0x3f800000)) }
func (f *fx) arrU32(k string, vs ...uint32) *fx {
	p := append(le32(tU32), le64(uint64(len(vs)))...)
	for _, v := range vs {
		p = append(p, le32(v)...)
	}
	return f.add(k, tArr, p)
}
func (f *fx) arrStr(k string, vs ...string) *fx {
	p := append(le32(tStr), le64(uint64(len(vs)))...)
	for _, v := range vs {
		p = append(p, gstr(v)...)
	}
	return f.add(k, tArr, p)
}

// rawArr writes an array header claiming n elements of type et, followed by body.
func (f *fx) rawArr(k string, et uint32, n uint64, body []byte) *fx {
	p := append(le32(et), le64(n)...)
	return f.add(k, tArr, append(p, body...))
}

// bytesHeader returns a header claiming n kvs and no kv bytes.
func (f *fx) bytesHeader(n uint64) []byte {
	b := append([]byte(f.magic), le32(f.version)...)
	b = append(b, le64(0)...)
	return append(b, le64(n)...)
}

func (f *fx) bytes() []byte {
	var b bytes.Buffer
	b.WriteString(f.magic)
	b.Write(le32(f.version))
	b.Write(le64(f.tensors))
	b.Write(le64(uint64(len(f.kvs))))
	for _, kv := range f.kvs {
		b.Write(kv)
	}
	return b.Bytes()
}

// llama builds a plain llama-style fixture.
func llama() *fx {
	f := newFx()
	f.tensors = 291
	return f.str("general.architecture", "llama").
		u32("llama.block_count", 32).
		u32("llama.embedding_length", 4096).
		u32("llama.attention.head_count", 32).
		u32("llama.attention.head_count_kv", 8).
		u32("llama.context_length", 8192)
}

func deepNest() []byte {
	var body []byte
	for i := 0; i < 100; i++ {
		body = append(body, le32(tArr)...)
		body = append(body, le64(1)...)
	}
	return newFx().rawArr("a", tArr, 1, body).bytes()
}
