package bench

// The tasks phase (release 0.1.8): a set of fixed tasks is sent once each to a
// loaded profile, and every answer is kept, with its tokens and speed, for a
// scorer outside Potluck. Potluck never runs what a model wrote and never
// judges it; a scorer posts points per task afterwards (PutScores), and the
// run then shows the total per scorer.
//
// Storage: the answers and reasoning texts are rows of task_results, apart
// from the run's step and phase rows (which hold only the counts and the
// per-task figures), and go with their step when a run is removed (ON DELETE
// CASCADE). Scores are rows of task_scores, one per task and scorer.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// ErrConflict means a change contradicts what is stored: a task set version
// whose text differs from the stored one, or a set that answers still use.
var ErrConflict = errors.New("conflict")

func conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

// Task states.
const (
	TaskAnswered = "answered"  // HTTP 200 and a whole answer (finish_reason says whether it hit max_tokens)
	TaskFailed   = "failed"    // the request failed or did not answer 200
	TaskTimedOut = "timed_out" // cut by the task's or the phase's time limit; what had arrived is kept
	TaskNotSent  = "not_sent"  // never sent: the phase's time ran out, the run was cancelled, the model did not load
)

// Thinking as seen in the answers. Potluck sends nothing about thinking: it
// follows the profile. A reasoning text in any answer is "seen".
const (
	ThinkingSeen    = "seen"
	ThinkingNotSeen = "not_seen"
)

// MaxAnswerBytes bounds one stored answer (and one reasoning text).
const MaxAnswerBytes = 4 << 20

// TaskScore is one scorer's points for one task.
type TaskScore struct {
	Scorer        string   `json:"scorer"`
	ScorerVersion string   `json:"scorer_version,omitempty"`
	Points        float64  `json:"points"`
	MaxPoints     float64  `json:"max_points"`
	FailedTests   []string `json:"failed_tests,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	UpdatedAt     int64    `json:"updated_at"`
}

// ScorerTotal adds up one scorer's points over a run (or a phase): Scored
// tasks of Tasks have a score.
type ScorerTotal struct {
	Scorer        string  `json:"scorer"`
	ScorerVersion string  `json:"scorer_version,omitempty"`
	Points        float64 `json:"points"`
	MaxPoints     float64 `json:"max_points"`
	Scored        int     `json:"tasks_scored"`
	Tasks         int     `json:"tasks"`
	UpdatedAt     int64   `json:"updated_at"`
}

// TaskItem is one task of a tasks phase as the run shows it: the figures,
// not the answer text.
type TaskItem struct {
	ResultID         int64                 `json:"result_id"`
	TaskID           string                `json:"task_id"`
	Title            string                `json:"title,omitempty"`
	State            string                `json:"state"`
	Status           int                   `json:"status,omitempty"`
	FinishReason     string                `json:"finish_reason,omitempty"`
	PromptTokens     int                   `json:"prompt_tokens"`
	CompletionTokens int                   `json:"completion_tokens"`
	TTFTMS           *float64              `json:"ttft_ms,omitempty"`
	DecodeTPS        float64               `json:"decode_tps,omitempty"`
	WallMS           float64               `json:"wall_ms"`
	AnswerChars      int                   `json:"answer_chars"`
	ReasoningChars   int                   `json:"reasoning_chars"`
	AnswerSHA256     string                `json:"answer_sha256,omitempty"`
	Error            string                `json:"error,omitempty"`
	StartedAt        int64                 `json:"started_at,omitempty"`
	Scores           map[string]*TaskScore `json:"scores,omitempty"` // added when read
}

// TasksResult is what a tasks phase did.
type TasksResult struct {
	SetID      string `json:"set_id"`
	SetVersion int    `json:"set_version"`
	SetSHA256  string `json:"set_sha256"`
	SetTitle   string `json:"set_title,omitempty"`
	Inline     bool   `json:"inline,omitempty"` // the set was given inside the benchmark
	Total      int    `json:"total"`
	Sent       int    `json:"sent"`
	Answered   int    `json:"answered"`
	Failed     int    `json:"failed"`    // failed and timed out
	TimedOut   int    `json:"timed_out"` // of Failed, cut by a time limit
	NotSent    int    `json:"not_sent"`
	// PhaseTimedOut: the phase's own time limit ended it.
	PhaseTimedOut    bool          `json:"phase_timed_out,omitempty"`
	Thinking         string        `json:"thinking"`
	DecodeTPS        *Summary      `json:"decode_tps,omitempty"` // over the answered tasks
	TTFT             *Summary      `json:"ttft_ms,omitempty"`
	CompletionTokens int           `json:"completion_tokens"`
	Items            []TaskItem    `json:"items"`
	Scores           []ScorerTotal `json:"scores,omitempty"` // added when read
}

// TaskResult is one stored task answer with where it came from.
type TaskResult struct {
	ID          int64  `json:"id"`
	RunID       string `json:"run_id"`
	StepID      int64  `json:"-"`
	StepIdx     int    `json:"step"`
	BenchmarkID string `json:"benchmark_id,omitempty"`
	Profile     string `json:"profile,omitempty"`
	PhaseIdx    int    `json:"phase"`
	Seq         int    `json:"seq"`
	SetID       string `json:"set_id"`
	SetVersion  int    `json:"set_version"`
	SetSHA256   string `json:"set_sha256"`
	TaskItem
	Answer    string `json:"answer,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

// TaskQuery narrows RunTasks: one step, and whether to include the texts.
type TaskQuery struct {
	Step    *int
	Answers bool
}

// ---- task sets ----

// StoredTaskSet is a stored task set version.
type StoredTaskSet struct {
	Set       *benchspec.TaskSet
	SHA256    string
	CreatedAt int64
}

// TaskSetSummary is a stored task set version without its prompts.
type TaskSetSummary struct {
	ID        string `json:"id"`
	Version   int    `json:"version"`
	SHA256    string `json:"sha256"`
	Title     string `json:"title,omitempty"`
	Tasks     int    `json:"tasks"`
	CreatedAt int64  `json:"created_at"`
	Results   int    `json:"results"` // stored answers to it
}

// SaveTaskSet stores a validated task set version. A version is fixed once
// stored: the same content again is accepted and changes nothing (created
// false); other content under the same id and version is ErrConflict.
func (s *Store) SaveTaskSet(ts *benchspec.TaskSet, now int64) (created bool, err error) {
	doc, err := json.Marshal(ts)
	if err != nil {
		return false, err
	}
	sum := ts.SHA256()
	err = s.write(func(tx *sql.Tx) error {
		var have string
		switch err := tx.QueryRow(`SELECT sha256 FROM task_sets WHERE id=? AND version=?`, ts.ID, ts.Version).Scan(&have); {
		case err == nil && have == sum:
			return nil
		case err == nil:
			return conflictf("task set %q version %d is stored with other prompts: give the changed set a new version", ts.ID, ts.Version)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		created = true
		_, err := tx.Exec(`INSERT INTO task_sets (id, version, sha256, title, tasks, doc, created_at) VALUES (?,?,?,?,?,?,?)`,
			ts.ID, ts.Version, sum, ts.Title, len(ts.Tasks), string(doc), now)
		return err
	})
	return created, err
}

// GetTaskSet is a stored version; version 0 is the newest.
func (s *Store) GetTaskSet(id string, version int) (*StoredTaskSet, error) {
	q := `SELECT doc, sha256, created_at FROM task_sets WHERE id=? AND version=?`
	args := []any{id, version}
	if version == 0 {
		q, args = `SELECT doc, sha256, created_at FROM task_sets WHERE id=? ORDER BY version DESC LIMIT 1`, []any{id}
	}
	var doc string
	out := &StoredTaskSet{}
	err := s.db.QueryRow(q, args...).Scan(&doc, &out.SHA256, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var ts benchspec.TaskSet
	if err := json.Unmarshal([]byte(doc), &ts); err != nil {
		return nil, err
	}
	out.Set = &ts
	return out, nil
}

// ListTaskSets lists every stored version, by id then version.
func (s *Store) ListTaskSets() ([]TaskSetSummary, error) {
	rows, err := s.db.Query(`SELECT t.id, t.version, t.sha256, t.title, t.tasks, t.created_at,
	  (SELECT COUNT(*) FROM task_results r WHERE r.set_id=t.id AND r.set_version=t.version AND r.set_sha256=t.sha256)
	  FROM task_sets t ORDER BY t.id, t.version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskSetSummary{}
	for rows.Next() {
		var t TaskSetSummary
		if err := rows.Scan(&t.ID, &t.Version, &t.SHA256, &t.Title, &t.Tasks, &t.CreatedAt, &t.Results); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTaskSet removes a stored version. A version with stored answers is
// kept (ErrConflict): the answers would lose their prompts.
func (s *Store) DeleteTaskSet(id string, version int) error {
	return s.write(func(tx *sql.Tx) error {
		var sum string
		if err := tx.QueryRow(`SELECT sha256 FROM task_sets WHERE id=? AND version=?`, id, version).Scan(&sum); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM task_results WHERE set_id=? AND set_version=? AND set_sha256=?`, id, version, sum).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return conflictf("task set %q version %d has %d stored answers: it is kept so they keep their prompts", id, version, n)
		}
		_, err := tx.Exec(`DELETE FROM task_sets WHERE id=? AND version=?`, id, version)
		return err
	})
}

// ---- task results ----

// AddTaskResult stores one task's answer and sets its ID.
func (s *Store) AddTaskResult(r *TaskResult) error {
	var ttft any
	if r.TTFTMS != nil {
		ttft = *r.TTFTMS
	}
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO task_results (step_id, run_id, phase_idx, seq, task_id, title, set_id, set_version, set_sha256, state, status, finish_reason,
		  prompt_tokens, completion_tokens, ttft_ms, decode_tps, wall_ms, error, started_at, answer_chars, reasoning_chars, answer_sha256, answer, reasoning)
		  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.StepID, r.RunID, r.PhaseIdx, r.Seq, r.TaskID, r.Title, r.SetID, r.SetVersion, r.SetSHA256, r.State, r.Status, r.FinishReason,
			r.PromptTokens, r.CompletionTokens, ttft, r.DecodeTPS, r.WallMS, r.Error, r.StartedAt, r.AnswerChars, r.ReasoningChars, r.AnswerSHA256, r.Answer, r.Reasoning)
		if err != nil {
			return err
		}
		r.ID, err = res.LastInsertId()
		r.ResultID = r.ID
		return err
	})
}

const taskCols = `t.id, t.run_id, t.step_id, s.idx, s.benchmark_id, s.profile, t.phase_idx, t.seq, t.task_id, t.title, t.set_id, t.set_version, t.set_sha256, t.state, t.status,
  t.finish_reason, t.prompt_tokens, t.completion_tokens, t.ttft_ms, t.decode_tps, t.wall_ms, t.error, t.started_at, t.answer_chars, t.reasoning_chars, t.answer_sha256`

func scanTask(sc interface{ Scan(...any) error }, texts bool) (TaskResult, error) {
	var r TaskResult
	var ttft sql.NullFloat64
	dst := []any{&r.ID, &r.RunID, &r.StepID, &r.StepIdx, &r.BenchmarkID, &r.Profile, &r.PhaseIdx, &r.Seq, &r.TaskID, &r.Title, &r.SetID, &r.SetVersion, &r.SetSHA256,
		&r.State, &r.Status, &r.FinishReason, &r.PromptTokens, &r.CompletionTokens, &ttft, &r.DecodeTPS, &r.WallMS, &r.Error, &r.StartedAt, &r.AnswerChars,
		&r.ReasoningChars, &r.AnswerSHA256}
	if texts {
		dst = append(dst, &r.Answer, &r.Reasoning)
	}
	if err := sc.Scan(dst...); err != nil {
		return r, err
	}
	if ttft.Valid {
		v := ttft.Float64
		r.TTFTMS = &v
	}
	r.ResultID = r.ID
	return r, nil
}

// RunTasks lists a run's task results in step, phase and send order, with
// their scores; the answer texts only when q.Answers.
func (s *Store) RunTasks(runID string, q TaskQuery) ([]TaskResult, error) {
	cols := taskCols
	if q.Answers {
		cols += `, t.answer, t.reasoning`
	}
	where, args := `t.run_id=?`, []any{runID}
	if q.Step != nil {
		where += ` AND s.idx=?`
		args = append(args, *q.Step)
	}
	rows, err := s.db.Query(`SELECT `+cols+` FROM task_results t JOIN run_steps s ON s.id=t.step_id WHERE `+where+` ORDER BY s.idx, t.step_id, t.phase_idx, t.seq, t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskResult{}
	for rows.Next() {
		r, err := scanTask(rows, q.Answers)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	scores, err := s.scoresOf(runID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Scores = scores[out[i].ID]
	}
	return out, nil
}

// TaskResult is one task result of a run with its texts and scores, or ErrNotFound.
func (s *Store) TaskResult(runID string, id int64) (*TaskResult, error) {
	r, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+`, t.answer, t.reasoning FROM task_results t JOIN run_steps s ON s.id=t.step_id WHERE t.run_id=? AND t.id=?`, runID, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	scores, err := s.scoresOf(runID)
	if err != nil {
		return nil, err
	}
	r.Scores = scores[r.ID]
	return &r, nil
}

// ---- scores ----

// ScoreInput is one task's score as a scorer posts it. The task is named by
// its result id, or by step (and phase when the step has more than one
// tasks phase) and task id.
type ScoreInput struct {
	Result      int64    `json:"result,omitempty"`
	Step        *int     `json:"step,omitempty"`
	Phase       *int     `json:"phase,omitempty"`
	Task        string   `json:"task,omitempty"`
	Points      float64  `json:"points"`
	MaxPoints   float64  `json:"max_points"`
	FailedTests []string `json:"failed_tests,omitempty"`
	Notes       string   `json:"notes,omitempty"`
}

// ScoresDoc is the body of PUT /api/bench/runs/{id}/scores/{scorer}.
type ScoresDoc struct {
	ScorerVersion string       `json:"scorer_version,omitempty"`
	Scores        []ScoreInput `json:"scores"`
}

// Limits of a scores post.
const (
	MaxScores      = 10000
	MaxFailedTests = 500
)

var scorerRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// PutScores replaces every score of one scorer on a run with doc: posting
// the same scores again changes nothing, so a scorer can simply post again.
// It returns the scorer's total over the run.
func (s *Store) PutScores(runID, scorer string, doc ScoresDoc, now int64) (*ScorerTotal, error) {
	if !scorerRe.MatchString(scorer) {
		return nil, invalidf("scorer %q must be 1 to 64 characters: lower-case letters, digits, '.', '-' or '_'", scorer)
	}
	if len(doc.ScorerVersion) > 200 {
		return nil, invalidf("scorer_version is longer than 200 characters")
	}
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE id=?`, runID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	if len(doc.Scores) == 0 || len(doc.Scores) > MaxScores {
		return nil, invalidf("scores: between 1 and %d are required", MaxScores)
	}
	results, err := s.RunTasks(runID, TaskQuery{})
	if err != nil {
		return nil, err
	}
	byID := map[int64]*TaskResult{}
	for i := range results {
		byID[results[i].ID] = &results[i]
	}
	var errs []string
	ids := make([]int64, len(doc.Scores))
	seen := map[int64]int{}
	for i, in := range doc.Scores {
		bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf("scores[%d]: %s", i, fmt.Sprintf(f, a...))) }
		id, why := resolveScore(in, results, byID)
		if why != "" {
			bad("%s", why)
			continue
		}
		if j, dup := seen[id]; dup {
			bad("the same task as scores[%d] is scored twice", j)
		}
		seen[id], ids[i] = i, id
		switch {
		case math.IsNaN(in.MaxPoints) || math.IsInf(in.MaxPoints, 0) || in.MaxPoints <= 0:
			bad("max_points must be more than 0")
		case math.IsNaN(in.Points) || math.IsInf(in.Points, 0) || in.Points < 0 || in.Points > in.MaxPoints:
			bad("points must be 0 to max_points (%g), got %g", in.MaxPoints, in.Points)
		}
		if len(in.FailedTests) > MaxFailedTests {
			bad("failed_tests: at most %d", MaxFailedTests)
		}
		for _, n := range in.FailedTests {
			if len(n) > 300 {
				bad("a failed test name is longer than 300 characters")
				break
			}
		}
		if len(in.Notes) > 4000 {
			bad("notes are longer than 4000 characters")
		}
	}
	if len(errs) > 0 {
		if len(errs) > 20 {
			errs = append(errs[:20], fmt.Sprintf("and %d more", len(errs)-20))
		}
		return nil, invalidf("%s", strings.Join(errs, "; "))
	}
	err = s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM task_scores WHERE run_id=? AND scorer=?`, runID, scorer); err != nil {
			return err
		}
		for i, in := range doc.Scores {
			var failed any
			if len(in.FailedTests) > 0 {
				b, _ := json.Marshal(in.FailedTests)
				failed = string(b)
			}
			if _, err := tx.Exec(`INSERT INTO task_scores (result_id, run_id, scorer, scorer_version, points, max_points, failed_tests, notes, updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
				ids[i], runID, scorer, doc.ScorerVersion, in.Points, in.MaxPoints, failed, in.Notes, now); err != nil {
				return err
			}
		}
		return refreshTaskPoints(tx, runID)
	})
	if err != nil {
		return nil, err
	}
	totals, err := s.ScoreTotals(runID)
	if err != nil {
		return nil, err
	}
	for i := range totals {
		if totals[i].Scorer == scorer {
			return &totals[i], nil
		}
	}
	return nil, errors.New("the scores were stored but cannot be read back")
}

func resolveScore(in ScoreInput, results []TaskResult, byID map[int64]*TaskResult) (int64, string) {
	if in.Result != 0 {
		if in.Step != nil || in.Phase != nil || in.Task != "" {
			return 0, "give result, or step and task, not both"
		}
		if byID[in.Result] == nil {
			return 0, fmt.Sprintf("result %d is not a task result of this run", in.Result)
		}
		return in.Result, ""
	}
	if in.Step == nil || in.Task == "" {
		return 0, "name the task: result, or step and task (and phase when the step has two tasks phases)"
	}
	var match []int64
	stepSeen := false
	for _, r := range results {
		if r.StepIdx != *in.Step {
			continue
		}
		stepSeen = true
		if r.TaskID == in.Task && (in.Phase == nil || r.PhaseIdx == *in.Phase) {
			match = append(match, r.ID)
		}
	}
	switch {
	case !stepSeen:
		return 0, fmt.Sprintf("step %d of this run has no task results", *in.Step)
	case len(match) == 0:
		return 0, fmt.Sprintf("task %q is not in step %d", in.Task, *in.Step)
	case len(match) > 1:
		return 0, fmt.Sprintf("task %q is in more than one tasks phase of step %d: give phase", in.Task, *in.Step)
	}
	return match[0], ""
}

// DeleteScores removes one scorer's scores from a run (ErrNotFound when it has none).
func (s *Store) DeleteScores(runID, scorer string) error {
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM task_scores WHERE run_id=? AND scorer=?`, runID, scorer)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return refreshTaskPoints(tx, runID)
	})
}

// scoresOf maps a run's result ids to their scores by scorer.
func (s *Store) scoresOf(runID string) (map[int64]map[string]*TaskScore, error) {
	rows, err := s.db.Query(`SELECT result_id, scorer, scorer_version, points, max_points, failed_tests, notes, updated_at FROM task_scores WHERE run_id=?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]*TaskScore{}
	for rows.Next() {
		var id int64
		var failed sql.NullString
		sc := &TaskScore{}
		if err := rows.Scan(&id, &sc.Scorer, &sc.ScorerVersion, &sc.Points, &sc.MaxPoints, &failed, &sc.Notes, &sc.UpdatedAt); err != nil {
			return nil, err
		}
		unmarshalSlice(failed, &sc.FailedTests)
		if out[id] == nil {
			out[id] = map[string]*TaskScore{}
		}
		out[id][sc.Scorer] = sc
	}
	return out, rows.Err()
}

// totalsOver adds up the scores of the given items by scorer, by scorer name.
func totalsOver(items []map[string]*TaskScore) []ScorerTotal {
	by := map[string]*ScorerTotal{}
	for _, m := range items {
		for name, sc := range m {
			t := by[name]
			if t == nil {
				t = &ScorerTotal{Scorer: name}
				by[name] = t
			}
			t.Points += sc.Points
			t.MaxPoints += sc.MaxPoints
			t.Scored++
			if sc.UpdatedAt >= t.UpdatedAt {
				t.UpdatedAt, t.ScorerVersion = sc.UpdatedAt, sc.ScorerVersion
			}
		}
	}
	out := []ScorerTotal{}
	for _, t := range by {
		t.Tasks = len(items)
		t.Points, t.MaxPoints = round3(t.Points), round3(t.MaxPoints)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scorer < out[j].Scorer })
	return out
}

// ScoreTotals is each scorer's total over a run's task results, by scorer name.
func (s *Store) ScoreTotals(runID string) ([]ScorerTotal, error) {
	results, err := s.RunTasks(runID, TaskQuery{})
	if err != nil {
		return nil, err
	}
	items := make([]map[string]*TaskScore, len(results))
	for i := range results {
		items[i] = results[i].Scores
	}
	return totalsOver(items), nil
}

// attachTaskScores puts the scores on the tasks phases of a run as read.
func (s *Store) attachTaskScores(r *Run) error {
	has := false
	for i := range r.Steps {
		for j := range r.Steps[i].Phases {
			has = has || r.Steps[i].Phases[j].Tasks != nil
		}
	}
	if !has {
		return nil
	}
	scores, err := s.scoresOf(r.ID)
	if err != nil || len(scores) == 0 {
		return err
	}
	for i := range r.Steps {
		for j := range r.Steps[i].Phases {
			tr := r.Steps[i].Phases[j].Tasks
			if tr == nil {
				continue
			}
			items := make([]map[string]*TaskScore, len(tr.Items))
			for k := range tr.Items {
				tr.Items[k].Scores = scores[tr.Items[k].ResultID]
				items[k] = tr.Items[k].Scores
			}
			if t := totalsOver(items); len(t) > 0 {
				tr.Scores = t
			}
		}
	}
	return nil
}

// ---- the phase ----

// resolveTaskSet is the phase's set: inline, or the stored version it names.
func (br *benchRun) resolveTaskSet(p *benchspec.Phase) (*benchspec.TaskSet, *TasksResult, error) {
	if p.Tasks != nil {
		set := p.Tasks
		return set, &TasksResult{SetID: set.ID, SetVersion: set.Version, SetSHA256: set.SHA256(), SetTitle: set.Title, Inline: true}, nil
	}
	got, err := br.x.m.o.Store.GetTaskSet(p.TaskSet, p.TaskSetVersion)
	switch {
	case errors.Is(err, ErrNotFound) && p.TaskSetVersion != 0:
		return nil, nil, fmt.Errorf("task set %q version %d is not stored", p.TaskSet, p.TaskSetVersion)
	case errors.Is(err, ErrNotFound):
		return nil, nil, fmt.Errorf("task set %q is not stored: store it with POST /api/bench/task-sets first", p.TaskSet)
	case err != nil:
		return nil, nil, fmt.Errorf("task set %q: %v", p.TaskSet, err)
	}
	set := got.Set
	return set, &TasksResult{SetID: set.ID, SetVersion: set.Version, SetSHA256: got.SHA256, SetTitle: set.Title}, nil
}

func capText(s string) (string, bool) {
	if len(s) <= MaxAnswerBytes {
		return s, false
	}
	return s[:MaxAnswerBytes], true
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// phaseTasks sends each task once, in order, and records every answer. A
// task that fails or runs out of time is recorded and the next one is sent;
// the phase ends early only at its own time limit or a cancel, and then
// records the rest as not sent. It never fails the step for a task.
func (br *benchRun) phaseTasks(i int, p *benchspec.Phase, pr *PhaseResult) {
	x, st := br.x, br.st
	set, tr, err := br.resolveTaskSet(p)
	if err != nil {
		br.fail(OutcomeError, err.Error())
		return
	}
	tr.Total, tr.Items, tr.Thinking = len(set.Tasks), []TaskItem{}, ThinkingNotSeen
	pr.Tasks = tr
	start := time.Now()
	var deadline time.Time
	if d := p.PhaseTimeout(); d > 0 {
		deadline = start.Add(d)
	}
	if sd, ok := br.ctx.Deadline(); ok {
		if lim := sd.Add(-benchspec.TasksPhaseMargin * time.Second); deadline.IsZero() || lim.Before(deadline) {
			deadline = lim
		}
	}
	pctx, cancel := context.WithCancel(br.ctx)
	if !deadline.IsZero() {
		pctx, cancel = context.WithDeadline(br.ctx, deadline)
	}
	defer cancel()

	seq := 0
	record := func(r *TaskResult) {
		seq++
		r.StepID, r.RunID, r.PhaseIdx, r.Seq = st.ID, st.RunID, i, seq
		r.SetID, r.SetVersion, r.SetSHA256 = tr.SetID, tr.SetVersion, tr.SetSHA256
		if err := x.m.o.Store.AddTaskResult(r); err != nil {
			x.m.log.Warn("cannot store a task answer", "run", st.RunID, "task", r.TaskID, "err", err)
		}
		tr.Items = append(tr.Items, r.TaskItem)
	}
	notSent := func(from int, why string) {
		for _, t := range set.Tasks[from:] {
			record(&TaskResult{TaskItem: TaskItem{TaskID: t.ID, Title: t.Title, State: TaskNotSent, Error: "not sent: " + why}})
		}
	}
	if !br.ensureLoaded() {
		notSent(0, "the model did not load")
		br.sumTasks(tr)
		return
	}
	n := len(set.Tasks)
	x.setUnits(br.track, n, fmt.Sprintf("Tasks: 0 of %d", n))
	for k, t := range set.Tasks {
		if x.a.cancelled() {
			br.fail(OutcomeCancelled, "cancelled")
			notSent(k, "the run was cancelled")
			break
		}
		if pctx.Err() != nil {
			tr.PhaseTimedOut = true
			notSent(k, "the phase's time limit ran out")
			break
		}
		x.setDetail(br.track, map[string]any{"task": t.ID, "text": fmt.Sprintf("Task %d of %d: %s", k+1, n, t.ID)})
		record(br.sendTask(pctx, set, t, tr))
		x.unitDone(br.track, fmt.Sprintf("Tasks: %d of %d", k+1, n))
	}
	br.sumTasks(tr)
}

// sendTask sends one task and turns the answer into a result.
func (br *benchRun) sendTask(pctx context.Context, set *benchspec.TaskSet, t benchspec.Task, tr *TasksResult) *TaskResult {
	x, st := br.x, br.st
	req := set.Request(t)
	r := &TaskResult{TaskItem: TaskItem{TaskID: t.ID, Title: t.Title, StartedAt: x.m.o.Now().UnixMilli()}}
	ctx, cancel := pctx, context.CancelFunc(func() {})
	if d := t.Timeout(); d > 0 {
		ctx, cancel = context.WithTimeout(pctx, d)
	}
	defer cancel()
	x.emitRequest(st, map[string]any{"phase": benchspec.PhaseTasks, "request": t.ID, "state": "started"})
	sent := time.Now()
	resp, err := x.m.o.API.Chat(ctx, st.RunID, BuildChatRequest(br.b.Profile, req), func(p ChatProgress) {
		x.emitRequest(st, map[string]any{"phase": benchspec.PhaseTasks, "request": t.ID, "state": "streaming", "tokens": p.Tokens, "elapsed_ms": p.ElapsedMS})
		x.setInflight(br.track, p.Tokens, req.MaxTok())
	})
	tr.Sent++
	switch {
	case err != nil && ctx.Err() != nil:
		r.State = TaskTimedOut
		if pctx.Err() != nil {
			tr.PhaseTimedOut = true
			r.Error = "timed out: the phase's time limit ran out"
		} else {
			r.Error = fmt.Sprintf("timed out after %s (the task's timeout_s)", t.Timeout())
		}
	case err != nil:
		r.State, r.Error = TaskFailed, "request failed: "+err.Error()
	case resp.Status != 200:
		r.State, r.Status, r.Error = TaskFailed, resp.Status, resp.ErrMessage
		if r.Error == "" {
			r.Error = fmt.Sprintf("the scheduler answered %d", resp.Status)
		}
	default:
		r.State, r.Status = TaskAnswered, 200
	}
	r.WallMS = round1(ms(time.Since(sent)))
	if resp != nil {
		if resp.WallMS > 0 {
			r.WallMS = round1(resp.WallMS)
		}
		r.TTFTMS, r.FinishReason = resp.TTFTMS, resp.FinishReason
		var tm wireTimings
		if len(resp.Timings) > 0 {
			json.Unmarshal(resp.Timings, &tm)
		}
		r.PromptTokens, r.CompletionTokens = resp.PromptTokens, resp.CompletionTokens
		if r.PromptTokens == 0 {
			r.PromptTokens = int(tm.PromptN + tm.CacheN)
		}
		if r.CompletionTokens == 0 {
			r.CompletionTokens = int(tm.PredictedN)
		}
		r.DecodeTPS = round1(tm.PredictedPerSecond)
		if r.DecodeTPS == 0 && r.CompletionTokens > 0 && r.TTFTMS != nil && r.WallMS > *r.TTFTMS {
			r.DecodeTPS = round1(float64(r.CompletionTokens) / ((r.WallMS - *r.TTFTMS) / 1000))
		}
		answer, cut := capText(resp.Text)
		reasoning, cutR := capText(resp.Reasoning)
		if cut || cutR {
			r.Error = strings.TrimPrefix(r.Error+"; the stored text is cut at 4 MiB", "; ")
		}
		if r.State != TaskFailed {
			r.Answer, r.Reasoning = answer, reasoning
			r.AnswerChars, r.ReasoningChars = len([]rune(resp.Text)), len([]rune(resp.Reasoning))
			if resp.Text != "" {
				r.AnswerSHA256 = sha(resp.Text)
			}
		}
	}
	x.emitRequest(st, map[string]any{"phase": benchspec.PhaseTasks, "request": t.ID, "state": r.State})
	e := TranscriptEntry{Phase: benchspec.PhaseTasks, PhaseIdx: br.curPhase, Name: t.ID, StartedAt: r.StartedAt, Messages: req.ChatMessages(),
		Sampling: map[string]any{"max_tokens": req.MaxTok(), "temperature": req.Temp(), "seed": req.SeedVal(), "stream": req.Streams()},
		Status:   r.Status, Error: r.Error, TTFTMS: r.TTFTMS, WallMS: r.WallMS, FinishReason: r.FinishReason,
		PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens}
	if resp != nil {
		e.Answer, e.Reasoning, e.Timings = resp.Text, resp.Reasoning, resp.Timings
	}
	br.addTranscript(e)
	return r
}

// sumTasks counts the items and adds up their figures.
func (br *benchRun) sumTasks(tr *TasksResult) {
	tr.Answered, tr.Failed, tr.TimedOut, tr.NotSent, tr.CompletionTokens = 0, 0, 0, 0, 0
	var dec, ttft []float64
	for _, it := range tr.Items {
		switch it.State {
		case TaskAnswered:
			tr.Answered++
			if it.DecodeTPS > 0 {
				dec = append(dec, it.DecodeTPS)
			}
			if it.TTFTMS != nil {
				ttft = append(ttft, *it.TTFTMS)
			}
		case TaskTimedOut:
			tr.Failed++
			tr.TimedOut++
		case TaskFailed:
			tr.Failed++
		case TaskNotSent:
			tr.NotSent++
		}
		tr.CompletionTokens += it.CompletionTokens
		if it.ReasoningChars > 0 {
			tr.Thinking = ThinkingSeen
		}
	}
	tr.DecodeTPS, tr.TTFT = Summarize(dec), Summarize(ttft)
}
