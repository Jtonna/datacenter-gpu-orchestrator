package gguf

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadValid(t *testing.T) {
	f := llama()
	f.arrStr("tokenizer.ggml.tokens", "a", "bb", "ccc").
		f32("llama.rope.freq_base").
		arrU32("tokenizer.ggml.token_type", 1, 2, 3)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Architecture != "llama" || m.BlockCount != 32 || m.EmbeddingLength != 4096 ||
		m.ContextLength != 8192 || m.Version != 3 || m.TensorCount != 291 || m.SlidingWindow {
		t.Fatalf("bad meta: %+v", m)
	}
	if len(m.HeadCount) != 32 || m.HeadCount[0] != 32 || len(m.HeadCountKV) != 32 || m.HeadCountKV[31] != 8 {
		t.Fatalf("bad heads: %v %v", m.HeadCount, m.HeadCountKV)
	}
	// key/value length absent: n_embd / n_head = 128; kv heads 8 => 1024.
	k, err := m.NEmbdKGqa(0)
	if err != nil || k != 1024 {
		t.Fatalf("k=%d err=%v", k, err)
	}
	v, err := m.NEmbdVGqa(31)
	if err != nil || v != 1024 {
		t.Fatalf("v=%d err=%v", v, err)
	}
	if x, ok := m.Scalar("llama.context_length"); !ok || x.(uint64) != 8192 {
		t.Fatalf("scalar: %v %v", x, ok)
	}
	if _, ok := m.Scalar("tokenizer.ggml.tokens"); ok {
		t.Fatal("array reported as scalar")
	}
	if _, ok := m.Scalar("general.architecture"); !ok {
		t.Fatal("string scalar missing")
	}
}

func TestV2(t *testing.T) {
	f := llama()
	f.version = 2
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil || m.Version != 2 {
		t.Fatalf("%v %+v", err, m)
	}
}

func TestKeyValueLengthAndSliding(t *testing.T) {
	f := newFx().str("general.architecture", "gemma3").
		u32("gemma3.block_count", 4).
		u32("gemma3.embedding_length", 2560).
		u32("gemma3.attention.head_count", 8).
		u32("gemma3.attention.head_count_kv", 4).
		u32("gemma3.attention.key_length", 256).
		u32("gemma3.attention.value_length", 128).
		u32("gemma3.attention.sliding_window", 1024)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !m.SlidingWindow || m.KeyLength != 256 || m.ValueLength != 128 {
		t.Fatalf("%+v", m)
	}
	if k, _ := m.NEmbdKGqa(0); k != 1024 {
		t.Fatalf("k=%d", k)
	}
	if v, _ := m.NEmbdVGqa(0); v != 512 {
		t.Fatalf("v=%d", v)
	}
	if n, err := m.KVElemsPerToken(); err != nil || n != 4*(1024+512) {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestSlidingWindowZeroIsFalse(t *testing.T) {
	f := llama().u32("llama.attention.sliding_window", 0)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil || m.SlidingWindow {
		t.Fatalf("%v %+v", err, m)
	}
}

func TestPerLayerArrays(t *testing.T) {
	f := newFx().str("general.architecture", "x").
		u32("x.block_count", 3).
		u32("x.embedding_length", 1024).
		arrU32("x.attention.head_count", 8, 8, 16).
		arrU32("x.attention.head_count_kv", 0, 2, 4)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// head size = n_embd / n_head(0) = 128, as llama-model.cpp:1601.
	want := []int{0, 2 * 128, 4 * 128}
	for il, w := range want {
		k, err := m.NEmbdKGqa(il)
		if err != nil || k != w {
			t.Fatalf("il=%d k=%d err=%v want %d", il, k, err, w)
		}
	}
	if n, _ := m.KVElemsPerToken(); n != 2*(0+256+512) {
		t.Fatalf("n=%d", n)
	}
	if _, err := m.NEmbdKGqa(3); err == nil {
		t.Fatal("expected out of range error")
	}
}

func TestSignedInt32Heads(t *testing.T) {
	f := newFx().str("general.architecture", "x").
		u32("x.block_count", 2).u32("x.embedding_length", 64).
		i32("x.attention.head_count", 4).i32("x.attention.head_count_kv", 2)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := m.NEmbdKGqa(0); k != 32 {
		t.Fatalf("k=%d", k)
	}
}

func TestHeadCountKVDefaultsToHeadCount(t *testing.T) {
	f := newFx().str("general.architecture", "x").
		u32("x.block_count", 2).u32("x.embedding_length", 64).
		u32("x.attention.head_count", 4)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// llama-model.cpp:1557: n_head_kv defaults to n_head.
	if k, _ := m.NEmbdKGqa(1); k != 64 {
		t.Fatalf("k=%d", k)
	}
}

func TestArchKeyAfterOthers(t *testing.T) {
	f := newFx().u32("x.block_count", 2).u32("x.embedding_length", 64).
		u32("x.attention.head_count", 4).str("general.architecture", "x")
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil || m.BlockCount != 2 {
		t.Fatalf("%v %+v", err, m)
	}
}

func TestMissingOptionalKeys(t *testing.T) {
	f := newFx().str("general.architecture", "x").u32("x.block_count", 2)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if m.EmbeddingLength != 0 || m.ContextLength != 0 || m.KeyLength != 0 {
		t.Fatalf("%+v", m)
	}
	if _, err := m.NEmbdKGqa(0); err == nil {
		t.Fatal("expected error deriving gqa without head_count")
	}
}

func TestMissingArchitecture(t *testing.T) {
	if _, err := Read(bytes.NewReader(newFx().u32("x.block_count", 2).bytes())); err == nil {
		t.Fatal("expected error")
	}
}

func TestHeadArrayLengthMismatch(t *testing.T) {
	f := newFx().str("general.architecture", "x").u32("x.block_count", 3).
		u32("x.embedding_length", 64).arrU32("x.attention.head_count", 4, 4)
	if _, err := Read(bytes.NewReader(f.bytes())); err == nil {
		t.Fatal("expected error")
	}
}

func TestMalformed(t *testing.T) {
	good := llama().bytes()
	bad := func(mut func(f *fx)) []byte { f := llama(); mut(f); return f.bytes() }
	huge := bytes.Repeat([]byte{0xff}, 8)
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, ""},
		{"truncated header", good[:10], ""},
		{"truncated mid kv", good[:len(good)-3], ""},
		{"truncated mid key", good[:30], ""},
		{"wrong magic", bad(func(f *fx) { f.magic = "GGML" }), "magic"},
		{"version 1", bad(func(f *fx) { f.version = 1 }), "version"},
		{"version 4", bad(func(f *fx) { f.version = 4 }), "version"},
		{"huge kv count", append(newFx().bytesHeader(1<<62), huge...), ""},
		{"absurd key length", append(newFx().bytesHeader(1), huge...), ""},
		{"absurd string value", newFx().add("general.architecture", tStr, huge).bytes(), ""},
		{"absurd array count", newFx().rawArr("a", tU32, 1<<62, nil).bytes(), ""},
		{"absurd str array count", newFx().rawArr("a", tStr, 1<<40, nil).bytes(), ""},
		{"overflow array bytes", newFx().rawArr("a", tU64, 1<<61+1, nil).bytes(), ""},
		{"bad value type", newFx().add("a", 99, nil).bytes(), ""},
		{"bad array elem type", newFx().rawArr("a", 77, 1, nil).bytes(), ""},
		{"nested arrays too deep", deepNest(), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := Read(bytes.NewReader(c.data))
			if err == nil {
				t.Fatalf("expected error, got %+v", m)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q lacks %q", err, c.want)
			}
		})
	}
}

func TestNestedArraysSkipped(t *testing.T) {
	inner := append(le32(tU32), le64(2)...)
	inner = append(inner, le32(1)...)
	inner = append(inner, le32(2)...)
	body := append(append([]byte{}, inner...), inner...)
	f := llama().rawArr("nest", tArr, 2, body).u32("llama.attention.key_length", 64)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil || m.KeyLength != 64 {
		t.Fatalf("%v %+v", err, m)
	}
}

func TestLargeArraySkipped(t *testing.T) {
	body := make([]byte, 5_000_000*4)
	f := llama().rawArr("big", tU32, 5_000_000, body)
	m, err := Read(bytes.NewReader(f.bytes()))
	if err != nil || m.BlockCount != 32 {
		t.Fatalf("%v", err)
	}
}

func TestReadMeta(t *testing.T) {
	data := llama().bytes()
	data = append(data, make([]byte, 1000)...) // pretend tensor data
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMeta(p)
	if err != nil || m.FileSize != int64(len(data)) || m.Architecture != "llama" {
		t.Fatalf("%v %+v", err, m)
	}
	if _, err := ReadMeta(filepath.Join(t.TempDir(), "nope.gguf")); err == nil {
		t.Fatal("expected error")
	}
}

func FuzzRead(f *testing.F) {
	f.Add(llama().bytes())
	f.Add(llama().arrStr("t", "a", "b").arrU32("x.attention.head_count", 1, 2).bytes())
	f.Add(deepNest())
	f.Add([]byte("GGUF"))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Read(bytes.NewReader(data))
		if err != nil {
			return
		}
		for il := 0; il < int(m.BlockCount) && il < 8; il++ {
			m.NEmbdKGqa(il)
			m.NEmbdVGqa(il)
		}
		m.KVElemsPerToken()
	})
}

// A hybrid model (attention every n-th layer, recurrent layers between)
// keeps a KV cache only on its attention layers: layer il has one when
// (il + 1) % full_attention_interval == 0, as llama.cpp marks them.
func TestHybridKVOnlyOnAttentionLayers(t *testing.T) {
	hybrid := func(arch string, interval uint32) *fx {
		f := newFx().str("general.architecture", arch).
			u32(arch+".block_count", 33).
			u32(arch+".embedding_length", 2560).
			u32(arch+".attention.head_count", 16).
			u32(arch+".attention.head_count_kv", 4).
			u32(arch+".attention.key_length", 256).
			u32(arch+".attention.value_length", 256)
		if interval > 0 {
			f.u32(arch+".full_attention_interval", interval)
		}
		return f
	}
	for _, c := range []struct {
		name     string
		f        *fx
		interval uint32
		layers   int
	}{
		{"interval 4", hybrid("qwen35", 4), 4, 8}, // layers 3, 7, ..., 31
		{"interval 3", hybrid("qwen35", 3), 3, 11},
		// qwen3next, qwen35 and qwen35moe default to 4 without the key.
		{"qwen35 default", hybrid("qwen35", 0), 4, 8},
		{"qwen35moe default", hybrid("qwen35moe", 0), 4, 8},
		{"qwen3next default", hybrid("qwen3next", 0), 4, 8},
		// Another architecture without the key: every layer has a cache.
		{"plain", hybrid("qwen3", 0), 0, 33},
	} {
		m, err := Read(bytes.NewReader(c.f.bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if m.FullAttentionInterval != c.interval {
			t.Errorf("%s: interval %d, want %d", c.name, m.FullAttentionInterval, c.interval)
		}
		if n, err := m.KVElemsPerToken(); err != nil || n != c.layers*2*4*256 {
			t.Errorf("%s: %d KV elements per token (err %v), want %d layers x 2048", c.name, n, err, c.layers)
		}
	}
}
