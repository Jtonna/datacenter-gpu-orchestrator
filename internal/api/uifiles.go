package api

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The web UI (design 9.1) is a set of static files embedded in the binary: a
// page, a stylesheet, plain JavaScript modules, and two bundled fonts.
// Nothing is fetched from another host.
//
// One UI is served, at /ui/ (the classic UI that was served at /ui/classic/
// is removed: the owner's word, 24 Nov). A path below /ui/ is the file of the
// same path in ui/next/ when that exists, else the file of the ui directory
// itself: ui/next/ holds the page, the stylesheet, the modules that draw
// (core/ui.js, core/shell.js and so on) and the rebuilt screens; ui/ holds
// the modules they share (the data helpers, the views mounted unchanged).
//
// ui/next/ itself is never served under its own name, and /ui/classic/ is
// not served at all (404).

//go:embed all:ui
var uiEmbed embed.FS

// uiFS is the ui directory as a file system.
var uiFS = func() fs.FS {
	sub, err := fs.Sub(uiEmbed, "ui")
	if err != nil {
		panic(err)
	}
	return sub
}()

// uiNextDir is the folder of the new shell inside uiFS.
const uiNextDir = "next/"

// uiContentTypes are the only kinds of file the UI serves. They are set
// here, not guessed from the operating system's registry.
var uiContentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
}

func uiExists(name string) bool {
	st, err := fs.Stat(uiFS, name)
	return err == nil && !st.IsDir()
}

// uiResolve maps a path below /ui/ (without the prefix, cleaned) to the file
// in uiFS that is served for it, or "" when none is.
func uiResolve(p string) string {
	if p == "" || p == "." {
		return ""
	}
	if strings.HasPrefix(p, uiNextDir) || p == "classic" || strings.HasPrefix(p, "classic/") {
		return ""
	}
	if uiExists(uiNextDir + p) {
		return uiNextDir + p
	}
	if uiExists(p) {
		return p
	}
	return ""
}

func (s *Server) uiRoutes(m *http.ServeMux) {
	// The bare address of the scheduler opens the UI. Only the exact root
	// path and only GET (and HEAD): any other path or method on / still gets
	// the API's 404. 302, not 301, so browsers do not cache it for good.
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
	m.HandleFunc("GET /ui/{$}", func(w http.ResponseWriter, r *http.Request) { serveUIFile(w, r, uiNextDir+"index.html") })
	m.HandleFunc("GET /ui/", s.handleUIFile)
	m.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusMovedPermanently) })
}

// handleUIFile serves one embedded file below /ui/. Read-only.
func (s *Server) handleUIFile(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/ui/")), "/")
	name := uiResolve(p)
	if name == "" {
		http.NotFound(w, r)
		return
	}
	serveUIFile(w, r, name)
}

func serveUIFile(w http.ResponseWriter, r *http.Request, name string) {
	ct, ok := uiContentTypes[path.Ext(name)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(uiFS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(b)
}
