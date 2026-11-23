package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// ---- the chat client keeps what a scorer needs ----

func TestChatKeepsFinishReasonUsageReasoningAndPartialAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		prompt := body.Messages[len(body.Messages)-1].Content
		if !body.Stream {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"plain","reasoning_content":"hmm"},"finish_reason":"length"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"let me think\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"def f():\"}}]}\n\n")
		fl.Flush()
		if prompt == "slow" {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" pass\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":9},\"timings\":{\"prompt_n\":21,\"predicted_n\":9,\"predicted_per_second\":12.5}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	api := &HTTPAPI{Base: srv.URL}
	on := true
	cr, err := api.Chat(context.Background(), "r", BuildChatRequest("m", benchspec.Request{Prompt: "fast", Stream: &on}), nil)
	if err != nil || cr.Text != "def f(): pass" || cr.Reasoning != "let me think" || cr.FinishReason != "length" || cr.PromptTokens != 21 || cr.CompletionTokens != 9 {
		t.Fatalf("streamed %+v %v", cr, err)
	}
	cr, err = api.Chat(context.Background(), "r", BuildChatRequest("m", benchspec.Request{Prompt: "fast"}), nil)
	if err != nil || cr.Text != "plain" || cr.Reasoning != "hmm" || cr.FinishReason != "length" || cr.PromptTokens != 11 || cr.CompletionTokens != 7 {
		t.Fatalf("plain %+v %v", cr, err)
	}
	// Cut off by its deadline: the error, and what had arrived so far.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cr, err = api.Chat(ctx, "r", BuildChatRequest("m", benchspec.Request{Prompt: "slow", Stream: &on}), nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want a deadline error, got %v", err)
	}
	if cr == nil || cr.Text != "def f():" || cr.Reasoning != "let me think" || cr.TTFTMS == nil {
		t.Fatalf("partial %+v", cr)
	}
}

// ---- the tasks phase ----

const codingSet = `{"id":"coding","version":1,"title":"Coding","system":"You are an expert Python programmer.","max_tokens":512,"seed":1,
  "tasks":[{"id":"t1","prompt":"TASK-ONE write parse_duration"},{"id":"t2","prompt":"TASK-TWO fix the bug"},{"id":"t3","prompt":"TASK-THREE long one"}]}`

func saveSet(t *testing.T, s *Store, js string) *benchspec.TaskSet {
	t.Helper()
	ts, err := benchspec.ParseTaskSetJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTaskSet(ts, 1); err != nil {
		t.Fatal(err)
	}
	return ts
}

func taskReplies(m map[string]fakeReply) func(string) fakeReply {
	return func(body string) fakeReply {
		for k, r := range m {
			if strings.Contains(body, k) {
				return r
			}
		}
		return fakeReply{text: "default answer"}
	}
}

func tasksPhase(st Step) *PhaseResult {
	for i := range st.Phases {
		if st.Phases[i].Type == benchspec.PhaseTasks {
			return &st.Phases[i]
		}
	}
	return nil
}

func TestTasksPhaseSendsEachTaskOnceAndKeepsEveryAnswer(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatFn: taskReplies(map[string]fakeReply{
		"TASK-ONE":   {text: "```python\ndef parse_duration(s): ...\n```", reasoning: "thinking about it", finish: "stop"},
		"TASK-TWO":   {code: 502, text: "child exited"},
		"TASK-THREE": {text: "half an answer", finish: "length"},
	})})
	tm := newTestMgr(t, api)
	set := saveSet(t, tm.store, codingSet)
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"tasks","task_set":"coding"},{"type":"unload"}]}`)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
	st := r.Steps[0]
	if st.Outcome != OutcomeReady || phaseTypesOf(st) != "load:done,tasks:done,unload:done" {
		t.Fatalf("a failed task does not fail the step: %s %s %s", st.Outcome, st.Error, phaseTypesOf(st))
	}
	// Each task once, in order, with the fixed sampling and nothing about thinking.
	api.mu.Lock()
	bodies := append([]string(nil), api.bodies...)
	api.mu.Unlock()
	if len(bodies) != 3 || !strings.Contains(bodies[0], "TASK-ONE") || !strings.Contains(bodies[1], "TASK-TWO") || !strings.Contains(bodies[2], "TASK-THREE") {
		t.Fatalf("bodies %v", bodies)
	}
	for _, b := range bodies {
		for _, want := range []string{`"temperature":0`, `"seed":1`, `"stream":true`, `"max_tokens":512`, `"cache_prompt":false`, `{"role":"system","content":"You are an expert Python programmer."}`} {
			if !strings.Contains(b, want) {
				t.Errorf("%s lacks %s", b, want)
			}
		}
		for _, no := range []string{"chat_template_kwargs", "enable_thinking", "reasoning"} {
			if strings.Contains(b, no) {
				t.Errorf("thinking follows the profile, but the request sets %s: %s", no, b)
			}
		}
	}
	p := tasksPhase(st)
	tr := p.Tasks
	if tr == nil || tr.SetID != "coding" || tr.SetVersion != 1 || tr.SetSHA256 != set.SHA256() || tr.Total != 3 || tr.Sent != 3 || tr.Answered != 2 || tr.Failed != 1 || tr.NotSent != 0 {
		t.Fatalf("tasks %+v", tr)
	}
	// The first answer after a load is slow on the fake (30 / 5): the median is of the answered two.
	if tr.Thinking != ThinkingSeen || tr.DecodeTPS == nil || tr.DecodeTPS.N != 2 || tr.DecodeTPS.Median != 18 {
		t.Fatalf("thinking %q decode %+v", tr.Thinking, tr.DecodeTPS)
	}
	one, two, three := tr.Items[0], tr.Items[1], tr.Items[2]
	if one.TaskID != "t1" || one.State != TaskAnswered || one.FinishReason != "stop" || one.CompletionTokens != 8 || one.PromptTokens == 0 || one.TTFTMS == nil ||
		one.DecodeTPS != 6 || one.WallMS <= 0 || one.ReasoningChars == 0 || one.AnswerChars == 0 || one.ResultID == 0 {
		t.Fatalf("t1 %+v", one)
	}
	if two.State != TaskFailed || two.Status != 502 || two.Error == "" {
		t.Fatalf("t2 %+v", two)
	}
	if three.State != TaskAnswered || three.FinishReason != "length" {
		t.Fatalf("t3 %+v", three)
	}
	// The answers live outside the run row, in full.
	if b, _ := json.Marshal(r); strings.Contains(string(b), "def parse_duration") {
		t.Fatal("the run JSON carries an answer text")
	}
	full, err := tm.store.TaskResult(r.ID, one.ResultID)
	if err != nil || full.Answer != "```python\ndef parse_duration(s): ...\n```" || full.Reasoning != "thinking about it" || full.SetSHA256 != set.SHA256() || full.StepIdx != 0 || full.BenchmarkID != "b" || full.Profile != "gemma" {
		t.Fatalf("stored answer %+v %v", full, err)
	}
	list, err := tm.store.RunTasks(r.ID, TaskQuery{})
	if err != nil || len(list) != 3 || list[0].Answer != "" {
		t.Fatalf("listing without answers %+v %v", list, err)
	}
	list, _ = tm.store.RunTasks(r.ID, TaskQuery{Answers: true})
	if list[2].Answer != "half an answer" {
		t.Fatalf("listing with answers %+v", list[2])
	}
	if _, err := tm.store.TaskResult("other-run", one.ResultID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a result of another run: %v", err)
	}
}

func TestTasksPhaseTimeouts(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatFn: taskReplies(map[string]fakeReply{
		"SLOW-TASK": {text: "never", delay: 3 * time.Second},
		"STEP-":     {text: "ok", delay: 700 * time.Millisecond},
	})})
	tm := newTestMgr(t, api)
	// A task with its own timeout is cut and the next one is sent.
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"tasks","tasks":{"id":"inline","version":1,"tasks":[
	  {"id":"slow","prompt":"SLOW-TASK","timeout_s":1},{"id":"next","prompt":"after the slow one"}]}},{"type":"unload"}]}`)
	st := r.Steps[0]
	tr := tasksPhase(st).Tasks
	if st.Outcome != OutcomeReady || tr == nil || tr.Items[0].State != TaskTimedOut || tr.Items[1].State != TaskAnswered || tr.TimedOut != 1 || tr.Failed != 1 || !tr.Inline {
		t.Fatalf("%s %+v", st.Outcome, tr)
	}
	if !strings.Contains(tr.Items[0].Error, "timed out after 1s") {
		t.Fatalf("error %q", tr.Items[0].Error)
	}

	// The phase's own timeout: what is not sent by then is recorded as not sent,
	// and the benchmark goes on to its unload.
	api2 := newFake().prof("gemma", fakeProf{chatFn: taskReplies(map[string]fakeReply{"STEP-": {text: "ok", delay: 700 * time.Millisecond}})})
	tm2 := newTestMgr(t, api2)
	r = tm2.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"tasks","timeout_s":1,"tasks":{"id":"inline","version":1,"tasks":[
	  {"id":"a","prompt":"STEP-A"},{"id":"b","prompt":"STEP-B"},{"id":"c","prompt":"STEP-C"}]}},{"type":"unload"}]}`)
	st = r.Steps[0]
	tr = tasksPhase(st).Tasks
	if st.Outcome != OutcomeReady || phaseTypesOf(st) != "tasks:done,unload:done" {
		t.Fatalf("%s %s %s", st.Outcome, st.Error, phaseTypesOf(st))
	}
	if !tr.PhaseTimedOut || tr.Items[0].State != TaskAnswered || tr.Items[1].State != TaskTimedOut || tr.Items[2].State != TaskNotSent || tr.NotSent != 1 || tr.Sent != 2 {
		t.Fatalf("%+v", tr)
	}
	if api2.has("gemma") {
		t.Fatal("left loaded")
	}
}

func TestTasksPhaseWithAMissingSetFailsTheStep(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"tasks","task_set":"nope"},{"type":"unload"}]}`)
	st := r.Steps[0]
	if st.Outcome != OutcomeError || !strings.Contains(st.Error, `task set "nope"`) {
		t.Fatalf("%s %s", st.Outcome, st.Error)
	}
}

func TestTasksPhaseCancelRecordsTheRestAsNotSent(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{chatFn: taskReplies(map[string]fakeReply{"STEP-": {text: "ok", delay: 150 * time.Millisecond}})})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"b","profile":"gemma","phases":[{"type":"tasks","tasks":{"id":"inline","version":1,"tasks":[
	  {"id":"a","prompt":"STEP-A"},{"id":"b","prompt":"STEP-B"},{"id":"c","prompt":"STEP-C"},{"id":"d","prompt":"STEP-D"}]}},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["b"],"cooldown_s":0,"only_when_idle":false}`)
	run := tm.run("s")
	deadline := time.Now().Add(5 * time.Second)
	for {
		api.mu.Lock()
		n := len(api.bodies)
		api.mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := tm.m.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	r := tm.finished(run.ID)
	st := r.Steps[0]
	tr := tasksPhase(st).Tasks
	if st.Outcome != OutcomeCancelled || tr == nil || tr.NotSent < 2 || tr.Items[len(tr.Items)-1].State != TaskNotSent || !strings.Contains(tr.Items[len(tr.Items)-1].Error, "cancelled") {
		t.Fatalf("%s %+v", st.Outcome, tr)
	}
}

// ---- task sets ----

func TestTaskSetsAreImmutablePerVersion(t *testing.T) {
	s, _ := openTemp(t)
	saveSet(t, s, codingSet)
	again, _ := benchspec.ParseTaskSetJSON([]byte(codingSet))
	again.Validate()
	created, err := s.SaveTaskSet(again, 2)
	if err != nil || created {
		t.Fatalf("the same content again is accepted and changes nothing: %v %v", created, err)
	}
	changed, _ := benchspec.ParseTaskSetJSON([]byte(strings.Replace(codingSet, "fix the bug", "fix two bugs", 1)))
	changed.Validate()
	if _, err := s.SaveTaskSet(changed, 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("a new text under the same version: %v", err)
	}
	changed.Version = 2
	if created, err := s.SaveTaskSet(changed, 3); err != nil || !created {
		t.Fatalf("version 2: %v %v", created, err)
	}
	newest, err := s.GetTaskSet("coding", 0)
	if err != nil || newest.Set.Version != 2 || newest.SHA256 != changed.SHA256() || newest.CreatedAt != 3 {
		t.Fatalf("newest %+v %v", newest, err)
	}
	v1, err := s.GetTaskSet("coding", 1)
	if err != nil || v1.Set.Version != 1 || !strings.Contains(v1.Set.Tasks[1].Prompt, "fix the bug") {
		t.Fatalf("v1 %+v %v", v1, err)
	}
	all, err := s.ListTaskSets()
	if err != nil || len(all) != 2 || all[0].Tasks != 3 || all[0].Title != "Coding" {
		t.Fatalf("list %+v %v", all, err)
	}
	if _, err := s.GetTaskSet("coding", 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := s.DeleteTaskSet("coding", 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTaskSet("coding", 0); got.Set.Version != 1 {
		t.Fatalf("after the delete the newest is version 1: %+v", got)
	}
}

func TestATaskSetWithResultsCannotBeDeleted(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	saveSet(t, tm.store, codingSet)
	tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"tasks","task_set":"coding"}]}`)
	if err := tm.store.DeleteTaskSet("coding", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete of a set with stored answers: %v", err)
	}
}

// ---- scores ----

func scoredRun(t *testing.T) (*testMgr, *Run) {
	t.Helper()
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	saveSet(t, tm.store, codingSet)
	r := tm.runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"tasks","task_set":"coding"},{"type":"unload"}]}`)
	if r.State != RunCompleted {
		t.Fatalf("%s %s", r.State, r.Error)
	}
	return tm, r
}

func ip(v int) *int { return &v }

func TestScoresAreIdempotentPerScorer(t *testing.T) {
	tm, r := scoredRun(t)
	s := tm.store
	doc := ScoresDoc{ScorerVersion: "score.py 1", Scores: []ScoreInput{
		{Step: ip(0), Task: "t1", Points: 17, MaxPoints: 17},
		{Step: ip(0), Phase: ip(1), Task: "t2", Points: 5, MaxPoints: 17, FailedTests: []string{"test_edge"}, Notes: "strict rule"},
	}}
	tot, err := s.PutScores(r.ID, "kit-tests", doc, 10)
	if err != nil || tot.Points != 22 || tot.MaxPoints != 34 || tot.Scored != 2 || tot.Tasks != 3 || tot.ScorerVersion != "score.py 1" {
		t.Fatalf("%+v %v", tot, err)
	}
	// The same post again changes nothing; a new post replaces the scorer's scores.
	if tot, err = s.PutScores(r.ID, "kit-tests", doc, 11); err != nil || tot.Points != 22 || tot.Scored != 2 {
		t.Fatalf("again %+v %v", tot, err)
	}
	doc.Scores = doc.Scores[:1]
	doc.ScorerVersion = "score.py 2"
	if tot, err = s.PutScores(r.ID, "kit-tests", doc, 12); err != nil || tot.Points != 17 || tot.Scored != 1 || tot.ScorerVersion != "score.py 2" {
		t.Fatalf("replaced %+v %v", tot, err)
	}
	// A second scorer sits beside the first.
	res, _ := s.RunTasks(r.ID, TaskQuery{})
	if _, err := s.PutScores(r.ID, "t5-rubric", ScoresDoc{Scores: []ScoreInput{{Result: res[2].ID, Points: 3, MaxPoints: 5}}}, 13); err != nil {
		t.Fatal(err)
	}
	totals, err := s.ScoreTotals(r.ID)
	if err != nil || len(totals) != 2 || totals[0].Scorer != "kit-tests" || totals[1].Scorer != "t5-rubric" || totals[1].Points != 3 {
		t.Fatalf("totals %+v %v", totals, err)
	}
	// The run shows them on its tasks phase, per task and per scorer.
	got, _ := s.GetRun(r.ID)
	tr := tasksPhase(got.Steps[0]).Tasks
	if len(tr.Scores) != 2 || tr.Items[0].Scores["kit-tests"] == nil || tr.Items[0].Scores["kit-tests"].Points != 17 || tr.Items[2].Scores["t5-rubric"].MaxPoints != 5 || tr.Items[1].Scores != nil {
		t.Fatalf("on the run %+v", tr)
	}
	list, _ := s.RunTasks(r.ID, TaskQuery{})
	if list[0].Scores["kit-tests"] == nil {
		t.Fatalf("listing %+v", list[0])
	}
	if err := s.DeleteScores(r.ID, "t5-rubric"); err != nil {
		t.Fatal(err)
	}
	if totals, _ = s.ScoreTotals(r.ID); len(totals) != 1 {
		t.Fatalf("%+v", totals)
	}
	if err := s.DeleteScores(r.ID, "t5-rubric"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestScoresAreChecked(t *testing.T) {
	tm, r := scoredRun(t)
	s := tm.store
	for name, c := range map[string]struct {
		scorer string
		in     []ScoreInput
		want   string
	}{
		"bad scorer":      {"Kit Tests!", []ScoreInput{{Step: ip(0), Task: "t1", Points: 1, MaxPoints: 1}}, "scorer"},
		"no scores":       {"k", nil, "scores"},
		"unknown task":    {"k", []ScoreInput{{Step: ip(0), Task: "t9", Points: 1, MaxPoints: 1}}, `task "t9"`},
		"unknown step":    {"k", []ScoreInput{{Step: ip(5), Task: "t1", Points: 1, MaxPoints: 1}}, "step 5"},
		"unknown result":  {"k", []ScoreInput{{Result: 999999, Points: 1, MaxPoints: 1}}, "result 999999"},
		"no target":       {"k", []ScoreInput{{Points: 1, MaxPoints: 1}}, "result, or step and task"},
		"too many points": {"k", []ScoreInput{{Step: ip(0), Task: "t1", Points: 18, MaxPoints: 17}}, "points"},
		"no max":          {"k", []ScoreInput{{Step: ip(0), Task: "t1", Points: 0, MaxPoints: 0}}, "max_points"},
		"negative":        {"k", []ScoreInput{{Step: ip(0), Task: "t1", Points: -1, MaxPoints: 3}}, "points"},
		"twice":           {"k", []ScoreInput{{Step: ip(0), Task: "t1", Points: 1, MaxPoints: 1}, {Step: ip(0), Task: "t1", Points: 1, MaxPoints: 1}}, "twice"},
	} {
		_, err := s.PutScores(r.ID, c.scorer, ScoresDoc{Scores: c.in}, 1)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	if _, err := s.PutScores("no-such-run", "k", ScoresDoc{Scores: []ScoreInput{{Step: ip(0), Task: "t1", Points: 1, MaxPoints: 1}}}, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if totals, _ := s.ScoreTotals(r.ID); len(totals) != 0 {
		t.Fatalf("a refused post stored something: %+v", totals)
	}
}

// A database written before the tasks phase opens, gains its tables, and its
// runs read as before.
func TestOldDatabaseGainsTheTaskTables(t *testing.T) {
	dir := t.TempDir()
	old, err := openStore(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	// The columns of that schema only (CreateRun writes the newer ones).
	if _, err := old.db.Exec(`INSERT INTO runs (id, suite_id, suite_name, state, triggered_by, queued_at, suite) VALUES ('old-1','s','S','completed','manual',1,'{"id":"s"}')`); err != nil {
		t.Fatal(err)
	}
	st := &Step{RunID: "old-1", BenchmarkID: "b", State: StepDone, Outcome: OutcomeReady}
	if err := old.AddStep(st); err != nil {
		t.Fatal(err)
	}
	if err := old.AddPhase(&PhaseResult{StepID: st.ID, RunID: "old-1", Type: "requests", State: PhaseDone}); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetRun("old-1")
	if err != nil || len(got.Steps) != 1 || len(got.Steps[0].Phases) != 1 || got.Steps[0].Phases[0].Tasks != nil {
		t.Fatalf("%+v %v", got, err)
	}
	if list, err := s.RunTasks("old-1", TaskQuery{}); err != nil || len(list) != 0 {
		t.Fatalf("%v %v", list, err)
	}
	if totals, err := s.ScoreTotals("old-1"); err != nil || len(totals) != 0 {
		t.Fatalf("%v %v", totals, err)
	}
}
