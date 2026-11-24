package modelname

import "testing"

// The short name drops "-it" and "-instruct" (the design shows
// "gemma-4-27b"), except when two model files of the catalog would then
// share one name: those keep the suffix, both of them.
func TestNamer_DropsTheSuffixUnlessTwoFilesWouldShareTheName(t *testing.T) {
	n := NewNamer([]string{
		"/models/Gemma-4-27B-it-Q4_K_M.gguf",       // alone: gemma-4-27b
		"/models/Llama-3.2-1B-Instruct-Q8_0.gguf",  // collides with the base model below
		"/models/Llama-3.2-1B-Q8_0.gguf",           // the base model
		"/models/Qwen_Qwen3.5-9B-Q4_K_M.gguf",      // no suffix
		"/models/Qwen_Qwen3.5-9B-Q8_0.gguf",        // the same model in another quantisation: the same name, no collision
		"/models/google_gemma-3-4b-it-Q4_0.gguf",   // an unrepeated publisher stays
		"/models/Mistral-7B-Instruct-v0.3-Q4.gguf", // the suffix is not last: kept
	})
	for _, c := range []struct{ file, want string }{
		{"/models/Gemma-4-27B-it-Q4_K_M.gguf", "gemma-4-27b"},
		{"/models/Llama-3.2-1B-Instruct-Q8_0.gguf", "llama-3.2-1b-instruct"},
		{"/models/Llama-3.2-1B-Q8_0.gguf", "llama-3.2-1b"},
		{"/models/Qwen_Qwen3.5-9B-Q4_K_M.gguf", "qwen3.5-9b"},
		{"/models/Qwen_Qwen3.5-9B-Q8_0.gguf", "qwen3.5-9b"},
		{"/models/google_gemma-3-4b-it-Q4_0.gguf", "google_gemma-3-4b"},
		{"/models/Mistral-7B-Instruct-v0.3-Q4.gguf", "mistral-7b-instruct-v0.3"},
		// a file the catalog does not have (a run of a model since removed):
		// named against the catalog as it is now
		{"/models/Gemma-4-27B-it-Q8_0.gguf", "gemma-4-27b"},
		{"/models/Gemma-4-27B-Q8_0.gguf", "gemma-4-27b"}, // its own name, no suffix to drop
		{"/models/Llama-3.2-1B-Instruct-F16.gguf", "llama-3.2-1b-instruct"},
		{"/models/Phi-4-mini-it-Q4_0.gguf", "phi-4-mini"},
		{"", ""},
	} {
		if got := n.Name(c.file); got != c.want {
			t.Errorf("Name(%q) = %q, want %q", c.file, got, c.want)
		}
	}
	// With no catalog (nil), the suffix is dropped.
	var none *Namer
	if got := none.Name("/models/Llama-3.2-1B-Instruct-Q8_0.gguf"); got != "llama-3.2-1b" {
		t.Errorf("nil namer: %q", got)
	}
}

// Adding the colliding file changes the name of the one already there: the
// name is derived each time from the catalog as it is.
func TestNamer_ACollidingFileArrives(t *testing.T) {
	it := "/models/Gemma-4-27B-it-Q4_K_M.gguf"
	before := NewNamer([]string{it})
	after := NewNamer([]string{it, "/models/gemma-4-27b-Q4_K_M.gguf"})
	if before.Name(it) != "gemma-4-27b" || after.Name(it) != "gemma-4-27b-it" || after.Name("/models/gemma-4-27b-Q4_K_M.gguf") != "gemma-4-27b" {
		t.Fatalf("before %q, after %q and %q", before.Name(it), after.Name(it), after.Name("/models/gemma-4-27b-Q4_K_M.gguf"))
	}
}
