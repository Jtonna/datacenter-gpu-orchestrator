package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// While a stream runs, Live shows the first-byte time and counts the data
// events; at the end the timings fill the result.
func TestForwardLiveCountsChunksAndReadsTimings(t *testing.T) {
	gate := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
		}
		w.(http.Flusher).Flush()
		<-gate
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":2460,"completion_tokens":318},"timings":{"cache_n":2000,"prompt_n":460,"prompt_ms":1116.5,"prompt_per_second":412.0,"predicted_n":318,"predicted_ms":22553.2,"predicted_per_second":14.1}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	live := &Live{}
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up), Live: live}, res)
	resp, err := http.Get(f.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for live.Chunks() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if live.Chunks() != 5 || live.FirstByte().IsZero() {
		t.Fatalf("live: %d chunks, first %v", live.Chunks(), live.FirstByte())
	}
	close(gate)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	r := <-res
	if r.InputTokens != 2460 || r.OutputTokens != 318 || r.CachedTokens != 2000 || r.PromptMS != 1116.5 ||
		r.PredictedMS != 22553.2 || r.PromptPerSecond != 412 || r.TokensPerSecond != 14.1 || r.Reason() != "" || !r.Stream {
		t.Fatalf("%+v", r)
	}
}

// Without a usage object, tokens in are prompt_n plus the reused cache_n.
func TestForwardTimingsOnlyCountReusedPromptTokens(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"timings":{"cache_n":100,"prompt_n":20,"predicted_n":5}}`)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up)}, res)
	resp, _ := http.Get(f.URL + "/x")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if r := <-res; r.InputTokens != 120 || r.CachedTokens != 100 || r.Stream {
		t.Fatalf("%+v", r)
	}
}

// A JSON (non-stream) answer never counts chunks.
func TestForwardLiveIgnoresNonStreams(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"x":"data: {"}`)
	}))
	defer up.Close()
	live := &Live{}
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up), Live: live}, res)
	resp, _ := http.Get(f.URL + "/x")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	r := <-res
	if live.Chunks() != 0 || live.FirstByte().IsZero() || r.CachedTokens != -1 || r.PromptMS != -1 {
		t.Fatalf("live %d, result %+v", live.Chunks(), r)
	}
	var nilLive *Live
	if nilLive.Chunks() != 0 || !nilLive.FirstByte().IsZero() {
		t.Fatal("nil Live")
	}
}

func TestResultReason(t *testing.T) {
	for _, c := range []struct {
		r    Result
		want string
	}{
		{Result{Status: 200}, ""},
		{Result{Status: 200, Stalled: true}, "stall"},
		{Result{Status: 200, Cut: true}, "cut"},
		{Result{Status: 502, Err: errors.New("x")}, "upstream_error"},
		{Result{Status: 499}, "client_left"},
		{Result{Status: 200, Aborted: true}, "aborted"},
		{Result{Status: 500}, ""},
	} {
		if got := c.r.Reason(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.r, got, c.want)
		}
	}
}
