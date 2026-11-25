package estimate

import "github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"

// CheckpointUbatch is the model server's default n_ubatch, which bounds a
// sliding-window checkpoint together with the window.
const CheckpointUbatch = 512

// Ckpt is the size of one context checkpoint of a model: the copy of a
// slot's state the model server keeps in the HOST process's memory (up to
// --ctx-checkpoints per slot).
type Ckpt struct {
	Family    gguf.Family
	CellBytes int64 // one checkpoint, bytes; 0 for a plain model
	// Estimate: CellBytes is a bound, not the exact figure (a sliding
	// window: the window's cells times the K+V of every attention layer,
	// where the server copies only the window layers' cells).
	Estimate bool
}

// Checkpoint is m's checkpoint size. Recurrent or hybrid: one recurrent
// cell (gguf RecurrentCellBytes), exact. Sliding window: at most (n_swa +
// n_ubatch) cells, padded to 256 (llama-kv-cache-iswa.cpp), times the
// per-token K+V of the attention layers at the cache type kvQuant, an
// upper bound. Plain attention: none.
func Checkpoint(m *gguf.Meta, kvQuant string) Ckpt {
	out := Ckpt{Family: m.Family()}
	switch out.Family {
	case gguf.FamilyRecurrent:
		out.CellBytes = m.RecurrentCellBytes()
	case gguf.FamilyWindowed:
		elems, err := m.KVElemsPerToken()
		bpe, err2 := KVBytesPerElement(kvQuant)
		if err != nil || err2 != nil {
			return out
		}
		cells := (int64(m.WindowSize) + CheckpointUbatch + 255) / 256 * 256
		out.CellBytes, out.Estimate = int64(float64(cells)*float64(elems)*bpe), true
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
