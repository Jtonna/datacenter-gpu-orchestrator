package api

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// uiFiles lists every embedded UI file (path below /ui/).
func uiFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(uiFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 10 {
		t.Fatalf("only %d UI files are embedded: %v", len(out), out)
	}
	return out
}

func uiText(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(uiFS, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func isUICode(name string) bool {
	switch path.Ext(name) {
	case ".html", ".css", ".js":
		return true
	}
	return false
}

// Every embedded file is served below /ui/ with the content type of its kind.
func TestUIFilesServedWithContentType(t *testing.T) {
	e := newEnv(t, chatOK)
	for _, name := range uiFiles(t) {
		want, ok := uiContentTypes[path.Ext(name)]
		if !ok {
			t.Errorf("%s: its extension has no content type in uiContentTypes, so it would not be served", name)
			continue
		}
		resp := e.get("/ui/" + name)
		b := body(t, resp)
		if resp.StatusCode != 200 {
			t.Errorf("GET /ui/%s: status %d", name, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Content-Type"); got != want {
			t.Errorf("GET /ui/%s: content type %q, want %q", name, got, want)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET /ui/%s: no nosniff header", name)
		}
		if b != uiText(t, name) {
			t.Errorf("GET /ui/%s: the body is not the embedded file", name)
		}
	}
	for _, p := range []string{"/ui/nope.js", "/ui/../api.go", "/ui/views/", "/ui/fonts", "/ui/core/dom"} {
		if resp := e.get(p); resp.StatusCode == 200 && !strings.Contains(body(t, resp), "<!doctype html>") {
			t.Errorf("GET %s: status 200 for something that is not a UI file", p)
		}
	}
}

// The UI is self-contained: no file loads from, or links to, another host
// (the page is opened on a LAN that may have no internet).
func TestUIHasNoExternalReference(t *testing.T) {
	ext := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9][a-z0-9.-]*\.[a-z]{2,}[/"'):\s]|https?://`)
	// The SVG namespace is an identifier, not an address that is fetched.
	allowed := strings.NewReplacer("http://www.w3.org/2000/svg", "", "http://www.w3.org/1999/xlink", "")
	for _, name := range uiFiles(t) {
		if !isUICode(name) {
			continue
		}
		for i, line := range strings.Split(allowed.Replace(uiText(t, name)), "\n") {
			if m := ext.FindString(line); m != "" {
				t.Errorf("%s:%d: external reference %q", name, i+1, m)
			}
		}
		if strings.Contains(uiText(t, name), "@import") {
			t.Errorf("%s: @import is not allowed", name)
		}
	}
	// The fonts are bundled, each with its licence.
	for _, f := range []string{"fonts/Manrope-Variable.woff2", "fonts/Manrope-OFL.txt", "fonts/JetBrainsMono-Regular.woff2", "fonts/JetBrainsMono-Medium.woff2", "fonts/JetBrainsMono-OFL.txt"} {
		if _, err := fs.Stat(uiFS, f); err != nil {
			t.Errorf("missing bundled font file %s", f)
		}
	}
}

// Loading and unloading a model is not possible from the web UI: no file
// names the endpoints that do it.
func TestUIHasNoLoadOrUnloadControl(t *testing.T) {
	bad := []string{"/api/load", "/api/unload", "/api/models/unload", `'/unload'`, `"/unload"`, "`/unload`"}
	for _, name := range uiFiles(t) {
		if !isUICode(name) {
			continue
		}
		text := uiText(t, name)
		for _, b := range bad {
			if strings.Contains(text, b) {
				t.Errorf("%s names %s: the UI must not be able to load or unload a model", name, b)
			}
		}
	}
}

// Every module a UI file imports, and every file the page links, is embedded.
func TestUIImportsResolve(t *testing.T) {
	have := map[string]bool{}
	for _, name := range uiFiles(t) {
		have[name] = true
	}
	imp := regexp.MustCompile(`(?m)^\s*(?:import|export)\s+(?:[\w*{}\s,$]+\s+from\s+)?['"]([^'"]+)['"]|import\(\s*['"]([^'"]+)['"]\s*\)`)
	for name := range have {
		if path.Ext(name) != ".js" {
			continue
		}
		for _, m := range imp.FindAllStringSubmatch(uiText(t, name), -1) {
			spec := m[1]
			if spec == "" {
				spec = m[2]
			}
			if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
				t.Errorf("%s imports %q: only relative imports of bundled files are allowed", name, spec)
				continue
			}
			if target := path.Join(path.Dir(name), spec); !have[target] {
				t.Errorf("%s imports %q, which is not an embedded file (%s)", name, spec, target)
			}
		}
	}
	page := uiText(t, "index.html")
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"#]+)"`).FindAllStringSubmatch(page, -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "../api/") {
			continue
		}
		if !have[path.Clean(ref)] {
			t.Errorf("index.html refers to %q, which is not an embedded file", ref)
		}
	}
	for _, m := range regexp.MustCompile(`url\(([^)]+)\)`).FindAllStringSubmatch(uiText(t, "app.css"), -1) {
		if ref := strings.Trim(m[1], `"'`); !strings.HasPrefix(ref, "data:") && !have[path.Clean(ref)] {
			t.Errorf("app.css refers to %q, which is not an embedded file", ref)
		}
	}
}

// Every write the UI sends is marked as coming from the web UI, with an actor.
func TestUIWritesCarrySourceAndActor(t *testing.T) {
	text := uiText(t, "core/api.js")
	for _, want := range []string{`'X-Source': 'ui'`, `'X-Actor'`, `If-Match`, `X-Comment`, `dry_run`} {
		if !strings.Contains(text, want) {
			t.Errorf("core/api.js lacks %s", want)
		}
	}
	// fetch is only used through the REST helper, so no view can skip the headers.
	for _, name := range uiFiles(t) {
		if path.Ext(name) == ".js" && name != "core/api.js" && regexp.MustCompile(`\bfetch\(|XMLHttpRequest`).MatchString(uiText(t, name)) {
			t.Errorf("%s calls fetch directly: use core/api.js", name)
		}
	}
}
