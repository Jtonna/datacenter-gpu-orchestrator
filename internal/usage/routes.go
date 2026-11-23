package usage

import "net/http"

// inferenceRoutes is the allow-list of the request log: the only paths
// recorded, POST only, on /v1 and on /upstream (the path after
// /upstream/<model>), and the paths the clean-up migration keeps. They are
// the pinned llama-server's POST routes that run the model
// (tools/server/server.cpp at a01665d30de2), with and without /v1, plus
// the routes a command profile may serve behind /v1 (audio translation,
// image generation). A path not on the list is never a row: tokenize,
// detokenize, apply-template, the input_tokens and count_tokens counters,
// props, slots, health, metrics, models, lora-adapters, the stream lookup,
// chat completion control.
var inferenceRoutes = map[string]bool{
	"/completion": true, "/completions": true, "/v1/completions": true,
	"/chat/completions": true, "/v1/chat/completions": true,
	"/responses": true, "/v1/responses": true,
	"/v1/messages": true,
	"/infill":      true,
	"/embedding":   true, "/embeddings": true, "/v1/embeddings": true,
	"/rerank": true, "/reranking": true, "/v1/rerank": true, "/v1/reranking": true,
	"/audio/transcriptions": true, "/v1/audio/transcriptions": true,
	"/audio/translations": true, "/v1/audio/translations": true,
	"/audio/speech": true, "/v1/audio/speech": true, "/tts": true,
	"/images/generations": true, "/v1/images/generations": true,
}

// IsInference reports whether a request with this method and path (as the
// model server sees it: /v1/... for /v1, the rest after /upstream/<model>
// for a passthrough) runs the model, and so is a row in the log.
func IsInference(method, path string) bool {
	return method == http.MethodPost && inferenceRoutes[path]
}
