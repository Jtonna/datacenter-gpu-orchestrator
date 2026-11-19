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
	return b.String()
}

// ExportCSV is one row per benchmark step.
func ExportCSV(r *Run) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	head := []string{"run_id", "suite", "idx", "group", "benchmark", "profile", "nodes", "state", "outcome", "http_status", "reason", "pass",
		"load_s", "avg_mb_s", "memory_mib", "requests", "failed_requests",
		"prefill_tps_mean", "prefill_tps_stdev", "decode_tps_mean", "decode_tps_median", "decode_tps_min", "decode_tps_max", "decode_tps_stdev",
		"ttft_ms_mean", "baseline_run_id", "load_vs_baseline_pct", "decode_vs_baseline_pct", "regression"}
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
		if err := w.Write(row); err != nil {
			return "", err
		}
	}
	w.Flush()
	return buf.String(), w.Error()
}
