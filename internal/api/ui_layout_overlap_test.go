package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// v88 was refused (26 Nov): on the Models list a program's Needs cell read
// "~5.3 GB, set by hand" with "Runs only on host-1, which is excluded" under
// it, wider than its column, painted over Profiles and Last used; a file with
// one written need ran into its neighbour ("set by hand1 profile"). The fake
// DOM of ui-exec has no layout, so this measures the pages in a real headless
// Chrome (scripts/ui-layout.mjs): in every table row, the drawn text of each
// cell against the drawn text of every other cell of the row. Two text
// rectangles of different cells that intersect, or that share a line with
// less than 4 px between them, fail (OVERLAP); so does a page wider than the
// window (SIDEWAYS), text past its row (PAST), and from 720 px on, text past
// the card the row is seen through (CUT). The fixture: --models written (three
// programs whose plans reject with the long exclusion reason and whose need is
// written, a file with one written need, the range rows) and --long-names
// (Requests rows with live's longest names). Skipped without Chrome (UI_BROWSER
// or the usual install paths) and with -short.
func TestUILayoutNoOverlap(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: no browser")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	get, base := fixtureStart(t, "--models", "written", "--long-names")
	var models any
	get("/api/models", &models) // the server is up
	mdl := []string{".mdl .mr:not(.mr-h)"}
	cases := []map[string]any{
		{"name": "models", "route": "#/models", "rows": mdl, "minRows": 9},
		{"name": "models-panel-program", "route": "#/models?model=command:image-gen-fast", "rows": mdl},
		{"name": "models-panel-file", "route": "#/models?model=tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf", "rows": mdl},
		{"name": "profiles", "route": "#/profiles", "minRows": 10},
		{"name": "profile-program", "route": "#/profiles?profile=image-gen-fast"},
		{"name": "requests", "route": "#/requests", "menu": "open", "widths": []int{1536, 1280}, "minRows": 10},
		{"name": "requests", "route": "#/requests", "menu": "closed", "minRows": 10},
	}
	for _, c := range cases {
		c["base"] = base
	}
	b, _ := json.Marshal(cases)
	list := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(list, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, here, _, _ := runtime.Caller(0)
	cmd := exec.Command(node, filepath.Join(filepath.Dir(here), "..", "..", "scripts", "ui-layout.mjs"), list)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 3 {
		t.Skip("no Chrome: " + strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("layout: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
