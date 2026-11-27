package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// a small module: a package that passes, one with a failing test and a
// failing subtest among many passing ones (with a long output after the
// failure, which tail -30 would have kept instead of the name), one that
// panics and one that does not build.
var sampleModule = map[string]string{
	"go.mod":         "module example.test/sample\n\ngo 1.21\n",
	"good/good.go":   "package good\n\nfunc Two() int { return 2 }\n",
	"good/a_test.go": "package good\n\nimport \"testing\"\n\nfunc TestTwo(t *testing.T) {\n\tif Two() != 2 {\n\t\tt.Fatal(Two())\n\t}\n}\n",
	"bad/bad.go":     "package bad\n",
	"bad/a_test.go": `package bad

import (
	"fmt"
	"testing"
)

func TestEarlyFailure(t *testing.T) { t.Error("the reason it failed") }

func TestTable(t *testing.T) {
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprint("case", i), func(t *testing.T) {
			if i == 3 {
				t.Error("case 3 is wrong")
			}
		})
	}
}

func TestLongOutputAfter(t *testing.T) {
	for i := 0; i < 200; i++ {
		t.Log("noise line", i)
	}
}
`,
	"boom/boom.go":   "package boom\n",
	"boom/a_test.go": "package boom\n\nimport \"testing\"\n\nfunc TestBoom(t *testing.T) {\n\tvar m map[string]int\n\tm[\"x\"] = 1\n}\n",
	"broken/b.go":    "package broken\n\nfunc F() int { return \"not an int\" }\n",
}

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestTestsNamesEveryFailureAndKeepsTheWholeOutput(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	mod := writeModule(t, sampleModule)
	out := filepath.Join(t.TempDir(), "out")
	code, stdout, stderr := runCLI("tests", "--dir", mod, "--out", out, "--env", "GOFLAGS=-buildvcs=false",
		"--stage", "good=-count=1 ./good/...", "--stage", "all=-count=1 ./...")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, stdout, stderr)
	}
	var sum testsSummary
	b, err := os.ReadFile(filepath.Join(out, "tests.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatal(err)
	}
	if len(sum.Stages) != 2 || !sum.Stages[0].Passed || sum.Stages[1].Passed {
		t.Fatalf("stages %+v", sum.Stages)
	}
	all := sum.Stages[1]
	var failed []string
	for _, f := range all.FailedTests {
		failed = append(failed, strings.TrimPrefix(f.Package, "example.test/sample/")+" "+f.Test)
		if f.LogLine < 1 {
			t.Errorf("%s %s: no log line", f.Package, f.Test)
		}
	}
	got := strings.Join(failed, ",")
	for _, w := range []string{"bad TestEarlyFailure", "bad TestTable/case3", "bad TestTable", "boom TestBoom"} {
		if !strings.Contains(","+got+",", ","+w+",") {
			t.Errorf("failing test %q not named: %s", w, got)
		}
	}
	if strings.Contains(got, "TestLongOutputAfter") || strings.Contains(got, "TestTwo") {
		t.Errorf("a passing test named as failing: %s", got)
	}
	if len(all.BuildFailed) != 1 || !strings.HasSuffix(all.BuildFailed[0].Package, "broken") || !strings.Contains(all.BuildFailed[0].Output, "not an int") {
		t.Errorf("build failures %+v", all.BuildFailed)
	}
	if len(all.Panics) != 1 || !strings.Contains(all.Panics[0], "TestBoom") {
		t.Errorf("panics %v", all.Panics)
	}
	if strings.Join(all.OKPackages, ",") != "example.test/sample/good" {
		t.Errorf("ok packages %v", all.OKPackages)
	}
	// the whole output is kept: the failure, the noise after it and the build error
	log, _ := os.ReadFile(all.Log)
	for _, w := range []string{"the reason it failed", "case 3 is wrong", "noise line 199", "not an int", "assignment to entry in nil map"} {
		if !strings.Contains(string(log), w) {
			t.Errorf("the stage log lacks %q", w)
		}
	}
	// the line names where the failing test's output starts
	lines := strings.Split(string(log), "\n")
	for _, f := range all.FailedTests {
		if f.Test == "TestEarlyFailure" && (f.LogLine > len(lines) || !strings.Contains(lines[f.LogLine-1], "TestEarlyFailure")) {
			t.Errorf("log line %d of TestEarlyFailure: %q", f.LogLine, lines[f.LogLine-1])
		}
	}
	if _, err := os.Stat(all.JSONL); err != nil {
		t.Error(err)
	}
	for _, w := range []string{"tests good: PASS", "tests all: FAIL", "FAIL example.test/sample/bad TestTable/case3", "BUILD FAILED", "PANIC in example.test/sample/boom TestBoom"} {
		if !strings.Contains(stdout, w) {
			t.Errorf("stdout lacks %q:\n%s", w, stdout)
		}
	}
}

func TestTestsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"tests"},
		{"tests", "--stage", "noargs="},
		{"tests", "--stage", "bad name=./..."},
		{"tests", "--stage", "a=./...", "--stage", "a=./..."},
	} {
		if code, _, _ := runCLI(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	code, out, _ := runCLI("tests", "--dry-run", "--dir", "m", "--out", "o", "--stage", "race=-race -count=1 ./...")
	if code != 0 || !strings.Contains(out, "test -json -race -count=1 ./...") {
		t.Fatalf("dry run: exit %d\n%s", code, out)
	}
	code, out, _ = runCLI("tests", "--help")
	if code != 0 || !strings.Contains(out, "names") || !strings.Contains(out, "-stage") {
		t.Fatalf("help: exit %d\n%s", code, out)
	}
}
