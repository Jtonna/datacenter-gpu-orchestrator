package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// cutUpstream is a model server whose answers wait for the test. Each
// window of a cut is produced on purpose; "started" is signalled once the
// fake has written (and flushed) all it will write before the cut:
//   - "hold" (no stream): nothing is sent, not even the headers, until the
//     request ends (the instance is killed, which ends the connection);
//   - "jsonhead" (no stream): status 200 and headers of a JSON body of 100
//     bytes (Content-Length), flushed, then nothing;
//   - "jsonpart" (no stream): the same, plus 20 of the 100 bytes;
//   - "stream":true and "quiet": nothing at all is sent (not even the headers);
//   - "stream":true and "headers": status 200 and the event-stream headers,
//     flushed, then nothing (no event);
//   - "stream":true otherwise: two events (tokens t1 and t2), each flushed,
//     then nothing;
//   - "die": the connection is dropped at once, with no answer (a model
//     server that crashed on its own);
//   - anything else: {} at once (the request that loads the model).
type cutUpstream struct{ started chan struct{} }

const cutEvent = `data: {"choices":[{"delta":{"content":"%s"},"finish_reason":null,"index":0}],"object":"chat.completion.chunk"}`

func (u *cutUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s := string(b)
	flush := func() { w.(http.Flusher).Flush() }
	hold := func() {
		u.started <- struct{}{}
		<-r.Context().Done()
	}
	stream := strings.Contains(s, `"stream":true`)
	switch {
	case strings.Contains(s, "die"):
		conn, _, _ := http.NewResponseController(w).Hijack()
		conn.Close()
	case stream && strings.Contains(s, "quiet"):
		hold()
	case stream && strings.Contains(s, "headers"):
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flush()
		hold()
	case stream:
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range []string{"t1", "t2"} {
			fmt.Fprintf(w, cutEvent+"\n\n", tok)
			flush()
		}
		hold()
	case strings.Contains(s, "jsonhead"), strings.Contains(s, "jsonpart"):
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		if strings.Contains(s, "jsonpart") {
			w.Write([]byte(`{"choices":[{"message`))
		}
		flush()
		hold()
	case strings.Contains(s, "hold"):
		hold()
	default:
		w.Write([]byte(`{}`))
	}
}

type cutAnswer struct {
	status     int
	retryAfter string
	body       string
	err        map[string]any
}

// send posts body to /v1/chat/completions in the background.
func sendCut(t *testing.T, url, body string) <-chan cutAnswer {
	ch := make(chan cutAnswer, 1)
	go func() {
		resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			ch <- cutAnswer{status: -1, body: err.Error()}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(bufio.NewReader(resp.Body))
		var m map[string]any
		json.Unmarshal(b, &m)
		e, _ := m["error"].(map[string]any)
		ch <- cutAnswer{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: string(b), err: e}
	}()
	return ch
}

func waitCut(t *testing.T, ch <-chan cutAnswer) cutAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Second):
		t.Fatal("no answer in 10 s")
		return cutAnswer{}
	}
}

func waitStarted(t *testing.T, u *cutUpstream) {
	t.Helper()
	select {
	case <-u.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the model server")
	}
}

// A request in flight cut by a forced unload is told why: 503, reason
// unloaded, "unloaded: <profile> was unloaded", with Retry-After, the same
// shape a loading or queued caller gets. Not 502 "upstream request failed".
func TestCutAnswer_ForcedUnload(t *testing.T) {
	u := &cutUpstream{started: make(chan struct{}, 4)}
	e := newEnv(t, u)
	body(t, e.post("/v1/chat/completions", chat("alpha"))) // loads alpha
	ch := sendCut(t, e.front.URL, `{"model":"alpha","messages":[{"role":"user","content":"hold"}]}`)
	waitStarted(t, u)
	if err := e.sched.Unload(context.Background(), "alpha", true); err != nil {
		t.Fatal(err)
	}
	a := waitCut(t, ch)
	if a.status != 503 || a.err["reason"] != "unloaded" || a.err["message"] != "unloaded: alpha was unloaded" || a.err["type"] != "server_error" || a.retryAfter == "" {
		t.Fatalf("cut by a forced unload: %d %q %s", a.status, a.retryAfter, a.body)
	}
}

// The same cut through the API's force unload (POST /api/unload).
func TestCutAnswer_ForcedUnloadRoute(t *testing.T) {
	u := &cutUpstream{started: make(chan struct{}, 4)}
	e := newEnv(t, u)
	body(t, e.post("/v1/chat/completions", chat("alpha")))
	ch := sendCut(t, e.front.URL, `{"model":"alpha","messages":[{"role":"user","content":"hold"}]}`)
	waitStarted(t, u)
	body(t, e.post("/api/unload", `{"model":"alpha","force":true}`))
	if a := waitCut(t, ch); a.status != 503 || a.err["reason"] != "unloaded" || a.err["message"] != "unloaded: alpha was unloaded" {
		t.Fatalf("cut by POST /api/unload force: %d %s", a.status, a.body)
	}
}

// A delete with interrupt says the model file is being deleted.
func TestCutAnswer_DeleteWithInterrupt(t *testing.T) {
	u := &cutUpstream{started: make(chan struct{}, 4)}
	te := newTrashEnvUp(t, nil, u)
	te.setup()
	body(t, te.post("/v1/chat/completions", `{"model":"a1","messages":[{"role":"user","content":"hi"}]}`))
	ch := sendCut(t, te.front.URL, `{"model":"a1","messages":[{"role":"user","content":"hold"}]}`)
	waitStarted(t, u)
	if st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"owner said so","interrupt":true}`, trashUI); st != 200 {
		t.Fatalf("delete: %d %v", st, m)
	}
	a := waitCut(t, ch)
	if a.status != 503 || a.err["reason"] != "unloaded" || a.err["message"] != "unloaded: a1 was unloaded: its model file is being deleted" {
		t.Fatalf("cut by a delete with interrupt: %d %s", a.status, a.body)
	}
}

// A model server that drops the connection on its own (not unloaded) is
// answered 502 "upstream request failed", as before.
func TestCutAnswer_UpstreamDiesOnItsOwn(t *testing.T) {
	u := &cutUpstream{started: make(chan struct{}, 4)}
	e := newEnv(t, u)
	body(t, e.post("/v1/chat/completions", chat("alpha")))
	a := waitCut(t, sendCut(t, e.front.URL, `{"model":"alpha","messages":[{"role":"user","content":"die"}]}`))
	if a.status != 502 || !strings.HasPrefix(fmt.Sprint(a.err["message"]), "upstream request failed") {
		t.Fatalf("upstream died: %d %s", a.status, a.body)
	}
}
