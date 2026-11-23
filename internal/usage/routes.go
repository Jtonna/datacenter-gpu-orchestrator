package usage

import "net/http"

// inferenceRoutes are the routes the request log records: the pinned
// llama-server's POST routes that run the model (tools/server/server.cpp at
// a01665d30de2), with and without /v1. Everything else (tokenize,
// detokenize, apply-template, the input_tokens and count_tokens counters,
// props, slots, health, metrics, models, lora-adapters, the stream lookup,
// chat completion control) passes through unrecorded.
var inferenceRoutes = map[string]bool{
	"/completion": true, "/completions": true, "/v1/completions": true,
	"/chat/completions": true, "/v1/chat/completions": true,
	"/responses": true, "/v1/responses": true,
	"/v1/messages": true,
	"/infill":      true,
	"/embedding":   true, "/embeddings": true, "/v1/embeddings": true,
	"/rerank": true, "/reranking": true, "/v1/rerank": true, "/v1/reranking": true,
	"/audio/transcriptions": true, "/v1/audio/transcriptions": true,
	"/audio/speech": true, "/v1/audio/speech": true, "/tts": true,
}

// IsInference reports whether a request with this method and path (as the
// model server sees it: /v1/... for /v1, the rest after /upstream/<model>
// for a passthrough) runs the model, and so is a row in the log.
func IsInference(method, path string) bool {
	return method == http.MethodPost && inferenceRoutes[path]
}

// InferencePaths is the list of recorded paths, for the clean-up migration.
func InferencePaths() []string {
	out := make([]string, 0, len(inferenceRoutes))
	for p := range inferenceRoutes {
		out = append(out, p)
	}
	return out
}
