package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const testsUsage = `potluck-gate tests: run go test in stages and keep every word of it.

Each --stage NAME=ARGS runs "go test -json ARGS" in --dir (the module), one
stage after the other. For every stage it keeps the WHOLE output in
<out>/<NAME>.log (the test output as go test prints it, build errors
included) and <out>/<NAME>.jsonl (the -json events), and it names:
  - every package that passed and every one that failed
  - every failing test, as package and test name (subtests too), with the
    log line where its output starts
  - every package that did not build, with its build errors
  - the DATA RACE reports and the panics, with the test they belong to
The names come from the -json events, not from the end of the output, so a
failure in a long run is never lost.

  potluck-gate tests --dir . --out gate/tests \
      --stage "unit=-count=1 ./..." \
      --stage "race=-race -count=1 ./..." --env CGO_ENABLED=1

Output: one line per stage and a summary in <out>/tests.json. Exit 0 when
every stage passed, 1 when one failed, 2 when a stage could not run.

flags:
`

type testsOpts struct {
	dir, out, goBin string
	stages          []stage
	env             []string
	dryRun          bool
}

type stage struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

var stageName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func parseStage(s string) (stage, error) {
	name, args, ok := strings.Cut(s, "=")
	if !ok || !stageName.MatchString(name) {
		return stage{}, fmt.Errorf("%q: want NAME=ARGS, NAME of letters, digits, - _ .", s)
	}
	f := strings.Fields(args)
	if len(f) == 0 {
		return stage{}, fmt.Errorf("%q: no go test arguments", s)
	}
	return stage{Name: name, Args: f}, nil
}

func testsMain(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tests", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o testsOpts
	var stages, env multiFlag
	fs.StringVar(&o.dir, "dir", ".", "the module to test")
	fs.StringVar(&o.out, "out", "gate-tests", "the folder for each stage's whole output and tests.json")
	fs.StringVar(&o.goBin, "go", "go", "the go command")
	fs.Var(&stages, "stage", "a stage: NAME=ARGS for go test (repeatable, run in order)")
	fs.Var(&env, "env", "an extra environment variable K=V for every stage (repeatable)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the commands; run nothing")
	fs.Usage = func() { fmt.Fprint(fs.Output(), testsUsage); fs.PrintDefaults() }
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.Usage()
			return 0
		}
		return 2
	}
	if len(stages) == 0 {
		fmt.Fprintln(stderr, "give at least one --stage NAME=ARGS")
		return 2
	}
	seen := map[string]bool{}
	for _, s := range stages {
		st, err := parseStage(s)
		if err != nil {
			fmt.Fprintln(stderr, "--stage:", err)
			return 2
		}
		if seen[st.Name] {
			fmt.Fprintf(stderr, "--stage: %s given twice\n", st.Name)
			return 2
		}
		seen[st.Name] = true
		o.stages = append(o.stages, st)
	}
	o.env = env
	if o.dryRun {
		for _, s := range o.stages {
			fmt.Fprintf(stdout, "dry run: %s: (cd %s && %s test -json %s) > %s\n", s.Name, o.dir, o.goBin, strings.Join(s.Args, " "),
				filepath.Join(o.out, s.Name+".log"))
		}
		return 0
	}
	sum, err := runTests(o, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "tests could not run:", err)
		return 2
	}
	for _, s := range sum.Stages {
		if !s.Passed {
			return 1
		}
	}
	return 0
}

// stageResult is what a stage says.
type stageResult struct {
	stage
	Passed      bool          `json:"passed"`
	ExitCode    int           `json:"exit_code"`
	DurationS   float64       `json:"duration_s"`
	OKPackages  []string      `json:"ok_packages"`
	FailedPkgs  []string      `json:"failed_packages"`
	FailedTests []failedTest  `json:"failed_tests"`
	BuildFailed []buildFailed `json:"build_failed"`
	Races       []string      `json:"data_races"` // the test of each DATA RACE report ("package" when outside a test)
	Panics      []string      `json:"panics"`     // the test of each panic
	Log         string        `json:"log"`
	JSONL       string        `json:"jsonl"`
}

type failedTest struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	LogLine int    `json:"log_line"` // the first line of its output in the stage's .log
}

type buildFailed struct {
	Package string `json:"package"`
	Output  string `json:"output"`
}

type testsSummary struct {
	Dir    string         `json:"dir"`
	Stages []*stageResult `json:"stages"`
}

// event is one go test -json event (go doc test2json).
type event struct {
	Action     string
	Package    string
	Test       string
	Output     string
	ImportPath string
}

func runTests(o testsOpts, stdout io.Writer) (*testsSummary, error) {
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return nil, err
	}
	sum := &testsSummary{Dir: o.dir}
	for _, s := range o.stages {
		r, err := runStage(o, s)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", s.Name, err)
		}
		sum.Stages = append(sum.Stages, r)
		fmt.Fprintln(stdout, r.line())
		for _, f := range r.FailedTests {
			fmt.Fprintf(stdout, "  FAIL %s %s (%s line %d)\n", f.Package, f.Test, filepath.Base(r.Log), f.LogLine)
		}
		for _, b := range r.BuildFailed {
			fmt.Fprintf(stdout, "  BUILD FAILED %s: %s\n", b.Package, truncate(strings.ReplaceAll(strings.TrimSpace(b.Output), "\n", " | "), 300))
		}
		for _, x := range r.Races {
			fmt.Fprintf(stdout, "  DATA RACE in %s\n", x)
		}
		for _, x := range r.Panics {
			fmt.Fprintf(stdout, "  PANIC in %s\n", x)
		}
	}
	if err := writeJSONFile(filepath.Join(o.out, "tests.json"), sum); err != nil {
		return nil, err
	}
	return sum, nil
}

func (r *stageResult) line() string {
	state := "PASS"
	if !r.Passed {
		state = "FAIL"
	}
	return fmt.Sprintf("tests %s: %s | ok packages %d | failed packages %d | failed tests %d | build failures %d | data races %d | panics %d | %.0fs | %s",
		r.Name, state, len(r.OKPackages), len(r.FailedPkgs), len(r.FailedTests), len(r.BuildFailed), len(r.Races), len(r.Panics), r.DurationS, r.Log)
}

func runStage(o testsOpts, s stage) (*stageResult, error) {
	r := &stageResult{stage: s, Log: filepath.Join(o.out, s.Name+".log"), JSONL: filepath.Join(o.out, s.Name+".jsonl")}
	logF, err := os.Create(r.Log)
	if err != nil {
		return nil, err
	}
	defer logF.Close()
	jsonF, err := os.Create(r.JSONL)
	if err != nil {
		return nil, err
	}
	defer jsonF.Close()
	args := append([]string{"test", "-json"}, s.Args...)
	cmd := exec.Command(o.goBin, args...)
	cmd.Dir = o.dir
	cmd.Env = append(os.Environ(), o.env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := newEventParser(logF)
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		jsonF.Write(line)
		jsonF.Write([]byte{'\n'})
		var ev event
		if json.Unmarshal(line, &ev) != nil {
			p.text(string(line) + "\n") // not an event: kept as it came
			continue
		}
		p.event(ev)
	}
	werr := cmd.Wait()
	r.DurationS = time.Since(t0).Seconds()
	if stderr.Len() > 0 { // go test's own messages (a bad flag, a package not found, some build errors)
		p.text("--- stderr of go test ---\n" + stderr.String())
		p.stderr(stderr.String())
	}
	if werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			return nil, werr
		}
		r.ExitCode = ee.ExitCode()
	}
	p.fill(r)
	r.Passed = r.ExitCode == 0 && len(r.FailedPkgs) == 0 && len(r.FailedTests) == 0 && len(r.BuildFailed) == 0 && len(r.Races) == 0
	return r, nil
}

// eventParser writes the plain log and names what failed.
type eventParser struct {
	w        io.Writer
	line     int
	firstOut map[string]int // package + test -> the first log line of its output
	failed   []failedTest
	pkgOK    map[string]bool
	pkgFail  map[string]bool
	build    map[string]*strings.Builder
	races    []string
	panics   []string
}

func newEventParser(w io.Writer) *eventParser {
	return &eventParser{w: w, firstOut: map[string]int{}, pkgOK: map[string]bool{}, pkgFail: map[string]bool{}, build: map[string]*strings.Builder{}}
}

func (p *eventParser) text(s string) {
	io.WriteString(p.w, s)
	p.line += strings.Count(s, "\n")
}

var (
	raceLine  = regexp.MustCompile(`^WARNING: DATA RACE`)
	panicText = regexp.MustCompile(`^panic: `)
)

func (p *eventParser) event(ev event) {
	key := ev.Package + " " + ev.Test
	switch ev.Action {
	case "build-output":
		b := p.build[ev.ImportPath]
		if b == nil {
			b = &strings.Builder{}
			p.build[ev.ImportPath] = b
		}
		b.WriteString(ev.Output)
		p.text(ev.Output)
	case "build-fail":
		if p.build[ev.ImportPath] == nil {
			p.build[ev.ImportPath] = &strings.Builder{}
		}
	case "output":
		if _, ok := p.firstOut[key]; !ok {
			p.firstOut[key] = p.line + 1
		}
		trimmed := strings.TrimSpace(ev.Output)
		where := ev.Package
		if ev.Test != "" {
			where += " " + ev.Test
		}
		if raceLine.MatchString(trimmed) {
			p.races = append(p.races, where)
		}
		if panicText.MatchString(trimmed) {
			p.panics = append(p.panics, where)
		}
		p.text(ev.Output)
	case "fail":
		if ev.Test != "" {
			p.failed = append(p.failed, failedTest{Package: ev.Package, Test: ev.Test, LogLine: p.firstOut[key]})
		} else {
			p.pkgFail[ev.Package] = true
		}
	case "pass":
		if ev.Test == "" {
			p.pkgOK[ev.Package] = true
		}
	}
}

// goBuildErr finds "# pkg" headers in go test's stderr (packages that did
// not build when go test reports them there, not as events).
var goBuildHeader = regexp.MustCompile(`(?m)^# (\S+)`)

func (p *eventParser) stderr(s string) {
	for _, m := range goBuildHeader.FindAllStringSubmatch(s, -1) {
		pkg := strings.TrimSuffix(strings.Fields(m[1])[0], "]")
		if p.build[pkg] == nil {
			b := &strings.Builder{}
			b.WriteString(s)
			p.build[pkg] = b
		}
	}
}

func (p *eventParser) fill(r *stageResult) {
	r.FailedTests = p.failed
	for k := range p.pkgOK {
		r.OKPackages = append(r.OKPackages, k)
	}
	for k := range p.pkgFail {
		r.FailedPkgs = append(r.FailedPkgs, k)
	}
	for k, b := range p.build {
		if strings.HasSuffix(k, ".test") { // the test binary of a package: named by its package
			k = strings.TrimSuffix(k, ".test")
		}
		r.BuildFailed = append(r.BuildFailed, buildFailed{Package: k, Output: b.String()})
	}
	sort.Strings(r.OKPackages)
	sort.Strings(r.FailedPkgs)
	sort.Slice(r.BuildFailed, func(i, j int) bool { return r.BuildFailed[i].Package < r.BuildFailed[j].Package })
	r.Races, r.Panics = p.races, p.panics
	if r.OKPackages == nil {
		r.OKPackages = []string{}
	}
	if r.FailedPkgs == nil {
		r.FailedPkgs = []string{}
	}
	if r.FailedTests == nil {
		r.FailedTests = []failedTest{}
	}
	if r.BuildFailed == nil {
		r.BuildFailed = []buildFailed{}
	}
}
