package bench

import (
	"encoding/csv"
	"strings"
	"testing"
)

func exportRun() *Run {
	lp, dp := 12.0, -3.5
	return &Run{
		ID: "run-20261118T030000-ab12", SuiteID: "nightly", SuiteName: "Nightly", State: RunCompleted, BaselineRunID: "run-prev",
		QueuedAt: 1795060800000, StartedAt: 1795060801000, FinishedAt: 1795061000000, Version: "0.1.0",
		Steps: []Step{
			{Idx: 0, BenchmarkID: "gemma-cold", Name: "Gemma cold", Profile: "gemma-4-12b", State: StepDone, Outcome: OutcomeReady,
				Load:   &LoadResult{LoadS: 66.1, AvgMBs: 101.4, MemoryMiB: 7900.4, Nodes: []NodeLoad{{Node: "ps5-a"}, {Node: "ps5-b"}}},
				Stats:  &Stats{Requests: 3, Decode: &Summary{N: 3, Mean: 24.5, Stdev: 0.4}, Prefill: &Summary{N: 3, Mean: 210.2}, TTFT: &Summary{N: 3, Mean: 310}},
				Expect: &ExpectResult{Pass: true}, Comparison: &Comparison{Found: true, LoadPct: &lp, DecodePct: &dp}},
			{Idx: 1, BenchmarkID: "huge", Name: "Too big | pipe", Profile: "devstral-24b:24k", State: StepDone, Outcome: OutcomeRefused, Reason: "does not fit",
				HTTPStatus: 503, Expect: &ExpectResult{Pass: true}},
		},
	}
}

func TestMarkdownExport(t *testing.T) {
	r := exportRun()
	r.Steps = r.Steps[:2]
	md := ExportMarkdown(r)
	t.Log("\n" + md)
	for _, want := range []string{
		"# Nightly: run run-20261118T030000-ab12",
		"| Benchmark | Profile | Nodes | Load s | MB/s | Prefill tok/s | Decode tok/s | Decode sd | Memory MiB | Outcome | vs baseline |",
		"| Gemma cold | gemma-4-12b | ps5-a, ps5-b | 66.1 | 101.4 | 210.2 | 24.50 | 0.40 | 7900 | ready, passed | load +12.0%, decode -3.5% |",
		"Too big \\| pipe", "refused, passed", "baseline run-prev",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	// One table row per benchmark.
	rows := 0
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "| ") && !strings.HasPrefix(l, "| Benchmark") && !strings.HasPrefix(l, "| ---") {
			rows++
		}
	}
	if rows != 2 {
		t.Errorf("%d table rows, want 2", rows)
	}
}

func TestCSVExport(t *testing.T) {
	r := exportRun()
	r.Steps = r.Steps[:2]
	out, err := ExportCSV(r)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil || len(recs) != 3 {
		t.Fatalf("%v %q", err, out)
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	for _, h := range []string{"run_id", "benchmark", "profile", "nodes", "outcome", "pass", "load_s", "avg_mb_s", "decode_tps_mean", "decode_tps_stdev", "prefill_tps_mean", "ttft_ms_mean", "memory_mib", "load_vs_baseline_pct", "decode_vs_baseline_pct", "regression"} {
		if _, ok := col[h]; !ok {
			t.Errorf("column %s missing in %v", h, recs[0])
		}
	}
	if recs[1][col["benchmark"]] != "gemma-cold" || recs[1][col["load_s"]] != "66.1" || recs[1][col["decode_tps_mean"]] != "24.5" || recs[1][col["nodes"]] != "ps5-a;ps5-b" {
		t.Errorf("%v", recs[1])
	}
	if recs[2][col["outcome"]] != "refused" || recs[2][col["load_s"]] != "" {
		t.Errorf("%v", recs[2])
	}
}

func phasedRun() *Run {
	r := exportRun()
	ttft := 31.5
	held := 30.0
	still := true
	r.Steps = r.Steps[:1]
	r.Steps[0].ModelFile, r.Steps[0].NodeCount = "gemma.gguf", 2
	r.Steps[0].Phases = []PhaseResult{
		{Idx: 0, Type: "load", Label: "cold load", State: PhaseDone, Outcome: PhaseOK, DurationS: 66.2, Load: &LoadResult{LoadS: 66.1, AvgMBs: 101.4, Bytes: 6700000000, MemoryMiB: 7900}},
		{Idx: 1, Type: "conversation", State: PhaseDone, Outcome: PhaseOK, DurationS: 9, Stats: &Stats{Requests: 2, Decode: &Summary{N: 2, Mean: 20}},
			Turns: []TurnResult{{Turn: 1, Name: "turn-1", Status: 200, PromptTokens: 30, PromptN: 30, DecodeTPS: 20, PrefillTPS: 100, TTFTMS: &ttft, WallMS: 500},
				{Turn: 2, Name: "turn-2", Status: 200, PromptTokens: 90, PromptN: 20, CacheN: 70, DecodeTPS: 19, PrefillTPS: 120, WallMS: 450}}},
		{Idx: 2, Type: "hold", State: PhaseDone, Outcome: PhaseOK, DurationS: 31, HeldS: &held, LoadedAfterHold: &still},
		{Idx: 3, Type: "concurrent_requests", State: PhaseDone, Outcome: PhaseOK, DurationS: 4, Concurrent: &ConcurrentResult{N: 3, WallMS: 4200, Errors: 1}},
		{Idx: 4, Type: "unload", State: PhaseDone, Outcome: PhaseOK, DurationS: 2, Unload: &UnloadResult{Loaded: true, Gone: true, UnloadS: 1.84}},
	}
	return r
}

func TestExportsAreAwareOfPhases(t *testing.T) {
	r := phasedRun()
	md := ExportMarkdown(r)
	t.Log("\n" + md)
	for _, want := range []string{
		"## Gemma cold phases", "| 1 | cold load (load) | done | 66.2 | load 66.1 s, 101.4 MB/s, 6.70 GB, 7900 MiB |",
		"Conversation (phase 2):", "| 2 | 90 | 70 | 120.0 | 19.00 | - | 450.0 |", "idle 30.0 s", "3 together, 4.2 s, 1 errors", "gone after 1.84 s",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	out, err := ExportCSV(r)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := csv.NewReader(strings.NewReader(out)).ReadAll()
	last := recs[1][len(recs[1])-3:]
	if recs[0][len(recs[0])-1] != "phases" || last[0] != "gemma.gguf" || last[1] != "2" || last[2] != "load:done;conversation:done;hold:done;concurrent_requests:done;unload:done" {
		t.Fatalf("%v", recs[1])
	}
	pcsv, err := ExportCSVPhases(r)
	if err != nil {
		t.Fatal(err)
	}
	recs, err = csv.NewReader(strings.NewReader(pcsv)).ReadAll()
	if err != nil || len(recs) != 6 || recs[1][5] != "load" || recs[1][10] != "66.1" || recs[5][19] != "1.84" || recs[4][22] != "1" {
		t.Fatalf("%v %v", recs, err)
	}
	tcsv, err := ExportCSVTurns(r)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ = csv.NewReader(strings.NewReader(tcsv)).ReadAll()
	if len(recs) != 3 || recs[2][8] != "70" || recs[1][11] != "31.5" || recs[2][11] != "" {
		t.Fatalf("%v", recs)
	}
}
