package bench

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// Reports (data model step 4) are made by fixed templates from the recorded
// data every time one is opened. Nothing is stored and no model writes or
// judges anything: the same run always gives the same report.
//
//   - Summary report: a run of a group (suite). Headline counts, the issues
//     first, one table per metric (average, lowest and highest), and an index
//     of the benchmark reports.
//   - Benchmark report: one benchmark executed in a run (a step of the run):
//     its inputs, then a section per phase.
//   - Section report: one phase of that benchmark: what it did, its figures,
//     its conditions, its issues and a link to the transcript.
//
// Issues are facts only (a step failed or timed out, a placement was refused,
// the scheduler stopped during the run, a request or task failed), never a
// judgement against a baseline or a pass mark.

// ReportFormat is the format id of a report.
const ReportFormat = "potluck-report/1"

// Report kinds.
const (
	ReportSummary   = "summary"
	ReportBenchmark = "benchmark"
	ReportSection   = "section"
)

// ReportRun is the run a report is about.
type ReportRun struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	SuiteID        string `json:"suite_id,omitempty"`
	BenchmarkID    string `json:"benchmark_id,omitempty"`
	State          string `json:"state"`
	Note           string `json:"note,omitempty"`
	Error          string `json:"error,omitempty"`
	Release        string `json:"release,omitempty"`
	ConfigRevision string `json:"config_revision,omitempty"`
	QueuedAt       int64  `json:"queued_at"`
	StartedAt      int64  `json:"started_at,omitempty"`
	FinishedAt     int64  `json:"finished_at,omitempty"`
}

// Headline counts the steps of a run. Completed: ended ready; failed: failed,
// timed out or an error; refused: by the scheduler, by a placement that could
// not be met, or by an excluded machine.
type Headline struct {
	Steps     int `json:"steps"`
	Ran       int `json:"ran"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Refused   int `json:"refused"`
	Cancelled int `json:"cancelled"`
	Skipped   int `json:"skipped"`
}

// Issue kinds.
const (
	IssueRunFailed          = "run_failed"
	IssueInterrupted        = "interrupted" // the scheduler stopped during the run
	IssueRunSkipped         = "run_skipped"
	IssueRunCancelled       = "run_cancelled"
	IssueStepFailed         = "step_failed"
	IssueTimedOut           = "timed_out"
	IssueStepError          = "step_error"
	IssueRefused            = "refused"
	IssuePlacementRefused   = "placement_refused"
	IssueRefusedByExclusion = "refused_by_exclusion"
	IssueStepCancelled      = "step_cancelled"
	IssuePhaseFailed        = "phase_failed"
	IssueRequestFailed      = "request_failed"
	IssueTasksFailed        = "tasks_failed"
)

// Issue is something that happened.
type Issue struct {
	Kind      string `json:"kind"`
	Step      *int   `json:"step,omitempty"`
	Phase     *int   `json:"phase,omitempty"`
	Benchmark string `json:"benchmark,omitempty"`
	Text      string `json:"text"`
}

// Figure is one measured quantity of a section: the values of a metric for
// one subject (request, turn, task) and machine, with their mean, lowest and
// highest.
type Figure struct {
	Metric  string   `json:"metric"`
	Unit    string   `json:"unit"`
	Subject string   `json:"subject,omitempty"`
	Node    string   `json:"node,omitempty"`
	N       int      `json:"n"`
	Value   float64  `json:"value"` // the mean
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Of      *float64 `json:"of,omitempty"`
}

// SectionReport is one phase of a benchmark.
type SectionReport struct {
	Idx        int               `json:"idx"`
	Type       string            `json:"type"`
	Title      string            `json:"title"`
	Label      string            `json:"label,omitempty"`
	State      string            `json:"state"`
	DurationS  float64           `json:"duration_s"`
	Conditions map[string]string `json:"conditions,omitempty"`
	Figures    []Figure          `json:"figures"`
	Issues     []Issue           `json:"issues"`
	Transcript string            `json:"transcript,omitempty"` // a link, when the phase sent prompts
	// Fill is a context_fill phase's marks and readings.
	Fill *FillResult `json:"fill,omitempty"`
}

// BenchmarkReport is one benchmark executed in a run.
type BenchmarkReport struct {
	Step        int               `json:"step"`
	BenchmarkID string            `json:"benchmark_id"`
	Recipe      string            `json:"recipe,omitempty"`
	Name        string            `json:"name"`
	Profile     string            `json:"profile"`
	Model       string            `json:"model,omitempty"`
	Inputs      *benchspec.Inputs `json:"inputs,omitempty"`
	Outcome     string            `json:"outcome"`
	Reason      string            `json:"reason,omitempty"`
	StartedAt   int64             `json:"started_at,omitempty"`
	FinishedAt  int64             `json:"finished_at,omitempty"`
	DurationS   float64           `json:"duration_s"`
	Issues      []Issue           `json:"issues"`
	StepFigures []Figure          `json:"step_figures"` // figures of the step as a whole (per machine)
	Sections    []SectionReport   `json:"sections"`
	Transcript  string            `json:"transcript,omitempty"`
}

// SummaryRow is one benchmark use (and phase) in a metric's table.
type SummaryRow struct {
	Benchmark string   `json:"benchmark"` // the step's benchmark id (a recipe use's expanded id)
	Name      string   `json:"name"`
	Phase     string   `json:"phase,omitempty"`
	Model     string   `json:"model,omitempty"`
	Ctx       int      `json:"ctx,omitempty"`
	Placement string   `json:"placement,omitempty"`
	N         int      `json:"n"`
	Average   float64  `json:"average"`
	Lowest    float64  `json:"lowest"`
	Highest   float64  `json:"highest"`
	Of        *float64 `json:"of,omitempty"`
}

// SummaryTable is one metric across the run.
type SummaryTable struct {
	Metric      string       `json:"metric"`
	Unit        string       `json:"unit"`
	Description string       `json:"description,omitempty"`
	Rows        []SummaryRow `json:"rows"`
}

// IndexEntry links a benchmark report from the summary.
type IndexEntry struct {
	Step    int    `json:"step"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	Report  string `json:"report"`
}

// Report is a generated report.
type Report struct {
	Format    string           `json:"format"`
	Kind      string           `json:"kind"`
	Run       ReportRun        `json:"run"`
	Headline  *Headline        `json:"headline,omitempty"`
	Issues    []Issue          `json:"issues"`
	Tables    []SummaryTable   `json:"tables,omitempty"`
	Index     []IndexEntry     `json:"index,omitempty"`
	Benchmark *BenchmarkReport `json:"benchmark,omitempty"`
	Section   *SectionReport   `json:"section,omitempty"`
}

// ReportOptions choose the report: none for the run's own report (the summary
// of a group run, the benchmark report of a direct run), Step for a benchmark
// report, Step and Phase for a section.
type ReportOptions struct {
	Step  *int
	Phase *int
	// Transcripts are the steps that saved a transcript.
	Transcripts map[int]bool
}

// ReportPath is the API path of a report.
func ReportPath(runID string, step, phase *int) string {
	p := "/api/bench/runs/" + url.PathEscape(runID) + "/report"
	q := url.Values{}
	if step != nil {
		q.Set("step", fmt.Sprint(*step))
	}
	if phase != nil {
		q.Set("phase", fmt.Sprint(*phase))
	}
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return p
}

func transcriptPath(runID string, step int, phase *int) string {
	p := fmt.Sprintf("/api/bench/runs/%s/steps/%d/transcript?format=md", url.PathEscape(runID), step)
	if phase != nil {
		p += fmt.Sprintf("#phase-%d", *phase)
	}
	return p
}

// BuildReport makes a report of a run from its steps and measurements.
func BuildReport(r *Run, ms []Measurement, o ReportOptions) (*Report, error) {
	rep := &Report{Format: ReportFormat, Run: reportRun(r), Issues: []Issue{}}
	byStep := map[int][]Measurement{}
	for _, m := range ms {
		byStep[m.Step] = append(byStep[m.Step], m)
	}
	find := func(idx int) *Step {
		for i := range r.Steps {
			if r.Steps[i].Idx == idx {
				return &r.Steps[i]
			}
		}
		return nil
	}
	if o.Step == nil && r.Kind == RunKindBenchmark && len(r.Steps) == 1 {
		s := r.Steps[0].Idx
		o.Step = &s
		if o.Phase != nil {
			o.Phase = nil
		}
	}
	if o.Step != nil {
		st := find(*o.Step)
		if st == nil {
			return nil, fmt.Errorf("%w: run %s has no step %d", ErrNotFound, r.ID, *o.Step)
		}
		br := benchmarkReport(r.ID, st, byStep[st.Idx], o.Transcripts[st.Idx])
		if o.Phase != nil {
			for i := range br.Sections {
				if br.Sections[i].Idx == *o.Phase {
					rep.Kind, rep.Section = ReportSection, &br.Sections[i]
					rep.Issues = append(rep.Issues, br.Sections[i].Issues...)
					return rep, nil
				}
			}
			return nil, fmt.Errorf("%w: step %d of run %s has no phase %d", ErrNotFound, *o.Step, r.ID, *o.Phase)
		}
		rep.Kind, rep.Benchmark = ReportBenchmark, br
		rep.Issues = append(rep.Issues, runIssues(r)...)
		rep.Issues = append(rep.Issues, br.Issues...)
		return rep, nil
	}

	rep.Kind = ReportSummary
	h := &Headline{Steps: len(r.Steps)}
	rep.Issues = append(rep.Issues, runIssues(r)...)
	for i := range r.Steps {
		st := &r.Steps[i]
		countStep(h, st)
		br := benchmarkReport(r.ID, st, byStep[st.Idx], o.Transcripts[st.Idx])
		rep.Issues = append(rep.Issues, br.Issues...)
		s := st.Idx
		rep.Index = append(rep.Index, IndexEntry{Step: st.Idx, Name: st.Name, Profile: st.Profile, Outcome: outcomeOf(st), Reason: firstNonEmpty(st.Reason, st.Error),
			Report: ReportPath(r.ID, &s, nil)})
	}
	rep.Headline = h
	rep.Tables = summaryTables(r, ms)
	return rep, nil
}

func reportRun(r *Run) ReportRun {
	out := ReportRun{ID: r.ID, Kind: r.Kind, Name: r.SuiteName, SuiteID: r.SuiteID, BenchmarkID: r.BenchmarkID, State: r.State, Note: r.Note, Error: r.Error,
		Release: r.Version, QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
	if out.Kind == "" {
		out.Kind = RunKindSuite
	}
	if r.Env != nil {
		out.ConfigRevision = r.Env.ConfigRevision
		if out.Release == "" {
			out.Release = r.Env.SchedulerVersion
		}
	}
	if out.Name == "" {
		out.Name = firstNonEmpty(r.SuiteID, r.BenchmarkID, r.ID)
	}
	return out
}

func outcomeOf(st *Step) string {
	if st.Outcome != "" {
		return st.Outcome
	}
	return st.State
}

func countStep(h *Headline, st *Step) {
	switch st.Outcome {
	case OutcomeReady:
		h.Ran++
		h.Completed++
	case OutcomeFailed, OutcomeError:
		h.Ran++
		h.Failed++
	case OutcomeRefused, OutcomeRefusedByExclusion:
		h.Ran++
		h.Refused++
	case OutcomeCancelled:
		h.Ran++
		h.Cancelled++
	default:
		if st.State == StepSkipped || st.Outcome == OutcomeSkipped {
			h.Skipped++
		}
	}
}

func runIssues(r *Run) []Issue {
	switch {
	case r.State == RunFailed && strings.Contains(r.Error, "interrupted"):
		return []Issue{{Kind: IssueInterrupted, Text: "The scheduler stopped during the run; the run ended there (" + r.Error + ")."}}
	case r.State == RunFailed:
		return []Issue{{Kind: IssueRunFailed, Text: "The run failed: " + firstNonEmpty(r.Error, "no reason recorded") + "."}}
	case r.State == RunSkipped:
		return []Issue{{Kind: IssueRunSkipped, Text: "The run was skipped: " + firstNonEmpty(r.Error, "the scheduler did not become idle") + "."}}
	case r.State == RunCancelled:
		return []Issue{{Kind: IssueRunCancelled, Text: "The run was cancelled" + suffix(r.Error) + "."}}
	}
	return nil
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// stepIssue is the issue of a step's outcome, if any.
func stepIssue(st *Step) *Issue {
	idx := st.Idx
	is := &Issue{Step: &idx, Benchmark: st.BenchmarkID}
	why := firstNonEmpty(st.Reason, st.Error)
	switch st.Outcome {
	case OutcomeReady, "", OutcomeSkipped:
		return nil
	case OutcomeRefused:
		if st.Spec != nil && st.Spec.Applied != nil && len(st.Plan) == 0 {
			is.Kind, is.Text = IssuePlacementRefused, fmt.Sprintf("%s: the placement was refused: %s.", st.Name, why)
		} else {
			is.Kind, is.Text = IssueRefused, fmt.Sprintf("%s: the scheduler refused it: %s.", st.Name, why)
		}
	case OutcomeRefusedByExclusion:
		is.Kind, is.Text = IssueRefusedByExclusion, fmt.Sprintf("%s: refused because its profile forces an excluded machine: %s.", st.Name, why)
	case OutcomeFailed:
		is.Kind, is.Text = IssueStepFailed, fmt.Sprintf("%s failed: %s.", st.Name, why)
		if strings.Contains(why, "timed out") {
			is.Kind = IssueTimedOut
		}
	case OutcomeCancelled:
		is.Kind, is.Text = IssueStepCancelled, fmt.Sprintf("%s was cancelled.", st.Name)
	default:
		is.Kind, is.Text = IssueStepError, fmt.Sprintf("%s could not be run: %s.", st.Name, why)
		if why == "interrupted" {
			is.Kind, is.Text = IssueInterrupted, fmt.Sprintf("%s was running when the scheduler stopped.", st.Name)
		}
	}
	is.Text = strings.ReplaceAll(is.Text, "..", ".")
	return is
}

var phaseTitles = func() map[string]string {
	m := map[string]string{}
	for _, p := range benchspec.Schema().PhaseTypes {
		m[p.Type] = p.Title
	}
	return m
}()

func benchmarkReport(runID string, st *Step, ms []Measurement, hasTranscript bool) *BenchmarkReport {
	br := &BenchmarkReport{Step: st.Idx, BenchmarkID: st.BenchmarkID, Name: st.Name, Profile: st.Profile, Model: st.ModelFile, Outcome: outcomeOf(st),
		Reason: firstNonEmpty(st.Reason, st.Error), StartedAt: st.StartedAt, FinishedAt: st.FinishedAt, Issues: []Issue{}, StepFigures: []Figure{}, Sections: []SectionReport{}}
	if st.FinishedAt > st.StartedAt && st.StartedAt > 0 {
		br.DurationS = round3(float64(st.FinishedAt-st.StartedAt) / 1000)
	}
	if st.Spec != nil {
		br.Recipe, br.Inputs = st.Spec.Recipe, st.Spec.Applied
	}
	if hasTranscript {
		br.Transcript = transcriptPath(runID, st.Idx, nil)
	}
	if is := stepIssue(st); is != nil {
		br.Issues = append(br.Issues, *is)
	}
	var whole []Measurement
	byPhase := map[int][]Measurement{}
	for _, m := range ms {
		if m.Phase == nil {
			whole = append(whole, m)
		} else {
			byPhase[*m.Phase] = append(byPhase[*m.Phase], m)
		}
	}
	br.StepFigures = figures(whole)
	sent := map[int]bool{}
	failedReq := map[int][]string{}
	for _, rq := range st.Requests {
		sent[rq.PhaseIdx] = true
		if rq.Error != "" || (rq.Status != 0 && rq.Status != 200) {
			failedReq[rq.PhaseIdx] = append(failedReq[rq.PhaseIdx], fmt.Sprintf("%s: %s", firstNonEmpty(rq.Name, fmt.Sprintf("request %d", rq.Seq)), firstNonEmpty(rq.Error, fmt.Sprintf("status %d", rq.Status))))
		}
	}
	for _, p := range st.Phases {
		idx := p.Idx
		sec := SectionReport{Idx: p.Idx, Type: p.Type, Title: firstNonEmpty(phaseTitles[p.Type], p.Type), Label: p.Label, State: p.State, DurationS: p.DurationS,
			Figures: figures(byPhase[p.Idx]), Conditions: conditionsOf(byPhase[p.Idx]), Issues: []Issue{}}
		sec.Fill = p.Fill
		if hasTranscript && (sent[p.Idx] || p.Tasks != nil) {
			sec.Transcript = transcriptPath(runID, st.Idx, &idx)
		}
		at := func(kind, text string) {
			s := st.Idx
			sec.Issues = append(sec.Issues, Issue{Kind: kind, Step: &s, Phase: &idx, Benchmark: st.BenchmarkID, Text: text})
		}
		if p.State == PhaseFailed && p.Error != "" && p.Error != st.Error && p.Error != st.Reason {
			at(IssuePhaseFailed, fmt.Sprintf("%s, phase %d (%s) failed: %s.", st.Name, p.Idx+1, sec.Title, p.Error))
		}
		if f := failedReq[p.Idx]; len(f) > 0 {
			at(IssueRequestFailed, fmt.Sprintf("%s, phase %d (%s): %d request(s) failed: %s.", st.Name, p.Idx+1, sec.Title, len(f), strings.Join(f, "; ")))
		}
		if t := p.Tasks; t != nil && (t.Failed+t.TimedOut+t.NotSent) > 0 {
			at(IssueTasksFailed, fmt.Sprintf("%s, phase %d (%s): %d task(s) failed, %d timed out, %d not sent.", st.Name, p.Idx+1, sec.Title, t.Failed, t.TimedOut, t.NotSent))
		}
		br.Issues = append(br.Issues, sec.Issues...)
		br.Sections = append(br.Sections, sec)
	}
	return br
}

// figures groups rows by metric, subject and machine, in the order they first appear.
func figures(ms []Measurement) []Figure {
	out := []Figure{}
	pos := map[string]int{}
	sums := []float64{}
	for _, m := range ms {
		k := m.Metric + "\x00" + m.Subject + "\x00" + m.Node
		i, ok := pos[k]
		if !ok {
			i = len(out)
			pos[k] = i
			out = append(out, Figure{Metric: m.Metric, Unit: m.Unit, Subject: m.Subject, Node: m.Node, Min: m.Value, Max: m.Value, Of: m.Of})
			sums = append(sums, 0)
		}
		f := &out[i]
		f.N++
		sums[i] += m.Value
		f.Min, f.Max = math.Min(f.Min, m.Value), math.Max(f.Max, m.Value)
		if f.Of != nil && (m.Of == nil || *m.Of != *f.Of) {
			f.Of = nil
		}
	}
	for i := range out {
		out[i].Value = round3(sums[i] / float64(out[i].N))
	}
	return out
}

// conditionsOf lists every condition of the rows with its values.
func conditionsOf(ms []Measurement) map[string]string {
	seen := map[string]map[string]bool{}
	for _, m := range ms {
		for k, v := range m.Conditions {
			if seen[k] == nil {
				seen[k] = map[string]bool{}
			}
			seen[k][fmt.Sprint(v)] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, vs := range seen {
		var l []string
		for v := range vs {
			l = append(l, v)
		}
		sort.Strings(l)
		out[k] = strings.Join(l, ", ")
	}
	return out
}

// summaryTables is one table per metric: a row per benchmark use and phase,
// with the average, lowest and highest of its values (repeats and repeated
// steps included). Per-machine figures stay in the benchmark reports.
func summaryTables(r *Run, ms []Measurement) []SummaryTable {
	names := map[int]string{}
	for _, st := range r.Steps {
		names[st.Idx] = st.Name
	}
	type acc struct {
		row  SummaryRow
		sum  float64
		ofOK bool
	}
	tables := map[string][]*acc{}
	pos := map[string]*acc{}
	for _, m := range ms {
		if m.Node != "" {
			continue
		}
		use := firstNonEmpty(m.Variant, m.BenchmarkID+"@"+m.Profile)
		phase := m.PhaseLabel
		if phase == "" && m.PhaseType != "" {
			phase = firstNonEmpty(phaseTitles[m.PhaseType], m.PhaseType)
		}
		k := m.Metric + "\x00" + use + "\x00" + phase
		a := pos[k]
		if a == nil {
			id := firstNonEmpty(m.Variant, m.BenchmarkID)
			a = &acc{row: SummaryRow{Benchmark: id, Name: firstNonEmpty(names[m.Step], id), Phase: phase, Model: m.Model, Ctx: m.Ctx, Placement: m.Placement,
				Lowest: m.Value, Highest: m.Value, Of: m.Of}, ofOK: true}
			pos[k] = a
			tables[m.Metric] = append(tables[m.Metric], a)
		}
		a.row.N++
		a.sum += m.Value
		a.row.Lowest, a.row.Highest = math.Min(a.row.Lowest, m.Value), math.Max(a.row.Highest, m.Value)
		if a.row.Of != nil && (m.Of == nil || *m.Of != *a.row.Of) {
			a.row.Of = nil
		}
	}
	var out []SummaryTable
	for _, def := range Metrics {
		accs := tables[def.Name]
		if len(accs) == 0 {
			continue
		}
		t := SummaryTable{Metric: def.Name, Unit: def.Unit, Description: def.Description}
		for _, a := range accs {
			a.row.Average = round3(a.sum / float64(a.row.N))
			t.Rows = append(t.Rows, a.row)
		}
		out = append(out, t)
	}
	return out
}

// ---- Markdown ----

func fmtTime(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05 UTC")
}

func fmtNum(v float64) string {
	a := math.Abs(v)
	switch {
	case v == math.Trunc(v) && a < 1e15:
		return fmt.Sprintf("%.0f", v)
	case a >= 100:
		return fmt.Sprintf("%.0f", v)
	case a >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func fmtVal(v float64, of *float64, unit string) string {
	s := fmtNum(v)
	if of != nil {
		s += " of " + fmtNum(*of)
	}
	if unit != "" {
		s += " " + unit
	}
	return s
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

// ReportMarkdown renders a report as Markdown.
func ReportMarkdown(rep *Report) string {
	var b strings.Builder
	switch rep.Kind {
	case ReportSummary:
		fmt.Fprintf(&b, "# Summary: %s\n\n", rep.Run.Name)
		runHeader(&b, rep.Run)
		h := rep.Headline
		b.WriteString("## Headline\n\n")
		fmt.Fprintf(&b, "%d steps. Ran %d: %d completed, %d failed, %d refused", h.Steps, h.Ran, h.Completed, h.Failed, h.Refused)
		if h.Cancelled > 0 {
			fmt.Fprintf(&b, ", %d cancelled", h.Cancelled)
		}
		if h.Skipped > 0 {
			fmt.Fprintf(&b, "; %d skipped", h.Skipped)
		}
		b.WriteString(".\n\n")
		issuesMD(&b, "## Issues", rep.Issues)
		if len(rep.Tables) > 0 {
			b.WriteString("## Measurements\n")
			for _, t := range rep.Tables {
				fmt.Fprintf(&b, "\n### %s (%s)\n\n", t.Metric, t.Unit)
				if t.Description != "" {
					b.WriteString(t.Description + "\n\n")
				}
				b.WriteString("| Benchmark | Phase | Model | Context | Placement | n | Average | Lowest | Highest |\n|---|---|---|---|---|---|---|---|---|\n")
				for _, r := range t.Rows {
					ctx := ""
					if r.Ctx > 0 {
						ctx = fmt.Sprint(r.Ctx)
					}
					fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %d | %s | %s | %s |\n", mdCell(r.Name), mdCell(r.Phase), mdCell(r.Model), ctx, mdCell(r.Placement), r.N,
						fmtVal(r.Average, r.Of, ""), fmtNum(r.Lowest), fmtNum(r.Highest))
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("## Benchmark reports\n\n| # | Benchmark | Profile | Outcome | Report |\n|---|---|---|---|---|\n")
		for _, e := range rep.Index {
			out := e.Outcome
			if e.Reason != "" && e.Outcome != OutcomeReady {
				out += ": " + e.Reason
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %s | [report](%s) |\n", e.Step+1, mdCell(e.Name), mdCell(e.Profile), mdCell(out), e.Report)
		}
	case ReportBenchmark:
		br := rep.Benchmark
		fmt.Fprintf(&b, "# %s\n\n", br.Name)
		runHeader(&b, rep.Run)
		benchmarkMD(&b, br, rep.Issues)
	case ReportSection:
		s := rep.Section
		fmt.Fprintf(&b, "# %s\n\nRun `%s` (%s).\n\n", sectionTitle(s), rep.Run.ID, rep.Run.Name)
		sectionMD(&b, s, "##")
	}
	return b.String()
}

func runHeader(b *strings.Builder, r ReportRun) {
	fmt.Fprintf(b, "| Run | `%s` (%s) |\n|---|---|\n", r.ID, r.Kind)
	fmt.Fprintf(b, "| State | %s%s |\n", r.State, mdCell(suffix(r.Error)))
	fmt.Fprintf(b, "| Started | %s |\n| Finished | %s |\n", fmtTime(r.StartedAt), fmtTime(r.FinishedAt))
	if r.Release != "" {
		fmt.Fprintf(b, "| Release | %s |\n", mdCell(r.Release))
	}
	if r.ConfigRevision != "" {
		fmt.Fprintf(b, "| Configuration revision | %s |\n", mdCell(r.ConfigRevision))
	}
	if r.Note != "" {
		fmt.Fprintf(b, "| Note | %s |\n", mdCell(r.Note))
	}
	b.WriteString("\n")
}

func issuesMD(b *strings.Builder, heading string, is []Issue) {
	b.WriteString(heading + "\n\n")
	if len(is) == 0 {
		b.WriteString("None.\n\n")
		return
	}
	for _, i := range is {
		fmt.Fprintf(b, "- %s\n", i.Text)
	}
	b.WriteString("\n")
}

func benchmarkMD(b *strings.Builder, br *BenchmarkReport, issues []Issue) {
	b.WriteString("## Inputs\n\n| Input | Value |\n|---|---|\n")
	if br.Recipe != "" {
		fmt.Fprintf(b, "| Recipe | `%s` |\n", br.Recipe)
	}
	fmt.Fprintf(b, "| Benchmark | `%s` |\n| Profile | `%s` |\n", br.BenchmarkID, br.Profile)
	if br.Model != "" {
		fmt.Fprintf(b, "| Model | %s |\n", mdCell(br.Model))
	}
	if in := br.Inputs; in != nil {
		if in.Ctx > 0 {
			fmt.Fprintf(b, "| Context | %d |\n", in.Ctx)
		}
		fmt.Fprintf(b, "| Placement | %s |\n", in.Place().String())
		if in.Thinking != nil {
			fmt.Fprintf(b, "| Thinking | %s |\n", map[bool]string{true: "on", false: "off"}[*in.Thinking])
		}
		if in.KVQuant != "" {
			fmt.Fprintf(b, "| KV cache | %s |\n", in.KVQuant)
		}
		if in.FlashAttn != nil {
			fmt.Fprintf(b, "| Flash attention | %v |\n", *in.FlashAttn)
		}
	}
	out := br.Outcome
	if br.Reason != "" && br.Outcome != OutcomeReady {
		out += ": " + br.Reason
	}
	fmt.Fprintf(b, "| Outcome | %s |\n| Duration | %s s |\n", mdCell(out), fmtNum(br.DurationS))
	if br.Transcript != "" {
		fmt.Fprintf(b, "| Transcript | [transcript](%s) |\n", br.Transcript)
	}
	b.WriteString("\n")
	issuesMD(b, "## Issues", issues)
	if len(br.StepFigures) > 0 {
		b.WriteString("## The whole step\n\n")
		figuresMD(b, br.StepFigures)
	}
	for i := range br.Sections {
		sectionMD(b, &br.Sections[i], "##")
	}
}

func sectionTitle(s *SectionReport) string {
	t := fmt.Sprintf("%d. %s", s.Idx+1, s.Title)
	if s.Label != "" {
		t += " (" + s.Label + ")"
	}
	return t
}

func sectionMD(b *strings.Builder, s *SectionReport, h string) {
	fmt.Fprintf(b, "%s %s\n\n", h, sectionTitle(s))
	fmt.Fprintf(b, "%s, %s s.", s.State, fmtNum(s.DurationS))
	if s.Transcript != "" {
		fmt.Fprintf(b, " [Transcript](%s).", s.Transcript)
	}
	b.WriteString("\n\n")
	if len(s.Conditions) > 0 {
		var keys []string
		for k := range s.Conditions {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+": "+s.Conditions[k])
		}
		b.WriteString("Conditions: " + strings.Join(parts, "; ") + ".\n\n")
	}
	if len(s.Figures) > 0 {
		figuresMD(b, s.Figures)
	}
	if s.Fill != nil {
		fillMD(b, s.Fill)
	}
	if len(s.Issues) > 0 {
		for _, i := range s.Issues {
			fmt.Fprintf(b, "- Issue: %s\n", i.Text)
		}
		b.WriteString("\n")
	}
}

func figuresMD(b *strings.Builder, fs []Figure) {
	b.WriteString("| Figure | Of | Machine | n | Value | Lowest | Highest |\n|---|---|---|---|---|---|---|\n")
	for _, f := range fs {
		fmt.Fprintf(b, "| %s | %s | %s | %d | %s | %s | %s |\n", f.Metric, mdCell(f.Subject), mdCell(f.Node), f.N, fmtVal(f.Value, f.Of, f.Unit), fmtNum(f.Min), fmtNum(f.Max))
	}
	b.WriteString("\n")
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
