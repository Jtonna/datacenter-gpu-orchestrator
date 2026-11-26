package api

import (
	"encoding/json"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// The benchmark report draws a tasks phase: the set, the counts and each
// scorer's total; and it still draws when the phase has no scores, or failed
// before it sent anything (the reason printed once). The per-task table is in
// the server's full report (Open full report).
func TestUI018TasksPhaseExecutes(t *testing.T) {
	e := newEnv(t, chatOK)
	var profiles map[string]any
	json.Unmarshal([]byte(body(t, e.get("/api/profiles"))), &profiles)
	ttft := 410.0
	scored := map[string]*bench.TaskScore{"kit-tests": {Scorer: "kit-tests", Points: 17, MaxPoints: 17}}
	half := map[string]*bench.TaskScore{"kit-tests": {Scorer: "kit-tests", Points: 8.5, MaxPoints: 17, FailedTests: []string{"test_edge"}, Notes: "it's half right"}}
	tasks := &bench.TasksResult{SetID: "coding", SetVersion: 1, SetTitle: "Coding tasks", SetSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Total: 4, Sent: 3, Answered: 2, Failed: 1, TimedOut: 1, NotSent: 1, PhaseTimedOut: true, Thinking: bench.ThinkingSeen,
		DecodeTPS: bench.Summarize([]float64{12.5, 14.1}), CompletionTokens: 900,
		Items: []bench.TaskItem{
			{ResultID: 11, TaskID: "t1", Title: "LRU cache", State: bench.TaskAnswered, Status: 200, FinishReason: "stop", PromptTokens: 210, CompletionTokens: 400, TTFTMS: &ttft, DecodeTPS: 12.5, WallMS: 32000, AnswerChars: 1800, ReasoningChars: 900, Scores: scored},
			{ResultID: 12, TaskID: "t2", Title: "Interval merge", State: bench.TaskAnswered, Status: 200, FinishReason: "length", PromptTokens: 190, CompletionTokens: 500, TTFTMS: &ttft, DecodeTPS: 14.1, WallMS: 36000, AnswerChars: 2100, Scores: half},
			{ResultID: 13, TaskID: "t3", State: bench.TaskTimedOut, WallMS: 60000, AnswerChars: 300, Error: "timed out: the phase's time limit ran out"},
			{ResultID: 14, TaskID: "t4", State: bench.TaskNotSent, Error: "not sent: the phase's time limit ran out"},
		},
		Scores: []bench.ScorerTotal{{Scorer: "kit-tests", ScorerVersion: "score.py strict", Points: 25.5, MaxPoints: 34, Scored: 2, Tasks: 4}},
	}
	unscored := *tasks
	unscored.Scores, unscored.Items = nil, []bench.TaskItem{{ResultID: 21, TaskID: "t1", State: bench.TaskAnswered, Status: 200, AnswerChars: 10}}
	step := func(idx int, tr *bench.TasksResult, outcome, errText string) bench.Step {
		return bench.Step{Idx: idx, BenchmarkID: "coding", Name: "Coding tasks", Profile: "alpha", State: bench.StepDone, Outcome: outcome, Error: errText,
			StartedAt: 1795400000000, FinishedAt: 1795400090000, Expect: &bench.ExpectResult{Pass: outcome == bench.OutcomeReady},
			Phases: []bench.PhaseResult{
				{Idx: 0, Type: "load", State: "done", Outcome: "ready", DurationS: 20},
				{Idx: 1, Type: "tasks", State: "done", Outcome: outcome, DurationS: 130, Tasks: tr, Error: errText},
			}}
	}
	missing := step(2, nil, bench.OutcomeError, `task set "nope" is not stored`)
	missing.Phases[1].State = "failed"
	run := bench.Run{ID: "run-t", SuiteID: "s", SuiteName: "Coding", State: bench.RunCompleted, Trigger: "manual", QueuedAt: 1795400000000,
		StartedAt: 1795400000000, FinishedAt: 1795400200000, Summary: &bench.RunSummary{Steps: 3, Passed: 2},
		Steps: []bench.Step{step(0, tasks, bench.OutcomeReady, ""), step(1, &unscored, bench.OutcomeReady, ""), missing}}
	fetch := map[string]any{"/api/bench/runs/run-t": run, "/api/bench/runs": map[string]any{"runs": []any{run}}, "/api/profiles": profiles}
	cases := []any{
		map[string]any{"name": "report of a tasks phase", "route": "#/benchmarks/runs/run-t/0",
			"page": []string{"Coding tasks v1", "2 of 4 answered", "1 timed out", "1 not sent", "kit-tests 25.5 / 34", "the phase's time limit ran out"}},
		map[string]any{"name": "report of a tasks phase with no scores", "route": "#/benchmarks/runs/run-t/1", "page": []string{"Coding tasks v1", "2 of 4 answered"}, "notOnPage": []string{"kit-tests"}},
		map[string]any{"name": "report of a step that failed before it sent anything", "route": "#/benchmarks/runs/run-t/2", "page": []string{"is not stored"}},
		map[string]any{"name": "the run's panel", "route": "#/benchmarks/runs/run-t", "page": []string{"Reports", "is not stored"}},
	}
	uiExec(t, map[string]any{"modules": []string{"views/benchmarks.js"}, "fetch": fetch, "cases": cases})
}
