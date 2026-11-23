package bench

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Task figures and scores in the exports (the tasks phase's open items): the
// phase line of the markdown and the phases CSV carry the counts, the median
// decode speed and each scorer's total; the markdown has a table per tasks
// phase; detail=tasks is one CSV row per task with a points column per scorer.

func scoreText(sc ScorerTotal) string {
	return fmt.Sprintf("%s %s of %s points (%d of %d scored)", sc.Scorer, fmtNum(sc.Points), fmtNum(sc.MaxPoints), sc.Scored, sc.Tasks)
}

// tasksNumbers are the headline figures of a tasks phase.
func tasksNumbers(t *TasksResult) []string {
	if t == nil {
		return nil
	}
	parts := []string{fmt.Sprintf("%d of %d tasks answered, %d failed (%d timed out), %d not sent", t.Answered, t.Total, t.Failed, t.TimedOut, t.NotSent)}
	if t.DecodeTPS != nil {
		parts = append(parts, "decode "+f2(t.DecodeTPS.Median)+" tok/s median")
	}
	for _, sc := range t.Scores {
		parts = append(parts, scoreText(sc))
	}
	return parts
}

// tasksCSV are the tasks columns of the phases CSV (empty for other phases).
func tasksCSV(t *TasksResult) []string {
	if t == nil {
		return []string{"", "", "", "", "", "", ""}
	}
	dec := ""
	if t.DecodeTPS != nil {
		dec = strconv.FormatFloat(t.DecodeTPS.Median, 'f', -1, 64)
	}
	var sc []string
	for _, s := range t.Scores {
		sc = append(sc, fmt.Sprintf("%s:%s/%s", s.Scorer, strconv.FormatFloat(s.Points, 'f', -1, 64), strconv.FormatFloat(s.MaxPoints, 'f', -1, 64)))
	}
	return []string{strconv.Itoa(t.Total), strconv.Itoa(t.Answered), strconv.Itoa(t.Failed), strconv.Itoa(t.TimedOut), strconv.Itoa(t.NotSent), dec, strings.Join(sc, ";")}
}

// scorersOf are the scorers of the items, sorted.
func scorersOf(items []TaskItem) []string {
	seen := map[string]bool{}
	for _, it := range items {
		for k := range it.Scores {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tasksMarkdown is the table of one tasks phase.
func tasksMarkdown(p *PhaseResult) string {
	t := p.Tasks
	var b strings.Builder
	set := t.SetID
	if t.SetVersion > 0 {
		set += fmt.Sprintf(" v%d", t.SetVersion)
	}
	fmt.Fprintf(&b, "\nTasks (phase %d, set %s):\n\n", p.Idx+1, mdEscape(set))
	scorers := scorersOf(t.Items)
	b.WriteString("| Task | State | Finish | Tokens out | First token ms | Decode tok/s |")
	for _, s := range scorers {
		b.WriteString(" " + mdEscape(s) + " |")
	}
	b.WriteString("\n| --- | --- | --- | ---: | ---: | ---: |" + strings.Repeat(" ---: |", len(scorers)) + "\n")
	for _, it := range t.Items {
		ttft := "-"
		if it.TTFTMS != nil {
			ttft = f1(*it.TTFTMS)
		}
		state := it.State
		if it.Error != "" {
			state += " (" + it.Error + ")"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s | %s |", mdEscape(it.TaskID), mdEscape(state), mdEscape(it.FinishReason), it.CompletionTokens, ttft, f2(it.DecodeTPS))
		for _, s := range scorers {
			if sc := it.Scores[s]; sc != nil {
				fmt.Fprintf(&b, " %s of %s |", fmtNum(sc.Points), fmtNum(sc.MaxPoints))
			} else {
				b.WriteString(" - |")
			}
		}
		b.WriteString("\n")
	}
	for _, sc := range t.Scores {
		fmt.Fprintf(&b, "\nTotal: %s.\n", scoreText(sc))
	}
	return b.String()
}

// ExportCSVTasks is one row per task of every tasks phase, with the points
// and maximum of each scorer that scored the run.
func ExportCSVTasks(r *Run) (string, error) {
	var all []TaskItem
	for i := range r.Steps {
		for _, p := range r.Steps[i].Phases {
			if p.Tasks != nil {
				all = append(all, p.Tasks.Items...)
			}
		}
	}
	scorers := scorersOf(all)
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	head := []string{"run_id", "step_idx", "benchmark", "profile", "phase_idx", "set_id", "set_version", "set_sha256", "task_id", "state", "finish_reason",
		"prompt_tokens", "completion_tokens", "ttft_ms", "decode_tps", "wall_ms", "answer_chars", "reasoning_chars", "answer_sha256", "error"}
	for _, s := range scorers {
		head = append(head, "points_"+s, "max_points_"+s)
	}
	if err := w.Write(head); err != nil {
		return "", err
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	for i := range r.Steps {
		st := &r.Steps[i]
		for _, p := range st.Phases {
			t := p.Tasks
			if t == nil {
				continue
			}
			for _, it := range t.Items {
				ttft := ""
				if it.TTFTMS != nil {
					ttft = num(*it.TTFTMS)
				}
				row := []string{r.ID, strconv.Itoa(st.Idx), st.BenchmarkID, st.Profile, strconv.Itoa(p.Idx), t.SetID, strconv.Itoa(t.SetVersion), t.SetSHA256, it.TaskID, it.State,
					it.FinishReason, strconv.Itoa(it.PromptTokens), strconv.Itoa(it.CompletionTokens), ttft, num(it.DecodeTPS), num(it.WallMS),
					strconv.Itoa(it.AnswerChars), strconv.Itoa(it.ReasoningChars), it.AnswerSHA256, it.Error}
				for _, s := range scorers {
					if sc := it.Scores[s]; sc != nil {
						row = append(row, num(sc.Points), num(sc.MaxPoints))
					} else {
						row = append(row, "", "")
					}
				}
				if err := w.Write(row); err != nil {
					return "", err
				}
			}
		}
	}
	w.Flush()
	return buf.String(), w.Error()
}
