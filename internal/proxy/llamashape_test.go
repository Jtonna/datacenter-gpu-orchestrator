package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The response shapes of the pinned llama-server (deploy/rpc-build.Dockerfile,
// LLAMA_COMMIT a01665d30de2), as its source builds them. They are not live
// captures: the objects are server_task_result_cmpl_final::to_json_oaicompat_chat
// and ..._chat_stream, usage_json_oaicompat, server_slot_stats::to_json and
// result_prompt_progress::to_json in tools/server/server-task.cpp and
// server-common.cpp, with nlohmann::json's key order (sorted). The token
// figures are those of that commit's own test_chat_completion_cached_tokens:
// a 77-token prompt sent twice, the second time with 76 tokens reused
// (cache_n is n_past, the reused prefix; prompt_n the tokens read).
//
// What the source settles:
//   - usage.prompt_tokens is slot.task->n_tokens(): every prompt token,
//     reused ones included; prompt_tokens_details.cached_tokens is the reused
//     ones. So tokens_in = prompt_tokens and tokens_reused = cached_tokens.
//   - timings.prompt_n counts only the tokens read; cache_n the reused ones.
//     Without a usage object (a stream without stream_options.include_usage,
//     or the native /completion), tokens_in = prompt_n + cache_n.
//   - A stream carries timings only on its last event, unless the request
//     sets "timings_per_token"; prompt progress only with "return_progress".
const (
	llamaChatFinal = `{"choices":[{"finish_reason":"length","index":0,"message":{"content":"Suddenly","role":"assistant"}}],` +
		`"created":1795000000,"id":"chatcmpl-x","model":"m","object":"chat.completion","system_fingerprint":"b1-a01665d",` +
		`"timings":{"cache_n":76,"predicted_ms":61.2,"predicted_n":8,"predicted_per_second":130.7,"predicted_per_token_ms":7.65,` +
		`"prompt_ms":9.1,"prompt_n":1,"prompt_per_second":109.9,"prompt_per_token_ms":9.1},` +
		`"usage":{"completion_tokens":8,"prompt_tokens":77,"prompt_tokens_details":{"cached_tokens":76},"total_tokens":85}}`

	llamaChunk = `data: {"choices":[{"delta":{"content":"%s"},"finish_reason":null,"index":0}],"created":1795000000,` +
		`"id":"chatcmpl-x","model":"m","object":"chat.completion.chunk","system_fingerprint":"b1-a01665d"}` + "\n\n"

	// The last event of a stream without include_usage: timings ride on the
	// finish event.
	llamaStreamEnd = `data: {"choices":[{"delta":{},"finish_reason":"length","index":0}],"created":1795000000,"id":"chatcmpl-x",` +
		`"model":"m","object":"chat.completion.chunk","system_fingerprint":"b1-a01665d",` +
		`"timings":{"cache_n":76,"predicted_ms":61.2,"predicted_n":8,"predicted_per_second":130.7,"predicted_per_token_ms":7.65,` +
		`"prompt_ms":9.1,"prompt_n":1,"prompt_per_second":109.9,"prompt_per_token_ms":9.1}}` + "\n\ndata: [DONE]\n\n"

	// With stream_options.include_usage: an event with empty choices carries
	// usage and the timings.
	llamaStreamUsage = `data: {"choices":[{"delta":{},"finish_reason":"length","index":0}],"created":1795000000,"id":"chatcmpl-x",` +
		`"model":"m","object":"chat.completion.chunk","system_fingerprint":"b1-a01665d"}` + "\n\n" +
		`data: {"choices":[],"created":1795000000,"id":"chatcmpl-x","model":"m","object":"chat.completion.chunk",` +
		`"system_fingerprint":"b1-a01665d","timings":{"cache_n":76,"predicted_ms":61.2,"predicted_n":8,"predicted_per_second":130.7,` +
		`"predicted_per_token_ms":7.65,"prompt_ms":9.1,"prompt_n":1,"prompt_per_second":109.9,"prompt_per_token_ms":9.1},` +
		`"usage":{"completion_tokens":8,"prompt_tokens":77,"prompt_tokens_details":{"cached_tokens":76},"total_tokens":85}}` +
		"\n\ndata: [DONE]\n\n"

	// A return_progress event while the prompt is read: a role-only delta
	// with prompt_progress (processed counts the reused tokens).
	llamaProgress = `data: {"choices":[{"delta":{"content":null,"role":"assistant"},"finish_reason":null,"index":0}],` +
		`"created":1795000000,"id":"chatcmpl-x","model":"m","object":"chat.completion.chunk",` +
		`"prompt_progress":{"cache":%d,"processed":%d,"time_ms":%d,"total":%d},"system_fingerprint":"b1-a01665d"}` + "\n\n"
)

func forwardBody(t *testing.T, contentType string, write func(w http.ResponseWriter), live *Live) Result {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		write(w)
	}))
	defer up.Close()
	res := make(chan Result, 1)
	f := front(t, Options{Addr: upstreamAddr(up), Live: live}, res)
	resp, err := http.Post(f.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return <-res
}

// tokens_in is every prompt token, reused ones included, read the same way
// from each shape the pinned server sends.
func TestLlamaShapes_PromptTokensIncludeTheReusedOnes(t *testing.T) {
	for _, c := range []struct {
		name, ctype string
		body        string
	}{
		{"json answer with usage", "application/json", llamaChatFinal},
		{"stream, timings only", "text/event-stream", fmt.Sprintf(llamaChunk, "Sud") + llamaStreamEnd},
		{"stream with include_usage", "text/event-stream", fmt.Sprintf(llamaChunk, "Sud") + llamaStreamUsage},
	} {
		r := forwardBody(t, c.ctype, func(w http.ResponseWriter) { fmt.Fprint(w, c.body) }, nil)
		if r.InputTokens != 77 || r.CachedTokens != 76 || r.OutputTokens != 8 || r.PromptPerSecond != 109.9 ||
			r.PromptMS != 9.1 || r.PredictedMS != 61.2 || r.TokensPerSecond != 130.7 {
			t.Errorf("%s: in %d reused %d out %d prefill %v/%vms decode %v/%vms", c.name, r.InputTokens, r.CachedTokens,
				r.OutputTokens, r.PromptPerSecond, r.PromptMS, r.TokensPerSecond, r.PredictedMS)
		}
	}
}

// With return_progress the stream says the prompt while it is read: Live
// has the total, the reused part and the progress; the progress events are
// neither output tokens nor the first token.
func TestLlamaShapes_PromptProgressIsLiveAndNotATokenOrTheFirstToken(t *testing.T) {
	gate, mid := make(chan struct{}), make(chan struct{})
	live := &Live{}
	done := make(chan Result, 1)
	go func() {
		done <- forwardBody(t, "text/event-stream", func(w http.ResponseWriter) {
			fmt.Fprintf(w, llamaProgress, 2000, 2000, 0, 6000)
			fmt.Fprintf(w, llamaProgress, 2000, 4048, 500, 6000)
			w.(http.Flusher).Flush()
			close(mid)
			<-gate
			fmt.Fprintf(w, llamaProgress, 2000, 6000, 1000, 6000)
			fmt.Fprintf(w, llamaChunk, "a")
			fmt.Fprintf(w, llamaChunk, "b")
			fmt.Fprint(w, strings.ReplaceAll(strings.ReplaceAll(llamaStreamEnd, `"cache_n":76`, `"cache_n":2000`), `"prompt_n":1,`, `"prompt_n":4000,`))
		}, live)
	}()
	<-mid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, p, _, ok := live.Prefill(); ok && p == 4048 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	total, reused, processed, tps, ok := live.Prefill()
	if !ok || total != 6000 || reused != 2000 || processed != 4048 || tps != 4096 {
		t.Fatalf("mid-prompt: %d %d %d %v %v", total, reused, processed, tps, ok)
	}
	if live.Chunks() != 0 || !live.FirstByte().IsZero() {
		t.Fatalf("progress counted as a token: %d chunks, first %v", live.Chunks(), live.FirstByte())
	}
	close(gate)
	r := <-done
	if live.Chunks() != 3 || live.FirstByte().IsZero() { // two content events and the finish event
		t.Fatalf("after: %d chunks, first %v", live.Chunks(), live.FirstByte())
	}
	if total, reused, processed, _, _ := live.Prefill(); total != 6000 || reused != 2000 || processed != 6000 {
		t.Fatalf("end: %d %d %d", total, reused, processed)
	}
	if r.InputTokens != 6000 || r.CachedTokens != 2000 {
		t.Fatalf("%+v", r)
	}
}

// With timings_per_token every event carries timings: the prompt is known
// from the first token on. A plain stream says nothing until its end.
func TestLlamaShapes_TimingsPerTokenGiveThePromptFromTheFirstToken(t *testing.T) {
	gate, mid := make(chan struct{}), make(chan struct{})
	live := &Live{}
	done := make(chan Result, 1)
	go func() {
		done <- forwardBody(t, "text/event-stream", func(w http.ResponseWriter) {
			fmt.Fprint(w, strings.Replace(fmt.Sprintf(llamaChunk, "a"), `"system_fingerprint":"b1-a01665d"}`,
				`"system_fingerprint":"b1-a01665d","timings":{"cache_n":2000,"predicted_ms":0,"predicted_n":1,"prompt_ms":1116.5,"prompt_n":460,"prompt_per_second":412.0}}`, 1))
			w.(http.Flusher).Flush()
			close(mid)
			<-gate
			fmt.Fprint(w, llamaStreamEnd)
		}, live)
	}()
	<-mid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, _, _, ok := live.Prefill(); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	total, reused, processed, tps, ok := live.Prefill()
	if !ok || total != 2460 || reused != 2000 || processed != 2460 || tps != 412 {
		t.Fatalf("%d %d %d %v %v", total, reused, processed, tps, ok)
	}
	close(gate)
	<-done

	plain := &Live{}
	forwardBody(t, "text/event-stream", func(w http.ResponseWriter) {
		fmt.Fprintf(w, llamaChunk, "a")
		w.(http.Flusher).Flush()
		if _, _, _, _, ok := plain.Prefill(); ok {
			t.Error("a plain stream's first token said the prompt")
		}
		fmt.Fprint(w, llamaStreamEnd)
	}, plain)
}

// A live capture from the pinned build (non-streamed chat, the same prompt
// sent twice): the first answer reads all 32 prompt tokens, the repeat
// reuses 28 and reads 4. usage.prompt_tokens stays 32 both times, so
// tokens_in is every prompt token with the reused ones counted, and
// tokens_reused is cached_tokens = cache_n. Without usage, prompt_n +
// cache_n gives the same 32. (Only the fields the capture kept: the
// choice text and the rates are left out.)
func TestLlamaShapes_LiveCaptureFirstAndRepeat(t *testing.T) {
	answer := func(cached, promptN, cacheN int, withUsage bool) string {
		b := `{"choices":[{"finish_reason":"length","index":0,"message":{"content":"x","role":"assistant"}}],"object":"chat.completion",` +
			fmt.Sprintf(`"timings":{"cache_n":%d,"predicted_n":160,"prompt_n":%d}`, cacheN, promptN)
		if withUsage {
			b += fmt.Sprintf(`,"usage":{"completion_tokens":160,"prompt_tokens":32,"prompt_tokens_details":{"cached_tokens":%d},"total_tokens":192}`, cached)
		}
		return b + "}"
	}
	for _, c := range []struct {
		name                    string
		cached, promptN, cacheN int
	}{
		{"first", 0, 32, 0},
		{"repeat", 28, 4, 28},
	} {
		for _, withUsage := range []bool{true, false} {
			body := answer(c.cached, c.promptN, c.cacheN, withUsage)
			r := forwardBody(t, "application/json", func(w http.ResponseWriter) { fmt.Fprint(w, body) }, nil)
			if r.InputTokens != 32 || r.CachedTokens != c.cached || r.OutputTokens != 160 {
				t.Errorf("%s (usage %v): in %d reused %d out %d", c.name, withUsage, r.InputTokens, r.CachedTokens, r.OutputTokens)
			}
		}
	}
}
