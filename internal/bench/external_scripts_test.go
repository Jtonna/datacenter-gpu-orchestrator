package bench

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The converters in scripts/ turn the driver scripts' summaries into a body
// that ParseExternalJSON and Validate accept. Skipped without Python 3.
func runConverter(t *testing.T, script string, args ...string) *ExternalSet {
	t.Helper()
	py := ""
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			if out, err := exec.Command(p, "-c", "import sys; print(sys.version_info[0])").Output(); err == nil && string(out[:1]) == "3" {
				py = p
				break
			}
		}
	}
	if py == "" {
		t.Skip("no Python 3")
	}
	cmd := exec.Command(py, append([]string{filepath.Join("..", "..", "scripts", script)}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("%s: %v %s", script, err, ee.Stderr)
		}
		t.Fatal(err)
	}
	x, err := ParseExternalJSON(out)
	if err != nil {
		t.Fatalf("%s output does not parse: %v", script, err)
	}
	if err := x.Validate(); err != nil {
		t.Fatalf("%s output is not valid: %v", script, err)
	}
	return x
}

func TestCodingConverter(t *testing.T) {
	td := filepath.Join("testdata", "external")
	x := runConverter(t, "bench-external-coding.py", filepath.Join(td, "coding-summary.json"), "--note", "synthetic", "--attach", filepath.Join(td, "context-ladder.json"))
	if x.ID != "coding-quality" || x.Note != "synthetic" || len(x.Tables) != 3 || len(x.Attachments) != 1 || x.Attachments[0].Name != "context-ladder.json" {
		t.Fatalf("%+v", x)
	}
	totals, tasks, rubric := x.Tables[0], x.Tables[1], x.Tables[2]
	if len(totals.Rows) != 2 || totals.Rows[0]["strict_pct"] != 50.0 || totals.Rows[1]["profile"] != "prof-b" {
		t.Fatalf("totals %+v", totals.Rows)
	}
	if len(tasks.Rows) != 3 || tasks.Rows[2]["end"] != "length" || tasks.Rows[2]["reason"] != "no code block" || tasks.Rows[0]["task"] != "t1" {
		t.Fatalf("tasks %+v", tasks.Rows)
	}
	if rubric.Title != "Rubric scores, t2" || len(rubric.Rows) != 2 || rubric.Columns[len(rubric.Columns)-1].Key != "note" || rubric.Rows[0]["refactor_quality"] != 2.0 {
		t.Fatalf("rubric %+v", rubric)
	}
}

func TestContextConverter(t *testing.T) {
	td := filepath.Join("testdata", "external")
	x := runConverter(t, "bench-external-context.py", filepath.Join(td, "context-summary.json"), "--ladder", filepath.Join(td, "context-ladder.json"))
	if x.ID != "context-scaling" || len(x.Tables) != 2 {
		t.Fatalf("%+v", x)
	}
	steps, mem := x.Tables[0], x.Tables[1]
	if len(steps.Rows) != 4 {
		t.Fatalf("steps %+v", steps.Rows)
	}
	a8, b8 := steps.Rows[1], steps.Rows[3]
	if a8["profile"] != "ctx-a-8192" || a8["recall"] != 4.0 || a8["lowest_mib"] != 8100.0 || a8["lowest_node"] != "node-b" || a8["need_mib"] != 5000.0 {
		t.Fatalf("a 8192 %+v", a8)
	}
	if b8["verdict"] != "not sent" || b8["reason"] != "the estimate leaves too little memory per node" || b8["gen_tps"] != nil {
		t.Fatalf("b 8192 %+v", b8)
	}
	if len(mem.Columns) != 4 || mem.Columns[2].Label != "node-a" || len(mem.Rows) != 3 || mem.Rows[2]["node_0"] != 7000.0 {
		t.Fatalf("mem %+v", mem)
	}
	// without the ladder: the summary's profile, no steps that were never sent
	x = runConverter(t, "bench-external-context.py", filepath.Join(td, "context-summary.json"), "--id", "ctx-2")
	if x.ID != "ctx-2" || len(x.Tables[0].Rows) != 3 || x.Tables[0].Rows[1]["profile"] != "ctx-a-2048" {
		t.Fatalf("%+v", x.Tables[0].Rows)
	}
}
