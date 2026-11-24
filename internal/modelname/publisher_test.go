package modelname

import "testing"

// thirteen is a catalog of thirteen common model files: two carry a
// publisher prefix the next word does not repeat (moonshotai_, THUDM_),
// two one it does (Qwen_Qwen...).
var thirteen = []struct{ file, want string }{
	{"/models/DeepSeek-V2-Lite-Chat.Q4_K_M.gguf", "deepseek-v2-lite-chat"},
	{"/models/Qwen_Qwen3.5-4B-Q4_K_M.gguf", "qwen3.5-4b"},
	{"/models/Devstral-Small-2-24B-Instruct-2512-Q4_K_M.gguf", "devstral-small-2-24b-instruct-2512"},
	{"/models/gemma-4-12b-it-Q4_K_M.gguf", "gemma-4-12b"},
	{"/models/Qwen3-Coder-30B-A3B-Instruct-Q4_K_M.gguf", "qwen3-coder-30b-a3b"},
	{"/models/Qwen3.8-27B-Q4_K_M.gguf", "qwen3.8-27b"},
	{"/models/Qwen2.5-72B-Instruct-Q2_K.gguf", "qwen2.5-72b"},
	{"/models/THUDM_GLM-4-32B-0414-Q4_K_M.gguf", "glm-4-32b-0414"}, // was thudm_glm-4-32b-0414
	{"/models/Llama-3.3-70B-Instruct-Q2_K.gguf", "llama-3.3-70b"},
	{"/models/Qwen_Qwen3-30B-A3B-Instruct-2507-Q4_K_M.gguf", "qwen3-30b-a3b-instruct-2507"},
	{"/models/mixtral-8x7b-instruct-v0.1-stacked.Q4_K_M.gguf", "mixtral-8x7b-instruct-v0.1-stacked"},
	{"/models/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf", "tinyllama-1.1b-chat-v1.0"},
	{"/models/moonshotai_Kimi-Dev-72B-IQ2_S.gguf", "kimi-dev-72b"}, // was moonshotai_kimi-dev-72b
}

// A leading word followed by an underscore, with no digit and no hyphen, is
// a publisher and is dropped; only the two prefixed names of the thirteen
// change.
func TestNamer_DropsAPublisherPrefix(t *testing.T) {
	files := make([]string, len(thirteen))
	for i, c := range thirteen {
		files[i] = c.file
	}
	n := NewNamer(files)
	for _, c := range thirteen {
		if got := n.Name(c.file); got != c.want {
			t.Errorf("Name(%q) = %q, want %q", c.file, got, c.want)
		}
	}
}

func TestNamer_PublisherPrefixCases(t *testing.T) {
	for _, c := range []struct {
		name    string
		catalog []string
		file    string
		want    string
	}{
		{"google", nil, "/models/google_gemma-3-4b-it-Q4_0.gguf", "gemma-3-4b"},
		{"upper case", nil, "/models/MOONSHOTAI_Kimi-Dev-72B-Q4_K_M.gguf", "kimi-dev-72b"},
		{"only a publisher and an underscore", nil, "/models/google_.gguf", "google"},
		{"only a publisher, an underscore and a quantisation", nil, "/models/google_Q4_K_M.gguf", "google"},
		{"a word with a digit stays", nil, "/models/llama3_8b-chat-Q4_0.gguf", "llama3_8b-chat"},
		{"a word with a hyphen stays", nil, "/models/meta-llama_Llama-3.2-1B-Q4_0.gguf", "meta-llama_llama-3.2-1b"},
		{"a hyphen after the first word is not a prefix", nil, "/models/Mistral-Small-3.1-24B-Q4_0.gguf", "mistral-small-3.1-24b"},
		{"two words before the underscore", nil, "/models/foo.bar_baz-7b.gguf", "foo.bar_baz-7b"},
		// Collision: without the prefix two catalog files share a name, so
		// the prefix stays (the other file has none to keep); the suffix
		// rule still applies after it.
		{"collision keeps the prefix", []string{"/models/google_gemma-3-4b-it-Q4_0.gguf", "/models/gemma-3-4b-it-Q8_0.gguf"},
			"/models/google_gemma-3-4b-it-Q4_0.gguf", "google_gemma-3-4b"},
		{"collision, the other file", []string{"/models/google_gemma-3-4b-it-Q4_0.gguf", "/models/gemma-3-4b-it-Q8_0.gguf"},
			"/models/gemma-3-4b-it-Q8_0.gguf", "gemma-3-4b"},
		// Two publishers of one model: both keep theirs.
		{"two publishers", []string{"/models/thudm_glm-4-9b-Q4_0.gguf", "/models/zai_glm-4-9b-Q4_0.gguf"},
			"/models/zai_glm-4-9b-Q4_0.gguf", "zai_glm-4-9b"},
		// No collision through the publisher, but through the suffix: the
		// prefix goes, the suffix stays.
		{"publisher dropped, suffix kept", []string{"/models/google_gemma-4-27b-it-Q4_0.gguf", "/models/gemma-4-27b-Q8_0.gguf"},
			"/models/google_gemma-4-27b-it-Q4_0.gguf", "gemma-4-27b-it"},
		// The same file in another quantisation is not a collision.
		{"two quantisations", []string{"/models/THUDM_GLM-4-32B-0414-Q4_K_M.gguf", "/models/THUDM_GLM-4-32B-0414-Q8_0.gguf"},
			"/models/THUDM_GLM-4-32B-0414-Q8_0.gguf", "glm-4-32b-0414"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := NewNamer(c.catalog).Name(c.file); got != c.want {
				t.Errorf("Name(%q) = %q, want %q", c.file, got, c.want)
			}
		})
	}
	var none *Namer
	if got := none.Name("/models/THUDM_GLM-4-32B-0414-Q4_K_M.gguf"); got != "glm-4-32b-0414" {
		t.Errorf("nil namer: %q", got)
	}
}
