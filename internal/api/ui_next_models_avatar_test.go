package api

import (
	"strconv"
	"testing"
	"time"
)

// A model's tile on the Models page (instance cards, the model's own page):
// the two letters come from the model's own name (the first two letters of
// the first word of short_name), never from its publisher, so Devstral and
// Mixtral (one publisher) read "De" and "Mi"; the family keeps its hue, and
// an unknown family takes a hue from that word, so DeepSeek's "De" differs
// in colour from Devstral's. A command profile ("A program on this
// machine") is the same small square with a prompt glyph: its class is
// av-cmd, never the "cmd" of the create-zone command field.
func TestUINextModelsAvatar(t *testing.T) {
	// olmo's family is unknown: its hue comes from "olmo", one of twelve 30 degrees apart
	x := 0
	for _, c := range "olmo" {
		x = (x*31 + int(c)) % 997
	}
	olmoHue := strconv.Itoa(15 + 30*(x%12))
	now := time.Now()
	fetch := ovFetch(now, ovRequests(now), true)
	llama := func(key, short, brand, publisher, profile string) map[string]any {
		return map[string]any{"key": key, "kind": "llama", "exists": true, "file": key, "name": short, "short_name": short, "brand": brand, "publisher": publisher,
			"size_bytes": 4000000000, "profile_count": 1, "profiles": []any{map[string]any{"id": profile, "ctx": 8192}}, "state": "ready"}
	}
	command := func(id, name string) map[string]any {
		return map[string]any{"key": "command:" + id, "kind": "command", "exists": true, "name": name, "short_name": name, "brand": "command",
			"profile_count": 1, "profiles": []any{map[string]any{"id": id, "needs_mib": 6144}}, "state": "ready"}
	}
	fetch["/api/models"] = map[string]any{"version": 1, "models": []any{
		llama("devstral-small-2505-Q4_K_M.gguf", "devstral-small-2505", "mistral", "Mistral AI", "devstral"),
		llama("mixtral-8x7b-instruct-Q4_K_M.gguf", "mixtral-8x7b-instruct", "mistral", "Mistral AI", "mixtral"),
		llama("deepseek-r1-distill-14b-Q4_K_M.gguf", "deepseek-r1-distill-14b", "deepseek", "DeepSeek", "deepseek"),
		llama("olmo-2-13b-Q4_K_M.gguf", "olmo-2-13b", "", "", "olmo"),
		llama("gemma-4-27b-it-Q4_K_M.gguf", "gemma-4-27b", "gemma", "Google", "gemma"),
		command("flux", "flux-schnell"),
		command("whisper", "whisper-large-v3"),
	}}
	st := ovStatus(now)
	inst := func(profile string) map[string]any {
		return map[string]any{"profile": profile, "state": "ready", "in_flight": 0, "parallel": 1, "nodes": []any{map[string]any{"id": ovA, "role": "host", "share": 1.0}}}
	}
	st["instances"] = []any{inst("devstral"), inst("mixtral"), inst("deepseek"), inst("olmo"), inst("gemma"), inst("flux"), inst("whisper")}
	st["queue"] = []any{}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: tiles from the model's own name", "route": "#/models", "status": st,
			"has": []string{"span.av[data-mono=De][style*='--hue:24']", "span.av[data-mono=Mi][style*='--hue:24']", "span.av[data-mono=Ge][style*='--hue:214']",
				"span.av[data-mono=De][style*='--hue:232']", "span.av[data-mono=Ol][style*='--hue:" + olmoHue + "']", "span.av.av-28.av-cmd"},
			"none": []string{"span.av.cmd", "span.av.plain"}},
		map[string]any{"name": "models: a command profile's page tile", "route": "#/models/command:flux", "status": st,
			"has": []string{"span.av.av-72.av-cmd"}, "none": []string{"span.av.cmd"}},
		map[string]any{"name": "models: a model's page tile", "route": "#/models/mixtral-8x7b-instruct-Q4_K_M.gguf", "status": st,
			"has": []string{"span.av.av-72[data-mono=Mi]"}},
	}})
}
