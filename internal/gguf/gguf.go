// Package gguf is a minimal, defensive GGUF header reader. It parses the
// container header and metadata key/value section only, never tensor data,
// and bounds every length it reads so corrupt input yields an error instead
// of a panic or an unbounded allocation.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Limits applied while parsing untrusted input.
const (
	maxKVCount     = 1 << 16 // metadata entries
	maxKeyLen      = 1 << 16 // bytes of a key, read into memory
	maxKeptString  = 1 << 16 // string values longer than this are skipped, not kept
	maxSkipString  = 1 << 30 // string values longer than this are an error
	maxArrayLen    = 1 << 32 // elements
	maxKeptArray   = 1 << 10 // integer arrays up to this length are retained
	maxArrayDepth  = 4
	maxBlockCount  = 1 << 16
	maxTensorCount = 1 << 32
)

// GGUF value type codes.
const (
	tU8 = iota
	tI8
	tU16
	tI16
	tU32
	tI32
	tF32
	tBool
	tStr
	tArr
	tU64
	tI64
	tF64
)

// Meta is the subset of GGUF header metadata the scheduler needs. Fields
// are zero when the key is absent.
type Meta struct {
	Version         uint32
	TensorCount     uint64
	Architecture    string
	BlockCount      uint32   // <arch>.block_count
	EmbeddingLength uint32   // <arch>.embedding_length
	HeadCount       []uint32 // <arch>.attention.head_count, one per layer (scalars are broadcast)
	HeadCountKV     []uint32 // <arch>.attention.head_count_kv, one per layer; defaults to HeadCount
	KeyLength       uint32   // <arch>.attention.key_length, 0 if absent
	ValueLength     uint32   // <arch>.attention.value_length, 0 if absent
	ContextLength   uint32   // <arch>.context_length
	SlidingWindow   bool     // <arch>.attention.sliding_window present and > 0
	FileSize        int64    // set by ReadMeta only

	scalars map[string]any
}

// ReadMeta reads the metadata of the GGUF file at path and records its size.
func ReadMeta(path string) (*Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	m, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("gguf: %s: %w", path, err)
	}
	m.FileSize = st.Size()
	return m, nil
}

type parser struct {
	r   *bufio.Reader
	buf [8]byte
}

func (p *parser) u32() (uint32, error) {
	if _, err := io.ReadFull(p.r, p.buf[:4]); err != nil {
		return 0, eof(err)
	}
	return binary.LittleEndian.Uint32(p.buf[:4]), nil
}

func (p *parser) u64() (uint64, error) {
	if _, err := io.ReadFull(p.r, p.buf[:8]); err != nil {
		return 0, eof(err)
	}
	return binary.LittleEndian.Uint64(p.buf[:8]), nil
}

func eof(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (p *parser) discard(n uint64) error {
	if n > math.MaxInt64 {
		return io.ErrUnexpectedEOF
	}
	_, err := io.CopyN(io.Discard, p.r, int64(n))
	return eof(err)
}

// str reads a string; if it is longer than keep it is skipped and ok is false.
func (p *parser) str(keep uint64) (s string, ok bool, err error) {
	n, err := p.u64()
	if err != nil {
		return "", false, err
	}
	if n > maxSkipString {
		return "", false, fmt.Errorf("string length %d exceeds limit", n)
	}
	if n > keep {
		return "", false, p.discard(n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(p.r, b); err != nil {
		return "", false, eof(err)
	}
	return string(b), true, nil
}

func fixedSize(t uint32) uint64 {
	switch t {
	case tU8, tI8, tBool:
		return 1
	case tU16, tI16:
		return 2
	case tU32, tI32, tF32:
		return 4
	case tU64, tI64, tF64:
		return 8
	}
	return 0
}

func isInt(t uint32) bool {
	switch t {
	case tU8, tI8, tU16, tI16, tU32, tI32, tU64, tI64:
		return true
	}
	return false
}

// scalar reads one fixed-size or string value of type t. Integers come back
// as uint64 (unsigned types) or int64 (signed types).
func (p *parser) scalar(t uint32) (any, error) {
	switch t {
	case tStr:
		s, ok, err := p.str(maxKeptString)
		if err != nil || !ok {
			return nil, err
		}
		return s, nil
	case tU8, tI8, tBool, tU16, tI16, tU32, tI32, tF32, tU64, tI64, tF64:
	default:
		return nil, fmt.Errorf("unknown value type %d", t)
	}
	n := fixedSize(t)
	if _, err := io.ReadFull(p.r, p.buf[:n]); err != nil {
		return nil, eof(err)
	}
	var u uint64
	switch n {
	case 1:
		u = uint64(p.buf[0])
	case 2:
		u = uint64(binary.LittleEndian.Uint16(p.buf[:2]))
	case 4:
		u = uint64(binary.LittleEndian.Uint32(p.buf[:4]))
	default:
		u = binary.LittleEndian.Uint64(p.buf[:8])
	}
	switch t {
	case tI8:
		return int64(int8(u)), nil
	case tI16:
		return int64(int16(u)), nil
	case tI32:
		return int64(int32(u)), nil
	case tI64:
		return int64(u), nil
	case tF32:
		return float64(math.Float32frombits(uint32(u))), nil
	case tF64:
		return math.Float64frombits(u), nil
	case tBool:
		return u != 0, nil
	}
	return u, nil
}

// skipArrayBody skips n elements of type et.
func (p *parser) skipArrayBody(et uint32, n uint64, depth int) error {
	if n > maxArrayLen {
		return fmt.Errorf("array length %d exceeds limit", n)
	}
	switch {
	case fixedSize(et) > 0:
		return p.discard(n * fixedSize(et)) // n <= 2^32, size <= 8: no overflow
	case et == tStr:
		for i := uint64(0); i < n; i++ {
			if _, _, err := p.str(0); err != nil {
				return err
			}
		}
		return nil
	case et == tArr:
		if depth+1 > maxArrayDepth {
			return errors.New("arrays nested too deeply")
		}
		for i := uint64(0); i < n; i++ {
			et2, err := p.u32()
			if err != nil {
				return err
			}
			n2, err := p.u64()
			if err != nil {
				return err
			}
			if err := p.skipArrayBody(et2, n2, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown array element type %d", et)
}

// Read parses a GGUF header and metadata from r. It stops after the last
// metadata entry and does not touch tensor info or data.
func Read(r io.Reader) (*Meta, error) {
	p := &parser{r: bufio.NewReaderSize(r, 64<<10)}
	var magic [4]byte
	if _, err := io.ReadFull(p.r, magic[:]); err != nil {
		return nil, eof(err)
	}
	if string(magic[:]) != "GGUF" {
		return nil, errors.New("bad magic: not a GGUF file")
	}
	ver, err := p.u32()
	if err != nil {
		return nil, err
	}
	if ver != 2 && ver != 3 {
		return nil, fmt.Errorf("unsupported GGUF version %d", ver)
	}
	tensors, err := p.u64()
	if err != nil {
		return nil, err
	}
	if tensors > maxTensorCount {
		return nil, fmt.Errorf("tensor count %d exceeds limit", tensors)
	}
	kvn, err := p.u64()
	if err != nil {
		return nil, err
	}
	if kvn > maxKVCount {
		return nil, fmt.Errorf("metadata count %d exceeds limit", kvn)
	}

	scalars := map[string]any{}
	arrays := map[string][]uint64{}
	for i := uint64(0); i < kvn; i++ {
		key, _, err := p.str(maxKeyLen)
		if err != nil {
			return nil, err
		}
		t, err := p.u32()
		if err != nil {
			return nil, err
		}
		if t != tArr {
			v, err := p.scalar(t)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", key, err)
			}
			if v != nil {
				scalars[key] = v
			}
			continue
		}
		et, err := p.u32()
		if err != nil {
			return nil, err
		}
		n, err := p.u64()
		if err != nil {
			return nil, err
		}
		if isInt(et) && n <= maxKeptArray {
			vals := make([]uint64, 0, n)
			for j := uint64(0); j < n; j++ {
				v, err := p.scalar(et)
				if err != nil {
					return nil, fmt.Errorf("key %q: %w", key, err)
				}
				switch x := v.(type) {
				case uint64:
					vals = append(vals, x)
				case int64:
					if x < 0 {
						return nil, fmt.Errorf("key %q: negative value", key)
					}
					vals = append(vals, uint64(x))
				}
			}
			arrays[key] = vals
			continue
		}
		if err := p.skipArrayBody(et, n, 1); err != nil {
			return nil, fmt.Errorf("key %q: %w", key, err)
		}
	}
	return build(ver, tensors, scalars, arrays)
}

func build(ver uint32, tensors uint64, scalars map[string]any, arrays map[string][]uint64) (*Meta, error) {
	m := &Meta{Version: ver, TensorCount: tensors, scalars: scalars}
	arch, _ := scalars["general.architecture"].(string)
	if arch == "" {
		return nil, errors.New("missing general.architecture")
	}
	m.Architecture = arch
	pre := arch + "."

	u32 := func(key string) (uint32, bool, error) {
		v, ok := scalars[key]
		if !ok {
			return 0, false, nil
		}
		var u uint64
		switch x := v.(type) {
		case uint64:
			u = x
		case int64:
			if x < 0 {
				return 0, false, fmt.Errorf("%s: negative value", key)
			}
			u = uint64(x)
		default:
			return 0, false, fmt.Errorf("%s: not an integer", key)
		}
		if u > math.MaxUint32 {
			return 0, false, fmt.Errorf("%s: value %d out of range", key, u)
		}
		return uint32(u), true, nil
	}
	var err error
	if m.BlockCount, _, err = u32(pre + "block_count"); err != nil {
		return nil, err
	}
	if m.BlockCount > maxBlockCount {
		return nil, fmt.Errorf("block_count %d exceeds limit", m.BlockCount)
	}
	if m.EmbeddingLength, _, err = u32(pre + "embedding_length"); err != nil {
		return nil, err
	}
	if m.ContextLength, _, err = u32(pre + "context_length"); err != nil {
		return nil, err
	}
	if m.KeyLength, _, err = u32(pre + "attention.key_length"); err != nil {
		return nil, err
	}
	if m.ValueLength, _, err = u32(pre + "attention.value_length"); err != nil {
		return nil, err
	}
	sw, _, err := u32(pre + "attention.sliding_window")
	if err != nil {
		return nil, err
	}
	m.SlidingWindow = sw > 0

	heads := func(key string) ([]uint32, error) {
		if a, ok := arrays[key]; ok {
			if len(a) != int(m.BlockCount) {
				return nil, fmt.Errorf("%s has %d entries, block_count is %d", key, len(a), m.BlockCount)
			}
			out := make([]uint32, len(a))
			for i, v := range a {
				if v > math.MaxUint32 {
					return nil, fmt.Errorf("%s: value out of range", key)
				}
				out[i] = uint32(v)
			}
			return out, nil
		}
		v, ok, err := u32(key)
		if err != nil || !ok {
			return nil, err
		}
		out := make([]uint32, m.BlockCount)
		for i := range out {
			out[i] = v
		}
		return out, nil
	}
	if m.HeadCount, err = heads(pre + "attention.head_count"); err != nil {
		return nil, err
	}
	if m.HeadCountKV, err = heads(pre + "attention.head_count_kv"); err != nil {
		return nil, err
	}
	if m.HeadCountKV == nil && m.HeadCount != nil {
		// llama-model.cpp:1557: n_head_kv defaults to n_head.
		m.HeadCountKV = append([]uint32(nil), m.HeadCount...)
	}
	return m, nil
}

// Scalar returns any non-array metadata value by exact key. Integers are
// uint64 (unsigned GGUF types) or int64 (signed), floats float64, plus bool
// and string. Strings longer than 64 KiB are not retained.
func (m *Meta) Scalar(key string) (any, bool) {
	v, ok := m.scalars[key]
	return v, ok
}

func (m *Meta) check(il int) error {
	if len(m.HeadCount) == 0 {
		return errors.New("gguf: head_count missing")
	}
	if il < 0 || il >= len(m.HeadCountKV) {
		return fmt.Errorf("gguf: layer %d out of range [0,%d)", il, len(m.HeadCountKV))
	}
	return nil
}

// headDim mirrors llama-model.cpp:1601-1605: n_embd / n_head(0) unless the
// explicit key length is present.
func (m *Meta) headDim(explicit uint32) int {
	if explicit > 0 {
		return int(explicit)
	}
	if m.HeadCount[0] == 0 {
		return 0
	}
	return int(m.EmbeddingLength / m.HeadCount[0])
}

// NEmbdKGqa is llama_hparams::n_embd_k_gqa(il): head key size times the KV
// head count of layer il (llama-hparams.cpp:156-160).
func (m *Meta) NEmbdKGqa(il int) (int, error) {
	if err := m.check(il); err != nil {
		return 0, err
	}
	return m.headDim(m.KeyLength) * int(m.HeadCountKV[il]), nil
}

// NEmbdVGqa is llama_hparams::n_embd_v_gqa(il) (llama-hparams.cpp:162-166).
func (m *Meta) NEmbdVGqa(il int) (int, error) {
	if err := m.check(il); err != nil {
		return 0, err
	}
	return m.headDim(m.ValueLength) * int(m.HeadCountKV[il]), nil
}

// KVElemsPerToken is the sum over all layers of n_embd_k_gqa + n_embd_v_gqa,
// the KV elements stored per cached token for full-context layers.
func (m *Meta) KVElemsPerToken() (int, error) {
	if m.BlockCount == 0 {
		return 0, errors.New("gguf: block_count missing")
	}
	total := 0
	for il := 0; il < int(m.BlockCount); il++ {
		k, err := m.NEmbdKGqa(il)
		if err != nil {
			return 0, err
		}
		v, _ := m.NEmbdVGqa(il)
		total += k + v
	}
	return total, nil
}
