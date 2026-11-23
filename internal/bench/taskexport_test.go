package bench

import (
	"strings"
	"testing"
)

// Task figures and scores reach the exports (markdown, phases CSV, one row
// per task) and the trends (answered tasks, decode speed, a scorer's points).
func TestTaskFiguresInExportsAndTrends(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := newTestMgr(t, api)
	tm.bench(`{"id":"code","profile":"gemma","phases":[{"type":"load"},
		{"type":"tasks","tasks":{"id":"kit","version":1,"tasks":[{"id":"fib","prompt":"Write fib."},{"id":"sql","prompt":"Write SQL."}]}},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","cooldown_s":0,"only_when_idle":false,"benchmarks":["code"]}`)
	var runs []*Run
	for k := 0; k < 2; k++ {
		r := tm.finished(tm.run("s").ID)
		res, err := tm.store.RunTasks(r.ID, TaskQuery{})
		if err != nil || len(res) != 2 {
			t.Fatalf("%v %d", err, len(res))
		}
		pts := float64(3 + k)
		if _, err := tm.store.PutScores(r.ID, "kit-tests", ScoresDoc{Scores: []ScoreInput{{Result: res[0].ID, Points: pts, MaxPoints: 5}, {Result: res[1].ID, Points: 1, MaxPoints: 5}}}, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := tm.store.PutScores(r.ID, "rubric", ScoresDoc{Scores: []ScoreInput{{Result: res[0].ID, Points: 2, MaxPoints: 2}}}, 10); err != nil {
			t.Fatal(err)
		}
		got, err := tm.store.GetRun(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, got)
	}
	r := runs[1]

	md := ExportMarkdown(r)
	for _, want := range []string{"2 of 2 tasks answered, 0 failed (0 timed out), 0 not sent", "kit-tests 5 of 10 points (2 of 2 scored)", "rubric 2 of 2 points (1 of 2 scored)",
		"Tasks (phase 2, set kit v1):", "| Task | State | Finish | Tokens out | First token ms | Decode tok/s | kit-tests | rubric |", "| fib | answered |", " 4 of 5 | 2 of 2 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}

	pcsv, err := ExportCSVPhases(r)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(pcsv), "\n")
	if !strings.HasSuffix(lines[0], ",tasks_total,tasks_answered,tasks_failed,tasks_timed_out,tasks_not_sent,task_decode_tps_median,task_scores") {
		t.Fatalf("head %s", lines[0])
	}
	if !strings.Contains(lines[2], ",2,2,0,0,0,") || !strings.HasSuffix(lines[2], "kit-tests:5/10;rubric:2/2") {
		t.Fatalf("tasks phase row %s", lines[2])
	}
	if !strings.HasSuffix(lines[1], ",,,,,,,") {
		t.Fatalf("a load phase has empty task columns: %s", lines[1])
	}

	tcsv, err := ExportCSVTasks(r)
	if err != nil {
		t.Fatal(err)
	}
	tl := strings.Split(strings.TrimSpace(tcsv), "\n")
	if len(tl) != 3 || !strings.HasSuffix(tl[0], ",points_kit-tests,max_points_kit-tests,points_rubric,max_points_rubric") ||
		!strings.HasSuffix(tl[1], ",4,5,2,2") || !strings.HasSuffix(tl[2], ",1,5,,") || !strings.Contains(tl[1], ",fib,answered,") {
		t.Fatalf("tasks csv:\n%s", tcsv)
	}

	for metric, want := range map[string]string{"tasks_answered": "2 2", "task_points": "4 5", "task_points_pct": "40 50"} {
		pts, err := tm.store.Trend("code", metric, 10)
		if err != nil {
			t.Fatal(err)
		}
		var vals []string
		for _, p := range pts {
			vals = append(vals, fmtNum(p.Value))
		}
		if strings.Join(vals, " ") != want {
			t.Errorf("%s: %v, want %s", metric, vals, want)
		}
	}
	pts, err := tm.store.TrendOf("code", "task_points", "rubric", 10)
	if err != nil || len(pts) != 2 || pts[1].Value != 2 {
		t.Fatalf("rubric: %v %+v", err, pts)
	}
	if pts, _ := tm.store.Trend("code", "task_decode_tps", 10); len(pts) != 2 {
		t.Fatalf("decode: %+v", pts)
	}
}
