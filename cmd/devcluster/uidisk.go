package main

// --ui-dir: the front serves /ui/ straight from disk so the UI files can be
// edited and reloaded without rebuilding the Go binary.

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

var uiTypes = map[string]string{
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

// defaultUIDir is internal/api/ui under the module root, if it exists.
func defaultUIDir() string {
	root, err := moduleRoot()
	if err != nil {
		return ""
	}
	d := filepath.Join(root, "internal", "api", "ui")
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	return ""
}

func uiDiskHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/ui/")
		if strings.Contains(rel, "..") || strings.Contains(rel, "\\") || strings.Contains(rel, ":") {
			writeErr(w, 400, "bad path", "bad request")
			return
		}
		if rel == "" {
			rel = "index.html"
		}
		ct, ok := uiTypes[path.Ext(rel)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", ct)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		w.Write(b)
	}
}

// devFrame is GET /dev/frame?w=390&h=844&route=<hash route>: the UI in a
// same-origin iframe of an exact size, for phone-width screenshots (desktop
// Chrome will not lay a window out narrower than about 500 px).
func devFrame(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dim := func(name string, def int) (int, bool) {
		v := q.Get(name)
		if v == "" {
			return def, true
		}
		n, err := strconv.Atoi(v)
		return n, err == nil && n >= 200 && n <= 4000
	}
	fw, ok1 := dim("w", 390)
	fh, ok2 := dim("h", 844)
	if !ok1 || !ok2 {
		writeErr(w, 400, "w and h must be whole numbers from 200 to 4000", "bad request")
		return
	}
	route := strings.TrimPrefix(q.Get("route"), "/")
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>frame</title><body style="margin:0;background:#0b0d0c"><iframe src="/ui/#/%s" style="position:fixed;left:0;top:0;width:%dpx;height:%dpx;border:0"></iframe></body>`,
		html.EscapeString(route), fw, fh)
}
