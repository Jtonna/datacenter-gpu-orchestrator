package scheduler

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
)

// Measured memory needs of real loads, against which the estimate from the
// model file is checked. Source: one bench session on identical
// shared-memory worker nodes (llama.cpp RPC workers, weights and context
// split equally, one slot, f16 KV cache, flash attention on). measuredMiB is
// the load's device total (model + KV + compute buffers) summed over the
// nodes, rounded up.
//
// The architectures are the public metadata of each model. Two values are
// inferred, not read: fileMiB is the model file size the scheduler saw (its
// estimate minus its KV figure; it matches the published file sizes), and
// the hybrid models' block_count (33 and 65) is the one the scheduler's own
// estimate implies (it grew by block_count x 2048 elements per token), i.e.
// the attention layers plus one multi-token-prediction layer.
var (
	archQwen35_4B = ggufgen.Spec{Arch: "qwen35", Blocks: 33, Embd: 2560, Heads: 16, HeadsKV: 4, KeyLength: 256, ValueLength: 256, FullAttentionInterval: 4}
	archQwen38_27 = ggufgen.Spec{Arch: "qwen35", Blocks: 65, Embd: 5120, Heads: 24, HeadsKV: 4, KeyLength: 256, ValueLength: 256, FullAttentionInterval: 4}
	archDevstral  = ggufgen.Spec{Arch: "mistral3", Blocks: 40, Embd: 5120, Heads: 32, HeadsKV: 8, KeyLength: 128, ValueLength: 128}
	archQwen3MoE  = ggufgen.Spec{Arch: "qwen3moe", Blocks: 48, Embd: 2048, Heads: 32, HeadsKV: 4, KeyLength: 128, ValueLength: 128}
	archGLM4_32   = ggufgen.Spec{Arch: "glm4", Blocks: 61, Embd: 6144, Heads: 48, HeadsKV: 2}
	archQwen2_72  = ggufgen.Spec{Arch: "qwen2", Blocks: 80, Embd: 8192, Heads: 64, HeadsKV: 8}
	archLlama70   = ggufgen.Spec{Arch: "llama", Blocks: 80, Embd: 8192, Heads: 64, HeadsKV: 8}
)

type measuredRow struct {
	label       string
	arch        ggufgen.Spec
	fileMiB     int64
	ctx         int
	nodes       int
	measuredMiB int
	// lowToday marks the models whose estimate was below the measurement;
	// for them the estimate must also stay close (estimateOverLimit).
	lowToday bool
}

var measuredRows = []measuredRow{
	{"qwen35 4B hybrid Q4_K_M", archQwen35_4B, 2874, 2048, 6, 3403, false},
	{"qwen35 4B hybrid Q4_K_M", archQwen35_4B, 2874, 8192, 6, 3739, false},
	{"qwen35 4B hybrid Q4_K_M", archQwen35_4B, 2874, 16384, 6, 4187, false},
	{"qwen35 4B hybrid Q4_K_M", archQwen35_4B, 2874, 32768, 6, 5083, false},
	{"qwen35 4B hybrid Q4_K_M", archQwen35_4B, 2874, 65536, 6, 6875, false},
	{"qwen35 4B hybrid Q4_K_M", archQwen35_4B, 2874, 131072, 6, 10459, false},
	{"qwen35 4B hybrid Q4_K_M", archQwen35_4B, 2874, 8192, 1, 3079, false},
	{"mistral3 24B Q4_K_M", archDevstral, 13671, 2048, 6, 15321, true},
	{"mistral3 24B Q4_K_M", archDevstral, 13671, 8192, 6, 16425, true},
	{"mistral3 24B Q4_K_M", archDevstral, 13671, 16384, 6, 17897, true},
	{"mistral3 24B Q4_K_M", archDevstral, 13671, 32768, 6, 20841, true},
	{"mistral3 24B Q4_K_M", archDevstral, 13671, 65536, 6, 26729, true},
	{"mistral3 24B Q4_K_M", archDevstral, 13671, 131072, 6, 38505, true},
	{"mistral3 24B Q4_K_M", archDevstral, 13671, 8192, 2, 15257, true},
	{"qwen3moe 30B-A3B coder Q4_K_M", archQwen3MoE, 17698, 2048, 6, 18401, true},
	{"qwen3moe 30B-A3B coder Q4_K_M", archQwen3MoE, 17698, 8192, 6, 19121, true},
	{"qwen3moe 30B-A3B coder Q4_K_M", archQwen3MoE, 17698, 16384, 6, 20081, true},
	{"qwen3moe 30B-A3B coder Q4_K_M", archQwen3MoE, 17698, 32768, 6, 22001, true},
	{"qwen3moe 30B-A3B coder Q4_K_M", archQwen3MoE, 17698, 65536, 6, 25841, true},
	{"qwen3moe 30B-A3B coder Q4_K_M", archQwen3MoE, 17698, 131072, 6, 33521, true},
	{"qwen3moe 30B-A3B coder Q4_K_M", archQwen3MoE, 17698, 8192, 3, 18725, true},
	// Published size 17.4 GiB; the low end of that rounding is used.
	{"qwen3moe 30B-A3B instruct Q4_K_M", archQwen3MoE, 17766, 8192, 6, 19193, true},
	{"qwen3moe 30B-A3B instruct Q4_K_M", archQwen3MoE, 17766, 8192, 3, 18797, true},
	{"glm4 32B Q4_K_M", archGLM4_32, 18769, 2048, 6, 19853, true},
	{"glm4 32B Q4_K_M", archGLM4_32, 18769, 8192, 6, 20363, true},
	{"glm4 32B Q4_K_M", archGLM4_32, 18769, 16384, 6, 21043, true},
	{"glm4 32B Q4_K_M", archGLM4_32, 18769, 32768, 6, 22403, true},
	{"glm4 32B Q4_K_M", archGLM4_32, 18769, 8192, 3, 19610, true},
	{"qwen35 27B hybrid Q4_K_M", archQwen38_27, 16635, 2048, 6, 16953, false},
	{"qwen35 27B hybrid Q4_K_M", archQwen38_27, 16635, 8192, 6, 17481, false},
	{"qwen35 27B hybrid Q4_K_M", archQwen38_27, 16635, 16384, 6, 18185, false},
	{"qwen35 27B hybrid Q4_K_M", archQwen38_27, 16635, 32768, 6, 19593, false},
	{"qwen35 27B hybrid Q4_K_M", archQwen38_27, 16635, 65536, 6, 22409, false},
	{"qwen35 27B hybrid Q4_K_M", archQwen38_27, 16635, 8192, 3, 16899, false},
	{"qwen2 72B Q2_K", archQwen2_72, 28431, 2048, 6, 30579, true},
	{"qwen2 72B Q2_K", archQwen2_72, 28431, 8192, 6, 32643, true},
	{"qwen2 72B Q2_K", archQwen2_72, 28431, 16384, 6, 35395, true},
	{"qwen2 72B Q2_K", archQwen2_72, 28431, 32768, 6, 40899, true},
	{"qwen2 72B Q2_K", archQwen2_72, 28431, 8192, 4, 32009, true},
	// Published sizes 26.0 and 24.6 GiB; the low end of the rounding is used.
	{"qwen2 72B IQ2_S", archQwen2_72, 26573, 8192, 6, 30737, true},
	{"qwen2 72B IQ2_S", archQwen2_72, 26573, 8192, 4, 30102, true},
	{"llama 70B Q2_K", archLlama70, 25139, 8192, 6, 29394, true},
	{"llama 70B Q2_K", archLlama70, 25139, 8192, 4, 28770, true},
}

// estimateOverLimit is how far above the measurement the estimate of a model
// in the lowToday group may be, with the default device_overhead_mib (whose
// fixed 600 MiB per device is most of the excess at short contexts).
const estimateOverLimit = 0.25

// estimatedLoad is what the scheduler expects a load of row to take: the
// expected shares of its nodes (expect_mib, the per-node fit check) with the
// split skew margin off, summed; and the total the placer's fit rule
// reserves, need x (1 + margin) + device overhead per device.
func estimatedLoad(t *testing.T, r measuredRow) (shares, placerTotal, baseMiB int) {
	t.Helper()
	h := newHarness(t)
	h.cfg.Defaults.SplitSkewMargin = 0
	var ns []core.Node
	pl := core.Placement{}
	for i := range r.nodes {
		id := fmt.Sprintf("w%d", i+1)
		n := ps5(id, i+10, 15000)
		ns = append(ns, n)
		pl.Nodes = append(pl.Nodes, core.PlacedNode{ID: n.ID, Addr: n.Addr, BudgetMiB: 12000})
	}
	h.nodes(ns...)
	p := h.llama("m", 0, func(p *core.Profile) { p.Ctx = r.ctx; p.FlashAttn = true })
	meta, err := gguf.Read(bytes.NewReader(ggufgen.Bytes(r.arch)))
	if err != nil {
		t.Fatal(err)
	}
	meta.FileSize = r.fileMiB << 20
	info, err := estimateFromMeta(meta, p)
	if err != nil {
		t.Fatal(err)
	}
	h.needs["m"] = info
	for _, s := range h.c.expectShares(p, pl) {
		shares += s
	}
	req, _, err := h.c.request(p)
	if err != nil {
		t.Fatal(err)
	}
	return shares, req.Need.WithMargin() + r.nodes*req.DeviceOverheadMiB, info.Need.MiB
}

// The estimate is never below what a load measured: neither the nodes'
// expected shares (what each node is held to) nor the placer's total.
func TestEstimate_NeverBelowMeasuredLoads(t *testing.T) {
	t.Logf("%-34s %7s %2s %9s %9s %9s %7s %9s", "model", "ctx", "n", "measured", "estimate", "shares", "err", "placer")
	for _, r := range measuredRows {
		shares, total, base := estimatedLoad(t, r)
		errPct := 100 * (float64(shares)/float64(r.measuredMiB) - 1)
		t.Logf("%-34s %7d %2d %9d %9d %9d %+6.1f%% %9d", r.label, r.ctx, r.nodes, r.measuredMiB, base, shares, errPct, total)
		if shares < r.measuredMiB {
			t.Errorf("%s ctx %d on %d nodes: expected shares sum to %d MiB, the load measured %d MiB (%+.1f%%)",
				r.label, r.ctx, r.nodes, shares, r.measuredMiB, errPct)
		}
		if total < r.measuredMiB {
			t.Errorf("%s ctx %d on %d nodes: the placer reserves %d MiB, the load measured %d MiB", r.label, r.ctx, r.nodes, total, r.measuredMiB)
		}
		if r.lowToday && float64(shares) > float64(r.measuredMiB)*(1+estimateOverLimit) {
			t.Errorf("%s ctx %d on %d nodes: expected shares %d MiB are more than %.0f%% above the measured %d MiB",
				r.label, r.ctx, r.nodes, shares, 100*estimateOverLimit, r.measuredMiB)
		}
	}
}
