package api

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// The cold load in the web UI: a load step's line in the benchmark report
// ("cold verified", or "cold not verified: <the server's reason>") and the
// group's evict_others option.
// Every key the UI reads must be one the server writes.
func TestUIColdLoadKeysMatchTheServer(t *testing.T) {
	ttft := 812.0
	step := jsonKeys(t, bench.Step{
		Phases: []bench.PhaseResult{{Idx: 0, Type: "load", Label: "cold load", Load: &bench.LoadResult{LoadS: 1},
			Cold: &bench.ColdResult{Requested: true, Reason: "not guaranteed: x", Local: &bench.ColdFigures{FileBytes: 1, Error: "e"},
				Nodes: []bench.ColdNode{{Node: "n", ID: "i"}}}},
			{Idx: 2, Type: "requests", Stats: &bench.Stats{Decode: &bench.Summary{Mean: 1}}}},
		Requests: []bench.RequestResult{{PhaseIdx: 1, TTFTMS: &ttft}},
	})
	yes := true
	suite := jsonKeys(t, benchspec.Suite{ID: "s", EvictOthers: &yes})
	runs := uiText(t, uiNextDir+"views/bench/runs.js")
	for _, k := range []string{"cold", "requested", "verified", "reason", "error",
		"phases", "idx", "type", "label", "load", "load_s", "ttft_ms", "stats", "decode_tps", "mean"} {
		if !strings.Contains(step, `"`+k+`"`) {
			t.Errorf("the server does not write %q", k)
		}
		if !strings.Contains(runs, k) {
			t.Errorf("runs.js does not read %q", k)
		}
	}
	for _, want := range []string{"'cold verified'", "'cold not verified'", "ph.cold.reason"} {
		if !strings.Contains(runs, want) {
			t.Errorf("runs.js lacks %s", want)
		}
	}
	if !strings.Contains(suite, `"evict_others"`) || !strings.Contains(uiText(t, uiNextDir+"views/bench/common.js"), "g.evict_others") {
		t.Errorf("evict_others: %s", suite)
	}
}
