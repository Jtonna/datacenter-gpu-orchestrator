package bench

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// Run facts (Benchmarks slice 2, B1 and B2): what the run boards draw on
// every row, computed from the rows that exist. Nothing here is stored: an
// older image simply does not send these fields.

// Run results: one word for a run's row icon.
const (
	ResultRunning    = "running"
	ResultQueued     = "queued"
	ResultWaiting    = "waiting" // paused
	ResultPassed     = "passed"  // every step completed with no issue
	ResultWithIssues = "with_issues"
	ResultFailed     = "failed"
	ResultRefused    = "refused" // a direct run whose step was refused
	ResultSkipped    = "skipped"

	// Filters only (result= on the runs list).
	ResultHasFailed  = "has_failed"  // a step failed, or the run did
	ResultHasRefused = "has_refused" // a step was refused
	ResultClean      = "clean"       // the same as passed
)

var resultFilters = map[string]bool{ResultRunning: true, ResultQueued: true, ResultWaiting: true, ResultPassed: true, ResultWithIssues: true,
	ResultFailed: true, ResultRefused: true, ResultCancelled: true, ResultSkipped: true, ResultHasFailed: true, ResultHasRefused: true, ResultClean: true}

// ResultFilters are the values result= accepts, sorted.
func ResultFilters() []string {
	var out []string
	for k := range resultFilters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ValidResultFilter says whether v is a result filter ("" is no filter).
func ValidResultFilter(v string) bool { return v == "" || resultFilters[v] }

// Cold summary states.
const (
	ColdAll     = "all"     // every load asked for a cold cache and it was verified
	ColdSome    = "some"    // some loads asked for a cold cache, or not all were verified
	ColdNone    = "none"    // loads, none asked for a cold cache
	ColdUnknown = "unknown" // no load recorded
)

// RunCounts are a run's step outcomes. Steps is what the run plans (its
// snapshot's expanded entries, or the step rows when there are more), Done
// the steps with an outcome, ToGo what is left while the run is not over.
// Completed = Passed + WithIssues (ready steps).
type RunCounts struct {
	Steps      int `json:"steps"`
	Done       int `json:"done"`
	ToGo       int `json:"to_go"`
	Completed  int `json:"completed"`
	Passed     int `json:"passed"`
	WithIssues int `json:"with_issues"`
	Failed     int `json:"failed"`
	Refused    int `json:"refused"`
	Cancelled  int `json:"cancelled"`
	Skipped    int `json:"skipped"`
}

// ColdSummary is what a run's loads did to the file caches.
type ColdSummary struct {
	Loads    int    `json:"loads"`
	Cold     int    `json:"cold"`     // asked for a cold cache
	Verified int    `json:"verified"` // and the cold cache was verified
	State    string `json:"state"`    // all, some, none, unknown
}

// RunInputs are what a direct run of one benchmark ran with.
type RunInputs struct {
	Recipe    string `json:"recipe,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Model     string `json:"model,omitempty"`
	Ctx       int    `json:"ctx,omitempty"`
	Placement string `json:"placement,omitempty"`
	NodeCount int    `json:"node_count,omitempty"`
}

// factStep is the part of a step the facts read.
type factStep struct {
	id        int64
	runID     string
	idx       int
	profile   string
	state     string
	outcome   string
	modelFile string
	nodeCount int
	stats     *Stats
	load      *LoadResult
	expect    *ExpectResult
	phases    []PhaseResult
}

// readyIssue says whether a ready step had an issue: an expectation not met,
// a failed request, turn, concurrent request or task, or a failed phase.
// Only phase results and step columns are read, which a retention trim
// keeps, so the answer does not change when a run is trimmed.
func readyIssue(st *factStep) bool {
	if st.expect != nil && !st.expect.Pass {
		return true
	}
	if st.stats != nil && st.stats.Failed > 0 {
		return true
	}
	for _, p := range st.phases {
		if p.State == PhaseFailed || p.State == PhaseCancelled {
			return true
		}
		if p.Stats != nil && p.Stats.Failed > 0 {
			return true
		}
		for _, t := range p.Turns {
			if t.Status != 200 {
				return true
			}
		}
		if p.Concurrent != nil && p.Concurrent.Errors > 0 {
			return true
		}
		if p.Tasks != nil && (p.Tasks.Failed > 0 || p.Tasks.TimedOut > 0 || p.Tasks.NotSent > 0) {
			return true
		}
	}
	return false
}

// planSteps is the number of steps a run's snapshot expands to (0 when the
// snapshot cannot be read), and the snapshot's first benchmark.
func planSteps(snap json.RawMessage) (int, *benchspec.Benchmark) {
	if len(snap) == 0 {
		return 0, nil
	}
	var p runPlan
	if json.Unmarshal(snap, &p) != nil || p.Suite == nil {
		return 0, nil
	}
	n := 0
	var first *benchspec.Benchmark
	for _, it := range p.Suite.Benchmarks {
		leaves := leafBenchmarks(&p, it)
		n += len(leaves)
		if first == nil && len(leaves) > 0 {
			first = leaves[0]
		}
	}
	return n, first
}

func finishedState(s string) bool {
	return s == RunCompleted || s == RunFailed || s == RunCancelled || s == RunSkipped
}

// applyFacts sets the counts, result, cold summary and (for a direct run)
// inputs of r from its steps.
func applyFacts(r *Run, steps []*factStep) {
	planned, first := planSteps(r.Suite)
	c := RunCounts{}
	cold := ColdSummary{}
	for _, st := range steps {
		switch st.outcome {
		case OutcomeReady:
			c.Completed++
			if readyIssue(st) {
				c.WithIssues++
			} else {
				c.Passed++
			}
		case OutcomeFailed, OutcomeError:
			c.Failed++
		case OutcomeRefused, OutcomeRefusedByExclusion:
			c.Refused++
		case OutcomeCancelled:
			c.Cancelled++
		case OutcomeSkipped:
			c.Skipped++
		default:
			if st.state == StepSkipped {
				c.Skipped++
			}
		}
		if st.outcome != "" || st.state == StepSkipped {
			c.Done++
		}
		loads := 0
		for _, p := range st.phases {
			if p.Type != "load" {
				continue
			}
			loads++
			if p.Cold != nil && p.Cold.Requested {
				cold.Cold++
				if p.Cold.Verified {
					cold.Verified++
				}
			}
		}
		if loads == 0 && len(st.phases) == 0 && st.load != nil {
			loads = 1 // a step of the first runner: one load, no phases
		}
		cold.Loads += loads
	}
	c.Steps = max(planned, len(steps))
	if !finishedState(r.State) {
		c.ToGo = max(c.Steps-c.Done, 0)
	}
	switch {
	case cold.Loads == 0:
		cold.State = ColdUnknown
	case cold.Cold == 0:
		cold.State = ColdNone
	case cold.Cold == cold.Loads && cold.Verified == cold.Loads:
		cold.State = ColdAll
	default:
		cold.State = ColdSome
	}
	r.Counts, r.Cold = &c, &cold
	r.Result = runResult(r, &c)
	if r.Kind == RunKindBenchmark {
		r.Inputs = directInputs(first, steps)
	}
}

func runResult(r *Run, c *RunCounts) string {
	switch r.State {
	case RunQueued:
		return ResultQueued
	case RunRunning:
		return ResultRunning
	case RunPaused:
		return ResultWaiting
	case RunCancelled:
		return ResultCancelled
	case RunSkipped:
		return ResultSkipped
	case RunFailed:
		return ResultFailed
	}
	// completed
	judged := c.Completed + c.Failed + c.Refused + c.Cancelled
	if r.Kind == RunKindBenchmark && judged > 0 {
		switch {
		case c.Failed > 0:
			return ResultFailed
		case c.Refused == judged:
			return ResultRefused
		}
	}
	if c.Failed > 0 || c.Refused > 0 || c.Cancelled > 0 || c.WithIssues > 0 {
		return ResultWithIssues
	}
	return ResultPassed
}

func directInputs(b *benchspec.Benchmark, steps []*factStep) *RunInputs {
	in := &RunInputs{}
	if b != nil {
		in.Recipe, in.Profile = b.Recipe, b.Profile
		if a := b.Applied; a != nil {
			in.Model, in.Ctx = a.Model, a.Ctx
			if a.Placement != nil {
				in.Placement = a.Placement.String()
			}
		}
	}
	if len(steps) > 0 {
		st := steps[0]
		if st.profile != "" {
			in.Profile = st.profile
		}
		if st.modelFile != "" {
			in.Model = st.modelFile
		}
		in.NodeCount = st.nodeCount
	}
	if *in == (RunInputs{}) {
		return nil
	}
	return in
}

// loadFacts reads the steps and phases of the given runs, by run id.
func (s *Store) loadFacts(ids []string) (map[string][]*factStep, error) {
	out := map[string][]*factStep{}
	byID := map[int64]*factStep{}
	const chunk = 400
	for lo := 0; lo < len(ids); lo += chunk {
		part := ids[lo:min(lo+chunk, len(ids))]
		var args []any
		in := inList("run_id", part, &args)
		rows, err := s.db.Query(`SELECT id, run_id, idx, profile, state, outcome, model_file, node_count, stats, load, expect FROM run_steps WHERE `+in+` ORDER BY run_id, idx, id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			st := &factStep{}
			var stats, load, expect sql.NullString
			if err := rows.Scan(&st.id, &st.runID, &st.idx, &st.profile, &st.state, &st.outcome, &st.modelFile, &st.nodeCount, &stats, &load, &expect); err != nil {
				rows.Close()
				return nil, err
			}
			unmarshalNull(stats, &st.stats)
			unmarshalNull(load, &st.load)
			unmarshalNull(expect, &st.expect)
			out[st.runID] = append(out[st.runID], st)
			byID[st.id] = st
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		args = nil
		in = inList("run_id", part, &args)
		prow, err := s.db.Query(`SELECT step_id, type, state, result FROM run_phases WHERE `+in+` ORDER BY step_id, idx, id`, args...)
		if err != nil {
			return nil, err
		}
		for prow.Next() {
			var stepID int64
			var typ, state string
			var body sql.NullString
			if err := prow.Scan(&stepID, &typ, &state, &body); err != nil {
				prow.Close()
				return nil, err
			}
			var p PhaseResult
			if body.Valid {
				_ = json.Unmarshal([]byte(body.String), &p)
			}
			p.Type, p.State = typ, state
			if st := byID[stepID]; st != nil {
				st.phases = append(st.phases, p)
			}
		}
		if err := prow.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- run numbers (B2) ----

// RunNumbers gives every run its number: its place in queue order
// (queued_at, then id), the oldest run 1. The order is taken over the run
// rows and the run_removals ledger together, so a run whose row were ever
// removed (no code path removes one today: retention trims and keeps the
// row; a remove must log the run in run_removals, as a trim does) keeps its
// place and the number of every other run does not move. A trim, a restart
// and new runs (queued later) do not change any existing number.
func (s *Store) RunNumbers() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT id, queued_at FROM runs
	  UNION ALL SELECT run_id, MIN(queued_at) FROM run_removals WHERE run_id NOT IN (SELECT id FROM runs) GROUP BY run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		id string
		at int64
	}
	var all []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.id, &k.at); err != nil {
			return nil, err
		}
		all = append(all, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].at != all[j].at {
			return all[i].at < all[j].at
		}
		return all[i].id < all[j].id
	})
	out := make(map[string]int, len(all))
	for i, k := range all {
		out[k.id] = i + 1
	}
	return out, nil
}

// RunIDByNumber resolves a run number to the run's id, or ErrNotFound (also
// for the number of a run whose row is gone).
func (s *Store) RunIDByNumber(n int) (string, error) {
	nums, err := s.RunNumbers()
	if err != nil {
		return "", err
	}
	for id, v := range nums {
		if v == n {
			var one int
			if err := s.db.QueryRow(`SELECT 1 FROM runs WHERE id=?`, id).Scan(&one); err != nil {
				return "", ErrNotFound
			}
			return id, nil
		}
	}
	return "", ErrNotFound
}

// ResolveRunRef turns a run reference (an id, or a number: all digits) into an id.
func (s *Store) ResolveRunRef(ref string) (string, error) {
	if n, err := strconv.Atoi(ref); err == nil && n > 0 && strings.Trim(ref, "0123456789") == "" {
		return s.RunIDByNumber(n)
	}
	return ref, nil
}

// withFacts adds the number and the facts to runs.
func (s *Store) withFacts(runs []*Run, nums map[string]int) error {
	if len(runs) == 0 {
		return nil
	}
	if nums == nil {
		var err error
		if nums, err = s.RunNumbers(); err != nil {
			return err
		}
	}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	facts, err := s.loadFacts(ids)
	if err != nil {
		return err
	}
	for _, r := range runs {
		r.Number = nums[r.ID]
		applyFacts(r, facts[r.ID])
	}
	return nil
}

func matchesResult(r *Run, f string) bool {
	switch f {
	case "":
		return true
	case ResultHasFailed:
		return r.Result == ResultFailed || (r.Counts != nil && r.Counts.Failed > 0)
	case ResultHasRefused:
		return r.Result == ResultRefused || (r.Counts != nil && r.Counts.Refused > 0)
	case ResultClean:
		return r.Result == ResultPassed
	}
	return r.Result == f
}

// ListRunsPage lists runs newest first with their number and facts, from
// f.Offset, and the total under the same filters (result= included).
func (s *Store) ListRunsPage(f RunFilter) ([]Run, int, error) {
	if !ValidResultFilter(f.Result) {
		return nil, 0, errors.New("result must be one of " + strings.Join(ResultFilters(), ", "))
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	offset := max(f.Offset, 0)
	where, args := runWhere(f)
	nums, err := s.RunNumbers()
	if err != nil {
		return nil, 0, err
	}
	if f.Result == "" {
		var total int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE `+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
		rs, err := s.queryRuns(where+` ORDER BY queued_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset))
		if err != nil {
			return nil, 0, err
		}
		if err := s.withFacts(rs, nums); err != nil {
			return nil, 0, err
		}
		return derefRuns(rs), total, nil
	}
	// A result filter needs every matching run's facts.
	rs, err := s.queryRuns(where+` ORDER BY queued_at DESC, id DESC`, args)
	if err != nil {
		return nil, 0, err
	}
	if err := s.withFacts(rs, nums); err != nil {
		return nil, 0, err
	}
	var kept []*Run
	for _, r := range rs {
		if matchesResult(r, f.Result) {
			kept = append(kept, r)
		}
	}
	total := len(kept)
	if offset >= total {
		return []Run{}, total, nil
	}
	return derefRuns(kept[offset:min(offset+limit, total)]), total, nil
}

func derefRuns(rs []*Run) []Run {
	out := make([]Run, len(rs))
	for i, r := range rs {
		r.Suite = nil // the list stays small; GET one run for the snapshot
		out[i] = *r
	}
	return out
}

func (s *Store) queryRuns(tail string, args []any) ([]*Run, error) {
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs WHERE `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// runWhere is the WHERE clause of a run filter (without result=).
func runWhere(f RunFilter) (string, []any) {
	q := `1=1`
	var args []any
	if f.Suite != "" {
		q += ` AND suite_id=?`
		args = append(args, f.Suite)
	}
	if f.Kind != "" {
		q += ` AND kind=?`
		args = append(args, f.Kind)
	}
	if f.Benchmark != "" {
		q += ` AND ((kind='benchmark' AND target_id=?) OR id IN (SELECT run_id FROM run_steps WHERE benchmark_id=? OR json_extract(spec, '$.recipe')=?))`
		args = append(args, f.Benchmark, f.Benchmark, f.Benchmark)
	}
	if f.Model != "" {
		q += ` AND id IN (SELECT run_id FROM run_steps WHERE model_file=?`
		args = append(args, f.Model)
		if len(f.ModelProfiles) > 0 {
			q += ` OR (model_file='' AND profile IN (?` + strings.Repeat(`,?`, len(f.ModelProfiles)-1) + `))`
			for _, p := range f.ModelProfiles {
				args = append(args, p)
			}
		}
		q += `)`
	}
	if f.State != "" {
		q += ` AND state=?`
		args = append(args, f.State)
	}
	return q, args
}

// ---- entry addresses (B2) ----

var (
	splitPart = regexp.MustCompile(`(?i)-\d{5}-of-\d{5}$`)
	notSlug   = regexp.MustCompile(`[^a-z0-9]+`)
)

// entryBase is a step's slug before clashes are settled: the model's tidy
// name (the file name without .gguf, a split part's number and a leading
// "<publisher>_", as TidyModelName in the api package reads it; the profile
// when no file was recorded), then the placement mode of a recipe use and
// the number of machines used, all in lower case with dashes.
func entryBase(st *Step) string {
	name := st.ModelFile
	var place string
	if st.Spec != nil && st.Spec.Applied != nil {
		if name == "" {
			name = st.Spec.Applied.Model
		}
		if p := st.Spec.Applied.Placement; p != nil {
			place = p.Mode
			if place == "" {
				place = benchspec.PlaceAuto
			}
		}
	}
	if name == "" {
		name = st.Profile
	}
	name = splitPart.ReplaceAllString(strings.TrimSuffix(strings.TrimSuffix(name, ".gguf"), ".GGUF"), "")
	if i := strings.Index(name, "_"); i > 0 && !strings.ContainsAny(name[:i], "-. ") && i+1 < len(name) {
		name = name[i+1:]
	}
	if st.NodeCount > 0 {
		place += strconv.Itoa(st.NodeCount)
	}
	s := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name+" "+place), "-"), "-")
	if s == "" {
		s = "entry"
	}
	return s
}

// EntrySlugs are the entry addresses of a run's steps, in step order. A slug
// shared by two entries gets "-<step index>"; members of one parallel step
// that still share it get "-<n>" after that (the second is -2).
func EntrySlugs(steps []Step) []string {
	out := make([]string, len(steps))
	seen := map[string]int{}
	for i := range steps {
		out[i] = entryBase(&steps[i])
		seen[out[i]]++
	}
	for i := range out {
		if seen[out[i]] > 1 {
			out[i] += "-" + strconv.Itoa(steps[i].Idx)
		}
	}
	used := map[string]int{}
	for i := range out {
		used[out[i]]++
		if n := used[out[i]]; n > 1 {
			out[i] += "-" + strconv.Itoa(n)
		}
	}
	return out
}
