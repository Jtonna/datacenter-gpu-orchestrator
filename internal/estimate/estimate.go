// Package estimate implements the memory-need arithmetic of design section
// 7.2 as pure functions over plain numbers. All fit rules use integer
// arithmetic so float rounding cannot decide a boundary case.
package estimate

import (
	"fmt"
	"math/bits"
)

const mib = 1 << 20

// kvBytesRatio gives bytes per KV element as num/16 so that the
// fractional q8_0 (17/16) and q4_0 (9/16) sizes are exact.
var kvBytesRatio = map[string]uint64{
	"f16":  32,
	"q8_0": 17,
	"q4_0": 9,
}

// KVBytesPerElement returns bytes per KV cache element for a cache type.
func KVBytesPerElement(quant string) (float64, error) {
	n, ok := kvBytesRatio[quant]
	if !ok {
		return 0, fmt.Errorf("estimate: unknown KV cache type %q", quant)
	}
	return float64(n) / 16, nil
}

// KVMiB returns n_layer × ctxTotal × (k + v) × bytes-per-element, rounded up
// to whole MiB. ctxTotal is ctx × parallel.
func KVMiB(nLayer, ctxTotal, nEmbdKGqa, nEmbdVGqa int, quant string) (int, error) {
	num, ok := kvBytesRatio[quant]
	if !ok {
		return 0, fmt.Errorf("estimate: unknown KV cache type %q", quant)
	}
	if nLayer < 0 || ctxTotal < 0 || nEmbdKGqa < 0 || nEmbdVGqa < 0 {
		return 0, fmt.Errorf("estimate: negative KV dimension")
	}
	x := uint64(nLayer)
	for _, f := range []uint64{uint64(ctxTotal), uint64(nEmbdKGqa) + uint64(nEmbdVGqa), num} {
		hi, lo := bits.Mul64(x, f)
		if hi != 0 {
			return 0, fmt.Errorf("estimate: KV size overflows")
		}
		x = lo
	}
	// x is in sixteenths of a byte.
	const unit = 16 * mib
	q := x / unit
	if x%unit != 0 {
		q++
	}
	if q > uint64(maxInt) {
		return 0, fmt.Errorf("estimate: KV size overflows")
	}
	return int(q), nil
}

const maxInt = int(^uint(0) >> 1)

// ComputeBytesPerCtxToken is the compute buffer each device of a load holds
// per token of context (n_ctx = ctx x parallel), on top of its share of the
// weights and the KV cache. Measured, not derived: on llama.cpp RPC workers
// with flash attention, the default batch sizes and an f16 KV cache, six
// model families (dense, MoE and hybrid; 2 to 8 KV heads; 2k to 128k context)
// all grew by exactly 4 KiB per context token per device beyond their KV
// cache. It does not depend on the model; it matches a buffer of n_ctx x
// n_ubatch (512) x 8 bytes, which each device holds for its own layers.
const ComputeBytesPerCtxToken = 4096

// ComputeMiB is the context-dependent compute buffer of one device, rounded
// up to whole MiB.
func ComputeMiB(ctxTotal int) int {
	if ctxTotal <= 0 {
		return 0
	}
	return int((int64(ctxTotal)*ComputeBytesPerCtxToken + mib - 1) / mib)
}

// NeedMiB is the estimated need: the model file size rounded up to MiB plus kvMiB.
func NeedMiB(fileSizeBytes int64, kvMiB int) int {
	w := int64(0)
	if fileSizeBytes > 0 {
		w = (fileSizeBytes + mib - 1) / mib
	}
	return int(w) + kvMiB
}

// FitsEstimated reports whether Σ(budget − overhead) ≥ need × 1.05, tested
// as 100·Σ ≥ 105·need. A device whose budget is below the overhead
// contributes zero rather than a negative amount.
func FitsEstimated(budgetsMiB []int, needMiB, deviceOverheadMiB int) bool {
	var sum int64
	for _, b := range budgetsMiB {
		if v := b - deviceOverheadMiB; v > 0 {
			sum += int64(v)
		}
	}
	return sum*100 >= int64(needMiB)*105
}

// FitsMeasured reports whether Σ budget ≥ needsMiB (a measured need already
// includes overhead and compute buffers).
func FitsMeasured(budgetsMiB []int, needsMiB int) bool {
	var sum int64
	for _, b := range budgetsMiB {
		if b > 0 {
			sum += int64(b)
		}
	}
	return sum >= int64(needsMiB)
}

// MarginMiB returns ceil(miB × (1000 + permille) / 1000): miB raised by a
// margin given in thousandths, in integer arithmetic.
func MarginMiB(miB, permille int) int {
	if miB <= 0 {
		return 0
	}
	if permille <= 0 {
		return miB
	}
	return int((int64(miB)*int64(1000+permille) + 999) / 1000)
}

// FitsEstimatedMargin is FitsEstimated with the margin in thousandths:
// Σ(budget − overhead) ≥ need × (1 + permille/1000). FitsEstimated is the
// permille = 50 case.
func FitsEstimatedMargin(budgetsMiB []int, needMiB, deviceOverheadMiB, permille int) bool {
	var sum int64
	for _, b := range budgetsMiB {
		if v := b - deviceOverheadMiB; v > 0 {
			sum += int64(v)
		}
	}
	return sum >= int64(MarginMiB(needMiB, permille))
}
