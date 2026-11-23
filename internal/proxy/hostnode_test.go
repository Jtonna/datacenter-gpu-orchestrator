package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A host on a node (phase 6) wants its per-launch key on every request; the
// client's own Authorization header is not passed on.
func TestForwardSendsTheKey(t *testing.T) {
	got := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		fmt.Fprint(w, "{}")
	}))
	defer up.Close()
	fr := front(t, Options{Addr: upstreamAddr(up), APIKey: "k0123456789abcdef"}, nil)
	req, _ := http.NewRequest(http.MethodPost, fr.URL+"/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer the-clients-own")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if a := <-got; a != "Bearer k0123456789abcdef" {
		t.Fatal(a)
	}
	// Without a key the request goes as it came (a host on the scheduler's machine).
	fr = front(t, Options{Addr: upstreamAddr(up)}, nil)
	req, _ = http.NewRequest(http.MethodPost, fr.URL+"/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer the-clients-own")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if a := <-got; a != "Bearer the-clients-own" {
		t.Fatal(a)
	}
}

// A request to a host that has gone silent (a powered-off console sends no
// reset) ends when the instance is removed, not when the client gives up.
func TestForwardEndsWhenTheInstanceIsGone(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // never answers, as a console that lost power
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	gone := make(chan struct{})
	res := make(chan Result, 1)
	fr := front(t, Options{Addr: upstreamAddr(up), Gone: gone}, res)
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(fr.URL+"/v1/completions", "application/json", nil)
		if err != nil {
			done <- nil
			return
		}
		done <- resp
	}()
	time.Sleep(100 * time.Millisecond)
	close(gone)
	select {
	case r := <-res:
		if !r.Cut || r.Status != http.StatusBadGateway {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request was not ended")
	}
	if resp := <-done; resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("%+v", resp)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
