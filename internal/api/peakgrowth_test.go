package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// growthEvent is an unload event with settle_use_mib, the profile's kind,
// ctx and parallel, and a load session.
func growthEvent(ts int64, session, kind string, ctx, parallel int, settle, peak *int) history.Event {
	e := unloadEvent(ts, "m", "shape-1", true, map[string]*int{"a": peak}, "a")
	cp := e.Data["cycle_peaks"].([]map[string]any)
	if settle != nil {
		cp[0]["settle_use_mib"] = *settle
	} else {
		cp[0]["settle_use_mib"], cp[0]["no_settle"] = nil, "the free figure was still moving at settle time"
	}
	e.SessionID = session
	e.Data["profile_kind"], e.Data["profile_ctx"], e.Data["profile_parallel"] = kind, ctx, parallel
	return e
}

// peak_cycles growth_readings (B8 F2, collected only): the growth from the
// settle use to the peak over the most tokens one request of the cycle's
// session held, with the guards; parallel > 1, a trimmed log, too few
// tokens or a missing figure give the reason instead.
func TestPeakCycles_GrowthReadings(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	pc := &peakCycles{rows: map[string]*peakCycleRow{}, now: func() time.Time { return now }}
	var calls []string
	pc.tokens = func(ctx context.Context, profile, session string, from, need, to int64) (usage.SessionTokens, error) {
		calls = append(calls, fmt.Sprintf("%s/%s %d %d %d", profile, session, from, need, to))
		switch session {
		case "s-full":
			return usage.SessionTokens{MaxHeld: 7000, Counted: 4}, nil
		case "s-short":
			return usage.SessionTokens{MaxHeld: 1000, Counted: 2}, nil
		case "s-big":
			return usage.SessionTokens{MaxHeld: 2048, Counted: 1}, nil
		}
		return usage.SessionTokens{Trimmed: true}, nil
	}
	t0 := now.UnixMilli() - 60_000
	pc.addLocked(growthEvent(t0, "s-full", "llama", 8192, 1, mib(5000), mib(5875)))                   // 875 MiB over 7,000 tokens: 128 KiB
	pc.addLocked(growthEvent(t0+1, "s-short", "llama", 8192, 1, mib(5000), mib(5100)))                // under a quarter of the context
	pc.addLocked(growthEvent(t0+2, "s-old", "llama", 8192, 1, mib(5000), mib(5100)))                  // the log no longer holds it
	pc.addLocked(growthEvent(t0+3, "s-full", "llama", 8192, 2, mib(5000), mib(5875)))                 // parallel 2
	pc.addLocked(growthEvent(t0+4, "s-full", "llama", 8192, 1, nil, mib(5875)))                       // no settle figure
	pc.addLocked(growthEvent(t0+5, "s-big", "llama", 8192, 1, mib(5000), mib(9000)))                  // 2,000 KiB: capped
	pc.addLocked(growthEvent(now.UnixMilli()-1000, "s-full", "llama", 8192, 1, mib(5000), mib(5875))) // too new
	rows := pc.view()["rows"].([]map[string]any)
	if len(rows) != 1 {
		t.Fatalf("%v", rows)
	}
	gr := rows[0]["growth_readings"].([]map[string]any)
	if len(gr) != 7 {
		t.Fatalf("%d readings: %v", len(gr), gr)
	}
	// newest first
	want := []string{growthPending, "", growthNoFigures, growthParallel, growthTrimmed, growthFewTokens, ""}
	for i, w := range want {
		if got, _ := gr[i]["not_computed"].(string); got != w {
			t.Errorf("reading %d: %q, want %q (%v)", i, got, w, gr[i])
		}
	}
	full := gr[6]["nodes"].([]map[string]any)[0]
	if full["kib_per_token"] != 128.0 || full["growth_mib"] != 875 || full["tokens"] != 7000 || full["ctx_share"] != 0.854 || full["capped"] != false || full["node"] != "a" {
		t.Errorf("reading %v", full)
	}
	big := gr[1]["nodes"].([]map[string]any)[0]
	if big["kib_per_token"] != 1024.0 || big["capped"] != true {
		t.Errorf("capped %v", big)
	}
	// The log is read once per cycle (not for parallel 2, nor the too-new one).
	n := len(calls)
	pc.view()
	if len(calls) != n || n != 5 {
		t.Errorf("log reads %d then %d: %v", n, len(calls), calls)
	}
	if calls[len(calls)-1] != fmt.Sprintf("m/s-full %d %d %d", t0-60_000-3600_000, t0-60_000, t0) { // the oldest is read last
		t.Errorf("window %s", calls[len(calls)-1])
	}
}

// An unload event from before the profile's kind was stored (no
// profile_kind, profile_ctx or profile_parallel) says so; it is not a
// non-llama profile, and the request log is not read for it. A non-llama
// event still says it is not a llama profile.
func TestPeakCycles_GrowthReadings_EventBeforeTheKindWasStored(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	pc := &peakCycles{rows: map[string]*peakCycleRow{}, now: func() time.Time { return now }}
	reads := 0
	pc.tokens = func(ctx context.Context, profile, session string, from, need, to int64) (usage.SessionTokens, error) {
		reads++
		return usage.SessionTokens{MaxHeld: 7000, Counted: 4}, nil
	}
	t0 := now.UnixMilli() - 60_000
	old := growthEvent(t0, "s-old-shape", "", 0, 0, mib(5000), mib(5875))
	delete(old.Data, "profile_kind")
	delete(old.Data, "profile_ctx")
	delete(old.Data, "profile_parallel")
	pc.addLocked(old)
	pc.addLocked(growthEvent(t0+1, "s-cmd", "command", 8192, 1, mib(5000), mib(5875)))
	rows := pc.view()["rows"].([]map[string]any)
	gr := rows[0]["growth_readings"].([]map[string]any)
	if len(gr) != 2 {
		t.Fatalf("%v", gr)
	}
	if got := gr[1]["not_computed"]; got != growthBeforeKind || got == growthNotLlama {
		t.Errorf("old-shape event: %v, want %q", got, growthBeforeKind)
	}
	if growthBeforeKind != "not computed: recorded before the profile's kind was stored" {
		t.Errorf("sentence %q", growthBeforeKind)
	}
	if got := gr[0]["not_computed"]; got != growthNotLlama {
		t.Errorf("command profile: %v", got)
	}
	if reads != 0 {
		t.Errorf("the log was read %d times", reads)
	}
}
