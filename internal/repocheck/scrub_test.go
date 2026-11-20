// Package repocheck holds checks on the repository itself.
package repocheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoDeploymentIdentifiers fails when a file in the repository names a machine,
// address or path of the first deployment. Examples and tests use generic node
// names, locally administered MAC addresses (02:00:00:...) and the documentation
// address range 192.0.2.0/24. The patterns are assembled from pieces so that this
// file does not match them itself.
func TestNoDeploymentIdentifiers(t *testing.T) {
	j := func(parts ...string) string { return strings.Join(parts, "") }
	macPrefixes := [][3]string{
		{"70", "66", "2a"}, {"c0", "15", "1b"}, {"0c", "70", "43"}, {"b4", "1f", "4d"},
		{"c8", "4a", "a0"}, {"b4", "45", "06"}, {"6c", "2b", "59"}, {"a8", "47", "4a"},
		{"b8", "82", "f2"}, {"6c", "1f", "f7"},
	}
	var macAlts []string
	for _, m := range macPrefixes {
		for _, sep := range []string{":", "%3a", "-", ""} {
			macAlts = append(macAlts, regexp.QuoteMeta(m[0]+sep+m[1]+sep+m[2]))
		}
	}
	patterns := map[string]*regexp.Regexp{
		"private link address":         regexp.MustCompile(j(`\b10\.0`, `\.0\.[0-9]+`)),
		"real host name":               regexp.MustCompile(j(`(?i)\b(om`, `en-runner|alien`, `ware-(llm|m15)|ps`, `5-[0-9a-f]{8})\b`)),
		"real hardware address prefix": regexp.MustCompile(`(?i)(^|[^0-9a-f])(` + strings.Join(macAlts, "|") + `)([:%-]|[0-9a-f]{6}\b)`),
		"personal path":                regexp.MustCompile(j(`(?i)(c:\+users\+[a-z]|/home/(ad`, `min|j)\b|ps`, `5llm)`)),
	}
	root := repoRoot(t)
	skipDir := map[string]bool{".git": true, "node_modules": true}
	binaryExt := map[string]bool{".png": true, ".jpg": true, ".ico": true, ".woff": true, ".woff2": true, ".gguf": true, ".exe": true, ".db": true}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if binaryExt[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for i, line := range strings.Split(string(b), "\n") {
			for name, re := range patterns {
				if m := re.FindString(line); m != "" {
					t.Errorf("%s:%d: %s: %q", filepath.ToSlash(rel), i+1, name, strings.TrimSpace(m))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
