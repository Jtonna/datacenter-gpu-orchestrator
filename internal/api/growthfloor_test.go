package api

import (
	"context"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// shapeEvent is a llama unload event of a real shape on one node: settle
// 5,000 MiB, the given peak, 7,000 tokens held at ctx 8,192.
func shapeEvent(ts int64, profile, shape, node string, peak int) history.Event {
	e := unloadEvent(ts, profile, shape, true, map[string]*int{node: mib(peak)}, node)
	e.Data["cycle_peaks"].([]map[string]any)[0]["settle_use_mib"] = 5000
	e.SessionID = "s-" + profile
	e.Data["profile_kind"], e.Data["profile_ctx"], e.Data["profile_parallel"] = "llama", 8192, 1
	return e
}

// The highest growth reading for a shape counts the rows of the same model
// file, ctx, parallel, kv_quant, flash_attn and host, whatever the extra
// args, the profile name or the node set; capped readings are left out
// and counted.
func TestPeakCycles_HighestGrowthOfAShape(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	pc := &peakCycles{rows: map[string]*peakCycleRow{}, now: func() time.Time { return now }}
	pc.tokens = func(ctx context.Context, profile, session string, from, need, to int64) (usage.SessionTokens, error) {
		return usage.SessionTokens{MaxHeld: 7000, Counted: 3}, nil
	}
	base := needstore.ShapeKey("llama", "m.gguf", 8192, 1, "", false, nil)
	t0 := now.UnixMilli() - 60_000
	pc.addLocked(shapeEvent(t0, "written", base, "a", 5875))                                                                               // 128 KiB
	pc.addLocked(shapeEvent(t0+1, "bench-m-8k", needstore.ShapeKey("llama", "m.gguf", 8192, 1, "f16", false, []string{"--x"}), "b", 5700)) // 102.4 KiB, other args and node
	pc.addLocked(shapeEvent(t0+2, "huge", base, "c", 9000))                                                                                // 4,000 MiB over 7,000 tokens: 585.14 KiB
	pc.addLocked(shapeEvent(t0+3, "capped", base, "d", 15000))                                                                             // 1,463 KiB: capped
	pc.addLocked(shapeEvent(t0+4, "hosted", needstore.ShapeKeyHost("llama", "m.gguf", 8192, 1, "", false, nil, "node"), "a", 6000))        // host on a node: 146.29
	pc.addLocked(shapeEvent(t0+5, "other-ctx", needstore.ShapeKey("llama", "m.gguf", 4096, 1, "", false, nil), "a", 9000))
	pc.addLocked(shapeEvent(t0+6, "other-kv", needstore.ShapeKey("llama", "m.gguf", 8192, 1, "q8_0", true, nil), "a", 9000))
	pc.addLocked(shapeEvent(t0+7, "other-model", needstore.ShapeKey("llama", "n.gguf", 8192, 1, "", false, nil), "a", 9000))

	got := pc.highestGrowth(growthShape{Model: "m.gguf", Ctx: 8192, Parallel: 1})
	if got.KiBPerToken != 585.14 || got.Readings != 3 || got.CappedSkipped != 1 {
		t.Fatalf("server: %+v", got)
	}
	got = pc.highestGrowth(growthShape{Model: "m.gguf", Ctx: 8192, Parallel: 1, KVQuant: "f16", Host: "node"})
	if got.KiBPerToken != 146.29 || got.Readings != 1 || got.CappedSkipped != 0 {
		t.Fatalf("host on a node: %+v", got)
	}
	if got := pc.highestGrowth(growthShape{Model: "m.gguf", Ctx: 2048, Parallel: 1}); got.Readings != 0 || got.KiBPerToken != 0 {
		t.Fatalf("no history: %+v", got)
	}
	var nilPC *peakCycles
	if got := nilPC.highestGrowth(growthShape{Model: "m.gguf", Ctx: 8192}); got.Readings != 0 {
		t.Fatalf("no tally: %+v", got)
	}
}

// The plan answer carries growth_history for the planned shape and the
// fill_reserve_kib_per_token setting.
func TestPlan_CarriesGrowthHistoryAndTheSetting(t *testing.T) {
	e := newEnv(t, chatOK)
	_, v := e.cdo("POST", "/api/plan", `{"model":"alpha"}`, nil)
	gh, ok := v["growth_history"].(map[string]any)
	if !ok || gh["readings"] != 0.0 || gh["kib_per_token"] != 0.0 || gh["capped_skipped"] != 0.0 {
		t.Fatalf("growth_history %v", v["growth_history"])
	}
	if _, ok := v["fill_reserve_kib_per_token"].(float64); !ok {
		t.Fatalf("fill_reserve_kib_per_token %v", v["fill_reserve_kib_per_token"])
	}
	// A reading of alpha's shape (a.gguf, 4,096, parallel 1, f16, server):
	// 700 MiB over 3,500 tokens is 204.8 KiB per token.
	pc := e.api.peaks // the server's own tally, written under its lock
	pc.setTokens(func(ctx context.Context, profile, session string, from, need, to int64) (usage.SessionTokens, error) {
		return usage.SessionTokens{MaxHeld: 3500, Counted: 2}, nil
	})
	ev := shapeEvent(time.Now().UnixMilli()-60_000, "alpha", needstore.ShapeKey("llama", "a.gguf", 4096, 1, "", false, nil), "a", 5700)
	ev.Data["profile_ctx"] = 4096
	func() {
		pc.mu.Lock()
		defer pc.mu.Unlock()
		pc.addLocked(ev)
	}()
	_, v = e.cdo("POST", "/api/plan", `{"model":"alpha"}`, nil)
	gh, _ = v["growth_history"].(map[string]any)
	if gh["readings"] != 1.0 || gh["kib_per_token"] != 204.8 {
		t.Fatalf("growth_history %v", v["growth_history"])
	}
}
