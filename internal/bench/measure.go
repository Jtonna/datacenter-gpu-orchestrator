package bench

// The measurement record (benchmark data model, step 1): every number a step
// measures is also stored as one row of one shape (what, value and unit,
// where it came from, what it describes, under which conditions), so that
// comparisons, trends and reports work for any benchmark someone writes
// later. The rows are derived from the step's own results by MeasurementsOf,
// a pure function: the step and phase results stay the source, and a run
// stored before measurements existed is mapped when the store opens.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// MeasureVersion is the version of the mapping. When it is above the one a
// database was mapped with, every stored run is mapped again at open.
const MeasureVersion = 2

// Measurement is one number of a run.
type Measurement struct {
	ID    int64  `json:"id,omitempty"`
	RunID string `json:"run_id"`
	// Where it came from: the step (its index in the run) and the phase
	// (nil: the step as a whole, or a step written before phases existed).
	Step       int    `json:"step"`
	StepID     int64  `json:"-"`
	Phase      *int   `json:"phase,omitempty"`
	PhaseType  string `json:"phase_type,omitempty"`
	PhaseLabel string `json:"phase_label,omitempty"`
	// BenchmarkID is the benchmark, the recipe for the use of a recipe;
	// Variant is then the use (the step's expanded benchmark id), and Ctx and
	// Placement are its inputs.
	BenchmarkID string `json:"benchmark_id"`
	Variant     string `json:"variant,omitempty"`
	Ctx         int    `json:"ctx,omitempty"`
	Placement   string `json:"placement,omitempty"`
	// What it describes: the profile, its model file, the machines it was
	// spread over, the machine when the number is per machine, and the
	// request, turn or task it belongs to (Seq: which repeat, 1-based).
	Profile   string `json:"profile"`
	Model     string `json:"model,omitempty"`
	NodeCount int    `json:"node_count,omitempty"`
	Node      string `json:"node,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Seq       int    `json:"seq,omitempty"`
	// What, its value and unit; Of is the whole a count is out of (5 of 5,
	// 12 of 17 points).
	Metric string   `json:"metric"`
	Value  float64  `json:"value"`
	Of     *float64 `json:"of,omitempty"`
	Unit   string   `json:"unit"`
	// Conditions: cold, cold_verified, load_mode, generation (first, steady,
	// after_hold), thinking (seen, not_seen), memory (system, gpu), window
	// (load, step), scorer, scorer_version.
	Conditions map[string]any `json:"conditions,omitempty"`
	At         int64          `json:"at"` // unix ms: when the request started, else when the step started
}

// MetricDef describes a metric of the record.
type MetricDef struct {
	Name           string   `json:"name"`
	Unit           string   `json:"unit"`
	Description    string   `json:"description"`
	From           []string `json:"from"` // the phase types that record it ("step": the step as a whole)
	PerNode        bool     `json:"per_node,omitempty"`
	HigherIsBetter *bool    `json:"higher_is_better,omitempty"`
}

func hib(v bool) *bool { return &v }

// Metrics lists every metric the record holds.
var Metrics = []MetricDef{
	{"load_s", "s", "Seconds from the load request until the model was ready.", []string{"load"}, false, hib(false)},
	{"load_bytes", "B", "Bytes the load sent to the workers.", []string{"load"}, false, nil},
	{"load_mb_s", "MB/s", "Average rate of the load.", []string{"load"}, false, hib(true)},
	{"memory_mib", "MiB", "Device memory the model took, all machines together.", []string{"load"}, false, nil},
	{"node_bytes", "B", "Bytes one machine received during the load.", []string{"load"}, true, nil},
	{"node_memory_mib", "MiB", "Memory the model took on one machine.", []string{"load"}, true, nil},
	{"lowest_available_load_mib", "MiB", "Lowest available memory of one machine while the model loaded.", []string{"step"}, true, hib(true)},
	{"lowest_available_mib", "MiB", "Lowest available memory of one machine over the whole step.", []string{"step"}, true, hib(true)},
	{"ttft_ms", "ms", "Time to first token of one request, turn or task.", []string{"warmup", "requests", "hold", "conversation", "tasks"}, false, hib(false)},
	{"decode_tps", "tok/s", "Tokens per second while generating.", []string{"warmup", "requests", "hold", "conversation", "concurrent_requests", "tasks"}, false, hib(true)},
	{"prefill_tps", "tok/s", "Prompt tokens per second.", []string{"warmup", "requests", "hold", "conversation", "concurrent_requests"}, false, hib(true)},
	{"wall_ms", "ms", "Total time of one request, turn or task.", []string{"warmup", "requests", "hold", "conversation", "concurrent_requests", "tasks"}, false, hib(false)},
	{"prompt_tokens", "tokens", "Prompt tokens of one request, turn or task (processed and reused).", []string{"warmup", "requests", "hold", "conversation", "tasks"}, false, nil},
	{"reused_tokens", "tokens", "Prompt tokens reused from the cache.", []string{"warmup", "requests", "hold", "conversation"}, false, hib(true)},
	{"completion_tokens", "tokens", "Tokens generated.", []string{"warmup", "requests", "hold", "conversation", "tasks"}, false, nil},
	{"held_s", "s", "Seconds the model stayed idle in a hold phase.", []string{"hold"}, false, nil},
	{"concurrent_wall_s", "s", "Seconds from the first simultaneous request to the last answer.", []string{"concurrent_requests"}, false, hib(false)},
	{"wait_ms", "ms", "Time a simultaneous request waited before the model started on it.", []string{"concurrent_requests"}, false, hib(false)},
	{"errors", "count", "Simultaneous requests that failed.", []string{"concurrent_requests"}, false, hib(false)},
	{"unload_s", "s", "Seconds from the unload request until the instance was gone.", []string{"unload"}, false, hib(false)},
	{"task_points", "points", "A scorer's points for one task, of its max_points (posted after the run).", []string{"tasks"}, false, hib(true)},
}

// MetricNames is the set of metric names.
func MetricNames() map[string]bool {
	out := map[string]bool{}
	for _, m := range Metrics {
		out[m.Name] = true
	}
	return out
}

// MeasurementsOf maps a step's results into measurement rows (task points
// excluded: they arrive with the scores). A skipped step has none. The use
// of a recipe is recorded under the recipe, with its variant and inputs.
func MeasurementsOf(st *Step) []Measurement {
	out := measurementsOf(st)
	if st == nil || st.Spec == nil || st.Spec.Recipe == "" {
		return out
	}
	for i := range out {
		m := &out[i]
		m.BenchmarkID, m.Variant = st.Spec.Recipe, st.BenchmarkID
		if in := st.Spec.Applied; in != nil {
			m.Ctx, m.Placement = in.Ctx, in.Place().String()
			if in.Thinking != nil {
				c := map[string]any{}
				for k, v := range m.Conditions {
					c[k] = v
				}
				c["thinking_input"] = map[bool]string{true: "on", false: "off"}[*in.Thinking]
				m.Conditions = c
			}
		}
	}
	return out
}

func measurementsOf(st *Step) []Measurement {
	if st == nil || st.Outcome == OutcomeSkipped || st.State == StepSkipped {
		return nil
	}
	var out []Measurement
	base := Measurement{RunID: st.RunID, Step: st.Idx, StepID: st.ID, BenchmarkID: st.BenchmarkID, Profile: st.Profile, Model: st.ModelFile,
		NodeCount: st.NodeCount, At: st.StartedAt}
	add := func(p *PhaseResult, metric string, v float64, unit string, mod func(*Measurement)) {
		m := base
		if p != nil {
			idx := p.Idx
			m.Phase, m.PhaseType, m.PhaseLabel = &idx, p.Type, p.Label
			if p.StartedAt > 0 {
				m.At = p.StartedAt
			}
		}
		m.Metric, m.Value, m.Unit = metric, v, unit
		if mod != nil {
			mod(&m)
		}
		out = append(out, m)
	}
	cond := func(kv ...any) func(*Measurement) {
		return func(m *Measurement) {
			if m.Conditions == nil {
				m.Conditions = map[string]any{}
			}
			for i := 0; i+1 < len(kv); i += 2 {
				m.Conditions[kv[i].(string)] = kv[i+1]
			}
		}
	}
	// lastLoad: the cold conditions of the latest load before a phase, which
	// the requests after it carry.
	var lastCold map[string]any
	loadRows := func(p *PhaseResult, l *LoadResult, c *ColdResult) {
		if l == nil || (l.LoadS == 0 && l.Outcome != "" && l.Outcome != OutcomeReady) {
			return
		}
		lc := map[string]any{"cold": c != nil && c.Requested}
		if c != nil && c.Requested {
			lc["cold_verified"] = c.Verified
		}
		lastCold = lc
		withLoad := func(m *Measurement) {
			m.Conditions = map[string]any{}
			for k, v := range lc {
				m.Conditions[k] = v
			}
			if l.LoadMode != "" {
				m.Conditions["load_mode"] = l.LoadMode
			}
		}
		add(p, "load_s", l.LoadS, "s", withLoad)
		if l.Bytes > 0 {
			add(p, "load_bytes", float64(l.Bytes), "B", withLoad)
		}
		if l.AvgMBs > 0 {
			add(p, "load_mb_s", l.AvgMBs, "MB/s", withLoad)
		}
		if l.MemoryMiB > 0 {
			add(p, "memory_mib", l.MemoryMiB, "MiB", withLoad)
		}
		for _, n := range l.Nodes {
			node := n.Node
			if n.Bytes != nil {
				add(p, "node_bytes", float64(*n.Bytes), "B", func(m *Measurement) { withLoad(m); m.Node = node })
			}
			if n.MemoryMiB != nil {
				add(p, "node_memory_mib", *n.MemoryMiB, "MiB", func(m *Measurement) { withLoad(m); m.Node = node })
			}
		}
	}
	afterLoad := func(m *Measurement) {
		if lastCold != nil {
			if m.Conditions == nil {
				m.Conditions = map[string]any{}
			}
			for k, v := range lastCold {
				m.Conditions[k] = v
			}
		}
	}
	requestRows := func(p *PhaseResult, r RequestResult, generation string) {
		if r.Status != 200 {
			return // a failed request has no figures: the step's result keeps its error
		}
		mod := func(m *Measurement) {
			m.Subject, m.Seq = r.Name, r.Seq
			if r.StartedAt > 0 {
				m.At = r.StartedAt
			}
			afterLoad(m)
			cond("generation", generation)(m)
		}
		if r.TTFTMS != nil {
			add(p, "ttft_ms", *r.TTFTMS, "ms", mod)
		}
		if r.DecodeTPS > 0 {
			add(p, "decode_tps", r.DecodeTPS, "tok/s", mod)
		}
		if r.PromptTPS > 0 {
			add(p, "prefill_tps", r.PromptTPS, "tok/s", mod)
		}
		add(p, "wall_ms", r.WallMS, "ms", mod)
		if r.PromptN+r.CacheN > 0 {
			add(p, "prompt_tokens", float64(r.PromptN+r.CacheN), "tokens", mod)
			add(p, "reused_tokens", float64(r.CacheN), "tokens", mod)
		}
		if r.PredictedN > 0 {
			add(p, "completion_tokens", float64(r.PredictedN), "tokens", mod)
		}
	}
	generationOf := func(phase string) string {
		switch phase {
		case PhaseWarmup, PhaseLoad:
			return "first"
		case PhaseHold:
			return "after_hold"
		}
		return "steady"
	}
	// seq per request name within a phase, for rows written without one
	numbered := func(rs []RequestResult) []RequestResult {
		out := append([]RequestResult(nil), rs...)
		n := map[string]int{}
		for i := range out {
			n[out[i].Name]++
			out[i].Seq = n[out[i].Name]
		}
		return out
	}

	if len(st.Phases) == 0 {
		// A step of the first runner: its load and its requests.
		loadRows(nil, st.Load, nil)
		var rs []RequestResult
		for _, r := range st.Requests {
			if r.Phase == PhaseWarmup || r.Phase == PhaseMeasured || r.Phase == PhaseLoad {
				rs = append(rs, r)
			}
		}
		for _, r := range numbered(rs) {
			requestRows(nil, r, generationOf(r.Phase))
		}
	}
	for i := range st.Phases {
		p := &st.Phases[i]
		switch p.Type {
		case "load":
			loadRows(p, p.Load, p.Cold)
		case "warmup", "requests", "hold":
			var rs []RequestResult
			for _, r := range st.Requests {
				if r.PhaseIdx == p.Idx && (r.Phase == PhaseWarmup || r.Phase == PhaseMeasured || r.Phase == PhaseHold) {
					rs = append(rs, r)
				}
			}
			for _, r := range numbered(rs) {
				requestRows(p, r, generationOf(r.Phase))
			}
			if p.Type == "hold" && p.HeldS != nil {
				add(p, "held_s", *p.HeldS, "s", nil)
			}
		case "conversation":
			for _, t := range p.Turns {
				if t.Status != 200 {
					continue
				}
				mod := func(m *Measurement) {
					m.Subject, m.Seq = "turn "+strconv.Itoa(t.Turn), t.Turn
					afterLoad(m)
				}
				if t.TTFTMS != nil {
					add(p, "ttft_ms", *t.TTFTMS, "ms", mod)
				}
				if t.DecodeTPS > 0 {
					add(p, "decode_tps", t.DecodeTPS, "tok/s", mod)
				}
				if t.PrefillTPS > 0 {
					add(p, "prefill_tps", t.PrefillTPS, "tok/s", mod)
				}
				add(p, "wall_ms", t.WallMS, "ms", mod)
				add(p, "prompt_tokens", float64(t.PromptTokens), "tokens", mod)
				add(p, "reused_tokens", float64(t.CacheN), "tokens", mod)
				if t.PredictedN > 0 {
					add(p, "completion_tokens", float64(t.PredictedN), "tokens", mod)
				}
			}
		case "concurrent_requests":
			c := p.Concurrent
			if c == nil {
				continue
			}
			nn := float64(c.N)
			add(p, "concurrent_wall_s", round3(c.WallMS/1000), "s", func(m *Measurement) { m.Subject = fmt.Sprintf("%d at once", c.N); afterLoad(m) })
			add(p, "errors", float64(c.Errors), "count", func(m *Measurement) { m.Of = &nn; m.Subject = fmt.Sprintf("%d at once", c.N) })
			for _, it := range c.Items {
				if it.Status != 200 {
					continue
				}
				mod := func(m *Measurement) { m.Subject, m.Seq = "request "+strconv.Itoa(it.Index), it.Index; afterLoad(m) }
				if it.WaitMS != nil {
					add(p, "wait_ms", *it.WaitMS, "ms", mod)
				}
				if it.DecodeTPS > 0 {
					add(p, "decode_tps", it.DecodeTPS, "tok/s", mod)
				}
				if it.PrefillTPS > 0 {
					add(p, "prefill_tps", it.PrefillTPS, "tok/s", mod)
				}
				add(p, "wall_ms", it.WallMS, "ms", mod)
			}
		case "tasks":
			tr := p.Tasks
			if tr == nil {
				continue
			}
			for _, it := range tr.Items {
				if it.State == TaskNotSent || it.State == TaskFailed {
					continue
				}
				thinking := ThinkingNotSeen
				if it.ReasoningChars > 0 {
					thinking = ThinkingSeen
				}
				mod := func(m *Measurement) {
					m.Subject = it.TaskID
					if it.StartedAt > 0 {
						m.At = it.StartedAt
					}
					afterLoad(m)
					cond("thinking", thinking, "task_state", it.State, "task_set", tr.SetID+"@"+strconv.Itoa(tr.SetVersion))(m)
				}
				if it.TTFTMS != nil {
					add(p, "ttft_ms", *it.TTFTMS, "ms", mod)
				}
				if it.DecodeTPS > 0 {
					add(p, "decode_tps", it.DecodeTPS, "tok/s", mod)
				}
				add(p, "wall_ms", it.WallMS, "ms", mod)
				if it.PromptTokens > 0 {
					add(p, "prompt_tokens", float64(it.PromptTokens), "tokens", mod)
				}
				add(p, "completion_tokens", float64(it.CompletionTokens), "tokens", mod)
			}
		case "unload":
			if p.Unload != nil && p.Unload.Loaded && p.Unload.Gone {
				add(p, "unload_s", p.Unload.UnloadS, "s", nil)
			}
		}
	}
	for _, ml := range st.MemLow {
		kind := ml.Kind
		if kind == "" {
			kind = "system"
		}
		node := ml.Node
		if ml.LoadMiB != nil {
			add(nil, "lowest_available_load_mib", float64(*ml.LoadMiB), "MiB", func(m *Measurement) { m.Node = node; cond("memory", kind, "window", "load")(m) })
		}
		if ml.StepMiB != nil {
			add(nil, "lowest_available_mib", float64(*ml.StepMiB), "MiB", func(m *Measurement) { m.Node = node; cond("memory", kind, "window", "step")(m) })
		}
	}
	return out
}

// ---- storage ----

const measureCols = `id, run_id, step_id, step_idx, phase_idx, phase_type, phase_label, benchmark_id, profile, model, node_count, node, subject, seq, metric, value, of_value, unit, conditions, at, variant, ctx, placement`

func insertMeasurements(tx *sql.Tx, ms []Measurement) error {
	if len(ms) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO measurements (run_id, step_id, step_idx, phase_idx, phase_type, phase_label, benchmark_id, profile, model, node_count, node, subject, seq, metric, value, of_value, unit, conditions, at, variant, ctx, placement)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range ms {
		var phase, of, cond any
		if m.Phase != nil {
			phase = *m.Phase
		}
		if m.Of != nil {
			of = *m.Of
		}
		if len(m.Conditions) > 0 {
			b, err := json.Marshal(m.Conditions)
			if err != nil {
				return err
			}
			cond = string(b)
		}
		if _, err := stmt.Exec(m.RunID, m.StepID, m.Step, phase, m.PhaseType, m.PhaseLabel, m.BenchmarkID, m.Profile, m.Model, m.NodeCount, m.Node,
			m.Subject, m.Seq, m.Metric, m.Value, of, m.Unit, cond, m.At, m.Variant, m.Ctx, m.Placement); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceStepMeasurements stores the measurements of a step in place of any
// it had (task points stay: they come from the scores).
func (s *Store) ReplaceStepMeasurements(st *Step) error {
	ms := MeasurementsOf(st)
	return s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM measurements WHERE step_id=? AND metric<>'task_points'`, st.ID); err != nil {
			return err
		}
		return insertMeasurements(tx, ms)
	})
}

// refreshTaskPoints rewrites a run's task_points rows from its scores.
func refreshTaskPoints(tx *sql.Tx, runID string) error {
	if _, err := tx.Exec(`DELETE FROM measurements WHERE run_id=? AND metric='task_points'`, runID); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT t.step_id, s.idx, t.phase_idx, s.benchmark_id, s.profile, s.model_file, s.node_count, t.task_id, t.started_at,
	  t.set_id, t.set_version, c.scorer, c.scorer_version, c.points, c.max_points, c.updated_at,
	  COALESCE(json_extract(s.spec, '$.recipe'), ''), COALESCE(json_extract(s.spec, '$.applied_inputs.ctx'), 0), COALESCE(json_extract(s.spec, '$.applied_inputs.placement'), '')
	  FROM task_scores c JOIN task_results t ON t.id=c.result_id JOIN run_steps s ON s.id=t.step_id WHERE c.run_id=? ORDER BY s.idx, t.phase_idx, t.seq, c.scorer`, runID)
	if err != nil {
		return err
	}
	var ms []Measurement
	for rows.Next() {
		var m Measurement
		var phase, setVersion int
		var setID, scorer, version string
		var max float64
		var updated int64
		var recipe string
		if err := rows.Scan(&m.StepID, &m.Step, &phase, &m.BenchmarkID, &m.Profile, &m.Model, &m.NodeCount, &m.Subject, &m.At,
			&setID, &setVersion, &scorer, &version, &m.Value, &max, &updated, &recipe, &m.Ctx, &m.Placement); err != nil {
			rows.Close()
			return err
		}
		if recipe != "" {
			m.BenchmarkID, m.Variant = recipe, m.BenchmarkID
		} else {
			m.Ctx, m.Placement = 0, ""
		}
		m.RunID, m.Phase, m.PhaseType, m.Metric, m.Unit, m.Of = runID, &phase, "tasks", "task_points", "points", &max
		m.Conditions = map[string]any{"scorer": scorer, "task_set": setID + "@" + strconv.Itoa(setVersion)}
		if version != "" {
			m.Conditions["scorer_version"] = version
		}
		if m.At == 0 {
			m.At = updated
		}
		ms = append(ms, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return insertMeasurements(tx, ms)
}

func scanMeasurement(sc interface{ Scan(...any) error }, extra ...any) (Measurement, error) {
	var m Measurement
	var phase sql.NullInt64
	var of sql.NullFloat64
	var cond sql.NullString
	dst := append([]any{&m.ID, &m.RunID, &m.StepID, &m.Step, &phase, &m.PhaseType, &m.PhaseLabel, &m.BenchmarkID, &m.Profile, &m.Model, &m.NodeCount,
		&m.Node, &m.Subject, &m.Seq, &m.Metric, &m.Value, &of, &m.Unit, &cond, &m.At, &m.Variant, &m.Ctx, &m.Placement}, extra...)
	if err := sc.Scan(dst...); err != nil {
		return m, err
	}
	if phase.Valid {
		v := int(phase.Int64)
		m.Phase = &v
	}
	if of.Valid {
		v := of.Float64
		m.Of = &v
	}
	if cond.Valid {
		json.Unmarshal([]byte(cond.String), &m.Conditions)
	}
	return m, nil
}

// MeasureQuery narrows the measurements of one run.
type MeasureQuery struct {
	Step    *int
	Metrics []string
}

func inList(col string, vals []string, args *[]any) string {
	ph := make([]string, len(vals))
	for i, v := range vals {
		ph[i] = "?"
		*args = append(*args, v)
	}
	return col + " IN (" + strings.Join(ph, ",") + ")"
}

// RunMeasurements lists a run's measurements in step, phase and record order.
func (s *Store) RunMeasurements(runID string, q MeasureQuery) ([]Measurement, error) {
	where, args := `run_id=?`, []any{runID}
	if q.Step != nil {
		where += ` AND step_idx=?`
		args = append(args, *q.Step)
	}
	if len(q.Metrics) > 0 {
		where += ` AND ` + inList("metric", q.Metrics, &args)
	}
	rows, err := s.db.Query(`SELECT `+measureCols+` FROM measurements WHERE `+where+` ORDER BY step_idx, COALESCE(phase_idx, -1), id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Measurement{}
	for rows.Next() {
		m, err := scanMeasurement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MeasureFilter selects measurements across runs, for history and trends.
type MeasureFilter struct {
	Run       string
	Benchmark string
	Profile   string
	Model     string
	Node      string
	Kind      string // suite or benchmark runs only
	Variant   string // one use of a recipe (an expanded benchmark id)
	Placement string // a recipe's placement input, as written ("any:2")
	Ctx       int    // a recipe's context input
	Metrics   []string
	Since     int64 // unix ms, 0: no bound
	Until     int64
	Limit     int // default 1000, at most MaxMeasureRows
}

// MaxMeasureRows bounds one query of measurements.
const MaxMeasureRows = 10000

// MeasurementRow is a measurement with what its run ran on.
type MeasurementRow struct {
	Measurement
	RunState       string `json:"run_state"`
	RunKind        string `json:"run_kind"`
	Release        string `json:"release,omitempty"`
	ConfigRevision string `json:"config_revision,omitempty"`
}

// QueryMeasurements lists measurements across runs, oldest first.
func (s *Store) QueryMeasurements(f MeasureFilter) ([]MeasurementRow, error) {
	where, args := `1=1`, []any{}
	if f.Ctx > 0 {
		where += ` AND m.ctx=?`
		args = append(args, f.Ctx)
	}
	for _, c := range []struct{ col, v string }{{"m.run_id", f.Run}, {"m.benchmark_id", f.Benchmark}, {"m.profile", f.Profile}, {"m.model", f.Model}, {"m.node", f.Node}, {"r.kind", f.Kind},
		{"m.variant", f.Variant}, {"m.placement", f.Placement}} {
		if c.v != "" {
			where += ` AND ` + c.col + `=?`
			args = append(args, c.v)
		}
	}
	if len(f.Metrics) > 0 {
		where += ` AND ` + inList("m.metric", f.Metrics, &args)
	}
	if f.Since > 0 {
		where += ` AND m.at>=?`
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where += ` AND m.at<=?`
		args = append(args, f.Until)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 1000
	}
	if limit > MaxMeasureRows {
		limit = MaxMeasureRows
	}
	args = append(args, limit)
	cols := "m." + strings.ReplaceAll(measureCols, ", ", ", m.")
	rows, err := s.db.Query(`SELECT `+cols+`, r.state, r.kind, r.version, COALESCE(json_extract(r.env, '$.config_revision'), '')
	  FROM measurements m JOIN runs r ON r.id=m.run_id WHERE `+where+` ORDER BY m.at, m.id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MeasurementRow{}
	for rows.Next() {
		var row MeasurementRow
		m, err := scanMeasurement(rows, &row.RunState, &row.RunKind, &row.Release, &row.ConfigRevision)
		if err != nil {
			return nil, err
		}
		row.Measurement = m
		out = append(out, row)
	}
	return out, rows.Err()
}

// backfillMeasurements maps every stored run into measurements when the
// database was mapped with an older MeasureVersion (or never).
func (s *Store) backfillMeasurements() error {
	var have string
	err := s.db.QueryRow(`SELECT value FROM bench_meta WHERE key='measurements'`).Scan(&have)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if n, _ := strconv.Atoi(have); n >= MeasureVersion {
		return nil
	}
	rows, err := s.db.Query(`SELECT id FROM runs ORDER BY queued_at`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		r, err := s.GetRun(id)
		if err != nil {
			return err
		}
		err = s.write(func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM measurements WHERE run_id=?`, id); err != nil {
				return err
			}
			for i := range r.Steps {
				if err := insertMeasurements(tx, MeasurementsOf(&r.Steps[i])); err != nil {
					return err
				}
			}
			return refreshTaskPoints(tx, id)
		})
		if err != nil {
			return fmt.Errorf("run %s: %w", id, err)
		}
	}
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO bench_meta (key, value) VALUES ('measurements', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.Itoa(MeasureVersion))
		return err
	})
}
