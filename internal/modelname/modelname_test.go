package modelname

import "testing"

// The same rule as the page's fileShort (internal/api/ui/next/core/ui.js),
// with the examples its tests use (ui_overview_fixes_test.go).
func TestShort(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/models/sub/Gemma-4-27B-it-Q4_K_M.gguf", "gemma-4-27b-it"},
		{"/models/Mistral-Small-3.1-24B-Q5_K_M.gguf", "mistral-small-3.1-24b"},
		{"/models/Qwen_Qwen3.5-9B-Q4_K_M.gguf", "qwen3.5-9b"},
		{"/models/DEEPSEEK_DeepSeek-R1-Distill-Q4_K_M.gguf", "deepseek-r1-distill"},
		{"/models/google_gemma-3-4b-it-Q4_0.gguf", "google_gemma-3-4b-it"},
		{`C:\models\Qwen3.5-4B-IQ3_XS.gguf`, "qwen3.5-4b"},
		{"model-BF16.gguf", "model"},
		{"Llama-3.2-1B-Instruct-F16.GGUF", "llama-3.2-1b-instruct"},
		{"plain", "plain"},
		{"Q4_K_M.gguf", ""},
		{"", ""},
		{"/models/", ""},
	} {
		if got := Short(c.in); got != c.want {
			t.Errorf("Short(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
