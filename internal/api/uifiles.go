package api

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The web UI (design 9.1) is a set of static files embedded in the binary:
// index.html (served at /ui/ by handleUI), a stylesheet, plain JavaScript
// modules, and two bundled fonts. Nothing is fetched from another host.

//go:embed all:ui
var uiEmbed embed.FS

// uiFS is the ui directory as a file system rooted at /ui/.
var uiFS = func() fs.FS {
	sub, err := fs.Sub(uiEmbed, "ui")
	if err != nil {
		panic(err)
	}
	return sub
}()

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

// handleUIFile serves one embedded file below /ui/. Read-only.
func (s *Server) handleUIFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/ui/")), "/")
	ct, ok := uiContentTypes[path.Ext(name)]
	if name == "" || !ok {
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
