package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The windows of a cut by a forced unload, each produced on purpose by the
// fake model server (cutUpstream) and, where the caller cannot see it, by the
// proxy's response hook (Options.OnResponse, called once the model server's
// status and headers have reached the proxy, just before the proxy writes
// its own status):
//
//  1. before any byte of the response: 503 JSON, reason unloaded, Retry-After;
//  2. stream: after the 200 and headers, before the first event: the 200,
//     exactly one `event: error` (reason unloaded, code 503), a clean end;
//  3. stream: after one or more events: those events, then the error event,
//     then a clean end.

// windowEnv is an env whose proxy signals on responded once the model
// server's headers arrived for a request sent after the model was loaded.
type windowEnv struct {
	*env
	u         *cutUpstream
	armed     atomic.Bool
	responded chan struct{}
}

func newWindowEnv(t *testing.T) *windowEnv {
	t.Helper()
	w := &windowEnv{u: &cutUpstream{started: make(chan struct{}, 4)}, responded: make(chan struct{}, 4)}
	w.env = newEnv(t, w.u, func(o *Options) {
		o.proxyOnResponse = func() {
			if w.armed.Load() {
				w.responded <- struct{}{}
			}
		}
	})
	body(t, w.post("/v1/chat/completions", chat("alpha"))) // loads alpha
	w.armed.Store(true)
	return w
}

func (w *windowEnv) cut(t *testing.T) {
	t.Helper()
	if err := w.sched.Unload(context.Background(), "alpha", true); err != nil {
		t.Fatal(err)
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// streamAnswer is what the app read, and how the read ended (nil: a clean
// end of the body). status -1: no answer at all (readErr says why).
type streamAnswer struct {
	status  int
	body    string
	readErr error
	err     map[string]any // the JSON error of an answer that is not a stream
	retry   string
}

// sendStream posts body in the background and reads the answer line by line;
// read is signalled once per line holding a token, after the app read it.
func sendStream(url, body string) (read <-chan struct{}, answer <-chan streamAnswer) {
	rd := make(chan struct{}, 16)
	ch := make(chan streamAnswer, 1)
	go func() {
		resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			ch <- streamAnswer{status: -1, readErr: err}
			return
		}
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		var got strings.Builder
		var rerr error
		for {
			line, err := br.ReadString('\n')
			got.WriteString(line)
			if strings.Contains(line, `"content":"t`) {
				rd <- struct{}{}
			}
			if err != nil {
				if err != io.EOF {
					rerr = err
				}
				break
			}
		}
		a := streamAnswer{status: resp.StatusCode, body: got.String(), readErr: rerr, retry: resp.Header.Get("Retry-After")}
		var m map[string]any
		if json.Unmarshal([]byte(a.body), &m) == nil {
			a.err, _ = m["error"].(map[string]any)
		}
		ch <- a
	}()
	return rd, ch
}

func waitStream(t *testing.T, ch <-chan streamAnswer) streamAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Second):
		t.Fatal("no answer in 10 s")
		return streamAnswer{}
	}
}

// errorEvent is the cut's SSE error event, exactly as the app reads it.
func errorEvent() string {
	msg, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "unloaded: alpha was unloaded", "type": "server_error", "code": 503, "reason": "unloaded"}})
	return "event: error\ndata: " + string(msg) + "\n\n"
}

func tokenEvent(tok string) string { return fmt.Sprintf(cutEvent, tok) + "\n\n" }

// Window 1, a stream: the model server sent nothing (not even its headers):
// 503 unloaded with Retry-After, as JSON, the same as a request that does not
// stream; no event (no stream was started).
func TestCutAnswer_Stream_BeforeAnyByte(t *testing.T) {
	w := newWindowEnv(t)
	_, ch := sendStream(w.front.URL, `{"model":"alpha","stream":true,"messages":[{"role":"user","content":"quiet"}]}`)
	waitSignal(t, w.u.started, "the request reaching the model server")
	select {
	case <-w.responded:
		t.Fatal("the model server's headers reached the proxy: not window 1")
	default:
	}
	w.cut(t)
	a := waitStream(t, ch)
	if a.status != 503 || a.err["reason"] != "unloaded" || a.err["message"] != "unloaded: alpha was unloaded" || a.retry == "" || a.readErr != nil {
		t.Fatalf("window 1: %d %q %v %s", a.status, a.retry, a.readErr, a.body)
	}
	if strings.Contains(a.body, "event: error") {
		t.Fatalf("an SSE event where no stream had started: %s", a.body)
	}
}

// Window 2, a stream: the model server's 200 and event-stream headers reached
// the proxy, no event yet. Not a silent, empty 200: the app reads the 200,
// exactly one error event (reason unloaded, code 503) and a clean end.
func TestCutAnswer_Stream_AfterHeadersBeforeFirstEvent(t *testing.T) {
	w := newWindowEnv(t)
	_, ch := sendStream(w.front.URL, `{"model":"alpha","stream":true,"messages":[{"role":"user","content":"headers"}]}`)
	waitSignal(t, w.u.started, "the model server sending its headers")
	waitSignal(t, w.responded, "the headers reaching the proxy")
	w.cut(t)
	a := waitStream(t, ch)
	if a.status != 200 || a.readErr != nil || a.body != errorEvent() {
		t.Fatalf("window 2: status %d, read error %v, body %q; want 200, a clean end and exactly %q", a.status, a.readErr, a.body, errorEvent())
	}
}

// Window 3, a stream: two events reached the app. It reads them, then the
// error event, then a clean end.
func TestCutAnswer_Stream_AfterEvents(t *testing.T) {
	w := newWindowEnv(t)
	read, ch := sendStream(w.front.URL, `{"model":"alpha","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	waitSignal(t, w.u.started, "the model server sending two events")
	waitSignal(t, read, "the app reading t1")
	waitSignal(t, read, "the app reading t2")
	w.cut(t)
	a := waitStream(t, ch)
	if want := tokenEvent("t1") + tokenEvent("t2") + errorEvent(); a.status != 200 || a.readErr != nil || a.body != want {
		t.Fatalf("window 3: status %d, read error %v, body %q; want 200, a clean end and %q", a.status, a.readErr, a.body, want)
	}
}

// A body that is not an event stream has no way to say "unloaded" once its
// status is out. What the app gets, pinned (window 1, nothing sent, is
// TestCutAnswer_ForcedUnload: 503 JSON). llama-server never opens these two
// windows for a non-streamed answer: it sends the whole JSON in one write
// once the answer is complete.
//
// After the model server's 200 and headers, before a byte of the body: the
// proxy has written the 200 (ModifyResponse ran, then WriteHeader), the copy
// of the body fails and the connection is aborted (httputil.ReverseProxy
// panics http.ErrAbortHandler): the app gets the 200 and its headers, no
// byte of the 100 promised, and a broken read (unexpected EOF).
func TestCutAnswer_NonStream_AfterHeaders(t *testing.T) {
	w := newWindowEnv(t)
	_, ch := sendStream(w.front.URL, `{"model":"alpha","messages":[{"role":"user","content":"jsonhead"}]}`)
	waitSignal(t, w.u.started, "the model server sending its headers")
	waitSignal(t, w.responded, "the headers reaching the proxy")
	w.cut(t)
	a := waitStream(t, ch)
	if a.status != 200 || a.readErr == nil || a.body != "" {
		t.Fatalf("non-stream after headers: status %d, read error %v, body %q; want 200, no byte, a broken read", a.status, a.readErr, a.body)
	}
}

// After part of the body: the 200, those bytes, then a broken read (the
// same abort; the Content-Length is never reached).
func TestCutAnswer_NonStream_AfterSomeBytes(t *testing.T) {
	w := newWindowEnv(t)
	_, ch := sendStream(w.front.URL, `{"model":"alpha","messages":[{"role":"user","content":"jsonpart"}]}`)
	waitSignal(t, w.u.started, "the model server sending part of its body")
	waitSignal(t, w.responded, "the headers reaching the proxy")
	w.cut(t)
	a := waitStream(t, ch)
	if a.status != 200 || a.readErr == nil || a.body != `{"choices":[{"message` {
		t.Fatalf("non-stream after some bytes: status %d, read error %v, body %q; want 200, the bytes, a broken read", a.status, a.readErr, a.body)
	}
}
