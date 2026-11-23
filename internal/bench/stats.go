package bench

import (
	"fmt"
	"math"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// Summarize returns mean, median, min, max and the sample standard deviation
// of xs, or nil for an empty series.
func Summarize(xs []float64) *Summary {
	if len(xs) == 0 {
		return nil
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	n := float64(len(s))
	mean := sum / n
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	var sd float64
	if len(s) > 1 {
		var ss float64
		for _, x := range s {
			ss += (x - mean) * (x - mean)
		}
		sd = math.Sqrt(ss / (n - 1))
	}
	return &Summary{N: len(s), Mean: mean, Median: med, Min: s[0], Max: s[len(s)-1], Stdev: sd}
}

// statsKinds is the order in which the kinds of request rows speak for a step:
// the measured requests first, and, for a benchmark that has none, the
// conversation turns, then the request after a hold, then the simultaneous ones.
var statsKinds = []string{PhaseMeasured, PhaseConversation, PhaseHold, PhaseConcurrent}

// ComputeStats works out the statistics of one step from its measured
// requests (or, when it has none, the first other kind that it has). Warmup
// requests are never included.
func ComputeStats(reqs []RequestResult) *Stats {
	for _, kind := range statsKinds {
		var rows []RequestResult
		for _, r := range reqs {
			if r.Phase == kind {
				rows = append(rows, r)
			}
		}
		if len(rows) > 0 {
			return statsOf(rows)
		}
	}
	return &Stats{}
}

// statsOf works out the statistics of the given request rows, whatever their phase.
func statsOf(reqs []RequestResult) *Stats {
	st := &Stats{}
	var decode, prefill, ttft, wall []float64
	hashes := map[string]map[string]bool{}
	for _, r := range reqs {
		st.Requests++
		if r.Status != 200 {
			st.Failed++
			continue
		}
		if r.DecodeTPS > 0 {
			decode = append(decode, r.DecodeTPS)
		}
		if r.PromptTPS > 0 {
			prefill = append(prefill, r.PromptTPS)
		}
		if r.TTFTMS != nil {
			ttft = append(ttft, *r.TTFTMS)
		}
		wall = append(wall, r.WallMS)
		if r.Deterministic && r.OutputSHA256 != "" {
			if hashes[r.Name] == nil {
				hashes[r.Name] = map[string]bool{}
			}
			hashes[r.Name][r.OutputSHA256] = true
		}
	}
	st.Decode, st.Prefill, st.TTFT, st.WallMS = Summarize(decode), Summarize(prefill), Summarize(ttft), Summarize(wall)
	if len(hashes) > 0 {
		st.OutputHashes = map[string]string{}
		for name, set := range hashes {
			if len(set) == 1 {
				for h := range set {
					st.OutputHashes[name] = h
				}
			} else {
				st.OutputHashes[name] = "varies"
			}
		}
	}
	return st
}

// Evaluate checks a finished step against the benchmark's expect block. With
// no expect block the step is expected to load and serve (outcome ready).
func Evaluate(spec *benchspec.Benchmark, st *Step) *ExpectResult {
	res := &ExpectResult{Pass: true}
	add := func(name string, pass bool, detail string) {
		res.Checks = append(res.Checks, Check{Name: name, Pass: pass, Detail: detail})
		if !pass {
			res.Pass = false
		}
	}
	want := spec.Expect.Want()
	got := st.Outcome
	if got == OutcomeRefusedByExclusion {
		// Not judged: the step did not run, on purpose (see RunSummary.Refused).
		res.Pass, res.NotJudged = false, true
		res.Checks = append(res.Checks, Check{Name: "outcome", Pass: false, Detail: "not judged: refused by an exclusion (" + st.Reason + ")"})
		return res
	}
	switch {
	case got == OutcomeError || got == OutcomeCancelled || got == OutcomeSkipped:
		add("outcome", false, fmt.Sprintf("wanted %s, the step ended %s", want, got))
	case got == want:
		add("outcome", true, "the scheduler answered as expected: "+got)
	default:
		d := fmt.Sprintf("wanted %s, got %s", want, got)
		if st.Reason != "" {
			d += " (" + st.Reason + ")"
		}
		add("outcome", false, d)
	}
	e := spec.Expect
	if e == nil || want != benchspec.OutcomeReady {
		return res
	}
	evaluateMetrics(spec, st, e, add)
	if e.MaxLoadS != nil {
		switch {
		case st.Load == nil:
			add("max_load_s", false, "no load time was measured")
		case st.Load.LoadS > *e.MaxLoadS:
			add("max_load_s", false, fmt.Sprintf("load took %.1f s, limit %.1f s", st.Load.LoadS, *e.MaxLoadS))
		default:
			add("max_load_s", true, fmt.Sprintf("load took %.1f s, limit %.1f s", st.Load.LoadS, *e.MaxLoadS))
		}
	}
	if e.MinDecodeTPS != nil {
		switch {
		case st.Stats == nil || st.Stats.Decode == nil:
			add("min_decode_tps", false, "no decode speed was measured")
		case st.Stats.Decode.Mean < *e.MinDecodeTPS:
			add("min_decode_tps", false, fmt.Sprintf("decode %.2f tok/s, minimum %.2f", st.Stats.Decode.Mean, *e.MinDecodeTPS))
		default:
			add("min_decode_tps", true, fmt.Sprintf("decode %.2f tok/s, minimum %.2f", st.Stats.Decode.Mean, *e.MinDecodeTPS))
		}
	}
	if e.OutputSHA256 != "" {
		first := ""
		for _, p := range spec.Expand() {
			if p.Type == benchspec.PhaseRequests && len(p.Requests) > 0 && st.Stats != nil {
				first = st.Stats.OutputHashes[p.Requests[0].Name]
				break
			}
		}
		switch {
		case first == "":
			add("output_sha256", false, "the first request produced no output hash")
		case first != e.OutputSHA256:
			add("output_sha256", false, fmt.Sprintf("output hash %s differs from the expected %s", short(first), short(e.OutputSHA256)))
		default:
			add("output_sha256", true, "output matches the pinned hash")
		}
	}
	return res
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// Thresholds are the regression limits (bench.regress_load_pct, bench.regress_decode_pct).
type Thresholds struct {
	LoadPct   float64
	DecodePct float64
}

func pct(cur, base float64) *float64 {
	if base == 0 {
		return nil
	}
	v := (cur - base) / base * 100
	return &v
}

// Compare sets a step against the same benchmark in the baseline run. Load
// time is compared in percent more (positive means slower), speeds in percent
// change (negative means slower). Regression is set when the load is more than
// LoadPct slower, decode more than DecodePct slower, a deterministic request's
// output hash differs, or a benchmark that loaded in the baseline no longer does.
func Compare(cur, base *Step, baselineRunID string, th Thresholds) *Comparison {
	c := &Comparison{BaselineRunID: baselineRunID}
	if base == nil || base.Outcome != OutcomeReady {
		return c
	}
	c.Found = true
	if cur.Outcome == OutcomeRefusedByExclusion {
		// Not judged, so not a regression either: the configuration keeps
		// the profile's forced node out on purpose.
		c.Reasons = append(c.Reasons, "not compared: refused by an exclusion")
		return c
	}
	if cur.Outcome != OutcomeReady {
		c.Regression = true
		why := "the baseline loaded and served; this run's outcome is " + cur.Outcome
		if cur.Reason != "" {
			why += " (" + cur.Reason + ")"
		}
		c.Reasons = append(c.Reasons, why)
		return c
	}
	if cur.Load != nil && base.Load != nil {
		c.LoadPct = pct(cur.Load.LoadS, base.Load.LoadS)
		if c.LoadPct != nil && *c.LoadPct > th.LoadPct {
			c.Regression = true
			c.Reasons = append(c.Reasons, fmt.Sprintf("load time %.1f s is %.1f%% slower than %.1f s (limit %.0f%%)", cur.Load.LoadS, *c.LoadPct, base.Load.LoadS, th.LoadPct))
		}
	}
	if cur.Stats != nil && base.Stats != nil {
		if cur.Stats.Decode != nil && base.Stats.Decode != nil {
			c.DecodePct = pct(cur.Stats.Decode.Mean, base.Stats.Decode.Mean)
			if c.DecodePct != nil && -*c.DecodePct > th.DecodePct {
				c.Regression = true
				c.Reasons = append(c.Reasons, fmt.Sprintf("decode %.2f tok/s is %.1f%% slower than %.2f (limit %.0f%%)", cur.Stats.Decode.Mean, -*c.DecodePct, base.Stats.Decode.Mean, th.DecodePct))
			}
		}
		if cur.Stats.Prefill != nil && base.Stats.Prefill != nil {
			c.PrefillPct = pct(cur.Stats.Prefill.Mean, base.Stats.Prefill.Mean)
		}
		if cur.Stats.TTFT != nil && base.Stats.TTFT != nil {
			c.TTFTPct = pct(cur.Stats.TTFT.Mean, base.Stats.TTFT.Mean)
		}
		var names []string
		for n := range cur.Stats.OutputHashes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a, b := cur.Stats.OutputHashes[n], base.Stats.OutputHashes[n]
			if a == "" || b == "" || a == "varies" || b == "varies" {
				continue
			}
			differs := a != b
			if c.HashDiffers == nil || differs {
				c.HashDiffers = &differs
			}
			if differs {
				c.Regression = true
				c.Reasons = append(c.Reasons, fmt.Sprintf("output of request %q changed (hash %s, baseline %s)", n, short(a), short(b)))
			}
		}
	}
	return c
}

// evaluateMetrics checks the expect keys that look at phases other than the
// first request series: time to first token, prefill speed, unload time,
// simultaneous requests and the prompt reuse of a conversation.
func evaluateMetrics(spec *benchspec.Benchmark, st *Step, e *benchspec.Expect, add func(string, bool, string)) {
	if e.MinPrefillTPS != nil {
		switch {
		case st.Stats == nil || st.Stats.Prefill == nil:
			add("min_prefill_tps", false, "no prefill speed was measured")
		default:
			add("min_prefill_tps", st.Stats.Prefill.Mean >= *e.MinPrefillTPS, fmt.Sprintf("prefill %.1f tok/s, minimum %.1f", st.Stats.Prefill.Mean, *e.MinPrefillTPS))
		}
	}
	if e.MaxTTFTMS != nil {
		switch {
		case st.Stats == nil || st.Stats.TTFT == nil:
			add("max_ttft_ms", false, "no time to first token was measured")
		default:
			add("max_ttft_ms", st.Stats.TTFT.Mean <= *e.MaxTTFTMS, fmt.Sprintf("first token after %.0f ms, limit %.0f ms", st.Stats.TTFT.Mean, *e.MaxTTFTMS))
		}
	}
	if e.MaxUnloadS != nil {
		worst, seen := 0.0, false
		for _, p := range st.Phases {
			if p.Type == benchspec.PhaseUnload && p.Unload != nil && p.Unload.Loaded {
				seen = true
				if p.Unload.UnloadS > worst {
					worst = p.Unload.UnloadS
				}
			}
		}
		switch {
		case !seen:
			add("max_unload_s", false, "no unload was measured")
		default:
			add("max_unload_s", worst <= *e.MaxUnloadS, fmt.Sprintf("the instance was gone after %.1f s, limit %.1f s", worst, *e.MaxUnloadS))
		}
	}
	if e.MaxConcurrentWall != nil || e.MaxErrors != nil {
		var wall float64
		errs, seen := 0, false
		for _, p := range st.Phases {
			if p.Type == benchspec.PhaseConcurrent && p.Concurrent != nil {
				seen = true
				if p.Concurrent.WallMS/1000 > wall {
					wall = p.Concurrent.WallMS / 1000
				}
				errs += p.Concurrent.Errors
			}
		}
		if e.MaxConcurrentWall != nil {
			if !seen {
				add("max_concurrent_wall_s", false, "the simultaneous requests did not run")
			} else {
				add("max_concurrent_wall_s", wall <= *e.MaxConcurrentWall, fmt.Sprintf("all requests were done after %.1f s, limit %.1f s", wall, *e.MaxConcurrentWall))
			}
		}
		if e.MaxErrors != nil {
			if !seen {
				add("max_errors", false, "the simultaneous requests did not run")
			} else {
				add("max_errors", errs <= *e.MaxErrors, fmt.Sprintf("%d requests failed, at most %d allowed", errs, *e.MaxErrors))
			}
		}
	}
	if e.MinReusedTokens != nil {
		least, turns := -1, 0
		for _, p := range st.Phases {
			if p.Type != benchspec.PhaseConversation {
				continue
			}
			for _, t := range p.Turns {
				if t.Turn < 2 {
					continue
				}
				turns++
				if least < 0 || t.CacheN < least {
					least = t.CacheN
				}
			}
		}
		switch {
		case turns == 0:
			add("min_reused_tokens", false, "no conversation turn after the first was measured")
		default:
			add("min_reused_tokens", least >= *e.MinReusedTokens, fmt.Sprintf("the least a later turn reused was %d prompt tokens, minimum %d", least, *e.MinReusedTokens))
		}
	}
}
