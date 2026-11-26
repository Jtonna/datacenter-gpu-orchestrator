package api

import (
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixtureURL starts scripts/ui-fixture-server.mjs with args and returns its
// base URL once it answers (for ui-exec's "base": GETs and the dry-run
// POST /api/plan go to it, every other write is refused).
func fixtureURL(t *testing.T, args ...string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(here), "..", "..", "scripts", "ui-fixture-server.mjs")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(node, append([]string{script, "--port", strconv.Itoa(port), "--host", "127.0.0.1"}, args...)...)
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	for i := 0; i < 50; i++ {
		if resp, err := http.Get(base + "/api/status"); err == nil {
			resp.Body.Close()
			return base
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the fixture server did not start:\n%s", out)
	return ""
}

// The Placement preview (phase 5, no mockup): the profile picker (generated
// profiles hidden until asked for or searched), the zones you may choose
// (a named-only zone stays pickable), and the result card for each verdict
// of POST /api/plan: make room with victims, cannot be placed, a tight fit,
// a forced start with a warning on a chosen zone, wait in the queue; the
// raw answer folded behind "Show the full answer". It writes nothing.
func TestUINextPlacement(t *testing.T) {
	base := fixtureURL(t, "--catalog", "--named-only", "laptop-02")
	plan := func(body string) string { return `^POST /api/plan ` + body + `$` }
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": base, "cases": []any{
		map[string]any{"name": "placement: make room", "route": "#/placement/gemma-4-27b-long", "status": "fetch",
			"expect": []string{"Placement preview", "Ask the scheduler", "Would make room", "It would claim zones that hold other models", "gemma-4-27b", "128k context",
				"Usable on its zones", "ps5-03", "ps5-04", "ps5-05", "Host", "Helper", "To make room it would unload", "gemma-4-12b-vision", "is idle", "is serving requests", "Show the full answer", "Fits"},
			"has":    []string{"button.plc-opt[aria-selected=true][data-profile=gemma-4-27b-long]", "details.plc-raw", "table.tbl"},
			"absent": []string{"bench-gemma", "m-qwen3"}},
		map[string]any{"name": "placement: cannot be placed", "route": "#/placement/llama-3.3-70b", "status": "fetch",
			"expect": []string{"Cannot be placed", "A request would be refused at once.", "It would need", "The fleet can hold", "30.5 GB"}, "none": []string{"table.tbl"}},
		map[string]any{"name": "placement: a tight fit", "route": "#/placement/gemma-4-12b-fast", "status": "fetch",
			// the banner says "Tight fit." once, then the sentence without its own "tight fit:"
			"expect": []string{"Would start now", "Tight fit", "Tight", "ps5-05", "Tight fit. 1"}, "absent": []string{"Tight fit. Tight fit", "Tight fit: "}},
		map[string]any{"name": "placement: waits", "route": "#/placement/qwen3-32b", "status": "fetch", "expect": []string{"Would wait", "No zone can take it now."}},
		map[string]any{"name": "placement: on a chosen named-only zone", "route": "#/placement/qwen3-14b?zones=02:00:00:00:00:08", "status": "fetch",
			"expect": []string{"Only these", "Named only", "laptop-02", "1 zone chosen", "Result on 1 chosen zone", "It would start anyway.", "Short"},
			"has":    []string{"input[data-zone=laptop-02][checked]"}},
		map[string]any{"name": "placement: pick a profile", "route": "#/placement/qwen3-14b", "status": "fetch", "click": []string{"button.plc-opt[data-profile=gemma-4-12b]"},
			"hash": "#/placement/gemma-4-12b"},
		map[string]any{"name": "placement: test profiles, asked for", "route": "#/placement/qwen3-14b?gen=1", "status": "fetch", "expect": []string{"TEST - bench-gemma-4-12b-c16k-n1", "test", "more: search to narrow the list."}},
		map[string]any{"name": "placement: generated, searched", "route": "#/placement/qwen3-14b?q=m-glm", "status": "fetch", "expect": []string{"m-glm-4.5-air-c4k-n2"}, "absent": []string{"gemma-4-27b-long"}},
		map[string]any{"name": "placement: nothing picked", "route": "#/placement", "status": "fetch", "expect": []string{"Pick a profile"}},
	}})

	// what the page sends: the profile, and the chosen zones by id
	fetch := fixtureAnswers(t, []string{"--named-only", "laptop-02"}, "/api/profiles", "/api/status")
	fetch["POST /api/plan"] = map[string]any{"model": "qwen3-14b", "action": "start", "placement": []any{}, "victims": []any{}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "placement: the request", "route": "#/placement/qwen3-14b?zones=02:00:00:00:00:08", "status": fetch["/api/status"],
			"fetchedQuery": []string{plan(`\{"model":"qwen3-14b","nodes":\["02:00:00:00:00:08"\]\}`)}},
		map[string]any{"name": "placement: any zone", "route": "#/placement/qwen3-14b", "status": fetch["/api/status"],
			"fetchedQuery": []string{plan(`\{"model":"qwen3-14b"\}`)}},
	}})
}
