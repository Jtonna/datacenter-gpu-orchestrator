package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// GET / (and HEAD /) on the scheduler sends a browser to the UI. Only the
// exact root path: every other unknown path, and any other method on /,
// still answers the API's 404 in JSON, and the routes beside it are untouched.
func TestRootRedirectsToTheUI(t *testing.T) {
	e := newEnv(t, chatOK)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, p string) *http.Response {
		req, _ := http.NewRequest(method, e.front.URL+p, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	for _, c := range []struct{ method, path string }{{"GET", "/"}, {"HEAD", "/"}, {"GET", "/?from=bookmark"}} {
		resp := do(c.method, c.path)
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
			t.Errorf("%s %s: %d Location %q, want 302 to /ui/", c.method, c.path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	for _, c := range []struct{ method, path string }{{"POST", "/"}, {"GET", "/nope"}, {"GET", "/index.html"}, {"GET", "/api"}, {"GET", "/api/"}} {
		resp := do(c.method, c.path)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want the API's 404", c.method, c.path, resp.StatusCode)
		}
		if resp.Header.Get("Location") != "" || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
			t.Errorf("%s %s: Location %q, type %q, body %s: want a JSON error and no redirect", c.method, c.path, resp.Header.Get("Location"), resp.Header.Get("Content-Type"), b)
		}
	}
	for _, p := range []string{"/health", "/api/status", "/ui/"} {
		if resp := do("GET", p); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", p, resp.StatusCode)
		}
	}
}
