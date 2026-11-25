package bench

import (
	"testing"
)

// ResultsByModel: the newest finished step per model file and per zone
// count, its figures from the measurement record (first load = cold load,
// a later load = reload, request phases only for speed and first token,
// steady generation preferred), a figure not measured left out, and the
// smallest fit from every run.
func TestResultsByModel(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run := func(id string, at int64) {
		exec(`INSERT INTO runs (id, suite_id, suite_name, state, queued_at, started_at, finished_at, suite) VALUES (?,?,?,?,?,?,?,'{}')`, id, "g", "G", "completed", at, at, at+1000)
	}
	stepID := int64(0)
	step := func(runID string, idx int, model string, nodes int, outcome, reason string, at int64) int64 {
		exec(`INSERT INTO run_steps (run_id, idx, benchmark_id, state, outcome, reason, started_at, finished_at, model_file, node_count) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			runID, idx, "cold-response", "completed", outcome, reason, at, at+500, model, nodes)
		stepID++
		return stepID
	}
	meas := func(sid int64, runID string, idx, phase int, ptype, node, metric string, v float64, cond string, ctx int) {
		var c any
		if cond != "" {
			c = cond
		}
		exec(`INSERT INTO measurements (run_id, step_id, step_idx, phase_idx, phase_type, benchmark_id, model, node, metric, value, unit, conditions, ctx) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			runID, sid, idx, phase, ptype, "cold-response", "", node, metric, v, "x", c, ctx)
	}
	// an old run: gemma on 2 zones (its load completed: the smallest fit)
	run("r1", 1000)
	s1 := step("r1", 0, "gemma.gguf", 2, "ready", "", 1000)
	meas(s1, "r1", 0, 0, "load", "", "load_s", 40, `{"cold":true,"cold_verified":true}`, 8192)
	// the newest run: gemma on 6 zones with every figure; gemma on 1 zone
	// refused; mixtral on 4 zones failed at its load
	run("r2", 5000)
	s2 := step("r2", 0, "gemma.gguf", 6, "ready", "", 5000)
	meas(s2, "r2", 0, 0, "load", "", "load_s", 21.1, `{"cold":true,"cold_verified":true}`, 8192)
	meas(s2, "r2", 0, 0, "load", "", "memory_mib", 9000, "", 8192)
	meas(s2, "r2", 0, 0, "load", "", "memory_mib", 9100, "", 8192)
	meas(s2, "r2", 0, 1, "warmup", "", "ttft_ms", 2340, "", 8192)
	meas(s2, "r2", 0, 1, "warmup", "", "decode_tps", 5, "", 8192)
	meas(s2, "r2", 0, 2, "requests", "", "ttft_ms", 1000, "", 8192)
	meas(s2, "r2", 0, 2, "requests", "", "ttft_ms", 2000, "", 8192)
	meas(s2, "r2", 0, 2, "requests", "", "decode_tps", 8, `{"generation":"first"}`, 8192)
	meas(s2, "r2", 0, 2, "requests", "", "decode_tps", 10, `{"generation":"steady"}`, 8192)
	meas(s2, "r2", 0, 2, "requests", "", "decode_tps", 12, `{"generation":"steady"}`, 8192)
	meas(s2, "r2", 0, 4, "load", "", "load_s", 5.8, `{}`, 8192)
	// lowest available: only machines the step used (its roles), system
	// memory only: ra-03 (not in the step) and the GPU figure are left out
	exec(`UPDATE run_steps SET roles=? WHERE id=?`, `[{"node":"host-1","role":"host"},{"node":"ra-01","role":"worker"},{"node":"ra-02","role":"worker"}]`, s2)
	meas(s2, "r2", 0, -1, "", "ra-01", "lowest_available_mib", 700, `{"memory":"system","window":"step"}`, 8192)
	meas(s2, "r2", 0, -1, "", "ra-02", "lowest_available_mib", 500, `{"memory":"system","window":"step"}`, 8192)
	meas(s2, "r2", 0, -1, "", "ra-03", "lowest_available_mib", 100, `{"memory":"system","window":"step"}`, 8192)
	meas(s2, "r2", 0, -1, "", "host-1", "lowest_available_mib", 300, `{"memory":"gpu","window":"step"}`, 8192)
	// the old step has no roles: no lowest figure (a dash), whatever was sampled
	meas(s1, "r1", 0, -1, "", "ra-09", "lowest_available_mib", 8149, `{"memory":"system","window":"step"}`, 8192)
	step("r2", 1, "gemma.gguf", 1, "refused", "Does not fit on 1 zone", 6000)
	s4 := step("r2", 2, "mixtral.gguf", 4, "failed", "child exited during load", 7000)
	_ = s4
	// a running step and a skipped one are not results
	step("r2", 3, "qwen.gguf", 2, "", "", 8000)
	step("r2", 4, "phi.gguf", 2, "skipped", "", 8000)

	got, err := st.ResultsByModel(ResultsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Model != "gemma.gguf" || got[1].Model != "mixtral.gguf" {
		t.Fatalf("models %+v", got)
	}
	g := got[0]
	if g.SmallestFit != 2 {
		t.Fatalf("smallest fit %d, want 2 (the old run's completed load)", g.SmallestFit)
	}
	if g.Latest == nil || g.Latest.RunID != "r2" || g.Latest.Step != 1 || g.Latest.Outcome != "refused" || g.Latest.Reason != "Does not fit on 1 zone" || g.Latest.ColdLoadS != nil {
		t.Fatalf("latest %+v", g.Latest)
	}
	if len(g.ByNodes) != 3 || g.ByNodes[0].NodeCount != 1 || g.ByNodes[1].NodeCount != 2 || g.ByNodes[2].NodeCount != 6 {
		t.Fatalf("by nodes %+v", g.ByNodes)
	}
	two, six := g.ByNodes[1], g.ByNodes[2]
	if two.RunID != "r1" || two.ColdLoadS == nil || *two.ColdLoadS != 40 || two.ReloadS != nil || two.DecodeTPS != nil || two.TTFTMs != nil || two.LowestAvailableMiB != nil {
		t.Fatalf("2 zones %+v", two)
	}
	f := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	if f(six.ColdLoadS) != 21.1 || six.Cold == nil || !*six.Cold || six.ColdVerified == nil || !*six.ColdVerified || f(six.ReloadS) != 5.8 ||
		f(six.DecodeTPS) != 11 || f(six.TTFTMs) != 1500 || f(six.MemoryMiB) != 9100 || f(six.LowestAvailableMiB) != 500 || six.LowestNode != "ra-02" || six.Ctx != 8192 {
		t.Fatalf("6 zones: cold %v %v %v reload %v tps %v ttft %v mem %v low %v ctx %d", f(six.ColdLoadS), six.Cold, six.ColdVerified, f(six.ReloadS), f(six.DecodeTPS), f(six.TTFTMs),
			f(six.MemoryMiB), f(six.LowestAvailableMiB), six.Ctx)
	}
	m := got[1]
	if m.SmallestFit != 0 || m.Latest.Outcome != "failed" || m.Latest.ColdLoadS != nil || len(m.ByNodes) != 1 {
		t.Fatalf("mixtral %+v", m)
	}
	// ctx: only steps measured at that context
	at, err := st.ResultsByModel(ResultsFilter{Ctx: 4096})
	if err != nil || len(at) != 0 {
		t.Fatalf("ctx 4096: %+v %v", at, err)
	}
	at, _ = st.ResultsByModel(ResultsFilter{Ctx: 8192})
	if len(at) != 1 || at[0].Latest.RunID != "r2" || at[0].Latest.Step != 0 {
		t.Fatalf("ctx 8192: %+v", at)
	}
}
