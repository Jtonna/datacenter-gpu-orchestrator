package bench

import (
	"math"
	"strings"
	"testing"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestSummarize(t *testing.T) {
	s := Summarize([]float64{10, 20, 30, 40})
	if s.N != 4 || !near(s.Mean, 25) || !near(s.Median, 25) || s.Min != 10 || s.Max != 40 || !near(s.Stdev, 12.909944487358056) {
		t.Fatalf("%+v", s)
	}
	s = Summarize([]float64{5, 1, 3})
	if !near(s.Median, 3) || !near(s.Mean, 3) {
		t.Fatalf("%+v", s)
	}
	if s := Summarize([]float64{7}); s.N != 1 || s.Stdev != 0 || s.Mean != 7 {
		t.Fatalf("%+v", s)
	}
	if Summarize(nil) != nil {
		t.Fatal("empty series should have no summary")
	}
}

func req(name, phase string, status int, decode, prefill float64, hash string) RequestResult {
	return RequestResult{Name: name, Phase: phase, Status: status, DecodeTPS: decode, PromptTPS: prefill, WallMS: 100, OutputSHA256: hash, Deterministic: true}
}

func TestComputeStatsExcludesWarmupAndFailures(t *testing.T) {
	ttft := 50.0
	r3 := req("a", PhaseMeasured, 200, 30, 200, "h1")
	r3.TTFTMS = &ttft
	reqs := []RequestResult{
		req("a", PhaseWarmup, 200, 5, 20, "h0"), // slow first request: must not count
		req("a", PhaseMeasured, 200, 20, 100, "h1"),
		req("a", PhaseMeasured, 200, 40, 300, "h1"),
		r3,
		req("a", PhaseMeasured, 503, 0, 0, ""),
	}
	st := ComputeStats(reqs)
	if st.Requests != 4 || st.Failed != 1 {
		t.Fatalf("counts %+v", st)
	}
	if st.Decode.N != 3 || !near(st.Decode.Mean, 30) || st.Decode.Min != 20 || st.Decode.Max != 40 {
		t.Fatalf("decode %+v (warmup leaked in?)", st.Decode)
	}
	if st.Prefill.N != 3 || !near(st.Prefill.Mean, 200) {
		t.Fatalf("prefill %+v", st.Prefill)
	}
	if st.TTFT == nil || st.TTFT.N != 1 || st.TTFT.Mean != 50 {
		t.Fatalf("ttft %+v", st.TTFT)
	}
	if st.OutputHashes["a"] != "h1" {
		t.Fatalf("hashes %+v", st.OutputHashes)
	}
	reqs = append(reqs, req("a", PhaseMeasured, 200, 30, 200, "h2"))
	if got := ComputeStats(reqs).OutputHashes["a"]; got != "varies" {
		t.Fatalf("differing outputs for one deterministic request: %q", got)
	}
}

func okStep(load float64, decode float64, hash string) *Step {
	return &Step{BenchmarkID: "a", State: StepDone, Outcome: OutcomeReady,
		Load:  &LoadResult{LoadS: load},
		Stats: &Stats{Decode: &Summary{N: 3, Mean: decode}, Prefill: &Summary{N: 3, Mean: 200}, OutputHashes: map[string]string{"q": hash}}}
}

func TestEvaluateExpect(t *testing.T) {
	spec := func(js string) *benchspec.Benchmark {
		b, err := benchspec.ParseBenchmarkJSON([]byte(`{"id":"a","profile":"p","requests":[{"name":"q","prompt":"x","max_tokens":4}],` + js + `}`))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	st := okStep(60, 25, "abc")
	if r := Evaluate(spec(`"name":"n"`), st); !r.Pass || len(r.Checks) != 1 || r.Checks[0].Name != "outcome" {
		t.Fatalf("default expectation is a ready load: %+v", r)
	}
	if r := Evaluate(spec(`"expect":{"max_load_s":50}`), st); r.Pass {
		t.Fatalf("slow load passed: %+v", r)
	}
	if r := Evaluate(spec(`"expect":{"max_load_s":70,"min_decode_tps":20}`), st); !r.Pass || len(r.Checks) != 3 {
		t.Fatalf("%+v", r)
	}
	if r := Evaluate(spec(`"expect":{"min_decode_tps":30}`), st); r.Pass {
		t.Fatalf("slow decode passed: %+v", r)
	}
	h := strings.Repeat("a", 64)
	if r := Evaluate(spec(`"expect":{"output_sha256":"`+h+`"}`), okStep(1, 1, h)); !r.Pass {
		t.Fatalf("%+v", r)
	}
	if r := Evaluate(spec(`"expect":{"output_sha256":"`+h+`"}`), okStep(1, 1, "zzz")); r.Pass {
		t.Fatalf("wrong hash passed: %+v", r)
	}
	// A benchmark that is supposed to be refused passes when the scheduler refuses.
	refused := &Step{Outcome: OutcomeRefused, HTTPStatus: 503, Reason: "does not fit"}
	if r := Evaluate(spec(`"expect":{"outcome":"refused"}`), refused); !r.Pass {
		t.Fatalf("expected refusal failed: %+v", r)
	}
	if r := Evaluate(spec(`"name":"n"`), refused); r.Pass {
		t.Fatalf("an unexpected refusal passed: %+v", r)
	}
	if r := Evaluate(spec(`"expect":{"outcome":"refused"}`), st); r.Pass {
		t.Fatalf("a load that worked passed as refused: %+v", r)
	}
	if r := Evaluate(spec(`"expect":{"outcome":"failed"}`), &Step{Outcome: OutcomeFailed}); !r.Pass {
		t.Fatalf("%+v", r)
	}
	if r := Evaluate(spec(`"name":"n"`), &Step{Outcome: OutcomeError, Error: "dial tcp"}); r.Pass {
		t.Fatalf("a runner error passed: %+v", r)
	}
}

func TestCompareFlagsRegressions(t *testing.T) {
	th := Thresholds{LoadPct: 15, DecodePct: 7}
	base := okStep(60, 30, "h")
	// Within the thresholds.
	c := Compare(okStep(66, 29, "h"), base, "base-1", th)
	if !c.Found || c.Regression || !near(*c.LoadPct, 10) || !near(*c.DecodePct, (29.0-30)/30*100) || c.BaselineRunID != "base-1" {
		t.Fatalf("%+v", c)
	}
	// Load more than 15% slower.
	c = Compare(okStep(70, 30, "h"), base, "b", th)
	if !c.Regression || len(c.Reasons) != 1 || !strings.Contains(c.Reasons[0], "load") {
		t.Fatalf("%+v", c)
	}
	// Decode more than 7% slower.
	c = Compare(okStep(60, 27.5, "h"), base, "b", th)
	if !c.Regression || !strings.Contains(c.Reasons[0], "decode") {
		t.Fatalf("%+v", c)
	}
	// Faster is never a regression.
	c = Compare(okStep(40, 45, "h"), base, "b", th)
	if c.Regression || *c.LoadPct >= 0 || *c.DecodePct <= 0 {
		t.Fatalf("%+v", c)
	}
	// A different deterministic output is a regression, even when the speed is the same.
	c = Compare(okStep(60, 30, "other"), base, "b", th)
	if !c.Regression || c.HashDiffers == nil || !*c.HashDiffers || !strings.Contains(strings.Join(c.Reasons, ";"), "output") {
		t.Fatalf("%+v", c)
	}
	// "varies" on either side is not compared.
	c = Compare(okStep(60, 30, "varies"), base, "b", th)
	if c.Regression || (c.HashDiffers != nil && *c.HashDiffers) {
		t.Fatalf("%+v", c)
	}
	// Nothing to compare against.
	if c := Compare(okStep(60, 30, "h"), nil, "b", th); c.Found || c.Regression {
		t.Fatalf("%+v", c)
	}
	if c := Compare(okStep(60, 30, "h"), &Step{Outcome: OutcomeRefused}, "b", th); c.Found {
		t.Fatalf("%+v", c)
	}
	// A step that stopped being ready when the baseline was ready is a regression.
	c = Compare(&Step{Outcome: OutcomeFailed, Reason: "child exited"}, base, "b", th)
	if !c.Found || !c.Regression {
		t.Fatalf("%+v", c)
	}
}
