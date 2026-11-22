package api

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// The cold load in the web UI: the badge and machine table of a cold load
// phase, the cold-and-reload result row, and the suite's evict_others field.
// Every key the UI reads must be one the server writes.
func TestUIColdLoadKeysMatchTheServer(t *testing.T) {
	ttft := 812.0
	step := jsonKeys(t, bench.Step{
		Phases: []bench.PhaseResult{{Idx: 0, Type: "load", Load: &bench.LoadResult{LoadS: 1},
			Cold: &bench.ColdResult{Requested: true, Reason: "not guaranteed: x", Local: &bench.ColdFigures{FileBytes: 1, Error: "e"},
				Nodes: []bench.ColdNode{{Node: "n", ID: "i"}}}},
			{Idx: 2, Type: "requests", Stats: &bench.Stats{Decode: &bench.Summary{Mean: 1}}}},
		Requests: []bench.RequestResult{{PhaseIdx: 1, TTFTMS: &ttft}},
	})
	yes := true
	suite := jsonKeys(t, benchspec.Suite{ID: "s", EvictOthers: &yes})
	runs := uiText(t, "views/benchmarks/runs.js")
	for _, k := range []string{"cold", "verified", "reason", "local", "nodes", "node", "id", "file_bytes", "cached_before", "cached_after", "error",
		"phases", "idx", "type", "load", "load_s", "requests", "phase_idx", "ttft_ms", "stats", "decode_tps", "mean"} {
		if !strings.Contains(step, `"`+k+`"`) {
			t.Errorf("the server does not write %q", k)
		}
		if !strings.Contains(runs, k) {
			t.Errorf("runs.js does not read %q", k)
		}
	}
	for _, want := range []string{"'cold: verified'", "'cold: not guaranteed'", "title: why", "coldSummary(step)", "coldBadge(p.cold)"} {
		if !strings.Contains(runs, want) {
			t.Errorf("runs.js lacks %s", want)
		}
	}
	if !strings.Contains(suite, `"evict_others"`) || !strings.Contains(uiText(t, "views/benchmarks/editor.js"), "key: 'evict_others'") {
		t.Errorf("evict_others: %s", suite)
	}
}
