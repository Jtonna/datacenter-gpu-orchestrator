package bench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

func python3(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			if out, err := exec.Command(p, "-c", "import sys; print(sys.version_info[0])").Output(); err == nil && string(out[:1]) == "3" {
				return p
			}
		}
	}
	t.Skip("no Python 3")
	return ""
}

func runTasksScript(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(python3(t), append([]string{"-I", filepath.Join("..", "..", "scripts", "bench-tasks.py")}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("bench-tasks.py %v: %v %s", args, err, ee.Stderr)
		}
		t.Fatal(err)
	}
	return string(out)
}

// A kit folder becomes a task set Potluck accepts.
func TestTasksScriptBuildsATaskSetFromAKit(t *testing.T) {
	out := runTasksScript(t, "taskset", filepath.Join("testdata", "tasks", "kit"), "--id", "coding-kit", "--version", "3")
	ts, err := benchspec.ParseTaskSetJSON([]byte(out))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := ts.Validate(); err != nil {
		t.Fatal(err)
	}
	if ts.ID != "coding-kit" || ts.Version != 3 || len(ts.Tasks) != 2 || !strings.Contains(ts.System, "expert Python") {
		t.Fatalf("%+v", ts)
	}
	t1 := ts.Tasks[0]
	if t1.ID != "t1" || t1.MaxTokens != 2048 || t1.TimeoutS != 600 || !strings.HasPrefix(t1.Prompt, "TASK-ONE") || t1.Title == "" {
		t.Fatalf("t1 %+v", t1)
	}
}

// pull writes a run's answers in the kit's layout; scores turns the kit's
// score files into a scores post that Potluck stores; post sends a ready one.
func TestTasksScriptPullsAnswersAndPostsScores(t *testing.T) {
	python3(t)
	tm, r := scoredRun(t)
	s := tm.store
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		parts := strings.Split(strings.Trim(q.URL.Path, "/"), "/") // api bench runs {id} tasks|scores [scorer]
		if len(parts) < 5 || parts[3] != r.ID {
			http.NotFound(w, q)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case q.Method == "GET" && parts[4] == "tasks":
			list, err := s.RunTasks(r.ID, TaskQuery{Answers: q.URL.Query().Get("answers") == "1"})
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"tasks": list})
		case q.Method == "PUT" && parts[4] == "scores" && len(parts) == 6:
			var doc ScoresDoc
			if err := json.NewDecoder(q.Body).Decode(&doc); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			tot, err := s.PutScores(r.ID, parts[5], doc, 5)
			if err != nil {
				http.Error(w, err.Error(), 422)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"total": tot})
		default:
			http.NotFound(w, q)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	if out := runTasksScript(t, "pull", srv.URL, r.ID, "--out", dir); !strings.Contains(out, "wrote 3 answers") {
		t.Fatalf("pull: %s", out)
	}
	label := filepath.Join(dir, "gemma-step0")
	var rec map[string]any
	b, err := os.ReadFile(filepath.Join(label, "t2.json"))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &rec)
	pot := rec["potluck"].(map[string]any)
	if rec["answer"] == "" || rec["finish_reason"] != "stop" || pot["run_id"] != r.ID || pot["task_id"] != "t2" || pot["result_id"] == nil {
		t.Fatalf("t2.json %v", rec)
	}
	if md, _ := os.ReadFile(filepath.Join(label, "t2.answer.md")); string(md) != rec["answer"] {
		t.Fatalf("answer.md %q", md)
	}

	// The kit's scorer wrote score files for two of the three tasks.
	writeScore := func(task string, strict, lenient [2]int, failed []string) {
		doc := map[string]any{"task": task,
			"strict":  map[string]any{"tests_passed": strict[0], "tests_total": strict[1], "failed_tests": failed, "reason": nil, "notes": []string{}},
			"lenient": map[string]any{"tests_passed": lenient[0], "tests_total": lenient[1], "failed_tests": []string{}}}
		b, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(label, task+".score.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeScore("t1", [2]int{17, 17}, [2]int{17, 17}, nil)
	writeScore("t2", [2]int{8, 16}, [2]int{12, 16}, []string{"test_edge"})
	out := runTasksScript(t, "scores", dir, "--scorer", "kit-tests")
	var doc ScoresDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Scores) != 2 || doc.Scores[1].Points != 8 || doc.Scores[1].MaxPoints != 16 ||
		len(doc.Scores[1].FailedTests) != 1 || doc.ScorerVersion != "score.py strict" {
		t.Fatalf("scores body: %v %s", err, out)
	}
	if out := runTasksScript(t, "scores", dir, "--scorer", "kit-tests", "--rule", "lenient", "--max-points", "17", "--post", srv.URL); !strings.Contains(out, "29.75 of 34 points over 2 of 3 tasks") {
		t.Fatalf("scores --post: %s", out)
	}
	rubric := filepath.Join(dir, "rubric.json")
	os.WriteFile(rubric, []byte(`{"scorer_version":"rubric 1","scores":[{"step":0,"task":"t3","points":12,"max_points":17,"notes":"clear, one gap"}]}`), 0o644)
	if out := runTasksScript(t, "post", srv.URL, r.ID, "t5-rubric", rubric); !strings.Contains(out, "12 of 17 points over 1 of 3 tasks") {
		t.Fatalf("post: %s", out)
	}
	totals, err := s.ScoreTotals(r.ID)
	if err != nil || len(totals) != 2 || totals[0].Scorer != "kit-tests" || totals[0].Points != 29.75 || totals[1].Scorer != "t5-rubric" {
		t.Fatalf("%v %+v", err, totals)
	}
}
