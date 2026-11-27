package genericguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T) (string, Allow) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := Root(wd)
	if err != nil {
		t.Fatal(err)
	}
	allow, err := LoadAllow(filepath.Join(root, "internal", "genericguard", "allow.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return root, allow
}

// TestGenericTree: the whole tree, as the gate sees it. A hit names the
// file, the line and the word; fix the text (zone, machine, the legacy
// fleet feed), or, for a true exception, add "path: text" to allow.txt.
func TestGenericTree(t *testing.T) {
	root, allow := load(t)
	hits, err := CheckTree(root, allow)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Errorf("not generic: %s", h)
	}
}

// The check itself: each kind of file, the allow list, and what is let through.
func TestGenericCheck(t *testing.T) {
	allow := Allow{"internal/bench/reasonshort.go": {`"is not a console"`}}
	for _, c := range []struct {
		path, content string
		hit           bool
	}{
		{"internal/api/ui/next/views/zones.js", "const why = 'a PlayStation is never rebooted';", true},
		{"internal/api/ui/next/views/zones.js", "// three PS5s", true},
		{"internal/api/ui/next/views/zones.js", "console.error('x'); console.warn(y)", false},
		{"internal/api/ui/next/views/zones.js", "const n = '3 zones';", false},
		{"internal/api/ui/README.md", "the fleet console lists them", true},
		{"internal/bench/inputs.go", "package bench\n\n// a console in a comment is not a product string\nvar s = \"zone\"\n", false},
		{"internal/bench/inputs.go", "package bench\n\nvar s = \"any 3 consoles\"\n", true},
		{"internal/bench/inputs.go", "package bench\n\nvar s = `on the OMEN`\n", true},
		{"internal/bench/reasonshort.go", "package bench\n\nvar old = strings.Contains(low, \"is not a console\")\n", false},
		{"internal/bench/reasonshort.go", "package bench\n\nvar s = \"a console\"\n", true},
		{"deploy/seed-config.yaml", "  - match: { hostname: \"alienware*\" }", true},
		{"configs/example.yaml", "force_nodes: [node-1, laptop-1]", false},
		{"deploy/Dockerfile", "# from the ps5 fork", true},
	} {
		checked, goStrings := Checked(c.path)
		if !checked {
			t.Fatalf("%s is not checked", c.path)
		}
		hits := CheckFile(c.path, []byte(c.content), goStrings, allow)
		if (len(hits) > 0) != c.hit {
			t.Errorf("%s %q: hits %v, want a hit: %v", c.path, c.content, hits, c.hit)
		}
	}
	for _, p := range []string{"internal/bench/inputs_test.go", "cmd/devcluster/main.go", "internal/testutil/fakefleet/fakefleet.go",
		"docs/runbook.md", "internal/agent/stats/testdata/x.prom", "scripts/ui-exec.mjs", "deploy/compose_test.go"} {
		if checked, _ := Checked(p); checked {
			t.Errorf("%s is checked; it holds tests, fixtures or history", p)
		}
	}
}

// The allow list holds only real lines: an entry whose text no longer
// appears in its file is dead and must go.
func TestGenericAllowListLive(t *testing.T) {
	root, allow := load(t)
	for p, texts := range allow {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("allow.txt: %s: %v", p, err)
			continue
		}
		for _, x := range texts {
			if !strings.Contains(string(b), x) {
				t.Errorf("allow.txt: %s no longer contains %q", p, x)
			}
		}
	}
}
