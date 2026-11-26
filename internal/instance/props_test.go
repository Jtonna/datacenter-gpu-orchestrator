package instance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The server's own context per slot: /props default_generation_settings.n_ctx
// (llama.cpp tools/server/server-context.cpp: meta.slot_n_ctx, the slot
// context after the trained-context cap).
func TestReadSlotCtx(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"default_generation_settings":{"n_ctx":2048,"params":{}},"total_slots":1}`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	n, err := ReadSlotCtx(context.Background(), addr, "k1")
	if err != nil || n != 2048 {
		t.Fatalf("got %d %v", n, err)
	}
	if auth != "Bearer k1" {
		t.Fatalf("authorization %q", auth)
	}
	// An older answer with only a top-level n_ctx.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"n_ctx":4096}`)) }))
	defer old.Close()
	if n, err := ReadSlotCtx(context.Background(), strings.TrimPrefix(old.URL, "http://"), ""); err != nil || n != 4096 {
		t.Fatalf("top-level n_ctx: %d %v", n, err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if _, err := ReadSlotCtx(context.Background(), strings.TrimPrefix(bad.URL, "http://"), ""); err == nil {
		t.Fatal("a 500 gave a context")
	}
}
