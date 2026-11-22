package release

import (
	"os"
	"path/filepath"
	"testing"
)

// LlamaBuildOf reads only release.json: an older release's folder that keeps
// nothing else still answers; a missing or bad one is an error.
func TestLlamaBuildOf(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "0.1.4"), 0o755)
	os.WriteFile(filepath.Join(root, "0.1.4", "release.json"), []byte(`{"release":"0.1.4","builds":{"llama":"a01665d3"}}`), 0o644)
	os.MkdirAll(filepath.Join(root, "0.1.3"), 0o755)
	os.WriteFile(filepath.Join(root, "0.1.3", "release.json"), []byte(`{"release":"0.1.2","builds":{"llama":"a01665d3"}}`), 0o644)
	if b, err := LlamaBuildOf(root, "0.1.4"); err != nil || b != "a01665d3" {
		t.Fatalf("0.1.4: %q %v", b, err)
	}
	for _, rel := range []string{"0.1.3", "0.0.1", "", "../x"} {
		if b, err := LlamaBuildOf(root, rel); err == nil {
			t.Errorf("%q: %q, want an error", rel, b)
		}
	}
}
