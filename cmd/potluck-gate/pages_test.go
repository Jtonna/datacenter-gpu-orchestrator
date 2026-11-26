package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The drawing harness (scripts/gate/pages.mjs) lives beside the scheduler's
// other node scripts; its rules are checked here so the Go gate runs them.
// No browser is needed: node only, and the test is skipped without node.
func nodeOrSkip(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the drawing harness's tests (scripts/gate) need Node.js 18 or later")
	}
	return node
}

var gateScripts = filepath.Join("..", "..", "scripts", "gate")

func TestPagesHarnessRules(t *testing.T) {
	node := nodeOrSkip(t)
	cmd := exec.Command(node, "--test", "pages-lib.test.mjs")
	cmd.Dir = gateScripts
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test scripts/gate: %v\n%s", err, out)
	}
	cmd = exec.Command(node, "pages.mjs", "--help")
	cmd.Dir = gateScripts
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "--allow-post") {
		t.Fatalf("pages.mjs --help: %v\n%s", err, out)
	}
}

// --dry-run against the real scheduler: the detail routes come from its
// answers (the first profile and model of the data), with no browser.
func TestPagesHarnessFindsRoutesFromTheAPI(t *testing.T) {
	node := nodeOrSkip(t)
	sched, _ := built(t)
	data, models, programs := realData(t, sched)
	copyTo := filepath.Join(t.TempDir(), "data")
	if err := copyDir(data, copyTo); err != nil {
		t.Fatal(err)
	}
	listen, err := freeAddr("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := parseSpec(sched)
	r, err := startScheduler(spec, runOpts{Listen: listen, Data: copyTo, Models: models, Programs: programs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.stop(10 * time.Second) })
	if err := r.waitReady(time.Minute); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "pages.mjs", "--base", r.base, "--dry-run")
	cmd.Dir = gateScripts
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"ROUTE #/overview", "ROUTE #/profiles/alpha  (shape profiles/*)", "ROUTE #/placement/alpha", "ROUTE #/models/alpha.gguf  (shape models/*)", "ROUTE #/models/deleted"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}
