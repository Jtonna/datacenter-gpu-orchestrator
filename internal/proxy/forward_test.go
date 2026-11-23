package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func upstreamAddr(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

// front serves Forward on a real listener so that flushing and client
// cancellation behave as they do in production.
func front(t *testing.T, o Options, res chan<- Result) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := Forward(w, r, o)
		if res != nil {
			res <- got
		}
		if got.Aborted {
			panic(http.ErrAbortHandler)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestForwardStreamsChunksImmediately(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: two\n\n")
	}))
	defer up.Close()
	f := front(t, Options{Addr: upstreamAddr(up)}, nil)
	resp, err := http.Get(f.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("first chunk = %q, %v (must arrive while upstream is still open)", line, err)
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestForwardStallAfterFirstChunkSendsErrorEventAndEnds(t *testing.T) {
	upDone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // hold until the proxy gives up on us
		close(upDone)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up), StallTimeout: 150 * time.Millisecond}, res)
	start := time.Now()
	resp, err := http.Get(f.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "data: one\n\n") {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(string(body), "event: error") || !strings.Contains(string(body), "stall") {
		t.Fatalf("no SSE error event in %q", body)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 3*time.Second {
		t.Fatalf("stall detected after %v", d)
	}
	if r := <-res; !r.Stalled {
		t.Fatalf("result = %+v, want Stalled", r)
	}
	select {
	case <-upDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not cancelled after the stall")
	}
}

func TestForwardNoStallBeforeFirstChunk(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond) // prompt processing: never timed
		fmt.Fprint(w, "data: one\n\n")
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up), StallTimeout: 100 * time.Millisecond}, res)
	resp, err := http.Get(f.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "data: one\n\n" {
		t.Fatalf("body = %q", body)
	}
	if r := <-res; r.Stalled {
		t.Fatal("stalled before the first chunk")
	}
}

func TestForwardStallDisabledWhenZeroAndForNonStreaming(t *testing.T) {
	for _, tc := range []struct {
		name, ct string
		stall    time.Duration
	}{
		{"zero timeout", "text/event-stream", 0},
		{"json response", "application/json", 80 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				fmt.Fprint(w, "first")
				w.(http.Flusher).Flush()
				time.Sleep(300 * time.Millisecond)
				fmt.Fprint(w, "second")
			}))
			defer up.Close()
			f := front(t, Options{Addr: upstreamAddr(up), StallTimeout: tc.stall}, nil)
			resp, err := http.Get(f.URL + "/x")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if string(body) != "firstsecond" {
				t.Fatalf("body = %q", body)
			}
		})
	}
}

func TestForwardClientCancelPropagatesUpstream(t *testing.T) {
	var cancelled atomic.Bool
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		cancelled.Store(true)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up)}, res)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", f.URL+"/x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	resp.Body.Close()
	select {
	case <-res:
	case <-time.After(3 * time.Second):
		t.Fatal("Forward did not return after the client left")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !cancelled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("upstream was not told the client left")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestForwardPathPrefixBodyAndQuery(t *testing.T) {
	var gotPath, gotQuery, gotBody, gotHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotHost = r.URL.EscapedPath(), r.URL.RawQuery, r.Host
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(201)
	}))
	defer up.Close()
	f := front(t, Options{Addr: upstreamAddr(up), Path: "/props", RawPath: "/props", Body: io.NopCloser(strings.NewReader("hello"))}, nil)
	resp, err := http.Post(f.URL+"/upstream/gemma%3A64k/props?x=1", "text/plain", strings.NewReader("world"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || gotPath != "/props" || gotQuery != "x=1" || gotBody != "hello" || gotHost != upstreamAddr(up) {
		t.Fatalf("status %d path %q query %q body %q host %q", resp.StatusCode, gotPath, gotQuery, gotBody, gotHost)
	}
}

func TestForwardUpstreamDownIs502OpenAIError(t *testing.T) {
	up := httptest.NewServer(nil)
	addr := upstreamAddr(up)
	up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: addr}, res)
	resp, err := http.Get(f.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 502 || !strings.Contains(string(b), `"error"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	if r := <-res; r.Status != 502 {
		t.Fatalf("result = %+v", r)
	}
}

func TestForwardTokensFromJSONBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"say \"usage\": {\"prompt_tokens\": 1}"}}],"usage":{"prompt_tokens":50,"completion_tokens":100,"total_tokens":150},"timings":{"predicted_per_second":42.5}}`)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up)}, res)
	resp, _ := http.Get(f.URL + "/x")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	r := <-res
	if r.InputTokens != 50 || r.OutputTokens != 100 || r.TokensPerSecond != 42.5 {
		t.Fatalf("result = %+v", r)
	}
}

func TestForwardTokensFromLargeJSONBodyTail(t *testing.T) {
	pad := strings.Repeat("x", 100<<10)
	body := `{"content":"` + pad + `","usage":{"prompt_tokens":7,"completion_tokens":9},"timings":{"predicted_per_second":3.5}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up)}, res)
	resp, _ := http.Get(f.URL + "/x")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != body {
		t.Fatal("forwarded body was altered")
	}
	r := <-res
	if r.InputTokens != 7 || r.OutputTokens != 9 || r.TokensPerSecond != 3.5 {
		t.Fatalf("result = %+v", r)
	}
}

func TestForwardTokensFromSSEFinalChunks(t *testing.T) {
	var sent strings.Builder
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 600; i++ { // well over 16 KB of content chunks
			s := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			sent.WriteString(s)
			fmt.Fprint(w, s)
		}
		s := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22},\"timings\":{\"predicted_per_second\":99.25}}\n\ndata: [DONE]\n\n"
		sent.WriteString(s)
		fmt.Fprint(w, s)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up)}, res)
	resp, _ := http.Get(f.URL + "/x")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != sent.String() {
		t.Fatal("forwarded stream was altered")
	}
	r := <-res
	if r.InputTokens != 11 || r.OutputTokens != 22 || r.TokensPerSecond != 99.25 {
		t.Fatalf("result = %+v", r)
	}
}

func TestForwardTokensAbsentAreMinusOne(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up)}, res)
	resp, _ := http.Get(f.URL + "/x")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	r := <-res
	if r.InputTokens != -1 || r.OutputTokens != -1 || r.TokensPerSecond != -1 {
		t.Fatalf("result = %+v", r)
	}
}

func TestExtractUsageUnit(t *testing.T) {
	cases := []struct {
		name, tail string
		in, out    int
		tps        float64
	}{
		{"openai", `{"usage":{"prompt_tokens":1,"completion_tokens":2}}`, 1, 2, -1},
		{"responses api names", `{"usage":{"input_tokens":3,"output_tokens":4}}`, 3, 4, -1},
		{"timings only", `{"timings":{"prompt_n":5,"predicted_n":6,"predicted_per_second":7.5}}`, 5, 6, 7.5},
		{"truncated front", `ntent":"zzz","usage":{"prompt_tokens":8,"completion_tokens":9}}`, 8, 9, -1},
		{"null usage", `data: {"usage":null}`, -1, -1, -1},
		{"empty", ``, -1, -1, -1},
	}
	for _, c := range cases {
		in, out, tps := extractUsage([]byte(c.tail))
		if in != c.in || out != c.out || tps != c.tps {
			t.Errorf("%s: got %d %d %v, want %d %d %v", c.name, in, out, tps, c.in, c.out, c.tps)
		}
	}
}

// A client that closes its connection in the middle of a stream is a
// client that left, not an aborted answer; Live keeps the events counted
// so far. A model server that dies in the middle of a stream is not.
func TestForward_ClientLeavingMidStreamIsClientLeft(t *testing.T) {
	sent := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
		}
		w.(http.Flusher).Flush()
		close(sent)
		for i := 0; ; i++ { // keeps streaming until the proxy hangs up
			if _, err := fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"u%d\"}}]}\n\n", i); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	defer up.Close()
	live := &Live{}
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up), Live: live}, res)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", f.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-sent
	bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	r := <-res
	if !r.ClientLeft || r.Reason() != "client_left" || r.Status != 200 || !r.Stream || live.Chunks() < 5 {
		t.Fatalf("%+v reason %q chunks %d", r, r.Reason(), live.Chunks())
	}

	// The model server drops the connection mid-stream: aborted, the client
	// is still there.
	dies := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := http.NewResponseController(w).Hijack()
		conn.Close()
	}))
	defer dies.Close()
	live = &Live{}
	f = front(t, Options{Addr: upstreamAddr(dies), Live: live}, res)
	resp, err = http.Post(f.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	r = <-res
	if r.ClientLeft || r.Reason() != "aborted" {
		t.Fatalf("%+v reason %q", r, r.Reason())
	}
}
