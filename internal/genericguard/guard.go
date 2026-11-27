// Package genericguard keeps Potluck generic: nothing the product ships or
// says may name a hardware brand or call a machine a "console". The check
// runs as a Go test over the whole tree (guard_test.go), so every gate that
// runs go test runs it; the local pre-commit hook runs the same check on
// the files a commit stages (go run ./internal/genericguard/cmd/genericguard).
//
// What is checked:
//   - internal/api/ui/**: every line (text, code and comments);
//   - deploy/** and configs/**: every line;
//   - Go product code (not _test.go, not testdata, not the test helpers and
//     dev harnesses listed in skipDirs): the string literals only.
//
// The few true exceptions are lines in allow.txt ("path: text", the text a
// line must contain to be let through).
package genericguard

import (
	"bufio"
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Words are the brand words and "console(s)" as a machine word. A browser
// console call in JavaScript (console.log and the like) is not a machine.
var Words = regexp.MustCompile(`(?i)playstation|\bps[45]|\borbis\b|\boberon\b|\bomen\b|alienware|\bconsoles?\b`)

var jsConsoleCall = regexp.MustCompile(`\bconsole\.[a-zA-Z]+`)

// skipDirs are product-tree folders that hold only tests, fixtures or dev
// harnesses (their own data may name test machines).
var skipDirs = []string{
	"cmd/devcluster",
	"cmd/potluck-fake-agents",
	"cmd/potluck-gate",
	"internal/testutil",
	"internal/e2e",
	"internal/genericguard",
}

// Hit is one line that names a brand or a console.
type Hit struct {
	Path string // slash path from the repository root
	Line int
	Text string
	Word string
}

func (h Hit) String() string {
	return fmt.Sprintf("%s:%d: %q in %s", h.Path, h.Line, h.Word, strings.TrimSpace(h.Text))
}

// Allow is the allow list: per path, the texts that let a line through.
type Allow map[string][]string

// LoadAllow reads allow.txt: "path: text" per line; # starts a comment.
func LoadAllow(file string) (Allow, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	a := Allow{}
	for i, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		p, text, ok := strings.Cut(ln, ": ")
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%s:%d: want \"path: text\"", file, i+1)
		}
		a[strings.TrimSpace(p)] = append(a[strings.TrimSpace(p)], strings.TrimSpace(text))
	}
	return a, nil
}

func (a Allow) allows(path, line string) bool {
	for _, t := range a[path] {
		if strings.Contains(line, t) {
			return true
		}
	}
	return false
}

// Checked says whether a file (slash path from the root) is checked, and
// whether only its Go string literals are.
func Checked(path string) (checked, goStrings bool) {
	switch {
	case strings.Contains(path, "/node_modules/"), strings.Contains(path, "/testdata/"):
		return false, false
	case strings.HasPrefix(path, "internal/api/ui/"):
		return true, false
	case strings.HasPrefix(path, "deploy/"), strings.HasPrefix(path, "configs/"):
		return !strings.HasSuffix(path, "_test.go"), false
	case strings.HasSuffix(path, ".go") && (strings.HasPrefix(path, "internal/") || strings.HasPrefix(path, "cmd/")):
		if strings.HasSuffix(path, "_test.go") {
			return false, false
		}
		for _, d := range skipDirs {
			if strings.HasPrefix(path, d+"/") {
				return false, false
			}
		}
		return true, true
	}
	return false, false
}

// CheckFile returns the hits of one file's content (path is the slash path
// from the root; the caller has asked Checked first).
func CheckFile(path string, content []byte, goStrings bool, allow Allow) []Hit {
	var hits []Hit
	add := func(line int, text, s string) {
		if strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".mjs") {
			s = jsConsoleCall.ReplaceAllString(s, "")
		}
		if m := Words.FindString(s); m != "" && !allow.allows(path, text) {
			hits = append(hits, Hit{Path: path, Line: line, Text: text, Word: m})
		}
	}
	lines := strings.Split(string(content), "\n")
	if !goStrings {
		for i, ln := range lines {
			add(i+1, ln, ln)
		}
		return hits
	}
	fset := token.NewFileSet()
	f := fset.AddFile(path, -1, len(content))
	var s scanner.Scanner
	s.Init(f, content, nil, 0)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.STRING {
			continue
		}
		p := fset.Position(pos)
		text := ""
		if p.Line-1 < len(lines) {
			text = lines[p.Line-1]
		}
		add(p.Line, text, lit)
	}
	return hits
}

// CheckTree walks the tree under root and returns every hit, sorted.
func CheckTree(root string, allow Allow) ([]Hit, error) {
	var hits []Hit
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		checked, goStrings := Checked(rel)
		if !checked {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil // a binary file
		}
		hits = append(hits, CheckFile(rel, b, goStrings, allow)...)
		return nil
	})
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Path != hits[j].Path {
			return hits[i].Path < hits[j].Path
		}
		return hits[i].Line < hits[j].Line
	})
	return hits, err
}

// Root finds the repository root (the folder with go.mod) from dir upward.
func Root(dir string) (string, error) {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		up := filepath.Dir(d)
		if up == d {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		d = up
	}
}

// Lines reads a list of slash paths, one per line (for the hook).
func Lines(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			out = append(out, filepath.ToSlash(t))
		}
	}
	return out
}
