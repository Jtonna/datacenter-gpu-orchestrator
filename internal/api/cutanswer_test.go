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

// cutUpstream is a model server whose answers wait for the test:
//   - "hold" in a request that does not stream: nothing is sent until the
//     request ends (the instance is killed, which the test acts out as the
//     connection ending);
//   - "stream":true: one token is sent, then nothing until the request ends;
//   - "die": the connection is dropped at once, with no answer (a model
//     server that crashed on its own);
//   - anything else: {} at once (the request that loads the model).
type cutUpstream struct{ started chan struct{} }

func (u *cutUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s := string(b)
	switch {
	case strings.Contains(s, "die"):
		conn, _, _ := http.NewResponseController(w).Hijack()
		conn.Close()
	case strings.Contains(s, `"stream":true`):
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"t1"},"finish_reason":null,"index":0}],"object":"chat.completion.chunk"}`+"\n\n")
		w.(http.Flusher).Flush()
		u.started <- struct{}{}
		<-r.Context().Done()
	case strings.Contains(s, "hold"):
		u.started <- struct{}{}
		<-r.Context().Done()
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

// A stream that already sent bytes keeps its 200; it ends with an SSE error
// event carrying the same reason and message, then closes cleanly.
func TestCutAnswer_Streamed(t *testing.T) {
	u := &cutUpstream{started: make(chan struct{}, 4)}
	e := newEnv(t, u)
	body(t, e.post("/v1/chat/completions", chat("alpha")))
	ch := sendCut(t, e.front.URL, `{"model":"alpha","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	waitStarted(t, u)
	if err := e.sched.Unload(context.Background(), "alpha", true); err != nil {
		t.Fatal(err)
	}
	a := waitCut(t, ch)
	if a.status != 200 || !strings.Contains(a.body, `"content":"t1"`) {
		t.Fatalf("stream: %d %s", a.status, a.body)
	}
	i := strings.Index(a.body, "event: error\ndata: ")
	if i < 0 {
		t.Fatalf("no error event: %q", a.body)
	}
	line := strings.TrimSpace(strings.SplitN(a.body[i+len("event: error\ndata: "):], "\n", 2)[0])
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("event data %q: %v", line, err)
	}
	ev := m["error"].(map[string]any)
	if ev["reason"] != "unloaded" || ev["message"] != "unloaded: alpha was unloaded" || ev["code"] != float64(503) {
		t.Fatalf("error event: %v", ev)
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
