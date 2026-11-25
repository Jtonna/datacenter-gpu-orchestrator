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
		// The native /completion route has no usage object: tokens_in is the
		// whole prompt, prompt_n + cache_n, as usage.prompt_tokens would be.
		{"native, first turn", `{"content":"x","timings":{"prompt_n":1984,"cache_n":0,"predicted_n":30}}`, 1984, 30, -1},
		{"native, later turn with a large cache", `{"content":"x","timings":{"cache_n":5982,"prompt_n":1144,"predicted_n":30}}`, 7126, 30, -1},
		{"openai with timings: usage wins, unchanged", `{"usage":{"prompt_tokens":7126,"completion_tokens":30},"timings":{"prompt_n":1144,"cache_n":5982,"predicted_n":30}}`, 7126, 30, -1},
		{"neither", `{"content":"x"}`, -1, -1, -1},
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

// No client credential header reaches the model server, for a server on the
// scheduler's machine (no launch key) and one hosted on a console (launch
// key), streamed and not: each header in ClientCredentialHeaders is dropped,
// and a hosted server gets exactly Potluck's launch key, never the client's
// value in its place or beside it.
func TestForward_NoClientCredentialHeaderReachesTheModelServer(t *testing.T) {
	type seen struct{ h http.Header }
	got := make(chan seen, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Clone()}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer up.Close()
	const marked = "sk-test-CANARY-0001"
	for _, launchKey := range []string{"", "launch-key-test-0001"} {
		f := front(t, Options{Addr: upstreamAddr(up), APIKey: launchKey}, nil)
		for _, stream := range []bool{false, true} {
			for _, name := range ClientCredentialHeaders {
				body := `{"model":"m"}`
				if stream {
					body = `{"model":"m","stream":true}`
				}
				req, _ := http.NewRequest("POST", f.URL+"/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Add(name, "Bearer "+marked)
				req.Header.Add(name, marked+"-second")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.ReadAll(resp.Body)
				resp.Body.Close()
				h := (<-got).h
				for k, vs := range h {
					if strings.Contains(strings.Join(vs, " "), marked) {
						t.Errorf("launch key %q, stream %v: %s reached the model server as %s: %q", launchKey, stream, name, k, vs)
					}
				}
				want := []string(nil)
				if launchKey != "" {
					want = []string{"Bearer " + launchKey}
				}
				if fmt.Sprint(h.Values("Authorization")) != fmt.Sprint(want) {
					t.Errorf("launch key %q, stream %v, client %s: the model server got Authorization %q", launchKey, stream, name, h.Values("Authorization"))
				}
				if h.Get("Content-Type") != "application/json" {
					t.Errorf("other headers must still pass: %v", h)
				}
			}
		}
	}
}

// What does leave for the model server, pinned. Besides the request itself,
// headers a person might take for identifying are passed on as the client
// sent them, except: the credential headers (ClientCredentialHeaders),
// Potluck's own X-Actor, X-Comment and X-Source (PotluckHeaders), the
// hop-by-hop headers, and the client's own Forwarded and X-Forwarded-*,
// which the proxy replaces with its own X-Forwarded-For (the address of the
// TCP connection the client came from), X-Forwarded-Host (the Host the
// client used) and X-Forwarded-Proto. If this list changes, update the
// release note and the runbook ("What a model server sees").
func TestForward_WhatTheModelServerSeesIsPinned(t *testing.T) {
	passed := []string{"User-Agent", "Origin", "Referer", "X-Real-Ip", "Via", "X-Client-Ip", "True-Client-Ip", "Cf-Connecting-Ip",
		"X-Request-Id", "Accept-Language", "Accept", "Content-Type", "X-Some-Custom-Header"}
	dropped := append(append([]string{"Forwarded", "Connection", "Te", "Upgrade", "Proxy-Connection", "Keep-Alive"}, ClientCredentialHeaders...), PotluckHeaders...)
	replaced := []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}
	got := make(chan http.Header, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Header.Clone(); fmt.Fprint(w, "{}") }))
	defer up.Close()
	for _, launchKey := range []string{"", "launch-key-test-0002"} {
		f := front(t, Options{Addr: upstreamAddr(up), APIKey: launchKey}, nil)
		req, _ := http.NewRequest("POST", f.URL+"/v1/chat/completions", strings.NewReader("{}"))
		for _, set := range [][]string{passed, dropped, replaced} {
			for _, h := range set {
				req.Header.Set(h, "client-"+h)
			}
		}
		req.Header.Set("Connection", "keep-alive") // a real hop-by-hop value
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		h := <-got
		for _, k := range passed {
			if h.Get(k) != "client-"+k {
				t.Errorf("launch key %q: %s should pass as sent, got %q", launchKey, k, h.Get(k))
			}
		}
		for _, k := range dropped {
			want := ""
			if k == "Authorization" && launchKey != "" {
				want = "Bearer " + launchKey // Potluck's own, not the client's
			}
			if v := h.Get(k); v != want {
				t.Errorf("launch key %q: %s should not pass, got %q", launchKey, k, v)
			}
		}
		for _, k := range replaced {
			if v := h.Get(k); v == "" || strings.Contains(v, "client-") {
				t.Errorf("launch key %q: %s should be the proxy's own, got %q", launchKey, k, v)
			}
		}
		if h.Get("X-Forwarded-For") != "127.0.0.1" || h.Get("X-Forwarded-Proto") != "http" || !strings.HasPrefix(h.Get("X-Forwarded-Host"), "127.0.0.1:") {
			t.Errorf("launch key %q: X-Forwarded-*: %q %q %q", launchKey, h.Get("X-Forwarded-For"), h.Get("X-Forwarded-Host"), h.Get("X-Forwarded-Proto"))
		}
		// nothing else arrives: the passed, the replaced, the body's own
		// length and encoding, and the launch key
		allowed := map[string]bool{"Content-Length": true, "Accept-Encoding": true}
		for _, k := range append(append([]string{}, passed...), replaced...) {
			allowed[http.CanonicalHeaderKey(k)] = true
		}
		if launchKey != "" {
			allowed["Authorization"] = true
		}
		for k := range h {
			if !allowed[k] {
				t.Errorf("launch key %q: unexpected header %s = %q reached the model server", launchKey, k, h.Values(k))
			}
		}
	}
}

// Potluck's own headers (X-Actor, X-Comment, X-Source) stay on the request
// Potluck handles, so the audit and the request log still read them; only
// the copy sent to the model server goes without them.
func TestForward_PotluckHeadersStayForPotluck(t *testing.T) {
	got := make(chan http.Header, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Header.Clone(); fmt.Fprint(w, "{}") }))
	defer up.Close()
	after := make(chan http.Header, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Forward(w, r, Options{Addr: upstreamAddr(up)})
		after <- r.Header.Clone() // what Potluck's own code reads after forwarding
	}))
	defer s.Close()
	req, _ := http.NewRequest("POST", s.URL+"/v1/chat/completions", strings.NewReader("{}"))
	for _, h := range PotluckHeaders {
		req.Header.Set(h, "set-"+h)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	up1, in := <-got, <-after
	for _, h := range PotluckHeaders {
		if up1.Get(h) != "" {
			t.Errorf("%s reached the model server", h)
		}
		if in.Get(h) != "set-"+h {
			t.Errorf("%s is gone from the request Potluck reads: %q", h, in.Get(h))
		}
	}
}

// A native /completion answer (timings, no usage object) on a later turn:
// the result's prompt tokens are the whole prompt (prompt_n + cache_n) and
// its cached tokens are cache_n, as for an OpenAI-style answer.
func TestForward_NativeAnswerWithoutUsageCountsTheWholePrompt(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"content":"ok","stop":true,"timings":{"prompt_n":1144,"cache_n":5982,"predicted_n":30,"predicted_per_second":50}}`)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up)}, res)
	resp, err := http.Post(f.URL+"/completion", "application/json", strings.NewReader(`{"prompt":"x","cache_prompt":true}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	r := <-res
	if r.InputTokens != 7126 || r.CachedTokens != 5982 || r.OutputTokens != 30 {
		t.Fatalf("in %d cached %d out %d, want 7126 5982 30", r.InputTokens, r.CachedTokens, r.OutputTokens)
	}
}
