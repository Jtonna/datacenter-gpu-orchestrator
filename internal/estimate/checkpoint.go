package estimate

import "github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"

// Ckpt is the size of one context checkpoint of a model: the copy of a
// slot's state the model server keeps in the HOST process's memory (up to
// --ctx-checkpoints per slot).
type Ckpt struct {
	Family    gguf.Family
	CellBytes int64 // one checkpoint, bytes; 0 for a plain model or when Unknown
	// Estimate: CellBytes is a bound, not the exact figure (a sliding
	// window: exact once a conversation is longer than the window).
	Estimate bool
	// Unknown: a windowed model whose window layers this reader cannot
	// tell from the header; no figure (read at the first load instead).
	Unknown bool
}

// Checkpoint is m's checkpoint size, as the pinned model server makes it
// (tools/server: llama_state_seq_get_size_ext with PARTIAL_ONLY).
//
//   - Recurrent or hybrid: one recurrent cell (gguf RecurrentCellBytes),
//     exact; the attention KV is not copied.
//   - Sliding window: the window cache's cells of the sequence that are
//     not masked (llama_kv_cache::state_write keeps pos > pos_max - n_swa),
//     so at most min(window, ctxPerSlot) cells, times the window layers'
//     K+V per token (gguf WindowKVElemsPerToken) at the cache type kvQuant;
//     the full-attention cache writes nothing under PARTIAL_ONLY. Unknown
//     when the window layers are not known from the header.
//   - Plain attention: none.
func Checkpoint(m *gguf.Meta, kvQuant string, ctxPerSlot int) Ckpt {
	out := Ckpt{Family: m.Family()}
	switch out.Family {
	case gguf.FamilyRecurrent:
		out.CellBytes = m.RecurrentCellBytes()
	case gguf.FamilyWindowed:
		elems, ok := m.WindowKVElemsPerToken()
		bpe, err := KVBytesPerElement(kvQuant)
		if !ok || err != nil {
			out.Unknown = true
			return out
		}
		cells := int64(m.WindowSize)
		if ctxPerSlot > 0 && int64(ctxPerSlot) < cells {
			cells = int64(ctxPerSlot)
		}
		out.CellBytes, out.Estimate = int64(float64(cells)*float64(elems)*bpe+0.5), true
	}
	return out
}

// CellMiB is one checkpoint in MiB.
func (c Ckpt) CellMiB() float64 { return float64(c.CellBytes) / mib }

// TotalMiB is n checkpoints, rounded up to a whole MiB.
func (c Ckpt) TotalMiB(n int) int {
	if n <= 0 || c.CellBytes <= 0 {
		return 0
	}
	return int((int64(n)*c.CellBytes + mib - 1) / mib)
}
