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

// Every embedded file is served with the content type of its kind: a file of
// ui/next/ below /ui/ under its path inside next/, every other file below
// /ui/classic/, and below /ui/ too unless ui/next/ has a file of that path
// (uifiles.go). ui/next/ is never served under its own name.
func TestUIFilesServedWithContentType(t *testing.T) {
	e := newEnv(t, chatOK)
	check := func(url, name string) {
		want, ok := uiContentTypes[path.Ext(name)]
		if !ok {
			t.Errorf("%s: its extension has no content type in uiContentTypes, so it would not be served", name)
			return
		}
		resp := e.get(url)
		b := body(t, resp)
		if resp.StatusCode != 200 {
			t.Errorf("GET %s: status %d", url, resp.StatusCode)
			return
		}
		if got := resp.Header.Get("Content-Type"); got != want {
			t.Errorf("GET %s: content type %q, want %q", url, got, want)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: no nosniff header", url)
		}
		if b != uiText(t, name) {
			t.Errorf("GET %s: the body is not the embedded file %s", url, name)
		}
	}
	have := map[string]bool{}
	for _, name := range uiFiles(t) {
		have[name] = true
	}
	for _, name := range uiFiles(t) {
		if rel, ok := strings.CutPrefix(name, uiNextDir); ok {
			check("/ui/"+rel, name)
			continue
		}
		check("/ui/classic/"+name, name)
		if !have[uiNextDir+name] {
			check("/ui/"+name, name)
		}
	}
	check("/ui/", uiNextDir+"index.html")
	check("/ui/classic/", "index.html")
	for _, p := range []string{"/ui/nope.js", "/ui/../api.go", "/ui/views/", "/ui/fonts", "/ui/core/dom",
		"/ui/next/index.html", "/ui/next/app.css", "/ui/classic/next/index.html", "/ui/classic/nope.js", "/ui/classic/views/"} {
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
	// served(name, ref) says whether the file that name refers to with ref
	// is served: a classic file below /ui/classic/ sees the classic files;
	// a file of ui/next/ is served below /ui/, where a path is the next/
	// file or else the classic one (uiResolve).
	served := func(name, ref string) (string, bool) {
		if rel, ok := strings.CutPrefix(name, uiNextDir); ok {
			target := path.Join(path.Dir(rel), ref)
			return target, !strings.HasPrefix(target, "..") && uiResolve(target) != ""
		}
		target := path.Join(path.Dir(name), ref)
		return target, have[target] && !strings.HasPrefix(target, uiNextDir)
	}
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
			if target, ok := served(name, spec); !ok {
				t.Errorf("%s imports %q, which is not an embedded file (%s)", name, spec, target)
			}
		}
	}
	for _, pg := range []struct{ page, api string }{{"index.html", "../../api/"}, {uiNextDir + "index.html", "../api/"}} {
		for _, m := range regexp.MustCompile(`(?:src|href)="([^"#]+)"`).FindAllStringSubmatch(uiText(t, pg.page), -1) {
			ref := m[1]
			if strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, pg.api) {
				continue
			}
			if _, ok := served(pg.page, ref); !ok {
				t.Errorf("%s refers to %q, which is not an embedded file", pg.page, ref)
			}
		}
	}
	for _, css := range []string{"app.css", uiNextDir + "app.css"} {
		if !have[css] {
			continue
		}
		for _, m := range regexp.MustCompile(`url\(([^)]+)\)`).FindAllStringSubmatch(uiText(t, css), -1) {
			if ref := strings.Trim(m[1], `"'`); !strings.HasPrefix(ref, "data:") {
				if _, ok := served(css, ref); !ok {
					t.Errorf("%s refers to %q, which is not an embedded file", css, ref)
				}
			}
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
		if path.Ext(name) == ".js" && name != "core/api.js" && name != uiNextDir+"core/api.js" && regexp.MustCompile(`\bfetch\(|XMLHttpRequest`).MatchString(uiText(t, name)) {
			t.Errorf("%s calls fetch directly: use core/api.js", name)
		}
	}
}

// Live data comes over the WebSocket only: no view polls on a timer. The two
// intervals that exist are the one-second clock tick of the store (it redraws
// "38 min ago", it fetches nothing) and the watchdog of the live channel;
// and in the new shell next/core/requests.js: its fallback poll of the
// request log, which runs only while the live channel (topic "requests") is
// down, and every(), the 100 ms clock of the running rows' length, which
// reads nothing. The Overview's totals and strips follow live messages.
func TestUIHasNoPollingTimer(t *testing.T) {
	allowed := map[string]bool{"core/store.js": true, "core/live.js": true, uiNextDir + "core/store.js": true, uiNextDir + "core/live.js": true,
		uiNextDir + "core/requests.js": true}
	for _, name := range uiFiles(t) {
		if path.Ext(name) != ".js" {
			continue
		}
		if strings.Contains(uiText(t, name), "setInterval") && !allowed[name] {
			t.Errorf("%s uses setInterval: refresh on a WebSocket message instead of a timer", name)
		}
	}
	if strings.Contains(uiText(t, "core/store.js"), "api.") || strings.Contains(uiText(t, "core/store.js"), "fetch") {
		t.Error("core/store.js must not call REST")
	}
}
