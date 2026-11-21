package bench

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Export formats of GET /api/bench/runs/{id}/export.
const (
	FormatJSON = "json"
	FormatCSV  = "csv"
	FormatMD   = "md"
)

func nodeNames(st *Step) []string {
	if st.Load == nil {
		return nil
	}
	var out []string
	for _, n := range st.Load.Nodes {
		out = append(out, n.Node)
	}
	if len(out) == 0 {
		out = append(out, st.Load.Workers...)
	}
	return out
}

func passText(st *Step) string {
	if st.Expect == nil {
		return ""
	}
	if st.Expect.Pass {
		return "passed"
	}
	return "failed"
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func ptrPct(p *float64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%+.1f%%", *p)
}

func baselineText(st *Step) string {
	c := st.Comparison
	if c == nil || !c.Found {
		return "n/a"
	}
	var parts []string
	if c.LoadPct != nil {
		parts = append(parts, "load "+ptrPct(c.LoadPct))
	}
	if c.DecodePct != nil {
		parts = append(parts, "decode "+ptrPct(c.DecodePct))
	}
	if c.HashDiffers != nil && *c.HashDiffers {
		parts = append(parts, "output changed")
	}
	s := strings.Join(parts, ", ")
	if s == "" {
		s = "same"
	}
	if c.Regression {
		s += " REGRESSION"
	}
	return s
}

func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

func tsText(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05 UTC")
}

// ExportMarkdown is a table a person can paste into a document: one row per
// benchmark, with profile, nodes, load, upload rate, prefill, decode (mean and
// standard deviation), memory, outcome and the change against the baseline.
func ExportMarkdown(r *Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: run %s\n\n", r.SuiteName, r.ID)
	fmt.Fprintf(&b, "State %s, started %s, finished %s.", r.State, tsText(r.StartedAt), tsText(r.FinishedAt))
	if r.Version != "" {
		fmt.Fprintf(&b, " Scheduler %s.", r.Version)
	}
	if r.BaselineRunID != "" {
		fmt.Fprintf(&b, " Compared with baseline %s.", r.BaselineRunID)
	}
	if r.Note != "" {
		fmt.Fprintf(&b, " Note: %s", mdEscape(r.Note))
	}
	if r.Error != "" {
		fmt.Fprintf(&b, " Error: %s.", mdEscape(r.Error))
	}
	b.WriteString("\n\n")
	b.WriteString("| Benchmark | Profile | Nodes | Load s | MB/s | Prefill tok/s | Decode tok/s | Decode sd | Memory MiB | Outcome | vs baseline |\n")
	b.WriteString("| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- |\n")
	for i := range r.Steps {
		st := &r.Steps[i]
		load, mbs, mem := "-", "-", "-"
		if st.Load != nil && st.Load.LoadS > 0 {
			load = f1(st.Load.LoadS)
			if st.Load.AvgMBs > 0 {
				mbs = f1(st.Load.AvgMBs)
			}
			if st.Load.MemoryMiB > 0 {
				mem = strconv.FormatFloat(st.Load.MemoryMiB, 'f', 0, 64)
			}
		}
		prefill, decode, sd := "-", "-", "-"
		if st.Stats != nil {
			if st.Stats.Prefill != nil {
				prefill = f1(st.Stats.Prefill.Mean)
			}
			if st.Stats.Decode != nil {
				decode, sd = f2(st.Stats.Decode.Mean), f2(st.Stats.Decode.Stdev)
			}
		}
		nodes := strings.Join(nodeNames(st), ", ")
		if nodes == "" {
			nodes = "-"
		}
		outcome := st.Outcome
		if outcome == "" {
			outcome = st.State
		}
		if p := passText(st); p != "" {
			outcome += ", " + p
		}
		if st.Reason != "" && st.Outcome != OutcomeReady {
			outcome += " (" + st.Reason + ")"
		}
		name := st.Name
		if name == "" {
			name = st.BenchmarkID
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", mdEscape(name), mdEscape(st.Profile), mdEscape(nodes),
			load, mbs, prefill, decode, sd, mem, mdEscape(outcome), mdEscape(baselineText(st)))
	}
	b.WriteString(ExportMarkdownPhases(r))
	return b.String()
}

// ExportCSV is one row per benchmark step.
func ExportCSV(r *Run) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	head := []string{"run_id", "suite", "idx", "group", "benchmark", "profile", "nodes", "state", "outcome", "http_status", "reason", "pass",
		"load_s", "avg_mb_s", "memory_mib", "requests", "failed_requests",
		"prefill_tps_mean", "prefill_tps_stdev", "decode_tps_mean", "decode_tps_median", "decode_tps_min", "decode_tps_max", "decode_tps_stdev",
		"ttft_ms_mean", "baseline_run_id", "load_vs_baseline_pct", "decode_vs_baseline_pct", "regression", "model_file", "worker_nodes", "phases",
		"load_mode", "host", "workers", "lowest_available_mib", "reload_from_cache"}
	if err := w.Write(head); err != nil {
		return "", err
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	pc := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', 2, 64)
	}
	for i := range r.Steps {
		st := &r.Steps[i]
		row := []string{r.ID, r.SuiteID, strconv.Itoa(st.Idx), st.Group, st.BenchmarkID, st.Profile, strings.Join(nodeNames(st), ";"), st.State, st.Outcome,
			strconv.Itoa(st.HTTPStatus), st.Reason, passText(st)}
		if st.Load != nil {
			row = append(row, num(st.Load.LoadS), num(st.Load.AvgMBs), num(st.Load.MemoryMiB))
		} else {
			row = append(row, "", "", "")
		}
		if st.Stats != nil {
			row = append(row, strconv.Itoa(st.Stats.Requests), strconv.Itoa(st.Stats.Failed))
			if p := st.Stats.Prefill; p != nil {
				row = append(row, num(p.Mean), num(p.Stdev))
			} else {
				row = append(row, "", "")
			}
			if d := st.Stats.Decode; d != nil {
				row = append(row, num(d.Mean), num(d.Median), num(d.Min), num(d.Max), num(d.Stdev))
			} else {
				row = append(row, "", "", "", "", "")
			}
			if t := st.Stats.TTFT; t != nil {
				row = append(row, num(t.Mean))
			} else {
				row = append(row, "")
			}
		} else {
			row = append(row, "", "", "", "", "", "", "", "", "", "")
		}
		if c := st.Comparison; c != nil {
			row = append(row, c.BaselineRunID, pc(c.LoadPct), pc(c.DecodePct), strconv.FormatBool(c.Regression))
		} else {
			row = append(row, "", "", "", "")
		}
		var ph []string
		for _, p := range st.Phases {
			ph = append(ph, p.Type+":"+p.State)
		}
		row = append(row, st.ModelFile, strconv.Itoa(st.NodeCount), strings.Join(ph, ";"))
		low := ""
		if v := LowestMiB(st); v != nil {
			low = strconv.FormatInt(*v, 10)
		}
		var rl []string
		for _, r := range st.Reload {
			t := r.Node + ":" + r.Verdict
			if r.CachedPct != nil {
				t += fmt.Sprintf("(%.0f%%)", *r.CachedPct)
			}
			rl = append(rl, t)
		}
		row = append(row, StepLoadMode(st), StepHost(st), strings.Join(StepWorkers(st), ";"), low, strings.Join(rl, ";"))
		if err := w.Write(row); err != nil {
			return "", err
		}
	}
	w.Flush()
	return buf.String(), w.Error()
}

// phaseNumbers are the headline figures of a phase, for the markdown and csv exports.
func phaseNumbers(p *PhaseResult) string {
	var parts []string
	switch p.Type {
	case "load":
		if p.Load != nil {
			parts = append(parts, "load "+f1(p.Load.LoadS)+" s")
			if p.Load.AvgMBs > 0 {
				parts = append(parts, f1(p.Load.AvgMBs)+" MB/s")
			}
			if p.Load.Bytes > 0 {
				parts = append(parts, strconv.FormatFloat(float64(p.Load.Bytes)/1e9, 'f', 2, 64)+" GB")
			}
			if p.Load.MemoryMiB > 0 {
				parts = append(parts, strconv.FormatFloat(p.Load.MemoryMiB, 'f', 0, 64)+" MiB")
			}
		}
	case "unload":
		if p.Unload != nil && p.Unload.Loaded {
			parts = append(parts, "gone after "+f2(p.Unload.UnloadS)+" s")
		}
	case "hold":
		if p.HeldS != nil {
			parts = append(parts, "idle "+f1(*p.HeldS)+" s")
		}
		if p.LoadedAfterHold != nil && !*p.LoadedAfterHold {
			parts = append(parts, "model was unloaded meanwhile")
		}
	case "concurrent_requests":
		if c := p.Concurrent; c != nil {
			parts = append(parts, strconv.Itoa(c.N)+" together, "+f1(c.WallMS/1000)+" s, "+strconv.Itoa(c.Errors)+" errors")
		}
	}
	if s := p.Stats; s != nil && p.Type != "load" && p.Type != "unload" {
		if s.Decode != nil {
			parts = append(parts, "decode "+f2(s.Decode.Mean)+" tok/s")
		}
		if s.Prefill != nil {
			parts = append(parts, "prefill "+f1(s.Prefill.Mean)+" tok/s")
		}
		if s.TTFT != nil {
			parts = append(parts, "ttft "+f1(s.TTFT.Mean)+" ms")
		}
	}
	return strings.Join(parts, ", ")
}

// ExportMarkdownPhases is the per-phase part of the markdown export: for each
// step that has phases, a table of them, and a table of the turns of every
// conversation. It is empty when no step has any.
func ExportMarkdownPhases(r *Run) string {
	var b strings.Builder
	for i := range r.Steps {
		st := &r.Steps[i]
		if len(st.Phases) == 0 {
			continue
		}
		name := st.Name
		if name == "" {
			name = st.BenchmarkID
		}
		fmt.Fprintf(&b, "\n## %s phases\n\n", mdEscape(name))
		b.WriteString("| # | Phase | State | Seconds | What it measured |\n| ---: | --- | --- | ---: | --- |\n")
		for _, p := range st.Phases {
			label := p.Type
			if p.Label != "" {
				label = p.Label + " (" + p.Type + ")"
			}
			state := p.State
			if p.Outcome != "" && p.Outcome != PhaseOK {
				state += ", " + p.Outcome
			}
			measured := phaseNumbers(&p)
			if measured == "" && p.Error != "" {
				measured = p.Error // a phase that stopped with nothing measured says why
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", p.Idx+1, mdEscape(label), mdEscape(state), f1(p.DurationS), mdEscape(measured))
		}
		for _, p := range st.Phases {
			if len(p.Turns) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\nConversation (phase %d):\n\n| Turn | Prompt tokens | Reused | Prefill tok/s | Decode tok/s | First token ms | Wall ms |\n| ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n", p.Idx+1)
			for _, t := range p.Turns {
				ttft := "-"
				if t.TTFTMS != nil {
					ttft = f1(*t.TTFTMS)
				}
				fmt.Fprintf(&b, "| %d | %d | %d | %s | %s | %s | %s |\n", t.Turn, t.PromptTokens, t.CacheN, f1(t.PrefillTPS), f2(t.DecodeTPS), ttft, f1(t.WallMS))
			}
		}
	}
	return b.String()
}

// ExportCSVPhases is one row per phase of every step.
func ExportCSVPhases(r *Run) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"run_id", "suite", "step_idx", "benchmark", "phase_idx", "type", "label", "state", "outcome", "duration_s", "load_s", "avg_mb_s", "bytes", "memory_mib",
		"requests", "decode_tps_mean", "prefill_tps_mean", "ttft_ms_mean", "held_s", "unload_s", "concurrent_n", "concurrent_wall_ms", "concurrent_errors", "error"}); err != nil {
		return "", err
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	for i := range r.Steps {
		st := &r.Steps[i]
		for _, p := range st.Phases {
			row := []string{r.ID, r.SuiteID, strconv.Itoa(st.Idx), st.BenchmarkID, strconv.Itoa(p.Idx), p.Type, p.Label, p.State, p.Outcome, num(p.DurationS)}
			if l := p.Load; l != nil {
				row = append(row, num(l.LoadS), num(l.AvgMBs), strconv.FormatInt(l.Bytes, 10), num(l.MemoryMiB))
			} else {
				row = append(row, "", "", "", "")
			}
			reqs, dec, pre, ttft := "", "", "", ""
			if s := p.Stats; s != nil {
				reqs = strconv.Itoa(s.Requests)
				if s.Decode != nil {
					dec = num(s.Decode.Mean)
				}
				if s.Prefill != nil {
					pre = num(s.Prefill.Mean)
				}
				if s.TTFT != nil {
					ttft = num(s.TTFT.Mean)
				}
			}
			held, unl := "", ""
			if p.HeldS != nil {
				held = num(*p.HeldS)
			}
			if p.Unload != nil {
				unl = num(p.Unload.UnloadS)
			}
			cn, cw, ce := "", "", ""
			if c := p.Concurrent; c != nil {
				cn, cw, ce = strconv.Itoa(c.N), num(c.WallMS), strconv.Itoa(c.Errors)
			}
			row = append(row, reqs, dec, pre, ttft, held, unl, cn, cw, ce, p.Error)
			if err := w.Write(row); err != nil {
				return "", err
			}
		}
	}
	w.Flush()
	return buf.String(), w.Error()
}

// ExportCSVTurns is one row per conversation turn.
func ExportCSVTurns(r *Run) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"run_id", "benchmark", "phase_idx", "turn", "name", "status", "prompt_tokens", "prompt_n", "reused_tokens", "prefill_tps", "decode_tps", "ttft_ms", "wall_ms", "predicted_n"}); err != nil {
		return "", err
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	for i := range r.Steps {
		st := &r.Steps[i]
		for _, p := range st.Phases {
			for _, t := range p.Turns {
				ttft := ""
				if t.TTFTMS != nil {
					ttft = num(*t.TTFTMS)
				}
				if err := w.Write([]string{r.ID, st.BenchmarkID, strconv.Itoa(p.Idx), strconv.Itoa(t.Turn), t.Name, strconv.Itoa(t.Status), strconv.Itoa(t.PromptTokens),
					strconv.Itoa(t.PromptN), strconv.Itoa(t.CacheN), num(t.PrefillTPS), num(t.DecodeTPS), ttft, num(t.WallMS), strconv.Itoa(t.PredictedN)}); err != nil {
					return "", err
				}
			}
		}
	}
	w.Flush()
	return buf.String(), w.Error()
}
