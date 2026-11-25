package estimate

import "github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"

// Ckpt is the size of one context checkpoint of a model: the copy of a
// slot's state the model server keeps in the HOST process's memory (up to
// --ctx-checkpoints per slot).
type Ckpt struct {
	Family    gguf.Family
	CellBytes int64 // one checkpoint, bytes; 0 for a plain model or when Unknown
	// Estimate is kept for callers; an exact rule gives an exact figure.
	Estimate bool
	// Unknown: the code has no exact rule for this model's checkpoint,
	// proven against a real server log line (ExactRecurrentArchs; every
	// window model): no figure until a load's log gives one.
	Unknown bool
	// BoundBytes is, for a window model whose window layers the header
	// tells, the header arithmetic min(window, ctx per slot) x the window
	// layers' K+V per token. Never shown or counted: it is only compared
	// with the first load's log line, to prove the arithmetic later.
	BoundBytes int64
	// FileBytes is the model file's size: with the context, which version
	// a figure read from a log belongs to.
	FileBytes int64
}

// ExactRecurrentArchs are the recurrent and hybrid architectures whose
// checkpoint the formula gives exactly, proven against the server's own
// log: qwen35 (Qwen 3.5 4B: 50.25 MiB; Qwen 3.8 27B: 149.62 MiB).
var ExactRecurrentArchs = map[string]bool{"qwen35": true}

// Checkpoint is m's checkpoint size, as the pinned model server makes it
// (tools/server: llama_state_seq_get_size_ext with PARTIAL_ONLY).
//
//   - Recurrent or hybrid, of an architecture in ExactRecurrentArchs: one
//     recurrent cell (gguf RecurrentCellBytes), exact; the attention KV is
//     not copied. Any other recurrent architecture: Unknown.
//   - Sliding window: Unknown. The header bound (BoundBytes) is the window
//     cache's unmasked cells of the sequence (llama_kv_cache::state_write
//     keeps pos > pos_max - n_swa), at most min(window, ctxPerSlot), times
//     the window layers' K+V per token (gguf WindowKVElemsPerToken) at the
//     cache type kvQuant; it is not yet proven against a log.
//   - Plain attention: none (the server makes no checkpoint).
func Checkpoint(m *gguf.Meta, kvQuant string, ctxPerSlot int) Ckpt {
	out := Ckpt{Family: m.Family(), FileBytes: m.FileSize}
	switch out.Family {
	case gguf.FamilyRecurrent:
		if !ExactRecurrentArchs[m.Architecture] {
			out.Unknown = true
			return out
		}
		out.CellBytes = m.RecurrentCellBytes()
	case gguf.FamilyWindowed:
		out.Unknown = true
		elems, ok := m.WindowKVElemsPerToken()
		bpe, err := KVBytesPerElement(kvQuant)
		if !ok || err != nil {
			return out
		}
		cells := int64(m.WindowSize)
		if ctxPerSlot > 0 && int64(ctxPerSlot) < cells {
			cells = int64(ctxPerSlot)
		}
		out.BoundBytes = int64(float64(cells)*float64(elems)*bpe + 0.5)
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
