package scheduler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The standard set's need figures on a model measured at three contexts on
// six devices (a confirmed own slope of 40 MiB per 1k context), and a
// candidate beyond them (64k): its need is derived from the line, never
// "measured" (the fault on live's list, 26 Nov); and the figure the fit is
// decided on is the need on the zones the placer may use, which is the one
// its refusal quotes (gemma-4-12b:256k: 18,499 MiB on 6 devices in the row,
// 105,440 MiB on 4 in the reason, with max_pool 4).
func TestStandardSet_DerivedNeedAndTheDecidedCount(t *testing.T) {
	setup := func(pool int) *harness {
		h := deriveHarness(t)
		h.cfg.Defaults.MaxPool = pool
		var ns []core.Node
		for i := 1; i <= 6; i++ {
			n := ps5(fmt.Sprintf("p%d", i), i, 4000)
			n.TotalMiB = 4000 // the cluster capacity is the total: on 4 zones it is too small, as on live
			ns = append(ns, n)
		}
		h.nodes(ns...)
		h.viewChanged()
		h.measure("m.gguf", 2048, 6, 8000)
		h.measure("m.gguf", 8192, 6, 8240)
		h.measure("m.gguf", 32768, 6, 9200)
		return h
	}
	cand := func() []*core.Profile {
		return []*core.Profile{{Name: "m:64k", Kind: core.KindLlama, Model: "m.gguf", Ctx: 65536, Parallel: 1, KVQuant: "f16"}}
	}

	// max_pool 6: it fits on six zones, by the line
	h := setup(6)
	cp := h.c.planCandidates(cand())[0]
	r := cp.Report
	if r.DerivedSlope == nil || r.DerivedSlope.MiBPer1kCtx != 40 || r.NeedMiB != 10480 || !r.Measured {
		t.Fatalf("the plan's need: %d measured=%v slope %+v (%s)", r.NeedMiB, r.Measured, r.DerivedSlope, r.NeedBasis)
	}
	if !cp.IdleFits || cp.ZonesCount != len(cp.IdleZones) || cp.ZonesCount != 6 || !DerivedSource(cp.ZonesNeedSource) || MeasuredSource(cp.ZonesNeedSource) || cp.ZonesNeedMiB != 10480 {
		t.Fatalf("decided on %d zones: %d MiB %q (fits %v on %v; may use %d; plan %s %s)", cp.ZonesCount, cp.ZonesNeedMiB, cp.ZonesNeedSource, cp.IdleFits, cp.IdleZones, cp.MayUse, r.Action, r.Reason)
	}

	// max_pool 4: the line is on six devices only (ownslope.go: a line
	// decides only on its own count), so on four the need is another
	// figure; that is the one the placer refuses, and the one it quotes
	h = setup(4)
	cp = h.c.planCandidates(cand())[0]
	r = cp.Report
	if cp.IdleFits || cp.ZonesCount != 4 || !cp.ByPool || !cp.ZonesNeedKnown {
		t.Fatalf("max_pool 4: fits %v, decided on %d (by pool %v)", cp.IdleFits, cp.ZonesCount, cp.ByPool)
	}
	if r.NeedMiB != 10480 || cp.ZonesNeedMiB == r.NeedMiB || cp.ZonesNeedSource == NeedOwnSlope {
		t.Fatalf("max_pool 4: plan %d, on 4 zones %d %q", r.NeedMiB, cp.ZonesNeedMiB, cp.ZonesNeedSource)
	}
	if r.Action != PlanReject || !strings.Contains(r.Reason, fmt.Sprintf("need %d MiB", cp.ZonesNeedMiB)) {
		t.Fatalf("the refusal quotes %q; the need on 4 zones is %d", r.Reason, cp.ZonesNeedMiB)
	}
	// nothing is kept of the candidate's need
	if _, ok := h.c.needs_["m:64k"+candidateScope]; ok {
		t.Fatal("the candidate's need was kept")
	}
}
