package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

type server struct {
	cfg        config
	ready      atomic.Bool
	chunks     int           // content chunks per streamed reply
	chunkDelay time.Duration // pause between chunks
	stallAfter int           // stop sending after this many chunks; -1 = never
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /props", s.props)
	mux.HandleFunc("GET /slots", s.slots)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("POST /v1/completions", s.completions)
	mux.HandleFunc("POST /v1/embeddings", s.embeddings)
	return s.requireReady(mux)
}

// requireReady mirrors llama-server's middleware: until the model is loaded,
// every endpoint answers 503 "Loading model" (tools/server/server-http.cpp).
func (s *server) requireReady(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			writeJSON(w, 503, map[string]any{"error": map[string]any{
				"message": "Loading model", "type": "unavailable_error", "code": 503,
			}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *server) slotCtx() int { return s.cfg.ctxSize / s.cfg.parallel }

func (s *server) modelName() string {
	if s.cfg.model == "" {
		return "fake-model"
	}
	return path.Base(strings.ReplaceAll(s.cfg.model, `\`, "/"))
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok"})
}

func (s *server) props(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"default_generation_settings": map[string]any{
			"id": 0, "n_ctx": s.slotCtx(), "is_processing": false, "params": map[string]any{},
		},
		"n_ctx":          s.slotCtx(),
		"total_slots":    s.cfg.parallel,
		"model_path":     s.cfg.model,
		"modalities":     map[string]any{"vision": false, "audio": false},
		"chat_template":  "",
		"build_info":     "fakellama",
		"endpoint_slots": true,
	})
}

func (s *server) slots(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, s.cfg.parallel)
	for i := range out {
		out[i] = map[string]any{"id": i, "n_ctx": s.slotCtx(), "is_processing": false, "id_task": -1}
	}
	writeJSON(w, 200, out)
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{
		"id": s.modelName(), "object": "model", "created": time.Now().Unix(), "owned_by": "llamacpp",
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": []any{entry}})
}

func (s *server) embeddings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"object": "list", "model": s.modelName(),
		"data":  []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}},
		"usage": map[string]any{"prompt_tokens": 2, "total_tokens": 2},
	})
}

func (s *server) completions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"object": "text_completion", "model": s.modelName(), "created": time.Now().Unix(),
		"choices": []any{map[string]any{"index": 0, "text": s.reply(), "finish_reason": "stop"}},
		"usage":   s.usage(),
		"timings": s.timings(),
	})
}

func (s *server) reply() string {
	var b strings.Builder
	for i := 0; i < s.chunks; i++ {
		b.WriteString(chunkText(i))
	}
	return b.String()
}

func chunkText(i int) string { return fmt.Sprintf("tok%d ", i) }

func (s *server) usage() map[string]any {
	return map[string]any{"prompt_tokens": 10, "completion_tokens": s.chunks, "total_tokens": 10 + s.chunks}
}

func (s *server) timings() map[string]any {
	return map[string]any{
		"prompt_n": 10, "prompt_ms": 5.0, "predicted_n": s.chunks, "predicted_ms": 10.0 * float64(s.chunks),
		"predicted_per_second": 100.0, "prompt_per_second": 2000.0, "cache_n": 0,
	}
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool `json:"stream"`
	}
	json.NewDecoder(r.Body).Decode(&req) // an empty or odd body just means non-streaming

	if !req.Stream {
		writeJSON(w, 200, map[string]any{
			"object": "chat.completion", "model": s.modelName(), "created": time.Now().Unix(),
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": s.reply()},
			}},
			"usage":   s.usage(),
			"timings": s.timings(),
		})
		return
	}
	s.streamChat(w, r)
}

func (s *server) streamChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)

	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"object": "chat.completion.chunk", "model": s.modelName(), "created": time.Now().Unix(),
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}

	for i := 0; i < s.chunks; i++ {
		if i == s.stallAfter {
			<-r.Context().Done() // stall until the client gives up
			return
		}
		if i > 0 && s.chunkDelay > 0 {
			select {
			case <-time.After(s.chunkDelay):
			case <-r.Context().Done():
				return
			}
		}
		send(chunk(map[string]any{"content": chunkText(i)}, nil))
	}
	if s.stallAfter == s.chunks {
		// stall point just after the last content chunk: hold back the final one
		<-r.Context().Done()
		return
	}
	final := chunk(map[string]any{}, "stop")
	final["usage"] = s.usage()
	final["timings"] = s.timings()
	send(final)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
